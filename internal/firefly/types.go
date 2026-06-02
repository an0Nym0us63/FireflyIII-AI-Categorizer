package firefly

// Category is a Firefly III category.
type Category struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Notes string `json:"notes,omitempty"`
}

// Account is a Firefly III account (used here for expense/revenue accounts).
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Split is one journal entry within a transaction group.
type Split struct {
	JournalID       string
	Type            string
	Date            string // ISO 8601
	Description     string
	SourceName      string // asset account name (for withdrawals)
	SourceID        string // asset account ID
	DestinationName string
	DestinationID   string // expense/revenue account ID (may be empty)
	Amount          string // Firefly returns amounts as decimal strings
	CategoryID      string
	CategoryName    string
	Tags            []string
	Notes           string
}

// Transaction is a Firefly III transaction group (may contain multiple splits).
type Transaction struct {
	ID     string
	Splits []Split
}

// UpdateOutcome carries classification results needed to update a transaction.
type UpdateOutcome struct {
	Outcome        string // "CLASSIFIED" | "ASSUMED" | "NEEDS_REVIEW"
	Category       string
	CategoryID     string
	DestinationID  string // non-empty when destination account was matched or created
	DestConfidence string // "CLASSIFIED" | "ASSUMED" — destination confidence (independent of Outcome)
	Reason         string
	Assumption     string
}

// TransactionRow is a flat summary used by the UI transactions list.
type TransactionRow struct {
	ID              string   `json:"id"`
	Date            string   `json:"date"`
	Description     string   `json:"description"`
	DestinationName string   `json:"destination_name"`
	Amount          string   `json:"amount"`
	CategoryID      string   `json:"category_id"`
	CategoryName    string   `json:"category_name"`
	Tags            []string `json:"tags"`
}

// TransactionsPage is the paginated response for the UI transactions list.
type TransactionsPage struct {
	Data       []TransactionRow `json:"data"`
	Page       int              `json:"page"`
	TotalPages int              `json:"total_pages"`
	Total      int              `json:"total"`
}

func hasCategory(id string) bool {
	return id != "" && id != "0"
}
