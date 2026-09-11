package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port     string
	EnableUI bool

	FireflyURL   string
	FireflyToken string

	AIProvider string // "openai" | "gemini" | "deepseek"

	OpenAIKey     string
	OpenAIModel   string
	OpenAIBaseURL string

	GeminiKey   string
	GeminiModel string

	DeepseekKey   string
	DeepseekModel string

	TagPrefix           string
	CustomSystemContext string

	HistoryCacheTTL     time.Duration
	HistoryLookbackDays int
	HistoryContextLimit int

	DestinationMatchEnabled   bool
	IncomeEnabled             bool // process deposits (income) as well as withdrawals
	WebhookProcessCategorized bool // if true, the webhook processes a txn even when it already has a category

	TagSuggestEnabled bool
	TagSuggestMax     int

	AmazonOrdersFile string
	PayPalCsvFile    string

	MailAccounts  []MailAccount
	MailDetectors []MailDetector
	SalaryPeople  []SalaryPerson

	ForceDestinations     []string  // if a txn's current destination is here, force re-pick
	ForceCategories       []string  // if a txn's current category is here, force re-categorize
	PlaceholderCategories []string  // treated as empty (normal flow: automatch then AI), not skipped
	TagRules              []TagRule // tags to strip (optionally replace) from transactions

	GeminiThinking  string
	GeminiGrounding bool
	AIDBFile        string

	SearchEngine string // "google", "duckduckgo", or "" (disabled)

	WorkerConcurrency int
	BatchConcurrency  int
	BulkConcurrency   int
}

// TagRule strips a tag from a transaction; To optionally replaces it.
type TagRule struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// MailAccount describes an IMAP mailbox to search for order emails.
// SalarySource maps a description keyword to a salary company + timing rule.
type SalarySource struct {
	Keyword    string `json:"keyword"`     // searched in the description (case-insensitive)
	SourceName string `json:"source_name"` // company / revenue account to set as the source
	DayLimit   int    `json:"day_limit"`   // if the txn day-of-month > DayLimit, book it on the 1st of next month
}

// SalaryPerson declares a person and the salary sources identifying their pay.
type SalaryPerson struct {
	Name    string         `json:"name"`
	Sources []SalarySource `json:"sources"`
}

type MailAccount struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	IMAPHost     string `json:"imap_host"`
	IMAPPort     int    `json:"imap_port"`
	IMAPUser     string `json:"imap_user"`
	IMAPPassword string `json:"imap_password"`
}

// MailDetector maps description keywords to a mailbox + expected sender(s).
type MailDetector struct {
	ID                 string   `json:"id"`
	Keywords           []string `json:"keywords"`            // matched in the bank description
	AccountID          string   `json:"account_id"`          // which MailAccount to search
	Senders            []string `json:"senders"`             // From addresses of the order emails
	ReplaceDestination bool     `json:"replace_destination"` // let the AI set the real merchant as destination
	Tag                string   `json:"tag"`                 // extra tag to always apply (e.g. "paypal")
	BackDays           int      `json:"back_days"`           // days before the bank date to search (0 = default 14)
	FwdDays            int      `json:"fwd_days"`            // days after the bank date to search (0 = default 2)
	SubjectContains    string   `json:"subject_contains"`    // only emails whose subject contains this (case-insensitive)
	Aggregate          bool     `json:"aggregate"`           // group emails by order number and sum amounts
	Direction          string   `json:"direction"`           // ""/"withdrawal" | "deposit" | "both" — which transaction sense this detector applies to
	DefaultCategory    string   `json:"default_category"`    // fallback category when parsing is ignored (no email found)
	DefaultDestination string   `json:"default_destination"` // fallback counterparty (dest for expense, source for income)
	DefaultTags        []string `json:"default_tags"`        // fallback tags
}

// AppliesTo reports whether this detector should run for the given direction
// ("withdrawal" or "deposit"). Empty scope means withdrawal-only (back-compat:
// existing detectors are expense-oriented, so income is never enriched unless
// the detector is explicitly scoped to deposit/both).
func (d MailDetector) AppliesTo(direction string) bool {
	switch d.Direction {
	case "both":
		return true
	case "deposit":
		return direction == "deposit"
	default: // "" or "withdrawal"
		return direction == "withdrawal"
	}
}

