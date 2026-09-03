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

type rawTag struct {
	Name       string `json:"name"`
	Action     string `json:"action"`
	Confidence string `json:"confidence"`
}

// rawResponse uses json.RawMessage for category so we can accept both the
// old flat-string format and the new {name, confidence} object format.
type rawResponse struct {
	Outcome     string          `json:"outcome"`
	Category    json.RawMessage `json:"category"`
	Reason      string          `json:"reason"`
	Assumption  *string         `json:"assumption"`
	Destination *rawDestination `json:"destination"`
	Tags        []rawTag        `json:"tags"`
	Items       []string        `json:"items"`
}

// parseResponse validates and converts a raw JSON string into a Result.
// It falls back to NeedsReview on any structural or validation error.
// When destinationMatching is true, the destination field is also parsed.
func parseResponse(raw, prompt string, req Request) Result {
	expenseAccounts := req.ExpenseAccounts
	destinationMatching := req.DestinationMatching
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

	// A category name not in the list is allowed: it will be created in Firefly.
	// (Previously such categories were rejected and sent to review.)

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

	var tags []TagResult
	if req.TagSuggestion && len(r.Tags) > 0 {
		max := req.TagMax
		if max <= 0 {
			max = 3
		}
		seen := make(map[string]bool)
		for _, t := range r.Tags {
			name := strings.TrimSpace(t.Name)
			if name == "" {
				continue
			}
			action := strings.ToUpper(strings.TrimSpace(t.Action))
			if action != "MATCH" && action != "CREATE" {
				continue
			}
			conf := strings.ToUpper(strings.TrimSpace(t.Confidence))
			if conf != "CLASSIFIED" && conf != "ASSUMED" {
				continue
			}
			// MATCH must reference an existing tag; otherwise treat it as CREATE.
			if action == "MATCH" && !tagNameIn(req.ExistingTags, name) {
				action = "CREATE"
			}
			key := strings.ToLower(name)
			if seen[key] {
				continue
			}
			seen[key] = true
			tags = append(tags, TagResult{Name: name, Action: action, Confidence: conf})
			if len(tags) >= max {
				break
			}
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
		Tags:        tags,
		Items:       r.Items,
	}
}

func tagNameIn(tags []string, name string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, name) {
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
