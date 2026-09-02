package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/openaccountants/firefly-iii-ai-categorize/internal/cache"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/classifier"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/config"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/firefly"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/job"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/pipeline"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/worker"
)

type Handler struct {
	baseCfg     *config.Config // original env-var config (never mutated)
	store       *config.Store
	registry    *job.Registry
	webhookPool *worker.Pool
	batchPool   *worker.Pool

	// Hot-swappable clients, protected by mu.
	mu        sync.RWMutex
	fc        *firefly.Client
	histCache *cache.Cache
	pipe      *pipeline.Pipeline

	// Transfer conversion history: description → asset account ID.
	transferHistory   map[string]string
	transferHistoryMu sync.RWMutex
}

func New(
	baseCfg *config.Config,
	store *config.Store,
	reg *job.Registry,
	webhookPool, batchPool *worker.Pool,
) (*Handler, error) {
	h := &Handler{
		baseCfg:         baseCfg,
		store:           store,
		registry:        reg,
		webhookPool:     webhookPool,
		batchPool:       batchPool,
		transferHistory: make(map[string]string),
	}
	// Best-effort: if not configured yet, server still starts.
	if err := h.reloadClients(); err != nil {
		slog.Warn("clients not fully initialised", "error", err)
	}
	return h, nil
}

func (h *Handler) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	r.Get("/health", h.health)
	r.Post("/webhook", h.webhookHandler)
	r.Post("/batch/run", h.batchRun)

	// Job endpoints
	r.Get("/jobs", h.listJobs)
	r.Get("/jobs/{id}", h.getJob)

	// API endpoints for the UI
	r.Get("/api/config", h.getConfig)
	r.Put("/api/config", h.putConfig)
	r.Post("/api/config/test", h.testFirefly)
	r.Get("/api/theme", h.getTheme)
	r.Get("/api/categories", h.getCategories)
	r.Get("/api/accounts", h.getAccounts)
	r.Get("/api/transactions", h.getTransactions)
	r.Get("/api/review", h.getReview)
	r.Put("/api/transactions/{id}/categorize", h.categorizeTransaction)
	r.Post("/api/transactions/{id}/tags/resolve", h.resolveTags)
	r.Get("/api/transfers/suggest", h.suggestTransferDestination)
	r.Post("/api/transactions/{id}/convert-to-transfer", h.convertToTransfer)

	// SSE
	r.Get("/events", h.events)

	if h.baseCfg.EnableUI {
		r.Handle("/*", http.FileServer(http.Dir("public")))
	}

	return r
}

// --- Health ---

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Webhook ---

// flexString unmarshals a JSON string or number into a string value.
// Firefly III sends integer IDs in webhook payloads but string IDs in the REST API.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

type webhookSplitData struct {
	TransactionJournalID flexString `json:"transaction_journal_id"`
	Type                 string     `json:"type"`
	Description          string     `json:"description"`
	DestinationName      string     `json:"destination_name"`
	Amount               string     `json:"amount"`
	CategoryID           string     `json:"category_id"`
	Tags                 []string   `json:"tags"`
	Notes                string     `json:"notes"`
}

type webhookPayload struct {
	Trigger  string `json:"trigger"`
	Response string `json:"response"`
	Content  struct {
		ID           flexString         `json:"id"`
		Transactions []webhookSplitData `json:"transactions"`
	} `json:"content"`
}

