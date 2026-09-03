package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/openaccountants/firefly-iii-ai-categorize/internal/amazon"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/cache"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/classifier"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/firefly"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/job"
)

const categoryTTL = 5 * time.Minute

// historyMatchLookup is how many past entries to fetch when checking for a
// confident history-based match (larger than the AI prompt context limit).
const historyMatchLookup = 20

// historyMatchMinCount is the minimum number of agreeing past transactions
// required to skip the AI call and auto-classify from history.
const historyMatchMinCount = 2

// historyMatchMaxRatio is the maximum ratio between the new transaction amount
// and a historical amount for the two to be considered "same ballpark".
// 2.0 means the amounts may differ by at most 2× in either direction.
const historyMatchMaxRatio = 2.0

// Pipeline runs the classify-and-update flow for a single transaction.
// It is shared by both the webhook handler and the batch runner.
type Pipeline struct {
	firefly    *firefly.Client
	classifier classifier.Classifier
	cache      *cache.Cache
	registry   *job.Registry
	contextN   int

	destinationMatch bool

	tagSuggest bool
	tagMax     int

	amazon *amazon.Index

	// Short-lived category cache to avoid re-fetching on every job in a batch.
	catMu      sync.RWMutex
	catCache   []firefly.Category
	catFetched time.Time

	// Short-lived tag cache (only used when tagSuggest is true).
	tagMu      sync.RWMutex
	tagCache   []string
	tagFetched time.Time

	// Short-lived expense-account cache (only used when destinationMatch is true).
	acctMu      sync.RWMutex
	acctCache   []firefly.Account
	acctFetched time.Time
}

func New(
	fc *firefly.Client,
	cl classifier.Classifier,
	ca *cache.Cache,
	reg *job.Registry,
	contextN int,
	destinationMatch bool,
	tagSuggest bool,
	tagMax int,
	amz *amazon.Index,
) *Pipeline {
	return &Pipeline{
		firefly:          fc,
		classifier:       cl,
		cache:            ca,
		registry:         reg,
		contextN:         contextN,
		destinationMatch: destinationMatch,
		tagSuggest:       tagSuggest,
		tagMax:           tagMax,
		amazon:           amz,
	}
}

// DestinationMatchEnabled returns whether destination matching is enabled in config.
func (p *Pipeline) DestinationMatchEnabled() bool {
	return p.destinationMatch
}

// RunOptions controls which parts of the pipeline execute.
type RunOptions struct {
	ClassifyCategory bool // if true, run category classification
	MatchDestination bool // if true, run destination matching
}

// Run executes the full classification pipeline for a queued job.
func (p *Pipeline) Run(ctx context.Context, j *job.Job, transactionID string, splits []firefly.Split) error {
	return p.RunWithOptions(ctx, j, transactionID, splits, RunOptions{
		ClassifyCategory: true,
		MatchDestination: p.destinationMatch,
	})
}

