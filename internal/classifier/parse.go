package classifier

import (
	"encoding/json"
	"fmt"
	"strings"
)

type rawCategory struct {
	Name       string `json:"name"`
	Confidence string `json:"confidence"`
}

type rawDestination struct {
	Name       string `json:"name"`
	Action     string `json:"action"`
	Confidence string `json:"confidence"`
}

// rawResponse uses json.RawMessage for category so we can accept both the
// old flat-string format and the new {name, confidence} object format.
type rawResponse struct {
	Outcome     string           `json:"outcome"`
	Category    json.RawMessage  `json:"category"`
	Reason      string           `json:"reason"`
	Assumption  *string          `json:"assumption"`
	Destination *rawDestination  `json:"destination"`
}

// parseResponse validates and converts a raw JSON string into a Result.
// It falls back to NeedsReview on any structural or validation error.
// When destinationMatching is true, the destination field is also parsed.
func parseResponse(raw, prompt string, categories []Category, expenseAccounts []AccountCandidate, destinationMatching bool) Result {
	cleaned := stripMarkdown(raw)

	var r rawResponse
	if err := json.Unmarshal([]byte(cleaned), &r); err != nil {
		return Result{
			Outcome:     NeedsReview,
			Reason:      fmt.Sprintf("classifier returned unparseable response: %v", err),
			RawPrompt:   prompt,
			RawResponse: raw,
		}
	}

	outcome := Outcome(r.Outcome)
	switch outcome {
	case Classified, Assumed, NeedsReview:
	default:
		outcome = NeedsReview
		r.Reason = fmt.Sprintf("classifier returned unknown outcome %q", r.Outcome)
		r.Category = nil
	}

	// Parse category — supports both old flat string and new {name, confidence} object.
	category := ""
	if len(r.Category) > 0 {
		// Try the new object format first.
		var catObj rawCategory
		if err := json.Unmarshal(r.Category, &catObj); err == nil && catObj.Name != "" {
			category = catObj.Name
		} else {
			// Fall back to old flat-string format.
			var catStr string
			if err := json.Unmarshal(r.Category, &catStr); err == nil {
				category = catStr
			}
		}
	}

	if category != "" && !categoryNameIn(categories, category) {
		// Don't use the unknown category name in the reason message since we've already
		// overwritten category to "" below — capture it first.
		badCat := category
		outcome = NeedsReview
		category = ""
		r.Reason = fmt.Sprintf("classifier returned unknown category %q", badCat)
	}

	assumption := ""
	if r.Assumption != nil {
		assumption = *r.Assumption
	}

	var dest *DestinationResult
	if destinationMatching && r.Destination != nil && r.Destination.Name != "" {
		d := r.Destination
		dest = &DestinationResult{
			Name:       d.Name,
			Action:     d.Action,
			Confidence: d.Confidence,
		}
		if dest.Action != "MATCH" && dest.Action != "CREATE" {
			dest = nil // invalid action → ignore
		}
		if dest.Confidence != "CLASSIFIED" && dest.Confidence != "ASSUMED" {
			dest = nil // invalid confidence → ignore
		}
		if dest != nil && dest.Action == "MATCH" && !accountNameIn(dest.Name, expenseAccounts) {
			// MATCH requires the name to exist in the provided list.
			dest = nil
		}
	}

	return Result{
		Outcome:     outcome,
		Category:    category,
		Reason:      r.Reason,
		Assumption:  assumption,
		RawPrompt:   prompt,
		RawResponse: raw,
		Destination: dest,
	}
}

func categoryNameIn(cats []Category, name string) bool {
	for _, c := range cats {
		if c.Name == name {
			return true
		}
	}
	return false
}

func accountNameIn(name string, accounts []AccountCandidate) bool {
	for _, a := range accounts {
		if strings.EqualFold(a.Name, name) {
			return true
		}
	}
	return false
}

func stripMarkdown(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		// Remove opening fence (```json or ```)
		end := strings.Index(s[3:], "\n")
		if end >= 0 {
			s = s[3+end+1:]
		}
		// Remove closing fence
		if idx := strings.LastIndex(s, "```"); idx >= 0 {
			s = s[:idx]
		}
		s = strings.TrimSpace(s)
	}
	return s
}
