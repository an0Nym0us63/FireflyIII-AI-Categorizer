package classifier

import (
	"encoding/json"
	"fmt"
	"strings"
)

type rawResponse struct {
	Outcome    string  `json:"outcome"`
	Category   *string `json:"category"`
	Reason     string  `json:"reason"`
	Assumption *string `json:"assumption"`
}

// parseResponse validates and converts a raw JSON string into a Result.
// It falls back to NeedsReview on any structural or validation error.
func parseResponse(raw, prompt string, categories []Category) Result {
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

	category := ""
	if r.Category != nil {
		category = *r.Category
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

	return Result{
		Outcome:     outcome,
		Category:    category,
		Reason:      r.Reason,
		Assumption:  assumption,
		RawPrompt:   prompt,
		RawResponse: raw,
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