func (h *Handler) webhookHandler(w http.ResponseWriter, r *http.Request) {
	pipe := h.getPipe()
	if pipe == nil {
		http.Error(w, "not configured — set up credentials in Settings", http.StatusServiceUnavailable)
		return
	}

	var payload webhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if payload.Trigger != "STORE_TRANSACTION" {
		slog.Debug("webhook skipped: trigger is not STORE_TRANSACTION", "trigger", payload.Trigger)
		writeJSON(w, http.StatusOK, map[string]interface{}{"skipped": true, "reason": fmt.Sprintf("trigger %q is not STORE_TRANSACTION", payload.Trigger)})
		return
	}
	if payload.Response != "TRANSACTIONS" {
		slog.Debug("webhook skipped: response is not TRANSACTIONS", "response", payload.Response)
		writeJSON(w, http.StatusOK, map[string]interface{}{"skipped": true, "reason": "response is not TRANSACTIONS"})
		return
	}
	if payload.Content.ID == "" {
		slog.Warn("webhook skipped: missing content.id")
		writeJSON(w, http.StatusOK, map[string]interface{}{"skipped": true, "reason": "missing content.id"})
		return
	}
	if len(payload.Content.Transactions) == 0 {
		slog.Debug("webhook skipped: no transactions in payload")
		writeJSON(w, http.StatusOK, map[string]interface{}{"skipped": true, "reason": "no transactions in payload"})
		return
	}

	first := payload.Content.Transactions[0]
	if first.Type != "withdrawal" {
		slog.Debug("webhook skipped: not a withdrawal", "type", first.Type, "txn_id", payload.Content.ID)
		writeJSON(w, http.StatusOK, map[string]interface{}{"skipped": true, "reason": fmt.Sprintf("transaction type %q is not a withdrawal", first.Type)})
		return
	}
	if first.CategoryID != "" && first.CategoryID != "0" {
		slog.Debug("webhook skipped: category already set", "category_id", first.CategoryID, "txn_id", payload.Content.ID)
		writeJSON(w, http.StatusOK, map[string]interface{}{"skipped": true, "reason": "category already set"})
		return
	}
	if first.Description == "" && first.DestinationName == "" {
		slog.Debug("webhook skipped: no description or destination", "txn_id", payload.Content.ID)
		writeJSON(w, http.StatusOK, map[string]interface{}{"skipped": true, "reason": "no description or destination — cannot classify"})
		return
	}

	splits := make([]firefly.Split, len(payload.Content.Transactions))
	for i, t := range payload.Content.Transactions {
		splits[i] = firefly.Split{
			JournalID:       string(t.TransactionJournalID),
			Type:            t.Type,
			Description:     t.Description,
			DestinationName: t.DestinationName,
			Amount:          t.Amount,
			CategoryID:      t.CategoryID,
			Tags:            t.Tags,
			Notes:           t.Notes,
		}
	}

	amount := parseAmount(first.Amount)
	j := h.registry.Create(string(payload.Content.ID), "", first.DestinationName, first.Description, amount)
	transactionID := string(payload.Content.ID)

	h.webhookPool.Submit(worker.Task{
		JobID: j.ID,
		Execute: func(ctx context.Context) error {
			return h.getPipe().Run(ctx, j, transactionID, splits)
		},
	})

	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": j.ID})
}

// --- Batch ---

type batchFilter struct {
	UncategorizedOnly bool     `json:"uncategorized_only"`
	TransactionIDs    []string `json:"transaction_ids"`
}

type batchRequest struct {
	Filter batchFilter `json:"filter"`
	DryRun bool        `json:"dry_run"`
	Force  bool        `json:"force"` // when true, re-classify even if category is set
	Mode   string      `json:"mode"`  // "classify" (default), "destination", "both"
}

func (h *Handler) batchRun(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	pipe := h.getPipe()
	if fc == nil || pipe == nil {
		http.Error(w, "not configured — set up credentials in Settings", http.StatusServiceUnavailable)
		return
	}

	req := batchRequest{Filter: batchFilter{UncategorizedOnly: true}}
	if body, err := io.ReadAll(r.Body); err == nil && len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
			return
		}
	}

	ctx := r.Context()
	var txns []firefly.Transaction
	var err error

	switch {
	case len(req.Filter.TransactionIDs) > 0:
		txns, err = fc.GetTransactionsByIDs(ctx, req.Filter.TransactionIDs)
	case req.Force || !req.Filter.UncategorizedOnly:
		txns, err = fc.GetAllWithdrawals(ctx)
	default:
		txns, err = fc.GetUncategorizedWithdrawals(ctx)
	}

	// Resolve pipeline run options from the request mode.
	pipeOpts := pipeline.RunOptions{ClassifyCategory: true, MatchDestination: pipe.DestinationMatchEnabled()}
	switch req.Mode {
	case "destination":
		pipeOpts = pipeline.RunOptions{ClassifyCategory: false, MatchDestination: true}
	case "both":
		pipeOpts = pipeline.RunOptions{ClassifyCategory: true, MatchDestination: true}
	case "classify", "":
		// Use defaults from config (already set above).
	}

	if err != nil {
		slog.Error("batch: failed to fetch transactions", "error", err)
		http.Error(w, "failed to fetch transactions from Firefly", http.StatusInternalServerError)
		return
	}

	if req.DryRun {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"dry_run":  true,
			"matching": len(txns),
		})
		return
	}

	batchID := uuid.New().String()
	enqueued := 0

	for _, txn := range txns {
		if len(txn.Splits) == 0 {
			continue
		}
		first := txn.Splits[0]
		amount := parseAmount(first.Amount)
		j := h.registry.Create(txn.ID, batchID, first.DestinationName, first.Description, amount)

		txnID := txn.ID
		splits := txn.Splits
		localPipe := pipe

		localOpts := pipeOpts
		h.batchPool.Submit(worker.Task{
			JobID: j.ID,
			Execute: func(ctx context.Context) error {
				return localPipe.RunWithOptions(ctx, j, txnID, splits, localOpts)
			},
		})
		enqueued++
	}

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"batch_id": batchID,
		"enqueued": enqueued,
	})
}

