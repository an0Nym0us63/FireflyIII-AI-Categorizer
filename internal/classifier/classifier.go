package classifier

import (
	"context"
	"regexp"
	"strings"
	"time"
)

type Outcome string

const (
	Classified  Outcome = "CLASSIFIED"
	Assumed     Outcome = "ASSUMED"
	NeedsReview Outcome = "NEEDS_REVIEW"
)

type HistoricalEntry struct {
	TransactionID        string
	DestinationName      string
	Description          string
	CategoryName         string
	GroupKey             string    // effective lookup key — destination name, or description when destination is blank
	Amount               float64   // 0 means unknown; used for history-match confidence check
	DestinationAccountID string    // expense account ID from a previous classification (empty if unknown)
	Tags                 []string  // semantic (non-control) tags from the past transaction
	Date                 time.Time // operation date (for recency weighting); zero if unknown
}

// GroupKey returns the key used to group and look up transaction history.
// French bank descriptions carry the merchant but wrapped in noise (card
// prefix, payment-processor prefix, reference, date) that differs on every
// visit, so grouping on the raw description never matches. GroupKey therefore
// derives a stable "merchant fingerprint" from the description and uses that.
// It falls back to the destination name (when it is a real payee) or the raw
// description only when no merchant token can be extracted.
//
// Because both the incoming transaction and the stored history entries go
// through this same function, the two sides always align on the same key.
func GroupKey(destinationName, description string) string {
	if fp := merchantFingerprint(description); fp != "" {
		return fp
	}
	if !isBlankName(destinationName) {
		return strings.ToLower(strings.TrimSpace(destinationName))
	}
	return strings.ToLower(strings.TrimSpace(description))
}

// tagControlMarkers identify the app's internal control tags (not semantic).
var tagControlMarkers = []string{":classified", ":assumed", ":needs-review", ":dest-assumed", ":reviewed", ":suggest:", ":tags-assumed"}

// SemanticTags returns the AI content tags, excluding the app's internal control
// tags (ai:classified, ai:suggest:..., etc.).
func SemanticTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		if strings.TrimSpace(t) == "" {
			continue
		}
		ctrl := false
		for _, m := range tagControlMarkers {
			if strings.Contains(t, m) {
				ctrl = true
				break
			}
		}
		if !ctrl {
			out = append(out, t)
		}
	}
	return out
}

// IsGenericAccountName reports whether name is blank or a generic placeholder
// (e.g. "(no name)", "Cash account"). Exposed so other packages (history cache)
// can drop unsorted transactions that still sit on such an account.
func IsGenericAccountName(name string) bool {
	return isBlankName(name)
}

var (
	reCardPrefix = regexp.MustCompile(`^X\d{3,4}\s+`)
	reDatePart   = regexp.MustCompile(`\b\d{2}/\d{2}(?:/\d{2,4})?\b`)
	reProcessor  = regexp.MustCompile(`^(HPY|SUMUP|SUM-UP|PAYPAL|PP|SQ|SQUARE|STRIPE|MOLLIE|CKO|ADYEN|IZ|IZETTLE|ZTL|ZETTLE|LYDIA|GOCARDLESS|GC|PADDLE|WISE|REVOLUT|SP|SC)\*`)
	reHasDigit   = regexp.MustCompile(`\d`)
)

// txnTypePrefixes are leading French bank keywords to strip before the merchant.
var txnTypePrefixes = []string{
	"PAIEMENT CB", "FACTURE CARTE", "ACHAT CB", "ACHAT CARTE",
	"PRLV SEPA", "PRELEVEMENT SEPA", "PRLV", "PRELEVEMENT",
	"VIR SEPA", "VIREMENT SEPA", "VIR", "VIREMENT",
	"RETRAIT DAB", "RETRAIT", "DAB", "CARTE", "CB", "SEPA",
}

// merchantStopwords are short leading words that don't identify a merchant on
// their own, so the next token is appended for context.
var merchantStopwords = map[string]bool{
	"LE": true, "LA": true, "LES": true, "L": true, "AU": true, "AUX": true,
	"DU": true, "DE": true, "DES": true, "CHEZ": true, "THE": true, "A": true,
}

