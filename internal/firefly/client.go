package firefly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

		su := splitUpdate{TransactionJournalID: s.JournalID, Tags: tags}
		if outcome.Outcome != "NEEDS_REVIEW" && outcome.CategoryID != "" {
			su.CategoryID = outcome.CategoryID
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
			DestinationName: s.DestinationName,
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

type splitData struct {
	TransactionJournalID string   `json:"transaction_journal_id"`
	Type                 string   `json:"type"`
	Date                 string   `json:"date"`
	Description          string   `json:"description"`
	DestinationName      string   `json:"destination_name"`
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