// --- Config API ---

type configResponse struct {
	FireflyURL          string `json:"firefly_url"`
	FireflyTokenSet     bool   `json:"firefly_token_set"`
	AIProvider          string `json:"ai_provider"`
	OpenAIKeySet        bool   `json:"openai_key_set"`
	OpenAIModel         string `json:"openai_model"`
	OpenAIBaseURL       string `json:"openai_base_url"`
	GeminiKeySet        bool   `json:"gemini_key_set"`
	GeminiModel         string `json:"gemini_model"`
	DeepseekKeySet      bool   `json:"deepseek_key_set"`
	DeepseekModel       string `json:"deepseek_model"`
	TagPrefix           string `json:"tag_prefix"`
	CustomSystemContext string `json:"custom_system_context"`
	Configured          bool   `json:"configured"`

	DestinationMatchEnabled bool `json:"destination_match_enabled"`

	TagSuggestEnabled bool `json:"tag_suggest_enabled"`

	SearchEngine string `json:"search_engine"`

	HistoryContextLimit int `json:"history_context_limit"`
	HistoryLookbackDays int `json:"history_lookback_days"`
	WorkerConcurrency   int `json:"worker_concurrency"`
	BatchConcurrency    int `json:"batch_concurrency"`
}

func (h *Handler) getConfig(w http.ResponseWriter, _ *http.Request) {
	cfg := h.effectiveConfig()
	writeJSON(w, http.StatusOK, configResponse{
		FireflyURL:          cfg.FireflyURL,
		FireflyTokenSet:     cfg.FireflyToken != "",
		AIProvider:          cfg.AIProvider,
		OpenAIKeySet:        cfg.OpenAIKey != "",
		OpenAIModel:         cfg.OpenAIModel,
		OpenAIBaseURL:       cfg.OpenAIBaseURL,
		GeminiKeySet:        cfg.GeminiKey != "",
		GeminiModel:         cfg.GeminiModel,
		DeepseekKeySet:      cfg.DeepseekKey != "",
		DeepseekModel:       cfg.DeepseekModel,
		TagPrefix:           cfg.TagPrefix,
		CustomSystemContext: cfg.CustomSystemContext,
		Configured:          cfg.IsConfigured(),

		DestinationMatchEnabled: cfg.DestinationMatchEnabled,

		TagSuggestEnabled: cfg.TagSuggestEnabled,

		SearchEngine: cfg.SearchEngine,

		HistoryContextLimit: cfg.HistoryContextLimit,
		HistoryLookbackDays: cfg.HistoryLookbackDays,
		WorkerConcurrency:   cfg.WorkerConcurrency,
		BatchConcurrency:    cfg.BatchConcurrency,
	})
}

// configUpdateRequest uses pointer types so absent fields (null/missing) mean
// "keep existing". For strings an explicit empty value clears the field; for
// ints a value of 0 or less is rejected.
type configUpdateRequest struct {
	FireflyURL          *string `json:"firefly_url"`
	FireflyToken        *string `json:"firefly_token"`
	AIProvider          *string `json:"ai_provider"`
	OpenAIKey           *string `json:"openai_api_key"`
	OpenAIModel         *string `json:"openai_model"`
	OpenAIBaseURL       *string `json:"openai_base_url"`
	GeminiKey           *string `json:"gemini_api_key"`
	GeminiModel         *string `json:"gemini_model"`
	DeepseekKey         *string `json:"deepseek_api_key"`
	DeepseekModel       *string `json:"deepseek_model"`
	TagPrefix           *string `json:"tag_prefix"`
	CustomSystemContext *string `json:"custom_system_context"`

	DestinationMatchEnabled *bool `json:"destination_match_enabled"`

	TagSuggestEnabled *bool `json:"tag_suggest_enabled"`

	SearchEngine *string `json:"search_engine"`

	HistoryContextLimit *int `json:"history_context_limit"`
	HistoryLookbackDays *int `json:"history_lookback_days"`
	WorkerConcurrency   *int `json:"worker_concurrency"`
	BatchConcurrency    *int `json:"batch_concurrency"`
}

func (h *Handler) putConfig(w http.ResponseWriter, r *http.Request) {
	var req configUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Merge non-nil fields from the request into the existing stored config.
	existing := h.store.Get()
	updated := mergeConfigUpdate(existing, req)

	if err := h.store.Save(updated); err != nil {
		http.Error(w, fmt.Sprintf("failed to save config: %v", err), http.StatusInternalServerError)
		return
	}

	if err := h.reloadClients(); err != nil {
		slog.Warn("config saved but client reload had errors", "error", err)
	}

	h.getConfig(w, r)
}

