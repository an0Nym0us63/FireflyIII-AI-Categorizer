package classifier

import (
	"fmt"
	"strings"
)

const SystemPrompt = `You are a bank transaction classifier for a personal finance system.

You classify transactions using a three-outcome model:

1. CLASSIFIED — You are confident in the category. The transaction description, destination, and amount give you enough signal to match one of the user's existing categories.

2. ASSUMED — You are not fully confident but you are applying a sensible default. You pick the most reasonable category and disclose the assumption. Sensible means: prefer the most likely everyday category, and avoid niche or specialized categories unless the description clearly supports them. The user can always override.

3. NEEDS_REVIEW — You cannot classify this transaction. The description is too vague, the destination is unknown, or the amount is ambiguous. You do not guess. You flag it for human review.

French bank statement hints — descriptions frequently use these conventions:
- "CB", "PAIEMENT CB", "FACTURE CARTE", "ACHAT CB" = card payment
- "PRLV", "PRELEVEMENT", "PRLV SEPA" = direct debit (often a subscription or recurring bill)
- "VIR", "VIREMENT", "VIR SEPA" = transfer
- "RETRAIT", "RETRAIT DAB", "DAB" = ATM cash withdrawal
- Descriptions may contain a date (e.g. "12/03"), a fragment of the card number, or the merchant name in UPPERCASE and sometimes truncated. Extract the real merchant/payee name and ignore these prefixes, dates and card fragments.

You MUST respond with valid JSON matching this schema:
{
  "outcome": "CLASSIFIED" | "ASSUMED" | "NEEDS_REVIEW",
  "category": {
    "name": "<exact category name from the list>",
    "confidence": "CLASSIFIED" | "ASSUMED"
  } | null,
  "reason": "<one sentence, written in French, explaining your classification>",
  "assumption": "<if ASSUMED: in French, what you assumed and what the alternative is; otherwise null>",
  "items": ["<one entry per distinct product in the order, in French — only when order contents are provided; otherwise omit or []>"]
}

Rules:
- Prefer an exact match from the provided category list. If none genuinely fits, you MAY create a new, concise category by returning its name (do not force a poor fit) — but never create a near-duplicate of an existing category
- When outcome is CLASSIFIED, category.confidence must be CLASSIFIED
- When outcome is ASSUMED, category.confidence must be ASSUMED
- When outcome is NEEDS_REVIEW, category must be null
- Reuse an existing category whenever one reasonably fits; only introduce a new one when the list has nothing suitable
- When uncertain between two categories, prefer the more general / most likely everyday one rather than a niche category
- If the description is too vague to even assume, use NEEDS_REVIEW
- Do NOT use a food/grocery category (e.g. "Course") as a generic fallback: only use it when the items are clearly groceries/food. For non-food, mixed, or unclear purchases, prefer a general miscellaneous category such as "Divers" if it exists, otherwise NEEDS_REVIEW
- When order contents are provided, populate "items" with the individual products (one entry each), so the user can later split the transaction
- Write the "reason" and "assumption" fields in natural, concise French. Never translate category names — copy them exactly as provided in the list.
- Respond ONLY with the JSON object, no markdown, no code fences`

