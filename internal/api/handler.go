package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/openaccountants/firefly-iii-ai-categorize/internal/aidb"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/amazon"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/cache"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/classifier"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/config"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/firefly"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/job"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/mailorder"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/pipeline"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/version"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/worker"
)

type Handler struct {
	baseCfg     *config.Config // original env-var config (never mutated)
	store       *config.Store
	registry    *job.Registry
	aidb        *aidb.DB
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

	// Short-lived cache of computed review groups. The UI polls /api/review
	// every 30s for the badge (and on every page load), and each computation
	// scans all withdrawals — so without this, a slow Firefly gets hammered.
	reviewMu         sync.Mutex
	reviewCache      []*reviewGroup
	reviewCachedAt   time.Time
	reviewRefreshing bool
	reviewSplits     map[string][]firefly.Split // txn id -> splits, to skip a re-fetch on validate

	// Async bulk-apply jobs with progress, so big batches don't block the UI.
	bulkMu   sync.Mutex
	bulkJobs map[string]*bulkProgress
}

type bulkProgress struct {
	Total   int  `json:"total"`
	Done    int  `json:"done"`
	Failed  int  `json:"failed"`
	Running bool `json:"running"`
}

func New(
	baseCfg *config.Config,
	store *config.Store,
	reg *job.Registry,
	webhookPool, batchPool *worker.Pool,
	adb *aidb.DB,
) (*Handler, error) {
	h := &Handler{
		baseCfg:         baseCfg,
		store:           store,
		registry:        reg,
		webhookPool:     webhookPool,
		batchPool:       batchPool,
		aidb:            adb,
		transferHistory: make(map[string]string),
		bulkJobs:        make(map[string]*bulkProgress),
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
	r.Get("/api/version", h.getVersion)
	r.Get("/api/gemini/models", h.getGeminiModels)
	r.Post("/api/mail/test", h.testMail)
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
	r.Get("/api/tags", h.getTags)
	r.Get("/api/transactions/{id}", h.getTransaction)
	r.Get("/api/transactions/{id}/automatch", h.getAutoMatch)
	r.Get("/api/transactions/{id}/similar", h.getSimilar)
	r.Post("/api/transactions/edit-bulk", h.editBulk)
	r.Get("/api/transactions/edit-bulk/{id}", h.bulkStatus)
	r.Put("/api/transactions/{id}/edit", h.editTransaction)
	r.Post("/api/transactions/details", h.getTransactionDetails)
	r.Get("/api/transactions", h.getTransactions)
	r.Get("/api/review", h.getReview)
	r.Put("/api/transactions/{id}/categorize", h.categorizeTransaction)
	r.Post("/api/transactions/{id}/tags/resolve", h.resolveTags)
	r.Post("/api/purge-ai-tags", h.purgeAITags)
	r.Post("/api/transactions/{id}/unreview", h.unreviewTransaction)
	r.Post("/api/transactions/{id}/dismiss", h.dismissTransaction)
	r.Post("/api/transactions/{id}/rerun", h.rerunTransaction)
	r.Post("/api/jobs/purge", h.purgeJobs)
	r.Post("/api/transactions/mark-treated", h.markTreated)
	r.Get("/api/transfers/suggest", h.suggestTransferDestination)
	r.Post("/api/transactions/{id}/convert-to-transfer", h.convertToTransfer)

	// SSE
	r.Get("/events", h.events)

	if h.baseCfg.EnableUI {
		fs := http.FileServer(http.Dir("public"))
		r.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			// Force revalidation so a freshly deployed UI is never served stale
			// from the browser cache (ETag/Last-Modified still yield 304s).
			w.Header().Set("Cache-Control", "no-cache")
			fs.ServeHTTP(w, req)
		}))
	}

	return r
}

// --- Health ---

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// maskMailAccounts returns a copy with IMAP passwords blanked for the UI.
func maskMailAccounts(in []config.MailAccount) []config.MailAccount {
	out := make([]config.MailAccount, len(in))
	for i, m := range in {
		m.IMAPPassword = ""
		out[i] = m
	}
	return out
}

// mergeMailAccounts replaces the stored list with the incoming one, generating
// IDs for new entries and keeping the existing password when a blank one is sent.
func mergeMailAccounts(existing, incoming []config.MailAccount) []config.MailAccount {
	byID := map[string]config.MailAccount{}
	for _, m := range existing {
		byID[m.ID] = m
	}
	out := make([]config.MailAccount, 0, len(incoming))
	for _, m := range incoming {
		if m.ID == "" {
			m.ID = uuid.New().String()
		}
		if m.IMAPPassword == "" {
			if old, ok := byID[m.ID]; ok {
				m.IMAPPassword = old.IMAPPassword
			}
		}
		out = append(out, m)
	}
	return out
}