type themeResponse struct {
	Dark    bool `json:"dark"`
	Browser bool `json:"browser"` // true = defer to prefers-color-scheme
}

func (h *Handler) getTheme(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		writeJSON(w, http.StatusOK, themeResponse{})
		return
	}
	val, err := fc.GetPreference(r.Context(), "darkMode")
	if err != nil {
		slog.Warn("theme: could not fetch darkMode preference", "error", err)
		writeJSON(w, http.StatusOK, themeResponse{})
		return
	}
	slog.Info("theme: darkMode preference", "value", val, "type", fmt.Sprintf("%T", val))
	if s, ok := val.(string); ok && s == "browser" {
		writeJSON(w, http.StatusOK, themeResponse{Browser: true})
		return
	}
	writeJSON(w, http.StatusOK, themeResponse{Dark: preferenceIsTruthy(val)})
}

// preferenceIsTruthy coerces a Firefly preference value to bool.
// The JSON decoder produces bool for JSON true/false, float64 for numbers,
// and string for quoted values — all of which Firefly versions have used.
func preferenceIsTruthy(val interface{}) bool {
	switch v := val.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v == "true" || v == "1" || v == "yes" || v == "dark"
	}
	return false
}

type testFireflyRequest struct {
	FireflyURL   string `json:"firefly_url"`
	FireflyToken string `json:"firefly_token"`
}

func (h *Handler) testFirefly(w http.ResponseWriter, r *http.Request) {
	var req testFireflyRequest
	// Body is optional — ignore decode errors so a bare POST still works.
	json.NewDecoder(r.Body).Decode(&req)

	var fc *firefly.Client
	if req.FireflyURL != "" {
		token := req.FireflyToken
		if token == "" {
			// No token supplied — fall back to the saved one so the user can
			// test a new URL without re-entering their existing token.
			token = h.effectiveConfig().FireflyToken
		}
		fc = firefly.New(req.FireflyURL, token, "")
	} else {
		fc = h.getFC()
	}

	if fc == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":    false,
			"error": "Firefly URL and token not configured",
		})
		return
	}
	cats, err := fc.GetCategories(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":         true,
		"categories": len(cats),
	})
}

// --- Categories ---

func (h *Handler) getCategories(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}
	cats, err := fc.GetCategories(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch categories: %v", err), http.StatusBadGateway)
		return
	}
	if cats == nil {
		cats = []firefly.Category{}
	}
	writeJSON(w, http.StatusOK, cats)
}

// --- Accounts ---

func (h *Handler) getAccounts(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}
	acctType := r.URL.Query().Get("type")
	var (
		accts []firefly.Account
		err   error
	)
	if acctType == "asset" {
		accts, err = fc.GetAssetAccounts(r.Context())
	} else {
		accts, err = fc.GetExpenseAccounts(r.Context())
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch accounts: %v", err), http.StatusBadGateway)
		return
	}
	if accts == nil {
		accts = []firefly.Account{}
	}
	writeJSON(w, http.StatusOK, accts)
}

// --- Review ---

type reviewTxn struct {
	ID     string `json:"id"`
	Date   string `json:"date"`
	Amount string `json:"amount"`
}

type reviewGroup struct {
	Outcome              string      `json:"outcome"`
	Description          string      `json:"description"`
	DestinationName      string      `json:"destination_name"`
	SourceName           string      `json:"source_name,omitempty"`
	CategoryName         string      `json:"category_name,omitempty"`
	CategoryID           string      `json:"category_id,omitempty"`
	DestinationAccountID string      `json:"destination_account_id,omitempty"`
	Transactions         []reviewTxn `json:"transactions"`
}

func (h *Handler) getReview(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}

	rg, err := fc.GetReviewGroups(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch review groups: %v", err), http.StatusBadGateway)
		return
	}

	var result []*reviewGroup
	result = append(result, buildReviewGroups("NEEDS_REVIEW", rg.NeedsReview)...)
	result = append(result, buildReviewGroups("ASSUMED", rg.Assumed)...)
	result = append(result, buildReviewGroups("DEST_ASSUMED", rg.DestAssumed)...)
	result = append(result, buildReviewGroups("TRANSFER_CATEGORY", rg.TransferCategory)...)

	writeJSON(w, http.StatusOK, result)
}