// RunWithOptions executes the classification pipeline with fine-grained control
// over which parts (category classification vs destination matching) are active.
func (p *Pipeline) RunWithOptions(ctx context.Context, j *job.Job, transactionID string, splits []firefly.Split, opts RunOptions) error {
	p.registry.SetInProgress(j.ID)

	categories, err := p.getCategories(ctx)
	if err != nil {
		p.registry.SetFailed(j.ID, err.Error())
		return fmt.Errorf("get categories: %w", err)
	}

	clCats := make([]classifier.Category, len(categories))
	for i, c := range categories {
		clCats[i] = classifier.Category{Name: c.Name, Notes: c.Notes}
	}

	var expenseAccounts []firefly.Account
	if opts.MatchDestination {
		var err error
		expenseAccounts, err = p.getExpenseAccounts(ctx)
		if err != nil {
			slog.Warn("failed to fetch expense accounts, continuing without destination matching", "error", err)
		}
	}
	clAccounts := make([]classifier.AccountCandidate, len(expenseAccounts))
	for i, a := range expenseAccounts {
		clAccounts[i] = classifier.AccountCandidate{ID: a.ID, Name: a.Name}
	}

	gkey := classifier.GroupKey(j.DestinationName, j.Description)
	amazonTxn := isAmazon(j.Description, j.DestinationName)

	// Fetch enough history for both the match check and the AI prompt.
	lookupLimit := historyMatchLookup
	if p.contextN > lookupLimit {
		lookupLimit = p.contextN
	}
	history := excludeTransaction(p.cache.GetHistory(ctx, gkey, lookupLimit), transactionID)

	// Try independent history matches for category and destination.
	// When both match, the LLM call can be skipped entirely. When only one
	// matches, the LLM is still called but the matched value overrides the
	// LLM's result for that field.
	historyMatchCat, histCatCount := tryHistoryMatch(history, j.Amount)
	historyMatchDestID, histDestCount := tryDestinationHistoryMatch(history, j.Amount)

	// For Amazon purchases, try to match the bank transaction to an order in the
	// user's Amazon order-history export and feed the product names to the LLM
	// so it categorizes from the actual contents rather than just "Amazon".
	var extraContext string
	var amazonUncertain bool
	if p.amazon.Loaded() && amazonTxn {
		var fireflyDate time.Time
		if len(splits) > 0 && len(splits[0].Date) >= 10 {
			if t, err := time.Parse("2006-01-02", splits[0].Date[:10]); err == nil {
				fireflyDate = t
			}
		}
		labelDate := amazon.ParseLabelDate(j.Description, fireflyDate)
		card := ""
		if m := reCardLast4.FindStringSubmatch(j.Description); m != nil {
			card = m[1]
		}
		if products, certain, ok := p.amazon.Lookup(derefAmount(j.Amount), labelDate, fireflyDate, card); ok && len(products) > 0 {
			extraContext = "Amazon order contents (matched by date): " + strings.Join(products, "; ")
			amazonUncertain = !certain
			slog.Info("amazon order matched", "id", transactionID, "items", len(products), "certain", certain)
		}
	}

	// Amazon is a meta-merchant — past category is not predictive. When we have
	// the order contents, skip the category history match so the LLM categorizes
	// from those contents. Without contents, keep the history match as a
	// fallback rather than sending the transaction to review empty-handed.
	if amazonTxn && extraContext != "" {
		historyMatchCat = ""
		histCatCount = 0
	}

	// Validate the destination history match against the current account list.
	if historyMatchDestID != "" {
		found := false
		for _, a := range expenseAccounts {
			if a.ID == historyMatchDestID {
				found = true
				break
			}
		}
		if !found {
			historyMatchDestID = ""
			histDestCount = 0
		}
	}

	// If both category and destination have history matches, skip the LLM entirely.
	// Also skip when we only care about destination and it has a history match.
	if (!opts.ClassifyCategory || historyMatchCat != "") && (historyMatchDestID != "" || !opts.MatchDestination) {
		categoryID := ""
		for _, c := range categories {
			if c.Name == historyMatchCat {
				categoryID = c.ID
				break
			}
		}
		reason := fmt.Sprintf("Auto-matched from %d previous transactions with the same payee.", histCatCount)
		outcome := firefly.UpdateOutcome{
			Outcome:       string(classifier.Classified),
			Category:      historyMatchCat,
			CategoryID:    categoryID,
			DestinationID: historyMatchDestID,
			Reason:        reason,
			Tags:          tryHistoryTags(history, j.Amount),
		}
		if historyMatchDestID != "" {
			outcome.DestConfidence = "CLASSIFIED"
		}
		if err := p.firefly.UpdateTransaction(ctx, transactionID, splits, outcome); err != nil {
			p.registry.SetFailed(j.ID, err.Error())
			return fmt.Errorf("update transaction: %w", err)
		}

		var destName, destAction string
		if historyMatchDestID != "" {
			destAction = "MATCH"
			for _, a := range expenseAccounts {
				if a.ID == historyMatchDestID {
					destName = a.Name
					break
				}
			}
		}
		p.registry.SetFinished(j.ID, string(classifier.Classified), historyMatchCat, reason, "", "", "", destName, destAction, nil, nil)
		p.cache.Append(classifier.HistoricalEntry{
			TransactionID:        transactionID,
			DestinationName:      j.DestinationName,
			Description:          j.Description,
			CategoryName:         historyMatchCat,
			GroupKey:             gkey,
			Amount:               derefAmount(j.Amount),
			DestinationAccountID: historyMatchDestID,
		})
		slog.Info("transaction history-matched",
			"id", transactionID,
			"category", historyMatchCat,
			"catMatches", histCatCount,
			"destMatches", histDestCount,
		)
		return nil
	}

	// Trim history to the configured context limit for the AI prompt.
	promptHistory := history
	if len(promptHistory) > p.contextN {
		promptHistory = promptHistory[:p.contextN]
	}

	// Fetch existing tags to offer the LLM for reuse (only when tagging is on
	// and we're actually classifying a category).
	var existingTags []string
	tagSuggest := p.tagSuggest && opts.ClassifyCategory
	if tagSuggest {
		if t, err := p.getTags(ctx); err != nil {
			slog.Warn("failed to fetch tags, continuing without tag suggestion", "error", err)
			tagSuggest = false
		} else {
			existingTags = t
		}
	}

	notes := ""
	if len(splits) > 0 {
		notes = splits[0].Notes
	}

	result, err := p.classifier.Classify(ctx, classifier.Request{
		Categories:          clCats,
		DestinationName:     j.DestinationName,
		Description:         j.Description,
		Amount:              j.Amount,
		History:             promptHistory,
		ExpenseAccounts:     clAccounts,
		DestinationMatching: opts.MatchDestination,
		CategoryOnly:        opts.ClassifyCategory && !opts.MatchDestination,
		ExistingTags:        existingTags,
		TagSuggestion:       tagSuggest,
		TagMax:              p.tagMax,
		ExtraContext:        extraContext,
		Notes:               notes,
	})
	if err != nil {
		p.registry.SetFailed(j.ID, err.Error())
		return fmt.Errorf("classify: %w", err)
	}

	categoryID := ""
	for _, c := range categories {
		if c.Name == result.Category {
			categoryID = c.ID
			break
		}
	}

	outcome := firefly.UpdateOutcome{
		Outcome:    string(result.Outcome),
		Category:   result.Category,
		CategoryID: categoryID,
		Reason:     result.Reason,
		Assumption: result.Assumption,
	}

	// An Amazon match disambiguated by amount (several orders that day) is not
	// certain — downgrade to ASSUMED so a human reviews the chosen category.
	if amazonUncertain && outcome.Outcome == string(classifier.Classified) {
		outcome.Outcome = string(classifier.Assumed)
		if strings.TrimSpace(outcome.Assumption) == "" {
			outcome.Assumption = "Correspondance Amazon incertaine (plusieurs commandes à cette date) — à vérifier."
		}
	}

	// Split tag suggestions: confident tags are applied, assumed ones are only
	// surfaced for human review (recorded in notes + flagged).
	for _, t := range result.Tags {
		if t.Confidence == "CLASSIFIED" {
			outcome.Tags = append(outcome.Tags, t.Name)
		} else {
			outcome.TagsAssumed = append(outcome.TagsAssumed, t.Name)
		}
	}

	// Override category with history match when available — the LLM was only
	// needed for destination (or the other way around).
	if opts.ClassifyCategory && historyMatchCat != "" {
		for _, c := range categories {
			if c.Name == historyMatchCat {
				outcome.CategoryID = c.ID
				break
			}
		}
		outcome.Category = historyMatchCat
		outcome.Outcome = string(classifier.Classified)
		outcome.Reason = fmt.Sprintf("Auto-matched from %d previous transactions. AI was consulted for destination.", histCatCount)
	}

	// Process destination account result (when enabled and valid).
	var destAccount string
	var destAction string
	if result.Destination != nil {
		destAccount = result.Destination.Name
		destAction = result.Destination.Action

		switch result.Destination.Action {
		case "MATCH":
			for _, a := range expenseAccounts {
				if strings.EqualFold(a.Name, result.Destination.Name) {
					outcome.DestinationID = a.ID
					break
				}
			}
			if outcome.DestinationID == "" {
				slog.Warn("destination MATCH failed to find account", "name", result.Destination.Name)
				destAccount = ""
				destAction = ""
			} else {
				outcome.DestConfidence = result.Destination.Confidence
			}
		case "CREATE":
			if result.Destination.Confidence != "CLASSIFIED" {
				// CREATE is only attempted when the LLM is confident.
				slog.Info("destination CREATE skipped — confidence is ASSUMED", "name", result.Destination.Name)
				destAccount = ""
				destAction = ""
				break
			}
			created, err := p.firefly.CreateExpenseAccount(ctx, result.Destination.Name)
			if err != nil {
				slog.Error("failed to create expense account", "name", result.Destination.Name, "error", err)
				destAccount = ""
				destAction = ""
				break
			}
			outcome.DestinationID = created.ID
			outcome.DestConfidence = result.Destination.Confidence
			slog.Info("created expense account", "name", created.Name, "id", created.ID)

			// Append to in-memory cache so subsequent jobs in the same batch
			// see the new account without waiting for the 5-minute TTL.
			p.acctMu.Lock()
			p.acctCache = append(p.acctCache, created)
			p.acctMu.Unlock()
		}
	}

	// Override destination with history match when the LLM didn't produce one
	// (or produced a weaker one) but history has a confident match.
	if historyMatchDestID != "" && outcome.DestinationID == "" {
		outcome.DestinationID = historyMatchDestID
		outcome.DestConfidence = "CLASSIFIED"
		destAction = "MATCH"
		for _, a := range expenseAccounts {
			if a.ID == historyMatchDestID {
				destAccount = a.Name
				break
			}
		}
	}

	if err := p.firefly.UpdateTransaction(ctx, transactionID, splits, outcome); err != nil {
		p.registry.SetFailed(j.ID, err.Error())
		return fmt.Errorf("update transaction: %w", err)
	}

	// Use history override values for the finished job when they were applied.
	finishedOutcome := string(result.Outcome)
	finishedCategory := result.Category
	finishedReason := result.Reason
	if !opts.ClassifyCategory {
		finishedOutcome = string(classifier.Classified)
		finishedCategory = ""
		finishedReason = "Destination-only matching (category not evaluated)."
	}
	if opts.ClassifyCategory && historyMatchCat != "" {
		finishedOutcome = string(classifier.Classified)
		finishedCategory = historyMatchCat
		finishedReason = outcome.Reason
	}
	if amazonUncertain && finishedOutcome == string(classifier.Classified) {
		finishedOutcome = string(classifier.Assumed)
	}

	p.registry.SetFinished(
		j.ID,
		finishedOutcome,
		finishedCategory,
		finishedReason,
		result.Assumption,
		result.RawPrompt,
		result.RawResponse,
		destAccount,
		destAction,
		outcome.Tags,
		outcome.TagsAssumed,
	)

	cachedCategory := finishedCategory
	if cachedCategory == "" && opts.ClassifyCategory {
		cachedCategory = result.Category
	}
	if cachedCategory != "" || opts.MatchDestination {
		p.cache.Append(classifier.HistoricalEntry{
			TransactionID:        transactionID,
			DestinationName:      j.DestinationName,
			Description:          j.Description,
			CategoryName:         cachedCategory,
			GroupKey:             gkey,
			Amount:               derefAmount(j.Amount),
			DestinationAccountID: outcome.DestinationID,
		})
	}

	slog.Info("transaction classified",
		"id", transactionID,
		"outcome", result.Outcome,
		"category", result.Category,
	)
	return nil
}