func mergeMailDetectors(incoming []config.MailDetector) []config.MailDetector {
	out := make([]config.MailDetector, 0, len(incoming))
	for _, d := range incoming {
		if d.ID == "" {
			d.ID = uuid.New().String()
		}
		out = append(out, d)
	}
	return out
}

// testMail verifies IMAP connectivity for an account. Uses the posted password,
// or the stored one for the given account ID when blank.
func (h *Handler) testMail(w http.ResponseWriter, r *http.Request) {
	var m config.MailAccount
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if m.IMAPPassword == "" && m.ID != "" {
		for _, ex := range h.effectiveConfig().MailAccounts {
			if ex.ID == m.ID {
				m.IMAPPassword = ex.IMAPPassword
				break
			}
		}
	}
	if err := mailorder.Test(mailorder.Account{
		Host: m.IMAPHost, Port: m.IMAPPort, User: m.IMAPUser, Password: m.IMAPPassword,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) getGeminiModels(w http.ResponseWriter, r *http.Request) {
	cfg := h.effectiveConfig()
	if cfg.GeminiKey == "" {
		http.Error(w, "Gemini API key not configured", http.StatusBadRequest)
		return
	}

	type modelEntry struct {
		Name                       string   `json:"name"`
		SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	}
	type modelsResp struct {
		Models        []modelEntry `json:"models"`
		NextPageToken string       `json:"nextPageToken"`
	}

	var names []string
	seen := map[string]bool{}
	pageToken := ""
	client := &http.Client{Timeout: 20 * time.Second}

	for page := 0; page < 10; page++ {
		u := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models?key=%s&pageSize=200", cfg.GeminiKey)
		if pageToken != "" {
			u += "&pageToken=" + pageToken
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to reach Gemini API: %v", err), http.StatusBadGateway)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			http.Error(w, fmt.Sprintf("Gemini API returned %d", resp.StatusCode), http.StatusBadGateway)
			return
		}
		var mr modelsResp
		if err := json.Unmarshal(body, &mr); err != nil {
			http.Error(w, "failed to parse Gemini models", http.StatusBadGateway)
			return
		}
		for _, m := range mr.Models {
			name := strings.TrimPrefix(m.Name, "models/")
			if !strings.Contains(strings.ToLower(name), "gemini") {
				continue
			}
			supportsGenerate := false
			for _, meth := range m.SupportedGenerationMethods {
				if meth == "generateContent" {
					supportsGenerate = true
					break
				}
			}
			if supportsGenerate && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		if mr.NextPageToken == "" {
			break
		}
		pageToken = mr.NextPageToken
	}

	sort.Strings(names)
	writeJSON(w, http.StatusOK, names)
}

func (h *Handler) getVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version": version.Version,
		"commit":  version.Commit,
	})
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
	CategoryName         string     `json:"category_name"`
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

// isForcedCategory reports whether a category name is in the force-recategorize
// list (placeholders like "A catégoriser" that shouldn't count as "already set").
func (h *Handler) isForcedCategory(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, c := range h.effectiveConfig().ForceCategories {
		if strings.EqualFold(strings.TrimSpace(c), name) {
			return true
		}
	}
	return false
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
	if first.CategoryID != "" && first.CategoryID != "0" && !h.isForcedCategory(first.CategoryName) {
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
			CategoryName:    t.CategoryName,
			Tags:            t.Tags,
			Notes:           t.Notes,
		}
	}

	amount := parseAmount(first.Amount)
	j := h.registry.Create(string(payload.Content.ID), "", first.DestinationName, first.Description, amount, "webhook")
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
		j := h.registry.Create(txn.ID, batchID, first.DestinationName, first.Description, amount, "batch")

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

	SearchEngine    string `json:"search_engine"`
	GeminiThinking  string `json:"gemini_thinking"`
	GeminiGrounding bool   `json:"gemini_grounding"`

	MailAccounts      []config.MailAccount  `json:"mail_accounts"`
	MailDetectors     []config.MailDetector `json:"mail_detectors"`
	ForceDestinations []string              `json:"force_destinations"`
	ForceCategories   []string              `json:"force_categories"`
	TagRules          []config.TagRule      `json:"tag_rules"`

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

		SearchEngine:    cfg.SearchEngine,
		GeminiThinking:  cfg.GeminiThinking,
		GeminiGrounding: cfg.GeminiGrounding,

		MailAccounts:      maskMailAccounts(cfg.MailAccounts),
		MailDetectors:     cfg.MailDetectors,
		ForceDestinations: cfg.ForceDestinations,
		ForceCategories:   cfg.ForceCategories,
		TagRules:          cfg.TagRules,

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

	SearchEngine    *string `json:"search_engine"`
	GeminiThinking  *string `json:"gemini_thinking"`
	GeminiGrounding *bool   `json:"gemini_grounding"`

	MailAccounts      *[]config.MailAccount  `json:"mail_accounts"`
	MailDetectors     *[]config.MailDetector `json:"mail_detectors"`
	ForceDestinations *[]string              `json:"force_destinations"`
	ForceCategories   *[]string              `json:"force_categories"`
	TagRules          *[]config.TagRule      `json:"tag_rules"`

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

// getTransactionDetails returns current (real) category/destination/tags/date for
// a batch of transaction IDs, for comparing against what the AI proposed.
func (h *Handler) getTransactionDetails(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	out := map[string]map[string]any{}
	if len(req.IDs) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	txns, err := fc.GetTransactionsByIDs(r.Context(), req.IDs)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch: %v", err), http.StatusBadGateway)
		return
	}
	for _, t := range txns {
		if len(t.Splits) == 0 {
			continue
		}
		s := t.Splits[0]
		date := s.Date
		if len(date) >= 10 {
			date = date[:10]
		}
		out[t.ID] = map[string]any{
			"destination_name": s.DestinationName,
			"category_name":    s.CategoryName,
			"tags":             classifier.SemanticTags(s.Tags),
			"date":             date,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// getAutoMatch returns the history auto-match explanation for a transaction.
func (h *Handler) getAutoMatch(w http.ResponseWriter, r *http.Request) {
	pipe := h.getPipe()
	if pipe == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	ex, err := pipe.ExplainAutoMatch(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, ex)
}

// getSimilar returns other transactions of the same merchant (for bulk apply).
func (h *Handler) getSimilar(w http.ResponseWriter, r *http.Request) {
	pipe := h.getPipe()
	if pipe == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	sims, err := pipe.SimilarTransactions(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Mark which are already treated/reviewed (status lives in the local DB).
	ids := make([]string, len(sims))
	for i := range sims {
		ids[i] = sims[i].ID
	}
	recs, _ := h.aidb.GetMany(ids)
	for i := range sims {
		if rec, ok := recs[sims[i].ID]; ok && rec.Reviewed {
			sims[i].Reviewed = true
		}
	}
	// By default only propose not-yet-treated transactions, so applying to
	// "others" never overwrites work already validated. ?include_reviewed=true
	// keeps everything.
	if r.URL.Query().Get("include_reviewed") != "true" {
		filtered := sims[:0]
		for _, s := range sims {
			if !s.Reviewed {
				filtered = append(filtered, s)
			}
		}
		sims = filtered
	}
	writeJSON(w, http.StatusOK, sims)
}

// editBulk applies category/destination/tags to several transactions at once.
func (h *Handler) editBulk(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		IDs             []string          `json:"ids"`
		JournalIDs      map[string]string `json:"journal_ids"`
		CategoryName    string            `json:"category_name"`
		DestinationName string            `json:"destination_name"`
		Tags            []string          `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.IDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{"job_id": "", "total": 0})
		return
	}

	jobID := uuid.New().String()
	prog := &bulkProgress{Total: len(req.IDs), Running: true}
	h.bulkMu.Lock()
	h.bulkJobs[jobID] = prog
	h.bulkMu.Unlock()

	// Run the whole batch in the background (detached from the browser). The
	// front polls /api/transactions/edit-bulk/{id} for progress.
	go func() {
		conc := h.baseCfg.BulkConcurrency
		if conc < 1 {
			conc = 4
		}
		var (
			wg  sync.WaitGroup
			mu  sync.Mutex
			sem = make(chan struct{}, conc)
		)
		applyOne := func(id string) bool {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			var splits []firefly.Split
			if jid := req.JournalIDs[id]; jid != "" {
				splits = []firefly.Split{{JournalID: jid}}
			} else {
				txns, err := fc.GetTransactionsByIDs(ctx, []string{id})
				if err != nil || len(txns) == 0 || len(txns[0].Splits) == 0 {
					return false
				}
				splits = txns[0].Splits
			}
			return fc.EditTransaction(ctx, id, splits, req.CategoryName, req.DestinationName, req.Tags) == nil
		}
		for _, id := range req.IDs {
			wg.Add(1)
			sem <- struct{}{}
			go func(id string) {
				defer wg.Done()
				defer func() { <-sem }()
				ok := applyOne(id)
				if !ok {
					ok = applyOne(id) // one retry on transient failure
				}
				mu.Lock()
				if ok {
					_ = h.aidb.MarkTreated(id)
					h.registry.MarkReviewedByTxn(id)
					prog.Done++
				} else {
					prog.Failed++
				}
				mu.Unlock()
			}(id)
		}
		wg.Wait()
		h.bulkMu.Lock()
		prog.Running = false
		h.bulkMu.Unlock()
		h.invalidateReviewCache()
	}()

	writeJSON(w, http.StatusOK, map[string]interface{}{"job_id": jobID, "total": len(req.IDs)})
}

// bulkStatus reports progress for an async bulk-apply job.
func (h *Handler) bulkStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	h.bulkMu.Lock()
	prog := h.bulkJobs[id]
	h.bulkMu.Unlock()
	if prog == nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, prog)
}

// getTransaction returns a single transaction's editable fields.
func (h *Handler) getTransaction(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}
	id := chi.URLParam(r, "id")
	txns, err := fc.GetTransactionsByIDs(r.Context(), []string{id})
	if err != nil || len(txns) == 0 || len(txns[0].Splits) == 0 {
		http.Error(w, "transaction not found", http.StatusNotFound)
		return
	}
	s := txns[0].Splits[0]
	writeJSON(w, http.StatusOK, map[string]any{
		"id":               id,
		"description":      s.Description,
		"destination_name": s.DestinationName,
		"category_name":    s.CategoryName,
		"tags":             classifier.SemanticTags(s.Tags),
		"amount":           s.Amount,
		"date":             s.Date,
	})
}

// editTransaction applies a manual edit (category / destination / tags).
func (h *Handler) editTransaction(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		CategoryName    string   `json:"category_name"`
		DestinationName string   `json:"destination_name"`
		Tags            []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	txns, err := fc.GetTransactionsByIDs(r.Context(), []string{id})
	if err != nil || len(txns) == 0 || len(txns[0].Splits) == 0 {
		http.Error(w, "transaction not found", http.StatusNotFound)
		return
	}
	if err := fc.EditTransaction(r.Context(), id, txns[0].Splits, req.CategoryName, req.DestinationName, req.Tags); err != nil {
		http.Error(w, fmt.Sprintf("failed to edit: %v", err), http.StatusBadGateway)
		return
	}
	_ = h.aidb.MarkTreated(id)
	h.registry.MarkReviewedByTxn(id)
	h.invalidateReviewCache()
	w.WriteHeader(http.StatusNoContent)
}

// getTags returns existing Firefly tag names (for autocomplete).
func (h *Handler) getTags(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}
	tags, err := fc.GetTags(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch tags: %v", err), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, tags)
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
	SuggestedTags        []string    `json:"suggested_tags,omitempty"`
	AppliedTags          []string    `json:"applied_tags,omitempty"`
	Reason               string      `json:"reason,omitempty"`
	Transactions         []reviewTxn `json:"transactions"`
}

func (h *Handler) getReview(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}

	if r.URL.Query().Get("reviewed") == "true" {
		groups, err := h.computeReviewedGroups(r.Context(), fc)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to fetch reviewed items: %v", err), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, groups)
		return
	}

	groups, err := h.cachedReviewGroups(r.Context(), fc)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch review groups: %v", err), http.StatusBadGateway)
		return
	}

	writeJSON(w, http.StatusOK, groups)
}

// unreviewTransaction clears the reviewed flag so an item returns to the queue.
func (h *Handler) unreviewTransaction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.aidb.Unreview(id); err != nil {
		http.Error(w, fmt.Sprintf("failed to unreview: %v", err), http.StatusInternalServerError)
		return
	}
	h.invalidateReviewCache()
	w.WriteHeader(http.StatusNoContent)
}

// purgeJobs clears the Jobs log (in-memory + DB). Categorizations and AI review
// status are unaffected (they live in Firefly and the ai_records table).
func (h *Handler) purgeJobs(w http.ResponseWriter, r *http.Request) {
	h.registry.Clear()
	w.WriteHeader(http.StatusNoContent)
}

// markTreated flags the given transactions as treated (reviewed) in bulk.
func (h *Handler) markTreated(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	for _, id := range req.IDs {
		_ = h.aidb.MarkTreated(id)
		h.registry.MarkReviewedByTxn(id)
	}
	h.invalidateReviewCache()
	writeJSON(w, http.StatusOK, map[string]int{"updated": len(req.IDs)})
}

// rerunTransaction re-runs the classifier on a single transaction (reuses its
// job) — used by the per-row "relancer" button.
func (h *Handler) rerunTransaction(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	pipe := h.getPipe()
	if fc == nil || pipe == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	id := chi.URLParam(r, "id")
	txns, err := fc.GetTransactionsByIDs(r.Context(), []string{id})
	if err != nil || len(txns) == 0 || len(txns[0].Splits) == 0 {
		http.Error(w, "transaction not found", http.StatusNotFound)
		return
	}
	txn := txns[0]
	first := txn.Splits[0]
	amount := parseAmount(first.Amount)
	j := h.registry.Create(id, "", first.DestinationName, first.Description, amount, "manual")
	splits := txn.Splits
	h.webhookPool.Submit(worker.Task{
		JobID: j.ID,
		Execute: func(ctx context.Context) error {
			return pipe.Run(ctx, j, id, splits)
		},
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": j.ID})
}

// dismissTransaction marks an item reviewed (dismissed) without changing it, so
// it stops appearing in the review queue.
func (h *Handler) dismissTransaction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.aidb.MarkReviewed(id); err != nil {
		http.Error(w, fmt.Sprintf("failed to dismiss: %v", err), http.StatusInternalServerError)
		return
	}
	h.registry.MarkReviewedByTxn(id)
	h.invalidateReviewCache()
	w.WriteHeader(http.StatusNoContent)
}

const reviewCacheTTL = 2 * time.Minute

// cachedReviewGroups serves review groups stale-while-revalidate: a cached copy
// (even expired) is returned immediately while a background refresh runs, so the
// UI never waits on a scan except on the very first cold call.
func (h *Handler) cachedReviewGroups(ctx context.Context, fc *firefly.Client) ([]*reviewGroup, error) {
	h.reviewMu.Lock()
	if h.reviewCache != nil {
		cached := h.reviewCache
		if time.Since(h.reviewCachedAt) >= reviewCacheTTL && !h.reviewRefreshing {
			h.reviewRefreshing = true
			go h.refreshReviewCache()
		}
		h.reviewMu.Unlock()
		return cached, nil
	}
	h.reviewMu.Unlock()

	// Cold cache: compute synchronously.
	groups, err := h.computeReviewGroups(ctx, fc)
	if err != nil {
		return nil, err
	}
	h.reviewMu.Lock()
	h.reviewCache = groups
	h.reviewCachedAt = time.Now()
	h.reviewMu.Unlock()
	return groups, nil
}

// refreshReviewCache recomputes the review cache in the background using a
// fresh context, so a cancelled request doesn't abort the refresh.
func (h *Handler) refreshReviewCache() {
	defer func() {
		h.reviewMu.Lock()
		h.reviewRefreshing = false
		h.reviewMu.Unlock()
	}()

	fc := h.getFC()
	if fc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	groups, err := h.computeReviewGroups(ctx, fc)
	if err != nil {
		slog.Warn("review: background refresh failed", "error", err)
		return
	}
	h.reviewMu.Lock()
	h.reviewCache = groups
	h.reviewCachedAt = time.Now()
	h.reviewMu.Unlock()
}

func (h *Handler) computeReviewGroups(ctx context.Context, fc *firefly.Client) ([]*reviewGroup, error) {
	records, err := h.aidb.PendingReview()
	if err != nil {
		return nil, err
	}

	catBucket := map[string]string{}   // txn id -> NEEDS_REVIEW / ASSUMED / DEST_ASSUMED
	suggested := map[string][]string{} // txn id -> pending tag suggestions
	reasonByID := map[string]string{}  // txn id -> AI reason (context for review)
	var ids []string
	seen := map[string]bool{}
	addID := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, r := range records {
		if r.Reason != "" {
			reasonByID[r.TransactionID] = r.Reason
		}
		switch {
		case r.Outcome == "NEEDS_REVIEW":
			catBucket[r.TransactionID] = "NEEDS_REVIEW"
			addID(r.TransactionID)
		case r.Outcome == "ASSUMED":
			catBucket[r.TransactionID] = "ASSUMED"
			addID(r.TransactionID)
		case r.DestConfidence == "ASSUMED":
			catBucket[r.TransactionID] = "DEST_ASSUMED"
			addID(r.TransactionID)
		}
		if len(r.SuggestedTags) > 0 {
			suggested[r.TransactionID] = r.SuggestedTags
			addID(r.TransactionID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}

	txns, err := fc.GetTransactionsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	txnByID := map[string]firefly.Transaction{}
	for _, t := range txns {
		txnByID[t.ID] = t
	}

	// Cache the splits so validating a row can skip re-fetching the transaction.
	splitsByID := map[string][]firefly.Split{}
	for _, t := range txns {
		splitsByID[t.ID] = t.Splits
	}
	h.reviewMu.Lock()
	h.reviewSplits = splitsByID
	h.reviewMu.Unlock()

	// Category / destination review (grouped by payee), in record order.
	var needsReview, assumed, destAssumed []firefly.Transaction
	for _, id := range ids {
		t, ok := txnByID[id]
		if !ok {
			continue
		}
		switch catBucket[id] {
		case "NEEDS_REVIEW":
			needsReview = append(needsReview, t)
		case "ASSUMED":
			assumed = append(assumed, t)
		case "DEST_ASSUMED":
			destAssumed = append(destAssumed, t)
		}
	}

	var result []*reviewGroup
	result = append(result, buildReviewGroups("NEEDS_REVIEW", needsReview)...)
	result = append(result, buildReviewGroups("ASSUMED", assumed)...)
	result = append(result, buildReviewGroups("DEST_ASSUMED", destAssumed)...)
	for _, g := range result {
		if len(g.Transactions) > 0 {
			g.Reason = reasonByID[g.Transactions[0].ID]
		}
	}

	// Tag suggestions — one row per transaction (not merged), in record order.
	for _, id := range ids {
		tags := suggested[id]
		if len(tags) == 0 {
			continue
		}
		t, ok := txnByID[id]
		if !ok || len(t.Splits) == 0 {
			continue
		}
		s := t.Splits[0]
		result = append(result, &reviewGroup{
			Outcome:         "TAGS",
			Description:     s.Description,
			DestinationName: s.DestinationName,
			CategoryName:    s.CategoryName,
			SuggestedTags:   tags,
			AppliedTags:     classifier.SemanticTags(s.Tags),
			Reason:          reasonByID[id],
			Transactions:    []reviewTxn{{ID: t.ID, Date: s.Date, Amount: s.Amount}},
		})
	}
	return result, nil
}

// computeReviewedGroups returns recently human-reviewed transactions (read-only).
func (h *Handler) computeReviewedGroups(ctx context.Context, fc *firefly.Client) ([]*reviewGroup, error) {
	records, err := h.aidb.ListReviewed(200)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.TransactionID)
	}
	txns, err := fc.GetTransactionsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	txnByID := map[string]firefly.Transaction{}
	for _, t := range txns {
		txnByID[t.ID] = t
	}

	var result []*reviewGroup
	for _, r := range records {
		t, ok := txnByID[r.TransactionID]
		if !ok || len(t.Splits) == 0 {
			continue
		}
		s := t.Splits[0]
		result = append(result, &reviewGroup{
			Outcome:         "REVIEWED",
			Description:     s.Description,
			DestinationName: s.DestinationName,
			CategoryName:    s.CategoryName,
			CategoryID:      s.CategoryID,
			AppliedTags:     classifier.SemanticTags(s.Tags),
			Transactions:    []reviewTxn{{ID: t.ID, Date: s.Date, Amount: s.Amount}},
		})
	}
	return result, nil
}

// invalidateReviewCache forces the next /api/review call to recompute, used
// after a human review action changes the set of flagged transactions.
func (h *Handler) invalidateReviewCache() {
	h.reviewMu.Lock()
	h.reviewCache = nil
	h.reviewMu.Unlock()
	// A category/destination/tag change also makes the auto-match history stale.
	if pipe := h.getPipe(); pipe != nil {
		pipe.InvalidateHistory()
	}
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
			g.AppliedTags = classifier.SemanticTags(s.Tags)
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

	// Reuse the splits cached when the review was computed, to skip a Firefly
	// round-trip per validation. Fall back to a fetch if not cached.
	h.reviewMu.Lock()
	splits, cached := h.reviewSplits[id]
	h.reviewMu.Unlock()
	if !cached {
		txns, err := fc.GetTransactionsByIDs(r.Context(), []string{id})
		if err != nil || len(txns) == 0 {
			http.Error(w, "transaction not found", http.StatusNotFound)
			return
		}
		splits = txns[0].Splits
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

	if err := fc.ApplyHumanCategory(r.Context(), id, splits, req.CategoryID, destID); err != nil {
		http.Error(w, fmt.Sprintf("failed to update transaction: %v", err), http.StatusBadGateway)
		return
	}

	_ = h.aidb.MarkReviewed(id)
	h.registry.MarkReviewedByTxn(id)
	h.invalidateReviewCache()
	w.WriteHeader(http.StatusNoContent)
}

// purgeAITags removes the app's old AI control tags from all withdrawals.
func (h *Handler) purgeAITags(w http.ResponseWriter, r *http.Request) {
	fc := h.getFC()
	if fc == nil {
		http.Error(w, "Firefly not configured", http.StatusServiceUnavailable)
		return
	}
	n, err := fc.PurgeControlTags(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("purge failed: %v", err), http.StatusBadGateway)
		return
	}
	h.invalidateReviewCache()
	writeJSON(w, http.StatusOK, map[string]int{"updated": n})
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

	// Suggestions live in the local DB, not as Firefly tags. Rejecting one is
	// therefore purely local — no Firefly round-trip needed. Only applying a
	// suggestion actually adds a real tag in Firefly.
	if len(req.Apply) > 0 {
		txns, err := fc.GetTransactionsByIDs(r.Context(), []string{id})
		if err != nil || len(txns) == 0 {
			http.Error(w, "transaction not found", http.StatusNotFound)
			return
		}
		if err := fc.ResolveSuggestedTags(r.Context(), id, txns[0].Splits, req.Apply, req.Reject); err != nil {
			http.Error(w, fmt.Sprintf("failed to update transaction: %v", err), http.StatusBadGateway)
			return
		}
	}

	// Drop the resolved suggestions from the local store.
	if rec, err := h.aidb.Get(id); err == nil && rec != nil {
		resolved := map[string]bool{}
		for _, n := range append(append([]string{}, req.Apply...), req.Reject...) {
			resolved[strings.ToLower(strings.TrimSpace(n))] = true
		}
		var remaining []string
		for _, t := range rec.SuggestedTags {
			if !resolved[strings.ToLower(strings.TrimSpace(t))] {
				remaining = append(remaining, t)
			}
		}
		_ = h.aidb.SetSuggestedTags(id, remaining)
	}
	h.registry.UpdateTagsByTxn(id, req.Apply, req.Reject)

	h.invalidateReviewCache()
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
	descFilter := q.Get("description")
	statusFilter := q.Get("status")
	idsOnly := q.Get("ids_only") == "true"

	// enrich attaches AI status + pending tag suggestions from the local store.
	enrich := func(rows []firefly.TransactionRow) {
		if len(rows) == 0 {
			return
		}
		ids := make([]string, len(rows))
		for i := range rows {
			ids[i] = rows[i].ID
		}
		recs, _ := h.aidb.GetMany(ids)
		for i := range rows {
			rec, ok := recs[rows[i].ID]
			rows[i].AIStatus = aiStatusFromRecord(rec, ok)
			if ok && len(rec.SuggestedTags) > 0 {
				rows[i].AISuggestedTags = rec.SuggestedTags
			}
		}
	}

	filtersActive := missingCategory || missingDestination || destFilter != "" ||
		categoryFilter != "" || descFilter != "" || (statusFilter != "" && statusFilter != "all")

	if filtersActive {
		// Build a native Firefly search query from every Firefly-able criterion so
		// the server does the heavy lifting (indexed, fast). AI status and the
		// "Cash account" notion are applied client-side on the reduced result.
		var all []firefly.TransactionRow
		sq := []string{"type:withdrawal"}
		if start != "" {
			sq = append(sq, "date_after:"+start)
		}
		if end != "" {
			sq = append(sq, "date_before:"+end)
		}
		if descFilter != "" {
			sq = append(sq, fmt.Sprintf("description_contains:%q", descFilter))
		}
		if categoryFilter != "" {
			sq = append(sq, fmt.Sprintf("category_is:%q", categoryFilter))
		}
		if destFilter != "" && destFilter != "(no name)" {
			sq = append(sq, fmt.Sprintf("destination_account_contains:%q", destFilter))
		}
		if missingCategory {
			sq = append(sq, "has_no_category:true")
		}

		txns, err := fc.SearchTransactionsRaw(r.Context(), strings.Join(sq, " "))
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to search transactions: %v", err), http.StatusBadGateway)
			return
		}
		for _, t := range txns {
			if len(t.Splits) == 0 {
				continue
			}
			s := t.Splits[0]
			if start != "" && len(s.Date) >= 10 && s.Date[:10] < start {
				continue
			}
			if end != "" && len(s.Date) >= 10 && s.Date[:10] > end {
				continue
			}
			all = append(all, firefly.TransactionRow{
				ID: t.ID, Date: s.Date, Description: s.Description, DestinationName: s.DestinationName,
				Amount: s.Amount, CategoryID: s.CategoryID, CategoryName: s.CategoryName, Tags: s.Tags,
			})
		}
		ids := make([]string, len(all))
		for i := range all {
			ids[i] = all[i].ID
		}
		recs, _ := h.aidb.GetMany(ids)

		var filtered []firefly.TransactionRow
		for _, row := range all {
			if !matchesTxnFilter(row, missingCategory, missingDestination, destFilter, categoryFilter, descFilter) {
				continue
			}
			rec, ok := recs[row.ID]
			if !aiFilterMatch(rec, ok, statusFilter) {
				continue
			}
			filtered = append(filtered, row)
		}

		if idsOnly {
			out := make([]string, len(filtered))
			for i := range filtered {
				out[i] = filtered[i].ID
			}
			writeJSON(w, http.StatusOK, out)
			return
		}

		pageNum := queryInt(q.Get("page"), 1)
		limit := queryInt(q.Get("limit"), 50)
		total := len(filtered)
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
		pageData := filtered[startIdx:endIdx]
		enrich(pageData)

		writeJSON(w, http.StatusOK, firefly.TransactionsPage{
			Data:       pageData,
			Page:       pageNum,
			TotalPages: totalPages,
			Total:      total,
		})
		return
	}

	if idsOnly {
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
	enrich(result.Data)
	writeJSON(w, http.StatusOK, result)
}

// aiStatusFromRecord derives the UI status string from a stored AI record.
func aiStatusFromRecord(rec aidb.Record, ok bool) string {
	if !ok {
		return "untreated"
	}
	if rec.Reviewed {
		return "reviewed"
	}
	switch {
	case rec.Outcome == "NEEDS_REVIEW":
		return "needs_review"
	case rec.Outcome == "ASSUMED":
		return "assumed"
	case rec.DestConfidence == "ASSUMED":
		return "dest_assumed"
	default:
		return "classified"
	}
}

// aiFilterMatch reports whether a record matches the requested status filter.
func aiFilterMatch(rec aidb.Record, ok bool, status string) bool {
	switch status {
	case "", "all":
		return true
	case "suggested":
		return ok && len(rec.SuggestedTags) > 0
	default:
		return aiStatusFromRecord(rec, ok) == status
	}
}

// isCashAccountName reports whether a destination is Firefly's generic cash
// account (treated as "no real destination").
func isCashAccountName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == "cash account" || n == "(cash account)" || n == "cash" ||
		n == "compte d'espèces" || n == "espèces" || strings.Contains(n, "cash account")
}

// matchesTxnFilter checks whether a transaction row matches the active UI filters.
func matchesTxnFilter(row firefly.TransactionRow, missingCategory, missingDestination bool, destFilter, categoryFilter, descFilter string) bool {
	if descFilter != "" && !strings.Contains(strings.ToLower(row.Description), strings.ToLower(descFilter)) {
		return false
	}
	if missingCategory && row.CategoryName != "" {
		return false
	}
	if categoryFilter != "" && !strings.EqualFold(row.CategoryName, categoryFilter) {
		return false
	}
	if missingDestination {
		dn := strings.TrimSpace(row.DestinationName)
		if dn != "" && !strings.EqualFold(dn, "(no name)") && !isCashAccountName(dn) {
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
		Provider:        cfg.AIProvider,
		OpenAIKey:       cfg.OpenAIKey,
		OpenAIModel:     cfg.OpenAIModel,
		OpenAIBaseURL:   cfg.OpenAIBaseURL,
		GeminiKey:       cfg.GeminiKey,
		GeminiModel:     cfg.GeminiModel,
		GeminiThinking:  cfg.GeminiThinking,
		GeminiGrounding: cfg.GeminiGrounding,
		DeepseekKey:     cfg.DeepseekKey,
		DeepseekModel:   cfg.DeepseekModel,
		CustomContext:   cfg.CustomSystemContext,
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

	pipe := pipeline.New(fc, cl, ca, h.registry, cfg.HistoryContextLimit, cfg.DestinationMatchEnabled, cfg.TagSuggestEnabled, cfg.TagSuggestMax, amazon.Load(cfg.AmazonOrdersFile), h.aidb, cfg.MailAccounts, cfg.MailDetectors, cfg.ForceDestinations, cfg.ForceCategories, cfg.TagRules)

	h.mu.Lock()
	h.fc = fc
	h.histCache = ca
	h.pipe = pipe
	h.mu.Unlock()

	slog.Info("clients reloaded", "provider", cfg.AIProvider, "firefly", cfg.FireflyURL)

	// Warm the review cache in the background so the first UI load is instant.
	h.reviewMu.Lock()
	if !h.reviewRefreshing {
		h.reviewRefreshing = true
		go h.refreshReviewCache()
	}
	h.reviewMu.Unlock()

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
	if req.GeminiThinking != nil {
		existing.GeminiThinking = *req.GeminiThinking
	}
	if req.GeminiGrounding != nil {
		existing.GeminiGrounding = *req.GeminiGrounding
	}
	if req.MailAccounts != nil {
		existing.MailAccounts = mergeMailAccounts(existing.MailAccounts, *req.MailAccounts)
	}
	if req.MailDetectors != nil {
		existing.MailDetectors = mergeMailDetectors(*req.MailDetectors)
	}
	if req.ForceDestinations != nil {
		existing.ForceDestinations = *req.ForceDestinations
	}
	if req.ForceCategories != nil {
		existing.ForceCategories = *req.ForceCategories
	}
	if req.TagRules != nil {
		existing.TagRules = *req.TagRules
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