func buildReviewGroups(outcome string, txns []firefly.Transaction) []*reviewGroup {
	groups := make(map[string]*reviewGroup)
	var order []string

	for _, txn := range txns {
		if len(txn.Splits) == 0 {
			continue
		}
		s := txn.Splits[0]
		key := s.Description
		if key == "" {
			key = s.DestinationName
		}

		if _, ok := groups[key]; !ok {
			g := &reviewGroup{
				Outcome:              outcome,
				Description:          s.Description,
				DestinationName:      s.DestinationName,
				SourceName:           s.SourceName,
				DestinationAccountID: s.DestinationID,
			}
			if outcome == "ASSUMED" || outcome == "DEST_ASSUMED" {
				g.CategoryName = s.CategoryName
				g.CategoryID = s.CategoryID
			}
			groups[key] = g
			order = append(order, key)
		}
		groups[key].Transactions = append(groups[key].Transactions, reviewTxn{
			ID:     txn.ID,
			Date:   s.Date,
			Amount: s.Amount,
		})
	}

	result := make([]*reviewGroup, 0, len(order))
	for _, key := range order {
		result = append(result, groups[key])
	}
	return result
}

type categorizeRequest struct {
	CategoryID        string `json:"category_id"`
	DestinationAction string `json:"destination_action,omitempty"` // "MATCH", "CREATE", or ""
	DestinationName   string `json:"destination_name,omitempty"`   // for CREATE
	DestinationID     string `json:"destination_id,omitempty"`     // for MATCH
}

