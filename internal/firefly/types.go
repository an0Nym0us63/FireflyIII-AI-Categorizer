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

	Tags        []string // confident semantic tags to apply to the transaction
	TagsAssumed []string // low-confidence semantic tags surfaced for review (not applied)
	Items       []string // individual order items (for notes / later splitting)
	RemoveTags  []string // tags to strip from the transaction's existing tags
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
	// Populated by the API from the local AI store, not from Firefly.
	AIStatus        string   `json:"ai_status,omitempty"`
	AISuggestedTags []string `json:"ai_suggested_tags,omitempty"`
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

// Direction distinguishes an expense (outflow) from an income (inflow),
// relative to the user's asset account. The external counterparty the
// categorizer resolves is the destination for outflows, the source for inflows.
type Direction string

const (
	Outflow Direction = "withdrawal" // expense; counterparty = destination
	Inflow  Direction = "deposit"    // income;  counterparty = source
)

// FireflyType returns the Firefly transaction "type" string for this direction.
func (d Direction) FireflyType() string { return string(d) }

// DirectionOf maps a Firefly transaction type string to a Direction.
func DirectionOf(txType string) Direction {
	if txType == "deposit" {
		return Inflow
	}
	return Outflow
}

// IsInflow reports whether this split is an income (deposit).
func (s Split) IsInflow() bool { return s.Type == "deposit" }

// CounterpartyName returns the external party of the split: the destination
// (payee / expense account) for a withdrawal, the source (payer / revenue
// account) for a deposit. This is the account the categorizer resolves.
func (s Split) CounterpartyName() string {
	if s.IsInflow() {
		return s.SourceName
	}
	return s.DestinationName
}

// CounterpartyID mirrors CounterpartyName for the account ID.
func (s Split) CounterpartyID() string {
	if s.IsInflow() {
		return s.SourceID
	}
	return s.DestinationID
}