// tryHistoryMatch returns the best category from history if at least
// historyMatchMinCount entries agree on it with no tie from another category.
// Entries whose amounts fall outside the ballpark of the new transaction are
// excluded from the vote; if either amount is unknown (zero), the amount check
// is skipped for that entry so the vote still counts.
func tryHistoryMatch(history []classifier.HistoricalEntry, amount *float64) (category string, count int) {
	votes := make(map[string]int)
	for _, h := range history {
		if h.CategoryName == "" {
			continue
		}
		if amount != nil && *amount > 0 && h.Amount > 0 {
			hi := math.Max(*amount, h.Amount)
			lo := math.Min(*amount, h.Amount)
			if hi/lo > historyMatchMaxRatio {
				continue
			}
		}
		votes[h.CategoryName]++
	}

	best, bestCount := "", 0
	for cat, n := range votes {
		if n > bestCount {
			best, bestCount = cat, n
		}
	}
	if bestCount < historyMatchMinCount {
		return "", 0
	}
	// Reject ties: another category must not have an equal vote count.
	for cat, n := range votes {
		if cat != best && n >= bestCount {
			return "", 0
		}
	}
	return best, bestCount
}

// tryHistoryTags returns the semantic tags that recur across the amount-matching
// history entries (present in at least historyMatchMinCount of them), so the
// bypass path can apply consistent tags without calling the LLM.
func tryHistoryTags(history []classifier.HistoricalEntry, amount *float64) []string {
	counts := map[string]int{}
	var order []string
	for _, h := range history {
		if amount != nil && *amount > 0 && h.Amount > 0 {
			hi := math.Max(*amount, h.Amount)
			lo := math.Min(*amount, h.Amount)
			if hi/lo > historyMatchMaxRatio {
				continue
			}
		}
		seen := map[string]bool{}
		for _, t := range h.Tags {
			key := strings.ToLower(strings.TrimSpace(t))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			if counts[key] == 0 {
				order = append(order, t) // keep first-seen casing
			}
			counts[key]++
		}
	}
	var out []string
	for _, t := range order {
		if counts[strings.ToLower(strings.TrimSpace(t))] >= historyMatchMinCount {
			out = append(out, t)
		}
	}
	return out
}

