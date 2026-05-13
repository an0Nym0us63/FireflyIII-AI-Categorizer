package classifier

import (
	"context"
	"strings"
)

type Outcome string

const (
	Classified  Outcome = "CLASSIFIED"
	Assumed     Outcome = "ASSUMED"
	NeedsReview Outcome = "NEEDS_REVIEW"
)

type HistoricalEntry struct {
	TransactionID   string
	DestinationName string
	Description     string
	CategoryName    string
	GroupKey        string  // effective lookup key — destination name, or description when destination is blank
	Amount          float64 // 0 means unknown; used for history-match confidence check
}

// GroupKey returns the field to use when grouping and looking up transaction
// history. When destination_name is absent or a generic placeholder, description
// is used instead so that past context is still surfaced for nameless payees.
func GroupKey(destinationName, description string) string {
	if isBlankName(destinationName) {
		return description
	}
	return destinationName
}

func isBlankName(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "no name", "(no name)", "unknown", "n/a":
		return true
	}
	return false
}

// Category is a classifiable Firefly III category with optional notes that
// provide the LLM with extra context about what belongs in each category.
type Category struct {
	Name  string
	Notes string
}

type Request struct {
	Categories      []Category
	DestinationName string
	Description     string
	Amount          *float64
	History         []HistoricalEntry
}

type Result struct {
	Outcome     Outcome
	Category    string // empty if NeedsReview
	Reason      string
	Assumption  string // non-empty only when Assumed
	RawPrompt   string
	RawResponse string
}

type Classifier interface {
	Classify(ctx context.Context, req Request) (Result, error)
}