func (h *Handler) categorizeTransaction(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}

	id := chi.URLParam(r, "id")
	var req categorizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	// category_id is optional — allowing destination-only updates.
	if req.CategoryID == "" && req.DestinationAction == "" {
		http.Error(w, "at least one of category_id or destination_action is required", http.StatusBadRequest)
		return
	}

	txns, err := fc.GetTransactionsByIDs(r.Context(), []string{id})
	if err != nil || len(txns) == 0 {
		http.Error(w, "transaction not found", http.StatusNotFound)
		return
	}

	destID := ""
	switch req.DestinationAction {
	case "MATCH":
		destID = req.DestinationID
	case "CREATE":
		if req.DestinationName == "" {
			http.Error(w, "destination_name is required for CREATE", http.StatusBadRequest)
			return
		}
		created, err := fc.CreateExpenseAccount(r.Context(), req.DestinationName)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to create destination account: %v", err), http.StatusBadGateway)
			return
		}
		destID = created.ID
		slog.Info("review: created expense account", "name", created.Name, "id", created.ID)
	}

	if err := fc.ApplyHumanCategory(r.Context(), id, txns[0].Splits, req.CategoryID, destID); err != nil {
		http.Error(w, fmt.Sprintf("failed to update transaction: %v", err), http.StatusBadGateway)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type resolveTagsRequest struct {
	Apply  []string `json:"apply"`
	Reject []string `json:"reject"`
}

// resolveTags applies or rejects the AI's low-confidence tag suggestions
// (stored as ai:suggest:<name> markers) on a transaction.
func (h *Handler) resolveTags(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}

	id := chi.URLParam(r, "id")
	var req resolveTagsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Apply) == 0 && len(req.Reject) == 0 {
		http.Error(w, "at least one of apply or reject is required", http.StatusBadRequest)
		return
	}

	txns, err := fc.GetTransactionsByIDs(r.Context(), []string{id})
	if err != nil || len(txns) == 0 {
		http.Error(w, "transaction not found", http.StatusNotFound)
		return
	}

	if err := fc.ResolveSuggestedTags(r.Context(), id, txns[0].Splits, req.Apply, req.Reject); err != nil {
		http.Error(w, fmt.Sprintf("failed to update transaction: %v", err), http.StatusBadGateway)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Transfer conversion ---

type suggestTransferResponse struct {
	AccountID   string `json:"account_id"`
	AccountName string `json:"account_name"`
	Source      string `json:"source"` // "history" or "llm"
}

// suggestTransferDestination suggests a destination asset account for a
// transfer conversion. It checks transfer history first, then falls back
// to the LLM with the list of asset accounts.
func (h *Handler) suggestTransferDestination(w http.ResponseWriter, r *http.Request) {
	pipe := h.getPipe()
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}

	desc := r.URL.Query().Get("description")
	if desc == "" {
		http.Error(w, "description query param is required", http.StatusBadRequest)
		return
	}

	// Check transfer history first.
	h.transferHistoryMu.RLock()
	histID, hasHist := h.transferHistory[strings.ToLower(desc)]
	h.transferHistoryMu.RUnlock()

	if hasHist && histID != "" {
		accts, err := fc.GetAssetAccounts(r.Context())
		if err == nil {
			for _, a := range accts {
				if a.ID == histID {
					writeJSON(w, http.StatusOK, suggestTransferResponse{
						AccountID:   a.ID,
						AccountName: a.Name,
						Source:      "history",
					})
					return
				}
			}
		}
		// History ID no longer valid, fall through to LLM.
	}

	if pipe == nil {
		writeJSON(w, http.StatusOK, suggestTransferResponse{})
		return
	}

	accts, err := fc.GetAssetAccounts(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch asset accounts: %v", err), http.StatusBadGateway)
		return
	}

	// Use a simple LLM prompt to pick the best destination account.
	clAccts := make([]classifier.AccountCandidate, len(accts))
	for i, a := range accts {
		clAccts[i] = classifier.AccountCandidate{ID: a.ID, Name: a.Name}
	}

	result, err := pipe.SuggestTransfer(r.Context(), desc, clAccts)
	if err != nil {
		slog.Error("transfer suggest failed", "error", err)
		writeJSON(w, http.StatusOK, suggestTransferResponse{})
		return
	}

	// Validate the LLM's suggestion.
	for _, a := range accts {
		if strings.EqualFold(a.Name, result.AccountName) {
			writeJSON(w, http.StatusOK, suggestTransferResponse{
				AccountID:   a.ID,
				AccountName: a.Name,
				Source:      "llm",
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, suggestTransferResponse{})
}

type convertToTransferRequest struct {
	DestinationID string `json:"destination_id"`
}

// convertToTransfer deletes a withdrawal and creates a transfer in its place.
func (h *Handler) convertToTransfer(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}

	id := chi.URLParam(r, "id")
	var req convertToTransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.DestinationID == "" {
		http.Error(w, "destination_id is required", http.StatusBadRequest)
		return
	}

	txns, err := fc.GetTransactionsByIDs(r.Context(), []string{id})
	if err != nil || len(txns) == 0 {
		http.Error(w, "transaction not found", http.StatusNotFound)
		return
	}

	txn := txns[0]
	if len(txn.Splits) == 0 {
		http.Error(w, "transaction has no splits", http.StatusBadRequest)
		return
	}

	s := txn.Splits[0]
	if s.SourceID == "" {
		http.Error(w, "transaction has no source account", http.StatusBadRequest)
		return
	}

	// Build tags: keep existing tags, remove AI outcome tags, add transfer tag.
	tagPrefix := h.effectiveConfig().TagPrefix
	aiOutcomeTags := map[string]bool{
		tagPrefix + ":needs-review": true,
		tagPrefix + ":assumed":      true,
		tagPrefix + ":classified":   true,
	}
	var tags []string
	for _, t := range s.Tags {
		if !aiOutcomeTags[t] {
			tags = append(tags, t)
		}
	}
	transferTag := tagPrefix + ":transfer"
	if !containsTag(tags, transferTag) {
		tags = append(tags, transferTag)
	}

	// Preserve AI notes but prepend the transfer note.
	notes := "Converted from withdrawal (AI categorized as Transfers)."
	if s.Notes != "" {
		notes = notes + "\n\n" + s.Notes
	}

	_, err = fc.CreateTransfer(r.Context(), firefly.CreateTransferParams{
		Date:          s.Date,
		Amount:        s.Amount,
		Description:   s.Description,
		SourceID:      s.SourceID,
		DestinationID: req.DestinationID,
		Notes:         notes,
		Tags:          tags,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create transfer: %v", err), http.StatusBadGateway)
		return
	}

	// Delete the original withdrawal.
	if err := fc.DeleteTransaction(r.Context(), id); err != nil {
		slog.Error("transfer converted but failed to delete original", "id", id, "error", err)
	}

	// Record in transfer history for future suggestions.
	h.transferHistoryMu.Lock()
	h.transferHistory[strings.ToLower(s.Description)] = req.DestinationID
	h.transferHistoryMu.Unlock()

	slog.Info("converted withdrawal to transfer", "old_id", id, "description", s.Description)
	w.WriteHeader(http.StatusNoContent)
}

func containsTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// --- Transactions list ---

func (h *Handler) getTransactions(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query()
	start := q.Get("start")
	end := q.Get("end")
	missingCategory := q.Get("missing_category") == "true"
	missingDestination := q.Get("missing_destination") == "true"
	destFilter := q.Get("destination")
	categoryFilter := q.Get("category")

	// When filtering is active, we must fetch all pages, filter, and re-paginate.
	if missingCategory || missingDestination || destFilter != "" || categoryFilter != "" {
		// ids_only mode with filters — collect all matching IDs.
		if q.Get("ids_only") == "true" {
			var ids []string
			for page := 1; ; page++ {
				result, err := fc.GetWithdrawalsPage(r.Context(), page, 200, start, end)
				if err != nil {
					http.Error(w, fmt.Sprintf("failed to fetch transactions: %v", err), http.StatusBadGateway)
					return
				}
				for _, row := range result.Data {
					if matchesTxnFilter(row, missingCategory, missingDestination, destFilter, categoryFilter) {
						ids = append(ids, row.ID)
					}
				}
				if page >= result.TotalPages {
					break
				}
			}
			writeJSON(w, http.StatusOK, ids)
			return
		}

		// Normal paged mode with filters.
		pageNum := queryInt(q.Get("page"), 1)
		limit := queryInt(q.Get("limit"), 50)
		var allFiltered []firefly.TransactionRow
		for page := 1; ; page++ {
			result, err := fc.GetWithdrawalsPage(r.Context(), page, 200, start, end)
			if err != nil {
				http.Error(w, fmt.Sprintf("failed to fetch transactions: %v", err), http.StatusBadGateway)
				return
			}
			for _, row := range result.Data {
				if matchesTxnFilter(row, missingCategory, missingDestination, destFilter, categoryFilter) {
					allFiltered = append(allFiltered, row)
				}
			}
			if page >= result.TotalPages {
				break
			}
		}

		// Paginate filtered results.
		total := len(allFiltered)
		totalPages := (total + limit - 1) / limit
		if totalPages < 1 {
			totalPages = 1
		}
		startIdx := (pageNum - 1) * limit
		endIdx := startIdx + limit
		if startIdx > total {
			startIdx = total
		}
		if endIdx > total {
			endIdx = total
		}
		pageData := allFiltered[startIdx:endIdx]

		writeJSON(w, http.StatusOK, firefly.TransactionsPage{
			Data:       pageData,
			Page:       pageNum,
			TotalPages: totalPages,
			Total:      total,
		})
		return
	}

	// ids_only=true: return all matching IDs across all pages (used for select-all)
	if q.Get("ids_only") == "true" {
		var ids []string
		for page := 1; ; page++ {
			result, err := fc.GetWithdrawalsPage(r.Context(), page, 200, start, end)
			if err != nil {
				http.Error(w, fmt.Sprintf("failed to fetch transactions: %v", err), http.StatusBadGateway)
				return
			}
			for _, row := range result.Data {
				ids = append(ids, row.ID)
			}
			if page >= result.TotalPages {
				break
			}
		}
		writeJSON(w, http.StatusOK, ids)
		return
	}

	pageNum := queryInt(q.Get("page"), 1)
	limit := queryInt(q.Get("limit"), 50)

	result, err := fc.GetWithdrawalsPage(r.Context(), pageNum, limit, start, end)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch transactions: %v", err), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// matchesTxnFilter checks whether a transaction row matches the active UI filters.
func matchesTxnFilter(row firefly.TransactionRow, missingCategory, missingDestination bool, destFilter, categoryFilter string) bool {
	if missingCategory && row.CategoryName != "" {
		return false
	}
	if categoryFilter != "" && !strings.EqualFold(row.CategoryName, categoryFilter) {
		return false
	}
	if missingDestination {
		dn := strings.TrimSpace(row.DestinationName)
		if dn != "" && !strings.EqualFold(dn, "(no name)") {
			return false
		}
	}
	if destFilter != "" {
		dn := strings.TrimSpace(row.DestinationName)
		if destFilter == "(no name)" {
			if dn != "" && !strings.EqualFold(dn, "(no name)") {
				return false
			}
		} else if !strings.EqualFold(dn, destFilter) {
			return false
		}
	}
	return true
}

// --- Job endpoints ---

func (h *Handler) listJobs(w http.ResponseWriter, r *http.Request) {
	batchID := r.URL.Query().Get("batch")
	var jobs []*job.Job
	if batchID != "" {
		jobs = h.registry.ListByBatch(batchID)
	} else {
		jobs = h.registry.List()
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (h *Handler) getJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	j, ok := h.registry.Get(id)
	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// --- SSE ---

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	if data, err := json.Marshal(map[string]interface{}{
		"type": "snapshot",
		"jobs": h.registry.List(),
	}); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	ch := h.registry.Subscribe()
	defer h.registry.Unsubscribe(ch)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case event, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// --- Hot-reload ---

// effectiveConfig returns the current config: env vars overlaid with stored config.
func (h *Handler) effectiveConfig() *config.Config {
	cfg := *h.baseCfg // shallow copy
	config.ApplyStored(&cfg, h.store.Get())
	return &cfg
}

// reloadClients rebuilds the Firefly client, history cache, and pipeline from
// the current effective config. Safe to call concurrently.
func (h *Handler) reloadClients() error {
	cfg := h.effectiveConfig()

	if cfg.FireflyURL == "" || cfg.FireflyToken == "" {
		h.mu.Lock()
		h.fc = nil
		h.histCache = nil
		h.pipe = nil
		h.mu.Unlock()
		return fmt.Errorf("FIREFLY_URL and FIREFLY_PERSONAL_TOKEN are required")
	}

	fc := firefly.New(cfg.FireflyURL, cfg.FireflyToken, cfg.TagPrefix)
	ca := cache.New(fc, cfg.HistoryCacheTTL, cfg.HistoryLookbackDays)

	cl, err := classifier.Build(classifier.BuildParams{
		Provider:      cfg.AIProvider,
		OpenAIKey:     cfg.OpenAIKey,
		OpenAIModel:   cfg.OpenAIModel,
		OpenAIBaseURL: cfg.OpenAIBaseURL,
		GeminiKey:     cfg.GeminiKey,
		GeminiModel:   cfg.GeminiModel,
		DeepseekKey:   cfg.DeepseekKey,
		DeepseekModel: cfg.DeepseekModel,
		CustomContext: cfg.CustomSystemContext,
	})
	if err != nil {
		// Set firefly client even if AI fails — categories/transactions still work.
		h.mu.Lock()
		h.fc = fc
		h.histCache = ca
		h.pipe = nil
		h.mu.Unlock()
		return fmt.Errorf("classifier init: %w", err)
	}

	pipe := pipeline.New(fc, cl, ca, h.registry, cfg.HistoryContextLimit, cfg.DestinationMatchEnabled, cfg.TagSuggestEnabled, cfg.TagSuggestMax)

	h.mu.Lock()
	h.fc = fc
	h.histCache = ca
	h.pipe = pipe
	h.mu.Unlock()

	slog.Info("clients reloaded", "provider", cfg.AIProvider, "firefly", cfg.FireflyURL)
	return nil
}

func (h *Handler) getFC() *firefly.Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.fc
}

func (h *Handler) getPipe() *pipeline.Pipeline {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.pipe
}

// --- Helpers ---

func mergeConfigUpdate(existing config.StoredConfig, req configUpdateRequest) config.StoredConfig {
	if req.FireflyURL != nil {
		existing.FireflyURL = *req.FireflyURL
	}
	if req.FireflyToken != nil && *req.FireflyToken != "" {
		existing.FireflyToken = *req.FireflyToken
	}
	if req.AIProvider != nil {
		existing.AIProvider = *req.AIProvider
	}
	if req.OpenAIKey != nil && *req.OpenAIKey != "" {
		existing.OpenAIKey = *req.OpenAIKey
	}
	if req.OpenAIModel != nil {
		existing.OpenAIModel = *req.OpenAIModel
	}
	if req.OpenAIBaseURL != nil {
		existing.OpenAIBaseURL = *req.OpenAIBaseURL
	}
	if req.GeminiKey != nil && *req.GeminiKey != "" {
		existing.GeminiKey = *req.GeminiKey
	}
	if req.GeminiModel != nil {
		existing.GeminiModel = *req.GeminiModel
	}
	if req.DeepseekKey != nil && *req.DeepseekKey != "" {
		existing.DeepseekKey = *req.DeepseekKey
	}
	if req.DeepseekModel != nil {
		existing.DeepseekModel = *req.DeepseekModel
	}
	if req.TagPrefix != nil {
		existing.TagPrefix = *req.TagPrefix
	}
	if req.CustomSystemContext != nil {
		existing.CustomSystemContext = *req.CustomSystemContext
	}
	if req.DestinationMatchEnabled != nil {
		existing.DestinationMatchEnabled = req.DestinationMatchEnabled
	}
	if req.TagSuggestEnabled != nil {
		existing.TagSuggestEnabled = req.TagSuggestEnabled
	}
	if req.SearchEngine != nil {
		existing.SearchEngine = *req.SearchEngine
	}
	if req.HistoryContextLimit != nil && *req.HistoryContextLimit > 0 {
		existing.HistoryContextLimit = *req.HistoryContextLimit
	}
	if req.HistoryLookbackDays != nil && *req.HistoryLookbackDays > 0 {
		existing.HistoryLookbackDays = *req.HistoryLookbackDays
	}
	if req.WorkerConcurrency != nil && *req.WorkerConcurrency > 0 {
		existing.WorkerConcurrency = *req.WorkerConcurrency
	}
	if req.BatchConcurrency != nil && *req.BatchConcurrency > 0 {
		existing.BatchConcurrency = *req.BatchConcurrency
	}
	return existing
}

func parseAmount(s string) *float64 {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

func queryInt(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