// tryDestinationHistoryMatch returns the best destination account ID from history
// if at least historyMatchMinCount entries agree on it with no tie. The same
// amount-ballpark filtering as tryHistoryMatch applies.
func tryDestinationHistoryMatch(history []classifier.HistoricalEntry, amount *float64) (accountID string, count int) {
	votes := make(map[string]int)
	for _, h := range history {
		if h.DestinationAccountID == "" {
			continue
		}
		if amount != nil && *amount > 0 && h.Amount > 0 {
			hi := math.Max(*amount, h.Amount)
			lo := math.Min(*amount, h.Amount)
			if hi/lo > historyMatchMaxRatio {
				continue
			}
		}
		votes[h.DestinationAccountID]++
	}

	best, bestCount := "", 0
	for id, n := range votes {
		if n > bestCount {
			best, bestCount = id, n
		}
	}
	if bestCount < historyMatchMinCount {
		return "", 0
	}
	for id, n := range votes {
		if id != best && n >= bestCount {
			return "", 0
		}
	}
	return best, bestCount
}

var reCardLast4 = regexp.MustCompile(`X(\d{4})`)

// isAmazon reports whether a transaction is an Amazon purchase, from its
// description or destination name.
func isAmazon(description, destinationName string) bool {
	s := strings.ToLower(description + " " + destinationName)
	return strings.Contains(s, "amazon") || strings.Contains(s, "amzn")
}

