package firefly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	token      string
	tagPrefix  string
	httpClient *http.Client
}

func New(baseURL, token, tagPrefix string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		tagPrefix:  tagPrefix,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// GetPreference fetches a single user preference value by name.
// Returns the raw JSON value (bool, string, number) and nil on success.
func (c *Client) GetPreference(ctx context.Context, name string) (interface{}, error) {
	u := fmt.Sprintf("%s/api/v1/preferences/%s", c.baseURL, name)
	var resp preferenceResponse
	if err := c.get(ctx, u, &resp); err != nil {
		return nil, err
	}
	return resp.Data.Attributes.Data, nil
}

// GetExpenseAccounts fetches all expense accounts, paginating through all pages.
// Used for destination account matching when DESTINATION_MATCH_ENABLED is true.
func (c *Client) GetExpenseAccounts(ctx context.Context) ([]Account, error) {
	var accounts []Account
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s/api/v1/accounts?type=expense&page=%d", c.baseURL, page)
		var resp accountsResponse
		if err := c.get(ctx, u, &resp); err != nil {
			return nil, fmt.Errorf("get expense accounts page %d: %w", page, err)
		}
		for _, item := range resp.Data {
			accounts = append(accounts, Account{
				ID:   item.ID,
				Name: item.Attributes.Name,
			})
		}
		if page >= resp.Meta.Pagination.TotalPages {
			break
		}
	}
	return accounts, nil
}

// GetAssetAccounts fetches all asset accounts, paginating through all pages.
func (c *Client) GetAssetAccounts(ctx context.Context) ([]Account, error) {
	var accounts []Account
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s/api/v1/accounts?type=asset&page=%d", c.baseURL, page)
		var resp accountsResponse
		if err := c.get(ctx, u, &resp); err != nil {
			return nil, fmt.Errorf("get asset accounts page %d: %w", page, err)
		}
		for _, item := range resp.Data {
			accounts = append(accounts, Account{
				ID:   item.ID,
				Name: item.Attributes.Name,
			})
		}
		if page >= resp.Meta.Pagination.TotalPages {
			break
		}
	}
	return accounts, nil
}

