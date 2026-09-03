package config

import (
	"encoding/json"
	"os"
	"sync"
)

const StoreFile = "config.json"

// StoredConfig holds fields that can be changed at runtime via the UI
// and persisted to disk. Each non-empty field overrides the corresponding
// env var. Empty string means "use the env var / default".
type StoredConfig struct {
	FireflyURL          string `json:"firefly_url,omitempty"`
	FireflyToken        string `json:"firefly_token,omitempty"`
	AIProvider          string `json:"ai_provider,omitempty"`
	OpenAIKey           string `json:"openai_api_key,omitempty"`
	OpenAIModel         string `json:"openai_model,omitempty"`
	OpenAIBaseURL       string `json:"openai_base_url,omitempty"`
	GeminiKey           string `json:"gemini_api_key,omitempty"`
	GeminiModel         string `json:"gemini_model,omitempty"`
	DeepseekKey         string `json:"deepseek_api_key,omitempty"`
	DeepseekModel       string `json:"deepseek_model,omitempty"`
	TagPrefix           string `json:"tag_prefix,omitempty"`
	CustomSystemContext string `json:"custom_system_context,omitempty"`

	HistoryContextLimit     int           `json:"history_context_limit,omitempty"`
	HistoryLookbackDays     int           `json:"history_lookback_days,omitempty"`
	DestinationMatchEnabled *bool         `json:"destination_match_enabled,omitempty"`
	TagSuggestEnabled       *bool         `json:"tag_suggest_enabled,omitempty"`
	TagSuggestMax           int           `json:"tag_suggest_max,omitempty"`
	SearchEngine            string        `json:"search_engine,omitempty"` // "google", "duckduckgo", or "" (disabled)
	GeminiThinking          string        `json:"gemini_thinking,omitempty"`
	MailMappings            []MailMapping `json:"mail_mappings,omitempty"`
	WorkerConcurrency       int           `json:"worker_concurrency,omitempty"`
	BatchConcurrency        int           `json:"batch_concurrency,omitempty"`
}

// Store manages a JSON config file that overlays environment variables.
type Store struct {
	mu   sync.RWMutex
	path string
	data StoredConfig
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Get() StoredConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

func (s *Store) Save(sc StoredConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = sc
	return s.write()
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &s.data)
}

func (s *Store) write() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}