func derefAmount(a *float64) float64 {
	if a == nil {
		return 0
	}
	return *a
}

// excludeTransaction removes any history entries that belong to the transaction
// currently being classified, preventing it from influencing its own result
// during re-categorization runs.
func excludeTransaction(history []classifier.HistoricalEntry, transactionID string) []classifier.HistoricalEntry {
	filtered := history[:0]
	for _, h := range history {
		if h.TransactionID != transactionID {
			filtered = append(filtered, h)
		}
	}
	return filtered
}

// getCategories returns cached categories, re-fetching when the TTL expires.
func (p *Pipeline) getCategories(ctx context.Context) ([]firefly.Category, error) {
	p.catMu.RLock()
	if !p.catFetched.IsZero() && time.Since(p.catFetched) < categoryTTL {
		cats := p.catCache
		p.catMu.RUnlock()
		return cats, nil
	}
	p.catMu.RUnlock()

	p.catMu.Lock()
	defer p.catMu.Unlock()

	if !p.catFetched.IsZero() && time.Since(p.catFetched) < categoryTTL {
		return p.catCache, nil
	}

	cats, err := p.firefly.GetCategories(ctx)
	if err != nil {
		return nil, err
	}
	p.catCache = cats
	p.catFetched = time.Now()
	return cats, nil
}