// DeleteTransaction deletes a transaction by ID.
func (c *Client) DeleteTransaction(ctx context.Context, id string) error {
	u := fmt.Sprintf("%s/api/v1/transactions/%s", c.baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// CreateTransferParams holds the fields needed to create a transfer transaction.
type CreateTransferParams struct {
	Date          string
	Amount        string
	Description   string
	SourceID      string
	DestinationID string
	Notes         string
	Tags          []string
}

// CreateTransfer creates a transfer transaction between two asset accounts.
func (c *Client) CreateTransfer(ctx context.Context, p CreateTransferParams) (Transaction, error) {
	type transferSplit struct {
		Type          string   `json:"type"`
		Date          string   `json:"date"`
		Amount        string   `json:"amount"`
		Description   string   `json:"description"`
		SourceID      string   `json:"source_id"`
		DestinationID string   `json:"destination_id"`
		Notes         string   `json:"notes,omitempty"`
		Tags          []string `json:"tags,omitempty"`
	}
	type transferBody struct {
		ApplyRules   bool            `json:"apply_rules"`
		FireWebhooks bool            `json:"fire_webhooks"`
		Transactions []transferSplit `json:"transactions"`
	}

	body := transferBody{
		ApplyRules:   true,
		FireWebhooks: false,
		Transactions: []transferSplit{{
			Type:          "transfer",
			Date:          p.Date,
			Amount:        p.Amount,
			Description:   p.Description,
			SourceID:      p.SourceID,
			DestinationID: p.DestinationID,
			Notes:         p.Notes,
			Tags:          p.Tags,
		}},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return Transaction{}, err
	}

	u := fmt.Sprintf("%s/api/v1/transactions", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return Transaction{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Transaction{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return Transaction{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}

	var tr singleTransactionResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return Transaction{}, fmt.Errorf("decode transfer response: %w", err)
	}
	return toTransaction(tr.Data), nil
}

// CreateExpenseAccount creates a new expense account via the Firefly III API.
func (c *Client) CreateExpenseAccount(ctx context.Context, name string) (Account, error) {
	u := fmt.Sprintf("%s/api/v1/accounts", c.baseURL)
	body := map[string]string{"name": name, "type": "expense"}
	data, err := json.Marshal(body)
	if err != nil {
		return Account{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return Account{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Account{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return Account{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}

	var ar accountResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return Account{}, fmt.Errorf("decode create account response: %w", err)
	}
	return Account{ID: ar.Data.ID, Name: ar.Data.Attributes.Name}, nil
}

// GetCategories fetches all categories, paginating through all pages.
func (c *Client) GetCategories(ctx context.Context) ([]Category, error) {
	var categories []Category
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s/api/v1/categories?page=%d", c.baseURL, page)
		var resp categoriesResponse
		if err := c.get(ctx, u, &resp); err != nil {
			return nil, fmt.Errorf("get categories page %d: %w", page, err)
		}
		for _, item := range resp.Data {
			categories = append(categories, Category{
				ID:    item.ID,
				Name:  item.Attributes.Name,
				Notes: item.Attributes.Notes,
			})
		}
		if page >= resp.Meta.Pagination.TotalPages {
			break
		}
	}
	return categories, nil
}

// GetTags fetches all existing tag names, paginating through all pages.
// Used to offer the LLM a reuse list when tag suggestion is enabled.
func (c *Client) GetTags(ctx context.Context) ([]string, error) {
	var tags []string
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s/api/v1/tags?page=%d", c.baseURL, page)
		var resp tagsResponse
		if err := c.get(ctx, u, &resp); err != nil {
			return nil, fmt.Errorf("get tags page %d: %w", page, err)
		}
		for _, item := range resp.Data {
			if item.Attributes.Tag != "" {
				tags = append(tags, item.Attributes.Tag)
			}
		}
		if page >= resp.Meta.Pagination.TotalPages {
			break
		}
	}
	return tags, nil
}

// GetCategorizedWithdrawals fetches all categorized withdrawals within the lookback window.
func (c *Client) GetCategorizedWithdrawals(ctx context.Context, lookbackDays int) ([]Transaction, error) {
	start := time.Now().AddDate(0, 0, -lookbackDays).Format("2006-01-02")
	params := url.Values{"type": {"withdrawal"}, "start": {start}}
	return c.fetchTransactions(ctx, params, func(s Split) bool {
		return s.CategoryName != ""
	})
}

// GetUncategorizedWithdrawals fetches all withdrawals with no category set.
func (c *Client) GetUncategorizedWithdrawals(ctx context.Context) ([]Transaction, error) {
	params := url.Values{"type": {"withdrawal"}}
	return c.fetchTransactions(ctx, params, func(s Split) bool {
		return !hasCategory(s.CategoryID)
	})
}

// GetAllWithdrawals fetches all withdrawals regardless of categorisation status.
func (c *Client) GetAllWithdrawals(ctx context.Context) ([]Transaction, error) {
	params := url.Values{"type": {"withdrawal"}}
	return c.fetchTransactions(ctx, params, func(_ Split) bool { return true })
}

// ReviewGroups holds the four categories of transactions that need human review.
type ReviewGroups struct {
	NeedsReview []Transaction
	Assumed     []Transaction
	DestAssumed []Transaction
}

// GetReviewGroups returns the transactions needing review. It first tries a
// fast path that resolves each review tag to its numeric ID and fetches only
// that tag's transactions (Firefly indexes this). If that path errors or is
// too slow, it falls back to scanning all withdrawals.
func (c *Client) GetReviewGroups(ctx context.Context) (*ReviewGroups, error) {
	fastCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	rg, err := c.reviewGroupsByTag(fastCtx)
	cancel()
	if err == nil {
		return rg, nil
	}
	slog.Warn("review: per-tag fetch failed, scanning all withdrawals instead", "error", err)
	return c.reviewGroupsByScan(ctx)
}

// reviewGroupsByTag fetches only the flagged transactions, resolving each
// review tag to its numeric ID first so the URL carries no ":" (which some
// Firefly setups mishandle) and only relevant transactions are fetched.
func (c *Client) reviewGroupsByTag(ctx context.Context) (*ReviewGroups, error) {
	tagIDs, err := c.listTags(ctx)
	if err != nil {
		return nil, err
	}

	rg := &ReviewGroups{}
	seen := make(map[string]bool) // txn IDs already in a higher-priority group

	order := []struct {
		name string
		dst  *[]Transaction
	}{
		{c.tagPrefix + ":needs-review", &rg.NeedsReview},
		{c.tagPrefix + ":assumed", &rg.Assumed},
		{c.tagPrefix + ":dest-assumed", &rg.DestAssumed},
	}

	for _, o := range order {
		id, ok := tagIDs[strings.ToLower(o.name)]
		if !ok {
			continue // tag never used — nothing flagged for it
		}
		txns, err := c.fetchByTagID(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, txn := range txns {
			if seen[txn.ID] {
				continue
			}
			var kept []Split
			for _, s := range txn.Splits {
				if contains(s.Tags, o.name) {
					kept = append(kept, s)
				}
			}
			if len(kept) > 0 {
				txn.Splits = kept
				*o.dst = append(*o.dst, txn)
				seen[txn.ID] = true
			}
		}
	}
	return rg, nil
}

// listTags returns a map of lowercased tag name → numeric tag ID.
func (c *Client) listTags(ctx context.Context) (map[string]string, error) {
	ids := make(map[string]string)
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s/api/v1/tags?page=%d", c.baseURL, page)
		var resp tagsResponse
		if err := c.get(ctx, u, &resp); err != nil {
			return nil, fmt.Errorf("list tags page %d: %w", page, err)
		}
		for _, item := range resp.Data {
			if item.Attributes.Tag != "" && item.ID != "" {
				ids[strings.ToLower(item.Attributes.Tag)] = item.ID
			}
		}
		if resp.Meta.Pagination.TotalPages == 0 || page >= resp.Meta.Pagination.TotalPages {
			break
		}
	}
	return ids, nil
}

// fetchByTagID fetches withdrawals carrying the tag identified by numeric ID.
func (c *Client) fetchByTagID(ctx context.Context, id string) ([]Transaction, error) {
	var out []Transaction
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s/api/v1/tags/%s/transactions?type=withdrawal&limit=200&page=%d", c.baseURL, id, page)
		var resp transactionsResponse
		if err := c.get(ctx, u, &resp); err != nil {
			return nil, fmt.Errorf("get transactions for tag id %s page %d: %w", id, page, err)
		}
		for _, item := range resp.Data {
			out = append(out, toTransaction(item))
		}
		if resp.Meta.Pagination.TotalPages == 0 || page >= resp.Meta.Pagination.TotalPages {
			break
		}
	}
	return out, nil
}

// reviewGroupsByScan scans all withdrawals once (large page size) and buckets
// transactions needing review by their AI tags. Fallback for reviewGroupsByTag.
func (c *Client) reviewGroupsByScan(ctx context.Context) (*ReviewGroups, error) {
	needsReviewTag := c.tagPrefix + ":needs-review"
	assumedTag := c.tagPrefix + ":assumed"
	destAssumedTag := c.tagPrefix + ":dest-assumed"

	rg := &ReviewGroups{}
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s/api/v1/transactions?type=withdrawal&limit=500&page=%d", c.baseURL, page)
		var resp transactionsResponse
		if err := c.get(ctx, u, &resp); err != nil {
			return nil, fmt.Errorf("get review groups page %d: %w", page, err)
		}

		for _, item := range resp.Data {
			txn := toTransaction(item)

			var kept []Split
			for _, s := range txn.Splits {
				if contains(s.Tags, needsReviewTag) {
					kept = append(kept, s)
				}
			}
			if len(kept) > 0 {
				txn.Splits = kept
				rg.NeedsReview = append(rg.NeedsReview, txn)
				continue
			}

			kept = nil
			for _, s := range txn.Splits {
				if contains(s.Tags, assumedTag) {
					kept = append(kept, s)
				}
			}
			if len(kept) > 0 {
				txn.Splits = kept
				rg.Assumed = append(rg.Assumed, txn)
				continue
			}

			kept = nil
			for _, s := range txn.Splits {
				if contains(s.Tags, destAssumedTag) {
					kept = append(kept, s)
				}
			}
			if len(kept) > 0 {
				txn.Splits = kept
				rg.DestAssumed = append(rg.DestAssumed, txn)
			}
		}

		if page >= resp.Meta.Pagination.TotalPages {
			break
		}
	}
	return rg, nil
}

// GetNeedsReviewWithdrawals fetches all withdrawals tagged with the needs-review tag.
func (c *Client) GetNeedsReviewWithdrawals(ctx context.Context) ([]Transaction, error) {
	params := url.Values{"type": {"withdrawal"}}
	tag := c.tagPrefix + ":needs-review"
	return c.fetchTransactions(ctx, params, func(s Split) bool {
		return contains(s.Tags, tag)
	})
}

// GetTransferCategoryWithdrawals fetches all withdrawals whose category is
// "Transfers" (case-insensitive). These are candidates for conversion to actual
// Firefly transfer transactions.
func (c *Client) GetTransferCategoryWithdrawals(ctx context.Context) ([]Transaction, error) {
	params := url.Values{"type": {"withdrawal"}}
	return c.fetchTransactions(ctx, params, func(s Split) bool {
		return strings.EqualFold(s.CategoryName, "Transfers")
	})
}

// GetAssumedWithdrawals fetches all withdrawals tagged with the assumed tag.
func (c *Client) GetAssumedWithdrawals(ctx context.Context) ([]Transaction, error) {
	params := url.Values{"type": {"withdrawal"}}
	tag := c.tagPrefix + ":assumed"
	return c.fetchTransactions(ctx, params, func(s Split) bool {
		return contains(s.Tags, tag)
	})
}

// GetDestAssumedWithdrawals fetches all withdrawals tagged with the dest-assumed tag.
func (c *Client) GetDestAssumedWithdrawals(ctx context.Context) ([]Transaction, error) {
	params := url.Values{"type": {"withdrawal"}}
	tag := c.tagPrefix + ":dest-assumed"
	return c.fetchTransactions(ctx, params, func(s Split) bool {
		return contains(s.Tags, tag)
	})
}

// ApplyHumanCategory sets a category on a previously-flagged transaction.
// It removes any AI outcome tags (needs-review, assumed) and adds a reviewed tag.
// When destinationID is non-empty, it also sets the destination expense account.
func (c *Client) ApplyHumanCategory(ctx context.Context, id string, splits []Split, categoryID, destinationID string) error {
	aiOutcomeTags := map[string]bool{
		c.tagPrefix + ":needs-review": true,
		c.tagPrefix + ":assumed":      true,
		c.tagPrefix + ":dest-assumed": true,
		c.tagPrefix + ":tags-assumed": true,
	}
	reviewedTag := c.tagPrefix + ":reviewed"

	type splitUpdate struct {
		TransactionJournalID string   `json:"transaction_journal_id"`
		Tags                 []string `json:"tags"`
		CategoryID           string   `json:"category_id,omitempty"`
		DestinationID        string   `json:"destination_id,omitempty"`
	}
	type body struct {
		ApplyRules   bool          `json:"apply_rules"`
		FireWebhooks bool          `json:"fire_webhooks"`
		Transactions []splitUpdate `json:"transactions"`
	}

	b := body{ApplyRules: true, FireWebhooks: false}
	for _, s := range splits {
		tags := make([]string, 0, len(s.Tags))
		for _, t := range s.Tags {
			if !aiOutcomeTags[t] {
				tags = append(tags, t)
			}
		}
		if !contains(tags, reviewedTag) {
			tags = append(tags, reviewedTag)
		}
		su := splitUpdate{
			TransactionJournalID: s.JournalID,
			Tags:                 tags,
			CategoryID:           categoryID,
		}
		if destinationID != "" {
			su.DestinationID = destinationID
		}
		b.Transactions = append(b.Transactions, su)
	}

	u := fmt.Sprintf("%s/api/v1/transactions/%s", c.baseURL, id)
	return c.put(ctx, u, b)
}

// ResolveSuggestedTags applies or rejects previously suggested tags on a
// transaction. Names listed in apply become real tags; names in reject are
// dropped. Either way their ai:suggest:<name> marker is removed. Suggestions
// not listed are left untouched.
func (c *Client) ResolveSuggestedTags(ctx context.Context, id string, splits []Split, apply, reject []string) error {
	applySet := make(map[string]bool)
	for _, n := range apply {
		applySet[strings.ToLower(strings.TrimSpace(n))] = true
	}
	rejectSet := make(map[string]bool)
	for _, n := range reject {
		rejectSet[strings.ToLower(strings.TrimSpace(n))] = true
	}
	prefix := c.tagPrefix + ":suggest:"

	type splitUpdate struct {
		TransactionJournalID string   `json:"transaction_journal_id"`
		Tags                 []string `json:"tags"`
	}
	type body struct {
		ApplyRules   bool          `json:"apply_rules"`
		FireWebhooks bool          `json:"fire_webhooks"`
		Transactions []splitUpdate `json:"transactions"`
	}

	b := body{ApplyRules: false, FireWebhooks: false}
	for _, s := range splits {
		tags := make([]string, 0, len(s.Tags))
		var toAdd []string
		for _, t := range s.Tags {
			if strings.HasPrefix(t, prefix) {
				name := t[len(prefix):]
				key := strings.ToLower(strings.TrimSpace(name))
				if applySet[key] {
					toAdd = append(toAdd, name)
					continue // drop the marker; the real tag is added below
				}
				if rejectSet[key] {
					continue // drop the marker without adding
				}
			}
			tags = append(tags, t)
		}
		for _, n := range toAdd {
			if !contains(tags, n) {
				tags = append(tags, n)
			}
		}
		b.Transactions = append(b.Transactions, splitUpdate{
			TransactionJournalID: s.JournalID,
			Tags:                 tags,
		})
	}

	u := fmt.Sprintf("%s/api/v1/transactions/%s", c.baseURL, id)
	return c.put(ctx, u, b)
}

// GetTransactionsByIDs fetches specific transaction groups by ID.
func (c *Client) GetTransactionsByIDs(ctx context.Context, ids []string) ([]Transaction, error) {
	var txns []Transaction
	for _, id := range ids {
		u := fmt.Sprintf("%s/api/v1/transactions/%s", c.baseURL, id)
		var resp singleTransactionResponse
		if err := c.get(ctx, u, &resp); err != nil {
			return nil, fmt.Errorf("get transaction %s: %w", id, err)
		}
		txns = append(txns, toTransaction(resp.Data))
	}
	return txns, nil
}

// GetWithdrawalsPage returns a paginated, flat list of withdrawals for the UI.
func (c *Client) GetWithdrawalsPage(ctx context.Context, page, limit int, startDate, endDate string) (TransactionsPage, error) {
	params := url.Values{
		"type":  {"withdrawal"},
		"page":  {fmt.Sprintf("%d", page)},
		"limit": {fmt.Sprintf("%d", limit)},
	}
	if startDate != "" {
		params.Set("start", startDate)
	}
	if endDate != "" {
		params.Set("end", endDate)
	}

	u := fmt.Sprintf("%s/api/v1/transactions?%s", c.baseURL, params.Encode())
	var resp transactionsResponse
	if err := c.get(ctx, u, &resp); err != nil {
		return TransactionsPage{}, fmt.Errorf("get transactions page %d: %w", page, err)
	}

	rows := make([]TransactionRow, 0, len(resp.Data))
	for _, item := range resp.Data {
		txn := toTransaction(item)
		if len(txn.Splits) == 0 {
			continue
		}
		s := txn.Splits[0]
		rows = append(rows, TransactionRow{
			ID:              txn.ID,
			Date:            s.Date,
			Description:     s.Description,
			DestinationName: s.DestinationName,
			Amount:          s.Amount,
			CategoryID:      s.CategoryID,
			CategoryName:    s.CategoryName,
			Tags:            s.Tags,
		})
	}

	return TransactionsPage{
		Data:       rows,
		Page:       page,
		TotalPages: resp.Meta.Pagination.TotalPages,
		Total:      resp.Meta.Pagination.Total,
	}, nil
}

// UpdateTransaction writes classification results back to Firefly III.
func (c *Client) UpdateTransaction(ctx context.Context, id string, splits []Split, outcome UpdateOutcome) error {
	tag := c.tagForOutcome(outcome.Outcome)

	type splitUpdate struct {
		TransactionJournalID string   `json:"transaction_journal_id"`
		Tags                 []string `json:"tags"`
		CategoryID           string   `json:"category_id,omitempty"`
		DestinationID        string   `json:"destination_id,omitempty"`
		Notes                string   `json:"notes,omitempty"`
	}
	type body struct {
		ApplyRules   bool          `json:"apply_rules"`
		FireWebhooks bool          `json:"fire_webhooks"`
		Transactions []splitUpdate `json:"transactions"`
	}

	b := body{ApplyRules: true, FireWebhooks: false}
	for _, s := range splits {
		tags := make([]string, len(s.Tags))
		copy(tags, s.Tags)
		if !contains(tags, tag) {
			tags = append(tags, tag)
		}
		// When destination was assumed (not classified), tag it for human review.
		if outcome.DestConfidence == "ASSUMED" && outcome.DestinationID != "" {
			destTag := c.tagPrefix + ":dest-assumed"
			if !contains(tags, destTag) {
				tags = append(tags, destTag)
			}
		}
		// Apply confident semantic tags suggested by the AI.
		for _, t := range outcome.Tags {
			t = strings.TrimSpace(t)
			if t == "" || contains(tags, t) {
				continue
			}
			tags = append(tags, t)
		}
		// Store low-confidence tag suggestions durably as control tags, one per
		// suggestion, so they can be validated later from the UI (applied or
		// rejected) rather than silently dropped.
		for _, t := range outcome.TagsAssumed {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			suggestTag := c.tagPrefix + ":suggest:" + t
			if !contains(tags, suggestTag) {
				tags = append(tags, suggestTag)
			}
		}

		su := splitUpdate{TransactionJournalID: s.JournalID, Tags: tags}
		if outcome.Outcome != "NEEDS_REVIEW" && outcome.CategoryID != "" {
			su.CategoryID = outcome.CategoryID
		}
		if outcome.DestinationID != "" {
			su.DestinationID = outcome.DestinationID
		}
		if notes := buildNotes(s.Notes, outcome); notes != "" {
			su.Notes = notes
		}
		b.Transactions = append(b.Transactions, su)
	}

	u := fmt.Sprintf("%s/api/v1/transactions/%s", c.baseURL, id)
	return c.put(ctx, u, b)
}

func (c *Client) tagForOutcome(outcome string) string {
	switch outcome {
	case "CLASSIFIED":
		return c.tagPrefix + ":classified"
	case "ASSUMED":
		return c.tagPrefix + ":assumed"
	default:
		return c.tagPrefix + ":needs-review"
	}
}

func buildNotes(existing string, outcome UpdateOutcome) string {
	var parts []string
	if existing != "" {
		parts = append(parts, existing)
	}
	if outcome.Reason != "" {
		parts = append(parts, "AI: "+outcome.Reason)
	}
	if outcome.Assumption != "" {
		parts = append(parts, "Assumption: "+outcome.Assumption)
	}
	return strings.Join(parts, "\n\n")
}

func (c *Client) fetchTransactions(ctx context.Context, params url.Values, keep func(Split) bool) ([]Transaction, error) {
	var txns []Transaction
	for page := 1; ; page++ {
		p := url.Values{}
		for k, v := range params {
			p[k] = v
		}
		p.Set("page", fmt.Sprintf("%d", page))

		u := fmt.Sprintf("%s/api/v1/transactions?%s", c.baseURL, p.Encode())
		var resp transactionsResponse
		if err := c.get(ctx, u, &resp); err != nil {
			return nil, fmt.Errorf("get transactions page %d: %w", page, err)
		}

		for _, item := range resp.Data {
			txn := toTransaction(item)
			var kept []Split
			for _, s := range txn.Splits {
				if keep(s) {
					kept = append(kept, s)
				}
			}
			if len(kept) > 0 {
				txn.Splits = kept
				txns = append(txns, txn)
			}
		}

		if page >= resp.Meta.Pagination.TotalPages {
			break
		}
	}
	return txns, nil
}

func toTransaction(item transactionData) Transaction {
	txn := Transaction{ID: item.ID}
	for _, s := range item.Attributes.Transactions {
		split := Split{
			JournalID:       s.TransactionJournalID,
			Type:            s.Type,
			Date:            s.Date,
			Description:     s.Description,
			SourceName:      s.SourceName,
			SourceID:        s.SourceID,
			DestinationName: s.DestinationName,
			DestinationID:   s.DestinationID,
			Amount:          s.Amount,
			CategoryID:      s.CategoryID,
			CategoryName:    s.CategoryName,
			Notes:           s.Notes,
		}
		if s.Tags != nil {
			split.Tags = s.Tags
		}
		txn.Splits = append(txn.Splits, split)
	}
	return txn
}

// --- HTTP helpers ---

func (c *Client) get(ctx context.Context, u string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) put(ctx context.Context, u string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func contains(s []string, v string) bool {
	for _, item := range s {
		if item == v {
			return true
		}
	}
	return false
}

// --- Firefly III API response shapes ---

type paginationMeta struct {
	TotalPages int `json:"total_pages"`
	Total      int `json:"total"`
}

type meta struct {
	Pagination paginationMeta `json:"pagination"`
}

type categoryAttributes struct {
	Name  string `json:"name"`
	Notes string `json:"notes"`
}

type categoryData struct {
	ID         string             `json:"id"`
	Attributes categoryAttributes `json:"attributes"`
}

type categoriesResponse struct {
	Data []categoryData `json:"data"`
	Meta meta           `json:"meta"`
}

type accountAttributes struct {
	Name string `json:"name"`
}

type accountData struct {
	ID         string            `json:"id"`
	Attributes accountAttributes `json:"attributes"`
}

type accountsResponse struct {
	Data []accountData `json:"data"`
	Meta meta          `json:"meta"`
}

type accountResponse struct {
	Data accountData `json:"data"`
}

type tagAttributes struct {
	Tag string `json:"tag"`
}

type tagData struct {
	ID         string        `json:"id"`
	Attributes tagAttributes `json:"attributes"`
}

type tagsResponse struct {
	Data []tagData `json:"data"`
	Meta meta      `json:"meta"`
}

type splitData struct {
	TransactionJournalID string   `json:"transaction_journal_id"`
	Type                 string   `json:"type"`
	Date                 string   `json:"date"`
	Description          string   `json:"description"`
	SourceName           string   `json:"source_name"`
	SourceID             string   `json:"source_id"`
	DestinationName      string   `json:"destination_name"`
	DestinationID        string   `json:"destination_id"`
	Amount               string   `json:"amount"`
	CategoryID           string   `json:"category_id"`
	CategoryName         string   `json:"category_name"`
	Tags                 []string `json:"tags"`
	Notes                string   `json:"notes"`
}

type transactionAttributes struct {
	Transactions []splitData `json:"transactions"`
}

type transactionData struct {
	ID         string                `json:"id"`
	Attributes transactionAttributes `json:"attributes"`
}

type transactionsResponse struct {
	Data []transactionData `json:"data"`
	Meta meta              `json:"meta"`
}

type singleTransactionResponse struct {
	Data transactionData `json:"data"`
}

type preferenceAttributes struct {
	Name string      `json:"name"`
	Data interface{} `json:"data"`
}

type preferenceData struct {
	Attributes preferenceAttributes `json:"attributes"`
}

type preferenceResponse struct {
	Data preferenceData `json:"data"`
}
