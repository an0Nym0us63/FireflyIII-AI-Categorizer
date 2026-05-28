package job

import "time"

type Status string

const (
	StatusQueued     Status = "queued"
	StatusInProgress Status = "in_progress"
	StatusFinished   Status = "finished"
	StatusFailed     Status = "failed"
)

type Job struct {
	ID      string `json:"id"`
	BatchID string `json:"batch_id,omitempty"`
	Status  Status `json:"status"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	TransactionID   string   `json:"transaction_id"`
	DestinationName string   `json:"destination_name"`
	Description     string   `json:"description"`
	Amount          *float64 `json:"amount,omitempty"`

	Outcome     string `json:"outcome,omitempty"`
	Category    string `json:"category,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Assumption  string `json:"assumption,omitempty"`
	RawPrompt   string `json:"raw_prompt,omitempty"`
	RawResponse string `json:"raw_response,omitempty"`

	DestinationAccount string `json:"destination_account,omitempty"`
	DestinationAction  string `json:"destination_action,omitempty"` // "MATCH" or "CREATE"

	Error string `json:"error,omitempty"`
}