// merchantFingerprint extracts a stable merchant key from a noisy French/CA
// bank description, or "" when nothing usable can be extracted. Heuristic by
// design — worst case it returns "" and the caller falls back.
func merchantFingerprint(description string) string {
	s := strings.ToUpper(strings.TrimSpace(description))
	if s == "" {
		return ""
	}

	// Drop leading card marker like "X0938 ".
	s = reCardPrefix.ReplaceAllString(s, "")

	// Cut everything from the first date onward (e.g. "01/09", "07/2026").
	if loc := reDatePart.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	s = strings.TrimSpace(s)

	// Strip leading transaction-type prefixes, repeatedly.
	changed := true
	for changed {
		changed = false
		for _, p := range txnTypePrefixes {
			if s == p {
				s = ""
			} else if strings.HasPrefix(s, p+" ") {
				s = strings.TrimSpace(s[len(p):])
				changed = true
			}
		}
	}

	// Strip a leading payment-processor prefix like "HPY*".
	s = strings.TrimSpace(reProcessor.ReplaceAllString(s, ""))
	if s == "" {
		return ""
	}

	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}

	// Cut a token at "*" (separates merchant from its reference) and trim edges.
	clean := func(tok string) string {
		if i := strings.IndexByte(tok, '*'); i >= 0 {
			tok = tok[:i]
		}
		return strings.Trim(tok, ".-_/")
	}

	first := clean(fields[0])
	key := first
	if merchantStopwords[first] && len(fields) > 1 {
		key = first + " " + clean(fields[1])
	}

	key = strings.ToLower(strings.TrimSpace(key))
	// Reject useless keys: too short or reference-like (contains a digit).
	if len(key) < 3 || reHasDigit.MatchString(key) {
		return ""
	}
	return key
}

func isBlankName(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "no name", "(no name)", "unknown", "n/a",
		"cash account", "(cash account)", "cash", "(cash)":
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

// AccountCandidate is an expense account that the LLM can match or decide to create.
type AccountCandidate struct {
	ID   string
	Name string
}

// DestinationResult captures the LLM's decision about the destination account.
type DestinationResult struct {
	Name       string // desired account name
	Action     string // "MATCH" or "CREATE"
	Confidence string // "CLASSIFIED" or "ASSUMED"
}

// TagResult captures the LLM's decision about a single semantic tag.
type TagResult struct {
	Name       string // tag name
	Action     string // "MATCH" (reuse existing) or "CREATE" (new tag)
	Confidence string // "CLASSIFIED" or "ASSUMED"
}

type Request struct {
	Categories      []Category
	DestinationName string
	Description     string
	Amount          *float64
	History         []HistoricalEntry

	// ExpenseAccounts is set when destination-account matching is enabled.
	ExpenseAccounts []AccountCandidate

	// ExistingTags is the list of existing Firefly tag names offered to the LLM
	// for reuse when tag suggestion is enabled.
	ExistingTags []string

	// TagSuggestion enables semantic tag suggestion in the prompt and parsing.
	TagSuggestion bool

	// TagMax caps how many tags the LLM may return (0 falls back to a default).
	TagMax int

	// ExtraContext is optional per-transaction context injected into the prompt
	// (e.g. Amazon order contents matched from an order-history export).
	ExtraContext string

	// Notes is the transaction's existing notes, surfaced to the LLM as hints.
	Notes string

	// MerchantFromContent asks the LLM to set the destination to the real
	// merchant found in ExtraContext (the bank payee is a payment processor).
	MerchantFromContent bool

	// DestinationMatching signals to the classifier that the system prompt
	// and response parsing should include destination-account logic.
	DestinationMatching bool

	// CategoryOnly signals that only category classification is needed
	// (destination info is stripped from the prompt). When true, the classifier
	// won't bother computing a destination even if expense accounts are provided.
	CategoryOnly bool

	// SystemPromptOverride replaces the built-in system prompt when non-empty.
	// Used for special-purpose LLM calls like transfer destination suggestion.
	SystemPromptOverride string
}

type Result struct {
	Outcome     Outcome
	Category    string // empty if NeedsReview
	Reason      string
	Assumption  string // non-empty only when Assumed
	RawPrompt   string
	RawResponse string

	// Destination is set when destination-account matching was enabled
	// and the LLM returned a valid destination suggestion.
	Destination *DestinationResult

	// Tags holds semantic tag suggestions when TagSuggestion was enabled.
	Tags []TagResult

	// Items lists the individual products in the order (from ExtraContext), for notes.
	Items []string
}

type Classifier interface {
	Classify(ctx context.Context, req Request) (Result, error)
}