// getExpenseAccounts returns cached expense accounts, re-fetching when the TTL expires.
func (p *Pipeline) getExpenseAccounts(ctx context.Context) ([]firefly.Account, error) {
	p.acctMu.RLock()
	if !p.acctFetched.IsZero() && time.Since(p.acctFetched) < categoryTTL {
		accts := p.acctCache
		p.acctMu.RUnlock()
		return accts, nil
	}
	p.acctMu.RUnlock()

	p.acctMu.Lock()
	defer p.acctMu.Unlock()

	if !p.acctFetched.IsZero() && time.Since(p.acctFetched) < categoryTTL {
		return p.acctCache, nil
	}

	accts, err := p.firefly.GetExpenseAccounts(ctx)
	if err != nil {
		return nil, err
	}
	p.acctCache = accts
	p.acctFetched = time.Now()
	return accts, nil
}

// getTags returns cached tag names, re-fetching when the TTL expires.
func (p *Pipeline) getTags(ctx context.Context) ([]string, error) {
	p.tagMu.RLock()
	if !p.tagFetched.IsZero() && time.Since(p.tagFetched) < categoryTTL {
		tags := p.tagCache
		p.tagMu.RUnlock()
		return tags, nil
	}
	p.tagMu.RUnlock()

	p.tagMu.Lock()
	defer p.tagMu.Unlock()

	if !p.tagFetched.IsZero() && time.Since(p.tagFetched) < categoryTTL {
		return p.tagCache, nil
	}

	tags, err := p.firefly.GetTags(ctx)
	if err != nil {
		return nil, err
	}
	p.tagCache = tags
	p.tagFetched = time.Now()
	return tags, nil
}

// TransferSuggestion is the result of suggesting a destination asset account
// for a transfer conversion.
type TransferSuggestion struct {
	AccountName string
	RawResponse string
}

// SuggestTransfer asks the LLM to pick the best destination asset account for
// a transfer, given the transaction description and a list of available accounts.
func (p *Pipeline) SuggestTransfer(ctx context.Context, description string, accounts []classifier.AccountCandidate) (TransferSuggestion, error) {
	var acctList string
	for _, a := range accounts {
		acctList += "  - " + a.Name + "\n"
	}

	result, err := p.classifier.Classify(ctx, classifier.Request{
		SystemPromptOverride: fmt.Sprintf(
			"You select the best destination asset account for a transfer.\n"+
				"The transaction description is: %q\n\n"+
				"Available asset accounts:\n%s\n"+
				"Pick the most likely destination account.\n"+
				"Respond with ONLY valid JSON: {\"account\": \"<exact account name from the list>\"}",
			description, acctList,
		),
	})
	if err != nil {
		return TransferSuggestion{}, err
	}

	var parsed struct {
		Account string `json:"account"`
	}
	cleaned := strings.TrimSpace(result.RawResponse)
	if strings.HasPrefix(cleaned, "```") {
		end := strings.Index(cleaned[3:], "\n")
		if end >= 0 {
			cleaned = cleaned[3+end+1:]
		}
		if idx := strings.LastIndex(cleaned, "```"); idx >= 0 {
			cleaned = cleaned[:idx]
		}
		cleaned = strings.TrimSpace(cleaned)
	}
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return TransferSuggestion{}, fmt.Errorf("parse transfer suggestion: %w (raw: %s)", err, result.RawResponse)
	}

	return TransferSuggestion{
		AccountName: parsed.Account,
		RawResponse: result.RawResponse,
	}, nil
}