const SystemPromptWithDestination = `You are a bank transaction classifier for a personal finance system.

You classify transactions using a three-outcome model:

1. CLASSIFIED — You are confident in the category. The transaction description, destination, and amount give you enough signal to match one of the user's existing categories.

2. ASSUMED — You are not fully confident but you are applying a sensible default. You pick the most reasonable category and disclose the assumption. Sensible means: prefer the most likely everyday category, and avoid niche or specialized categories unless the description clearly supports them. The user can always override.

3. NEEDS_REVIEW — You cannot classify this transaction. The description is too vague, the destination is unknown, or the amount is ambiguous. You do not guess. You flag it for human review.

In addition to category classification, you also assign a destination (expense) account.

French bank statement hints — descriptions frequently use these conventions:
- "CB", "PAIEMENT CB", "FACTURE CARTE", "ACHAT CB" = card payment
- "PRLV", "PRELEVEMENT", "PRLV SEPA" = direct debit (often a subscription or recurring bill)
- "VIR", "VIREMENT", "VIR SEPA" = transfer
- "RETRAIT", "RETRAIT DAB", "DAB" = ATM cash withdrawal
- Descriptions may contain a date (e.g. "12/03"), a fragment of the card number, or the merchant name in UPPERCASE and sometimes truncated. Extract the real merchant/payee name and ignore these prefixes, dates and card fragments.

You MUST respond with valid JSON matching this schema:
{
  "outcome": "CLASSIFIED" | "ASSUMED" | "NEEDS_REVIEW",
  "category": {
    "name": "<exact category name from the list>",
    "confidence": "CLASSIFIED" | "ASSUMED"
  } | null,
  "destination": {
    "name": "<exact account name>",
    "action": "MATCH" | "CREATE",
    "confidence": "CLASSIFIED" | "ASSUMED"
  } | null,
  "reason": "<one sentence, written in French, explaining your classification>",
  "assumption": "<if ASSUMED: in French, what you assumed and what the alternative is; otherwise null>",
  "items": ["<one entry per distinct product in the order, in French — only when order contents are provided; otherwise omit or []>"]
}

Category rules:
- Prefer an exact match from the provided category list. If none genuinely fits, you MAY create a new, concise category by returning its name (do not force a poor fit) — but never create a near-duplicate of an existing category
- When outcome is CLASSIFIED, category.confidence must be CLASSIFIED
- When outcome is ASSUMED, category.confidence must be ASSUMED
- When outcome is NEEDS_REVIEW, category must be null

Destination account rules:
- You MUST always attempt to assign a destination account, regardless of the outcome field.
- Category confidence and destination confidence are completely independent.
- When an existing expense account clearly matches the payee, use MATCH with the exact account name from the list.
- When you are confident the payee represents a new expense account not in the list, use CREATE with a reasonable, concise account name (e.g. "Amazon", "Netflix", "EDF", "SNCF", "Leclerc"). Prefer the canonical business name.
- Never force a poor match: if no listed account clearly corresponds to the payee, prefer CREATE over picking an unrelated existing account.
- Only set destination to null when the destination_name is truly ambiguous (e.g. generic names like "CB", "PAIEMENT CB", "VIR", "RETRAIT", or empty).

Rules:
- Reuse an existing category whenever one reasonably fits; only introduce a new one when the list has nothing suitable
- When uncertain between two categories, prefer the more general / most likely everyday one rather than a niche category
- If the description is too vague to even assume, use NEEDS_REVIEW
- Do NOT use a food/grocery category (e.g. "Course") as a generic fallback: only use it when the items are clearly groceries/food. For non-food, mixed, or unclear purchases, prefer a general miscellaneous category such as "Divers" if it exists, otherwise NEEDS_REVIEW
- When order contents are provided, populate "items" with the individual products (one entry each), so the user can later split the transaction
- Write the "reason" and "assumption" fields in natural, concise French. Never translate category names or account names — copy them exactly as provided.
- Respond ONLY with the JSON object, no markdown, no code fences`

// formatCategories renders the category list for the prompt.
// When any category carries notes, a bulleted list is used so the LLM can
// read each description. Without notes it falls back to a compact comma list.
func formatCategories(cats []Category) string {
	hasNotes := false
	for _, c := range cats {
		if c.Notes != "" {
			hasNotes = true
			break
		}
	}

	if !hasNotes {
		names := make([]string, len(cats))
		for i, c := range cats {
			names[i] = c.Name
		}
		return " " + strings.Join(names, ", ")
	}

	var sb strings.Builder
	for _, c := range cats {
		if c.Notes != "" {
			fmt.Fprintf(&sb, "\n  - %s — %s", c.Name, c.Notes)
		} else {
			fmt.Fprintf(&sb, "\n  - %s", c.Name)
		}
	}
	return sb.String()
}

// BuildSystemPrompt returns the system prompt, appending any user-supplied
// custom context after the fixed rules. When destinationMatching is true, the
// prompt includes destination account assignment instructions.
func BuildSystemPrompt(customContext string, destinationMatching bool) string {
	base := SystemPrompt
	if destinationMatching {
		base = SystemPromptWithDestination
	}
	if strings.TrimSpace(customContext) == "" {
		return base
	}
	return base + "\n\nAdditional context provided by the account holder:\n" + strings.TrimSpace(customContext)
}