// BackDaysOr returns the configured look-back window or the default.
func (d MailDetector) BackDaysOr(def int) int {
	if d.BackDays > 0 {
		return d.BackDays
	}
	return def
}

// FwdDaysOr returns the configured look-forward window or the default.
func (d MailDetector) FwdDaysOr(def int) int {
	if d.FwdDays > 0 {
		return d.FwdDays
	}
	return def
}

// Load reads config from environment variables and overlays the config file.
// Required fields (FireflyURL, token, AI key) are not validated here — missing
// fields are reported gracefully at the API layer so the server can start and
// allow first-time configuration via the UI.
func Load() (*Config, *Store, error) {
	cfg := &Config{
		Port:                      getEnv("PORT", "3000"),
		EnableUI:                  getEnv("ENABLE_UI", "true") != "false",
		AIProvider:                getEnv("AI_PROVIDER", "openai"),
		FireflyURL:                getEnv("FIREFLY_URL", ""),
		FireflyToken:              getEnv("FIREFLY_PERSONAL_TOKEN", ""),
		OpenAIKey:                 getEnv("OPENAI_API_KEY", ""),
		OpenAIModel:               getEnv("OPENAI_MODEL", "gpt-4o-mini"),
		OpenAIBaseURL:             getEnv("OPENAI_BASE_URL", ""),
		GeminiKey:                 getEnv("GEMINI_API_KEY", ""),
		GeminiModel:               getEnv("GEMINI_MODEL", "gemini-3.1-flash-lite"),
		DeepseekKey:               getEnv("DEEPSEEK_API_KEY", ""),
		DeepseekModel:             getEnv("DEEPSEEK_MODEL", "deepseek-chat"),
		TagPrefix:                 getEnv("TAG_PREFIX", "ai"),
		HistoryContextLimit:       getEnvInt("HISTORY_CONTEXT_LIMIT", 5),
		HistoryLookbackDays:       getEnvInt("HISTORY_LOOKBACK_DAYS", 365),
		DestinationMatchEnabled:   getEnv("DESTINATION_MATCH_ENABLED", "false") == "true",
		IncomeEnabled:             getEnv("INCOME_ENABLED", "false") == "true",
		WebhookProcessCategorized: getEnv("WEBHOOK_PROCESS_CATEGORIZED", "false") == "true",
		TagSuggestEnabled:         getEnv("TAG_SUGGEST_ENABLED", "false") == "true",
		TagSuggestMax:             getEnvInt("TAG_SUGGEST_MAX", 3),
		AmazonOrdersFile:          getEnv("AMAZON_ORDERS_FILE", "/data/amazon_orders.csv"),
		PayPalCsvFile:             getEnv("PAYPAL_CSV_FILE", "/data/paypal"),
		GeminiThinking:            getEnv("GEMINI_THINKING", "low"),
		AIDBFile:                  getEnv("AI_DB_FILE", "/data/ai.db"),
		WorkerConcurrency:         getEnvInt("WORKER_CONCURRENCY", 1),
		BatchConcurrency:          getEnvInt("BATCH_CONCURRENCY", 3),
		BulkConcurrency:           getEnvInt("BULK_CONCURRENCY", 4),
	}

	ttlStr := getEnv("HISTORY_CACHE_TTL", "10m")
	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid HISTORY_CACHE_TTL %q: %w", ttlStr, err)
	}
	cfg.HistoryCacheTTL = ttl

	storeFile := getEnv("CONFIG_FILE", StoreFile)
	store, err := NewStore(storeFile)
	if err != nil {
		return nil, nil, fmt.Errorf("config store: %w", err)
	}

	ApplyStored(cfg, store.Get())

	return cfg, store, nil
}

