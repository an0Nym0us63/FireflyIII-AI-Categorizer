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

	TagPrefix          string
	CustomSystemContext string

	HistoryCacheTTL     time.Duration
	HistoryLookbackDays int
	HistoryContextLimit int

	WorkerConcurrency int
	BatchConcurrency  int
}

// Load reads config from environment variables and overlays the config file.
// Required fields (FireflyURL, token, AI key) are not validated here — missing
// fields are reported gracefully at the API layer so the server can start and
// allow first-time configuration via the UI.
func Load() (*Config, *Store, error) {
	cfg := &Config{
		Port:                getEnv("PORT", "3000"),
		EnableUI:            getEnv("ENABLE_UI", "false") == "true",
		AIProvider:          getEnv("AI_PROVIDER", "openai"),
		FireflyURL:          getEnv("FIREFLY_URL", ""),
		FireflyToken:        getEnv("FIREFLY_PERSONAL_TOKEN", ""),
		OpenAIKey:           getEnv("OPENAI_API_KEY", ""),
		OpenAIModel:         getEnv("OPENAI_MODEL", "gpt-4o-mini"),
		OpenAIBaseURL:       getEnv("OPENAI_BASE_URL", ""),
		GeminiKey:           getEnv("GEMINI_API_KEY", ""),
		GeminiModel:         getEnv("GEMINI_MODEL", "gemini-3.1-flash-lite"),
		DeepseekKey:         getEnv("DEEPSEEK_API_KEY", ""),
		DeepseekModel:       getEnv("DEEPSEEK_MODEL", "deepseek-chat"),
		TagPrefix:           getEnv("TAG_PREFIX", "ai"),
		HistoryContextLimit: getEnvInt("HISTORY_CONTEXT_LIMIT", 5),
		HistoryLookbackDays: getEnvInt("HISTORY_LOOKBACK_DAYS", 90),
		WorkerConcurrency:   getEnvInt("WORKER_CONCURRENCY", 1),
		BatchConcurrency:    getEnvInt("BATCH_CONCURRENCY", 5),
	}

	ttlStr := getEnv("HISTORY_CACHE_TTL", "10m")
	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid HISTORY_CACHE_TTL %q: %w", ttlStr, err)
	}
	cfg.HistoryCacheTTL = ttl

	store, err := NewStore(StoreFile)
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
}

// IsConfigured returns true if all fields required to run the pipeline are present.
func (c *Config) IsConfigured() bool {
	if c.FireflyURL == "" || c.FireflyToken == "" {
		return false
	}
	switch c.AIProvider {
	case "openai":
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