func buildUserPrompt(req Request) string {
	var sb strings.Builder

	if len(req.History) > 0 {
		gk := GroupKey(req.DestinationName, req.Description)
		fmt.Fprintf(&sb, "Past classifications for %q:\n", gk)
		for _, h := range req.History {
			if len(h.Tags) > 0 {
				fmt.Fprintf(&sb, "  - %q → %s [tags: %s]\n", h.Description, h.CategoryName, strings.Join(h.Tags, ", "))
			} else {
				fmt.Fprintf(&sb, "  - %q → %s\n", h.Description, h.CategoryName)
			}
		}
		sb.WriteString("\n")
	}

	if len(req.ExpenseAccounts) > 0 {
		sb.WriteString("Existing expense accounts:\n")
		for _, a := range req.ExpenseAccounts {
			// Skip placeholder accounts like "(no name)".
			if isBlankName(a.Name) {
				continue
			}
			fmt.Fprintf(&sb, "  - %s\n", a.Name)
		}
		sb.WriteString("\n")
	}

	fmt.Fprintf(&sb, "Available categories:%s\n\n", formatCategories(req.Categories))

	if req.TagSuggestion {
		max := req.TagMax
		if max <= 0 {
			max = 3
		}

		if len(req.ExistingTags) > 0 {
			sb.WriteString("Existing tags (reuse these when relevant):\n")
			for _, t := range req.ExistingTags {
				if strings.TrimSpace(t) == "" {
					continue
				}
				fmt.Fprintf(&sb, "  - %s\n", t)
			}
			sb.WriteString("\n")
		}

		fmt.Fprintf(&sb, "Tagging is enabled. In ADDITION to the fields above, include a \"tags\" array in your JSON (at most %d items). Each item: {\"name\": <tag>, \"action\": \"MATCH\"|\"CREATE\", \"confidence\": \"CLASSIFIED\"|\"ASSUMED\"}.\n", max)
		sb.WriteString("- Tags must ADD information beyond the category and destination. NEVER output a tag that repeats, translates, or is a synonym of the chosen category (e.g. category \"Restauration\" → do NOT tag \"restaurant\"/\"resto\"). If you have nothing that adds information, return an empty array — that is expected and fine.\n")
		sb.WriteString("- Favour tags that describe the concrete CONTEXT or subject of the purchase — what it is really about — and be specific and imaginative; go well beyond obvious words. Do NOT use generic transaction metadata as tags (avoid \"ponctuel\", \"récurrent\", \"en-ligne\", \"espèces\" and the like — they add nothing). Also NEVER tag generic shipping/delivery concepts (\"colis\", \"livraison\", \"expédition\", \"envoi\", \"package\") — every online order ships, so these add nothing. If nothing specific and useful comes to mind, return an empty array.\n")
		if strings.TrimSpace(req.ExtraContext) != "" {
			fmt.Fprintf(&sb, "- The exact contents of this purchase are given below. Be thorough and specific: derive SEVERAL tags (use the full budget of %d when the product supports it) describing the product from different angles — its BRAND, its PRODUCT TYPE/object, and a clear ATTRIBUTE or use. Example: contents \"Anker Soundcore casque bluetooth\" → anker, casque audio, bluetooth (three useful tags, not just \"anker\"). When SEVERAL distinct products are listed, give one CONCRETE product-type tag per item (e.g. contents \"gourde ... ciseaux ...\" → gourde, ciseaux) rather than a single vague umbrella theme like \"scolarité\". If several items are the SAME type, use that tag only once (don't repeat it). Adapt to whatever the products actually are. Facts read directly from the contents (brand, product type) are certain — mark those CLASSIFIED.\n", max)
		}
		sb.WriteString("- Prefer reusing an existing tag: use MATCH with the exact name from the list. Only use CREATE for a new, concise tag when none fits.\n")
		sb.WriteString("- Only include a tag you are actually confident is relevant. Use confidence CLASSIFIED only when clearly correct, otherwise ASSUMED.\n\n")
	}

	sb.WriteString("Transaction to classify:\n")
	// When destination is a blank placeholder, show description instead
	// so the LLM still has useful context.
	dest := req.DestinationName
	if isBlankName(dest) && req.Description != "" {
		dest = req.Description
	}
	fmt.Fprintf(&sb, "  Destination: %s\n", dest)
	fmt.Fprintf(&sb, "  Description: %s\n", req.Description)
	if req.Amount != nil {
		fmt.Fprintf(&sb, "  Amount: %.2f\n", *req.Amount)
	}

	if strings.TrimSpace(req.Notes) != "" {
		fmt.Fprintf(&sb, "\nExisting notes on this transaction (may contain useful hints):\n%s\n", strings.TrimSpace(req.Notes))
	}

	if strings.TrimSpace(req.ExtraContext) != "" {
		fmt.Fprintf(&sb, "\nKnown contents of this purchase (use this to choose the category):\n%s\n", strings.TrimSpace(req.ExtraContext))
	}

	if req.MerchantFromContent && req.DestinationMatching {
		fmt.Fprintf(&sb, "\nIMPORTANT — destination: the bank payee %q is a PAYMENT PROCESSOR / intermediary, NOT the real merchant. You MUST identify the ACTUAL shop/merchant from the content above and set the destination to it (MATCH an existing account, else CREATE it — you can be CLASSIFIED since it is read from the order). NEVER set or keep the destination as %q or any generic payment-processor name.\n", req.DestinationName, req.DestinationName)
	}

	return sb.String()
}