// ApplyStored overlays non-empty stored config values onto cfg.
func ApplyStored(cfg *Config, sc StoredConfig) {
	if sc.FireflyURL != "" {
		cfg.FireflyURL = sc.FireflyURL
	}
	if sc.FireflyToken != "" {
		cfg.FireflyToken = sc.FireflyToken
	}
	if sc.AIProvider != "" {
		cfg.AIProvider = sc.AIProvider
	}
	if sc.OpenAIKey != "" {
		cfg.OpenAIKey = sc.OpenAIKey
	}
	if sc.OpenAIModel != "" {
		cfg.OpenAIModel = sc.OpenAIModel
	}
	// OpenAIBaseURL can legitimately be empty; only apply if stored
	cfg.OpenAIBaseURL = sc.OpenAIBaseURL
	if sc.GeminiKey != "" {
		cfg.GeminiKey = sc.GeminiKey
	}
	if sc.GeminiModel != "" {
		cfg.GeminiModel = sc.GeminiModel
	}
	if sc.DeepseekKey != "" {
		cfg.DeepseekKey = sc.DeepseekKey
	}
	if sc.DeepseekModel != "" {
		cfg.DeepseekModel = sc.DeepseekModel
	}
	if sc.TagPrefix != "" {
		cfg.TagPrefix = sc.TagPrefix
	}
	// CustomSystemContext is only ever set via the file (not env vars), so always
	// apply it — including empty string, which clears a previously saved value.
	cfg.CustomSystemContext = sc.CustomSystemContext

	if sc.HistoryContextLimit > 0 {
		cfg.HistoryContextLimit = sc.HistoryContextLimit
	}
	if sc.HistoryLookbackDays > 0 {
		cfg.HistoryLookbackDays = sc.HistoryLookbackDays
	}
	if sc.WorkerConcurrency > 0 {
		cfg.WorkerConcurrency = sc.WorkerConcurrency
	}
	if sc.BatchConcurrency > 0 {
		cfg.BatchConcurrency = sc.BatchConcurrency
	}
	// DestinationMatchEnabled is a bool toggle; the stored value overrides
	// the env var only when explicitly set (distinct from zero-value).
	// We track this via a pointer so the store can signal "was configured".
	if sc.DestinationMatchEnabled != nil {
		cfg.DestinationMatchEnabled = *sc.DestinationMatchEnabled
	}
	if sc.IncomeEnabled != nil {
		cfg.IncomeEnabled = *sc.IncomeEnabled
	}
	if sc.WebhookProcessCategorized != nil {
		cfg.WebhookProcessCategorized = *sc.WebhookProcessCategorized
	}
	if sc.TagSuggestEnabled != nil {
		cfg.TagSuggestEnabled = *sc.TagSuggestEnabled
	}
	if sc.TagSuggestMax > 0 {
		cfg.TagSuggestMax = sc.TagSuggestMax
	}
	if sc.SearchEngine != "" {
		cfg.SearchEngine = sc.SearchEngine
	}
	if sc.GeminiThinking != "" {
		cfg.GeminiThinking = sc.GeminiThinking
	}
	cfg.GeminiGrounding = sc.GeminiGrounding
	if sc.MailAccounts != nil {
		cfg.MailAccounts = sc.MailAccounts
	}
	if sc.MailDetectors != nil {
		cfg.MailDetectors = sc.MailDetectors
	}
	if sc.ForceDestinations != nil {
		cfg.ForceDestinations = sc.ForceDestinations
	}
	if sc.ForceCategories != nil {
		cfg.ForceCategories = sc.ForceCategories
	}
	if sc.PlaceholderCategories != nil {
		cfg.PlaceholderCategories = sc.PlaceholderCategories
	}
	if sc.SalaryPeople != nil {
		cfg.SalaryPeople = sc.SalaryPeople
	}
	if sc.TagRules != nil {
		cfg.TagRules = sc.TagRules
	}
}

// IsConfigured returns true if all fields required to run the pipeline are present.
func (c *Config) IsConfigured() bool {
	if c.FireflyURL == "" || c.FireflyToken == "" {
		return false
	}
	switch c.AIProvider {
	case "openai":
		// If an OpenAI-compatible Base URL is provided (e.g. local Ollama),
		// an API key may be intentionally left blank. Treat that as configured
		// when a BaseURL is present.
		if c.OpenAIBaseURL != "" {
			return true
		}
		return c.OpenAIKey != ""
	case "gemini":
		return c.GeminiKey != ""
	case "deepseek":
		return c.DeepseekKey != ""
	}
	return false
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	s := getEnv(key, "")
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}
