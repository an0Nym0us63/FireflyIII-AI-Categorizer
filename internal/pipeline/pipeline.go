package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openaccountants/firefly-iii-ai-categorize/internal/aidb"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/amazon"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/cache"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/classifier"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/config"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/firefly"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/job"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/mailorder"
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

	aidb *aidb.DB

	mailAccounts  []config.MailAccount
	mailDetectors []config.MailDetector

	forceDestinations []string
	forceCategories   []string
	tagRules          []config.TagRule

	histMu   sync.Mutex
	histMemo map[string]memoEntry

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
	adb *aidb.DB,
	mailAccounts []config.MailAccount,
	mailDetectors []config.MailDetector,
	forceDestinations []string,
	forceCategories []string,
	tagRules []config.TagRule,
) *Pipeline {
	return &Pipeline{
		firefly:           fc,
		classifier:        cl,
		cache:             ca,
		registry:          reg,
		contextN:          contextN,
		destinationMatch:  destinationMatch,
		tagSuggest:        tagSuggest,
		tagMax:            tagMax,
		amazon:            amz,
		aidb:              adb,
		mailAccounts:      mailAccounts,
		mailDetectors:     mailDetectors,
		forceDestinations: forceDestinations,
		forceCategories:   forceCategories,
		tagRules:          tagRules,
		histMemo:          map[string]memoEntry{},
	}
}

type memoEntry struct {
	entries []classifier.HistoricalEntry
	at      time.Time
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

	gkey := classifier.GroupKey(j.DestinationName, j.Description)
	amazonTxn := isAmazon(j.Description, j.DestinationName)

	var fireflyDate time.Time
	if len(splits) > 0 && len(splits[0].Date) >= 10 {
		if t, err := time.Parse("2006-01-02", splits[0].Date[:10]); err == nil {
			fireflyDate = t
		}
	}

	var extraContext string
	var amazonUncertain bool
	var skipIfEmpty bool
	var paymentTag string
	var forceDestination bool
	var installmentTag bool
	var mailMerchant string
	var mailCandidates int
	var mailSearchHits int
	var mailNote string

	// Opaque merchants configured with a mail detector (PayPal, Amazon,
	// AliExpress…): find the order-confirmation email and feed it to the LLM.
	if det := p.matchMailDetector(j.Description); det != nil {
		skipIfEmpty = true
		if body, inst, ok, cands, hits, note := p.findOrderEmail(det, fireflyDate, derefAmount(j.Amount)); ok {
			extraContext = "Order confirmation email (use it to choose category, destination and tags):\n" + body
			slog.Info("order email matched", "id", transactionID, "installment", inst)
			if det.ReplaceDestination {
				opts.MatchDestination = true // determine the real merchant from the email
				forceDestination = true
				mailMerchant = mailorder.ExtractMerchant(body)
			}
			if t := strings.TrimSpace(det.Tag); t != "" {
				paymentTag = t
			}
			if inst {
				installmentTag = true
			}
		} else {
			mailCandidates = cands
			mailSearchHits = hits
			mailNote = note
		}
	}

	// For Amazon purchases, match the bank transaction to an order in the
	// order-history export and feed the product names to the LLM.
	if extraContext == "" && p.amazon.Loaded() && amazonTxn {
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

	// Opaque merchant with no order content found → leave the transaction
	// untouched (don't waste an LLM call on a useless label).
	if skipIfEmpty && extraContext == "" {
		win := ""
		if !fireflyDate.IsZero() {
			win = fmt.Sprintf(" [fenêtre %s → %s]",
				fireflyDate.AddDate(0, 0, -mailBackDays).Format("2006-01-02"),
				fireflyDate.AddDate(0, 0, mailFwdDays).Format("2006-01-02"))
		}
		reason := "Aucun email de commande trouvé — non catégorisé."
		if mailCandidates > 0 {
			reason = fmt.Sprintf("%d email(s) dans la fenêtre de dates mais aucun au montant %.2f € — non catégorisé.", mailCandidates, derefAmount(j.Amount))
		} else {
			reason = fmt.Sprintf("Aucun email au montant dans la fenêtre (±14j) — %d résultat(s) de recherche brut(s). Non catégorisé.", mailSearchHits)
		}
		reason += win
		if mailNote != "" {
			reason += " (" + mailNote + ")"
		}
		p.registry.SetFinished(j.ID, "SKIPPED", "", reason, "", "", "", "", "", nil, nil)
		slog.Info("skipped opaque merchant with no order content", "id", transactionID, "candidates", mailCandidates, "hits", mailSearchHits, "note", mailNote)
		return nil
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

	// Same-merchant history via Firefly's indexed search, windowed around the
	// transaction date and recency-weighted.
	history := excludeTransaction(p.merchantHistory(ctx, gkey, fireflyDate), transactionID)

	historyMatchCat, _ := weightedCategory(history, fireflyDate, derefAmount(j.Amount))
	historyMatchDestID, _ := weightedDestination(history, fireflyDate, derefAmount(j.Amount))
	histCatCount := len(history)
	histDestCount := len(history)

	// With real order content, let the LLM decide the category from it rather
	// than reusing a possibly-wrong past category.
	if extraContext != "" {
		historyMatchCat = ""
		histCatCount = 0
	}
	// Mail-detector merchants (PayPal/Amazon/AliExpress…) must NEVER auto-match on
	// history — each of their transactions is different; they're driven by the
	// order email/content only.
	if skipIfEmpty {
		historyMatchCat = ""
		histCatCount = 0
		historyMatchDestID = ""
		histDestCount = 0
	}

	// Forced re-pick: if the transaction's current destination/category is in the
	// configured force lists, ignore auto-matching so the AI chooses a new one.
	curDest, curCat := "", ""
	if len(splits) > 0 {
		curDest = splits[0].DestinationName
		curCat = splits[0].CategoryName
	}
	if containsFoldStr(p.forceDestinations, curDest) {
		opts.MatchDestination = true
		forceDestination = true
		historyMatchDestID = ""
		histDestCount = 0
	}
	if containsFoldStr(p.forceCategories, curCat) {
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
		histTags := weightedTags(history, fireflyDate, derefAmount(j.Amount))
		outcome := firefly.UpdateOutcome{
			Outcome:       string(classifier.Classified),
			Category:      historyMatchCat,
			CategoryID:    categoryID,
			DestinationID: historyMatchDestID,
			Reason:        reason,
			Tags:          histTags,
		}
		if historyMatchDestID != "" {
			outcome.DestConfidence = "CLASSIFIED"
		}
		if err := p.firefly.UpdateTransaction(ctx, transactionID, splits, outcome); err != nil {
			p.registry.SetFailed(j.ID, err.Error())
			return fmt.Errorf("update transaction: %w", err)
		}

		if p.aidb != nil {
			destConf := ""
			if historyMatchDestID != "" {
				destConf = "CLASSIFIED"
			}
			_ = p.aidb.Upsert(aidb.Record{
				TransactionID:  transactionID,
				Outcome:        string(classifier.Classified),
				Category:       historyMatchCat,
				DestConfidence: destConf,
				Reason:         reason,
			})
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
		p.registry.SetFinished(j.ID, string(classifier.Classified), historyMatchCat, reason, "", "", "", destName, destAction, histTags, nil)
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

	notes := cleanNotes("")
	if len(splits) > 0 {
		notes = cleanNotes(splits[0].Notes)
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
		MerchantFromContent: forceDestination,
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
		Items:      result.Items,
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
	// When the category is auto-matched from history (same payee), also reapply
	// the recurring tags from that payee's past transactions.
	if historyMatchCat != "" {
		for _, t := range weightedTags(history, fireflyDate, derefAmount(j.Amount)) {
			if !containsFoldStr(outcome.Tags, t) {
				outcome.Tags = append(outcome.Tags, t)
			}
		}
	}

	// Payment-method tag from the mail detector (e.g. "paypal").
	if paymentTag != "" {
		has := false
		for _, t := range outcome.Tags {
			if strings.EqualFold(t, paymentTag) {
				has = true
				break
			}
		}
		if !has {
			outcome.Tags = append(outcome.Tags, paymentTag)
		}
	}
	// 4-installment payment.
	if installmentTag {
		has := false
		for _, t := range outcome.Tags {
			if strings.EqualFold(t, "4x") {
				has = true
				break
			}
		}
		if !has {
			outcome.Tags = append(outcome.Tags, "4x")
		}
	}

	// Tag rules: strip configured tags from the transaction (optionally replace).
	if len(p.tagRules) > 0 {
		var existing []string
		if len(splits) > 0 {
			existing = splits[0].Tags
		}
		for _, rule := range p.tagRules {
			if strings.TrimSpace(rule.From) == "" {
				continue
			}
			present := containsFoldStr(existing, rule.From) || containsFoldStr(outcome.Tags, rule.From)
			outcome.RemoveTags = append(outcome.RemoveTags, rule.From)
			outcome.Tags = removeFold(outcome.Tags, rule.From)
			if present && strings.TrimSpace(rule.To) != "" && !containsFoldStr(outcome.Tags, rule.To) {
				outcome.Tags = append(outcome.Tags, rule.To)
			}
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

	// Prefer the LLM's destination (it can name things intelligently). Fall back
	// to the merchant extracted from the email only when the LLM gave nothing or
	// kept the payment processor (the bank payee) as the destination.
	llmGaveRealMerchant := outcome.DestinationID != "" && !strings.EqualFold(destAccount, j.DestinationName)
	if forceDestination && mailMerchant != "" && !llmGaveRealMerchant {
		matchedID := ""
		for _, a := range expenseAccounts {
			if strings.EqualFold(a.Name, mailMerchant) {
				matchedID = a.ID
				break
			}
		}
		if matchedID != "" {
			outcome.DestinationID = matchedID
			outcome.DestConfidence = "CLASSIFIED"
			destAccount = mailMerchant
			destAction = "MATCH"
		} else if created, err := p.firefly.CreateExpenseAccount(ctx, mailMerchant); err == nil {
			outcome.DestinationID = created.ID
			outcome.DestConfidence = "CLASSIFIED"
			destAccount = created.Name
			destAction = "CREATE"
			p.acctMu.Lock()
			p.acctCache = append(p.acctCache, created)
			p.acctMu.Unlock()
		} else {
			slog.Warn("failed to create merchant account from email", "name", mailMerchant, "error", err)
		}
	}

	if err := p.firefly.UpdateTransaction(ctx, transactionID, splits, outcome); err != nil {
		p.registry.SetFailed(j.ID, err.Error())
		return fmt.Errorf("update transaction: %w", err)
	}

	if p.aidb != nil {
		_ = p.aidb.Upsert(aidb.Record{
			TransactionID:  transactionID,
			Outcome:        outcome.Outcome,
			Category:       outcome.Category,
			DestConfidence: outcome.DestConfidence,
			Reason:         outcome.Reason,
			Assumption:     outcome.Assumption,
			SuggestedTags:  outcome.TagsAssumed,
		})
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

const (
	mailBackDays = 14 // order emails can precede the bank charge by many days
	mailFwdDays  = 2
)

func containsFoldStr(list []string, v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
	}
	return false
}

func removeFold(list []string, v string) []string {
	out := list[:0:0]
	for _, x := range list {
		if !strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(v)) {
			out = append(out, x)
		}
	}
	return out
}

// cleanNotes strips bank-import boilerplate ("MORE DETAILS" block) and our own
// generated "AI:" lines so only genuine human hints reach the LLM.
func cleanNotes(s string) string {
	if s == "" {
		return ""
	}
	// Drop the bank import boilerplate block and everything after it.
	if i := strings.Index(strings.ToUpper(s), "MORE DETAILS"); i >= 0 {
		// back up to the start of that line
		start := strings.LastIndex(s[:i], "\n")
		if start < 0 {
			start = 0
		}
		s = s[:start]
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "AI:") || strings.HasPrefix(t, "Reason:") || strings.HasPrefix(t, "Assumption:") ||
			strings.HasPrefix(t, "Articles:") || strings.HasPrefix(t, "Étiquettes suggérées") {
			continue
		}
		out = append(out, t)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// matchMailDetector returns the first detector whose keyword appears in the
// transaction description (case-insensitive), or nil.
func (p *Pipeline) matchMailDetector(description string) *config.MailDetector {
	d := strings.ToLower(description)
	for i := range p.mailDetectors {
		for _, kw := range p.mailDetectors[i].Keywords {
			kw = strings.ToLower(strings.TrimSpace(kw))
			if kw != "" && strings.Contains(d, kw) {
				return &p.mailDetectors[i]
			}
		}
	}
	return nil
}

func (p *Pipeline) accountByID(id string) *config.MailAccount {
	for i := range p.mailAccounts {
		if p.mailAccounts[i].ID == id {
			return &p.mailAccounts[i]
		}
	}
	return nil
}

// findOrderEmail searches the detector's mailbox for the order email near date.
// Returns the body, whether it's a 4x installment, whether found, and how many
// candidate emails (sender+date) were seen (for diagnostics).
func (p *Pipeline) findOrderEmail(det *config.MailDetector, date time.Time, amount float64) (string, bool, bool, int, int, string) {
	acc := p.accountByID(det.AccountID)
	if acc == nil || acc.IMAPHost == "" || acc.IMAPUser == "" || date.IsZero() {
		return "", false, false, 0, 0, ""
	}
	res, err := mailorder.FindOrderEmail(mailorder.Account{
		Host: acc.IMAPHost, Port: acc.IMAPPort, User: acc.IMAPUser, Password: acc.IMAPPassword,
	}, det.Senders, date, amount, mailBackDays, mailFwdDays)
	if err != nil {
		slog.Warn("order email search failed", "error", err)
		return "", false, false, 0, 0, err.Error()
	}
	return res.Text, res.Installment, res.Found, res.Candidates, res.SearchHits, res.Note
}

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

// ─── Auto-match explanation (transparency for the UI) ───────────────────────

type AutoMatchEntry struct {
	Description string   `json:"description"`
	Amount      float64  `json:"amount"`
	Category    string   `json:"category"`
	Destination string   `json:"destination"`
	Tags        []string `json:"tags"`
	AmountOK    bool     `json:"amount_ok"`
	Counted     bool     `json:"counted"`
	Reason      string   `json:"reason"`
}

type AutoMatchExplanation struct {
	GroupKey             string           `json:"group_key"`
	Amount               float64          `json:"amount"`
	AmountRatio          float64          `json:"amount_ratio"`
	MinCount             int              `json:"min_count"`
	Entries              []AutoMatchEntry `json:"entries"`
	CategoryVotes        map[string]int   `json:"category_votes"`
	DestVotes            map[string]int   `json:"dest_votes"`
	TagVotes             map[string]int   `json:"tag_votes"`
	MatchedCategory      string           `json:"matched_category"`
	MatchedCategoryCount int              `json:"matched_category_count"`
	MatchedDestination   string           `json:"matched_destination"`
	MatchedTags          []string         `json:"matched_tags"`
	MailDetector         bool             `json:"mail_detector"`
	Notes                []string         `json:"notes"`
}

// ExplainAutoMatch reproduces the history auto-match for a transaction using the
// live data (all same-merchant withdrawals), flagging each occurrence as counted
// or excluded (with the reason), so the "why?" popup shows the full picture.
func (p *Pipeline) ExplainAutoMatch(ctx context.Context, transactionID string) (*AutoMatchExplanation, error) {
	txns, err := p.firefly.GetTransactionsByIDs(ctx, []string{transactionID})
	if err != nil || len(txns) == 0 || len(txns[0].Splits) == 0 {
		return nil, fmt.Errorf("transaction not found")
	}
	s := txns[0].Splits[0]
	var amount float64
	if v, perr := strconv.ParseFloat(strings.TrimSpace(s.Amount), 64); perr == nil {
		amount = math.Abs(v)
	}
	gkey := classifier.GroupKey(s.DestinationName, s.Description)
	var txnDate time.Time
	if len(s.Date) >= 10 {
		txnDate, _ = time.Parse("2006-01-02", s.Date[:10])
	}

	ex := &AutoMatchExplanation{
		GroupKey: gkey, Amount: amount, AmountRatio: 1.75, MinCount: historyMatchMinCount,
		CategoryVotes: map[string]int{}, DestVotes: map[string]int{}, TagVotes: map[string]int{},
	}
	if p.matchMailDetector(s.Description) != nil {
		ex.MailDetector = true
		ex.Notes = append(ex.Notes, "Ce marchand est géré par un détecteur mail → l'auto-match par historique est désactivé (catégorisation depuis l'email de commande).")
	}
	if gkey == "" {
		ex.Notes = append(ex.Notes, "Clé marchand vide → pas de regroupement possible.")
		return ex, nil
	}

	all, err := p.firefly.SearchWithdrawals(ctx, gkey)
	if err != nil {
		return nil, err
	}

	var voteHistory []classifier.HistoricalEntry
	for _, t := range all {
		if t.ID == transactionID || len(t.Splits) == 0 {
			continue
		}
		sp := t.Splits[0]
		if classifier.GroupKey(sp.DestinationName, sp.Description) != gkey {
			continue
		}
		amt, _ := strconv.ParseFloat(strings.TrimSpace(sp.Amount), 64)
		amt = math.Abs(amt)
		semTags := classifier.SemanticTags(sp.Tags)

		amtOK := amountWeight(amount, amt) > 0

		reason, counted := "", false
		var spDate time.Time
		if len(sp.Date) >= 10 {
			spDate, _ = time.Parse("2006-01-02", sp.Date[:10])
		}
		inWindow := true
		if !txnDate.IsZero() && !spDate.IsZero() {
			dd := spDate.Sub(txnDate)
			if dd < 0 {
				dd = -dd
			}
			if dd.Hours()/24 > autoMatchWindowDays {
				inWindow = false
			}
		}
		switch {
		case !inWindow:
			reason = "hors fenêtre ±24 mois"
		case sp.CategoryName == "":
			reason = "non catégorisée"
		case classifier.IsGenericAccountName(sp.DestinationName):
			reason = "compte générique (Cash)"
		case !amtOK:
			reason = "montant trop différent (>1.75×)"
		default:
			counted = true
		}

		ex.Entries = append(ex.Entries, AutoMatchEntry{
			Description: sp.Description, Amount: amt, Category: sp.CategoryName,
			Destination: sp.DestinationName, Tags: semTags, AmountOK: amtOK,
			Counted: counted, Reason: reason,
		})
		if counted {
			if sp.CategoryName != "" {
				ex.CategoryVotes[sp.CategoryName]++
			}
			if sp.DestinationName != "" {
				ex.DestVotes[sp.DestinationName]++
			}
			seen := map[string]bool{}
			for _, tg := range semTags {
				k := strings.ToLower(strings.TrimSpace(tg))
				if k == "" || seen[k] {
					continue
				}
				seen[k] = true
				ex.TagVotes[tg]++
			}
			voteHistory = append(voteHistory, classifier.HistoricalEntry{
				CategoryName: sp.CategoryName, DestinationName: sp.DestinationName,
				DestinationAccountID: sp.DestinationID, Amount: amt, Tags: semTags, Date: spDate,
			})
		}
	}

	cat, _ := weightedCategory(voteHistory, txnDate, amount)
	catN := len(voteHistory)
	ex.MatchedCategory, ex.MatchedCategoryCount = cat, catN
	if destID, _ := weightedDestination(voteHistory, txnDate, amount); destID != "" {
		for _, e := range voteHistory {
			if e.DestinationAccountID == destID {
				ex.MatchedDestination = e.DestinationName
				break
			}
		}
		if ex.MatchedDestination == "" {
			ex.MatchedDestination = destID
		}
	}
	ex.MatchedTags = weightedTags(voteHistory, txnDate, amount)

	if len(ex.Entries) == 0 {
		ex.Notes = append(ex.Notes, "Aucune transaction du même marchand trouvée.")
	}
	if cat == "" {
		best, bestN := "", 0
		for c, n := range ex.CategoryVotes {
			if n > bestN {
				best, bestN = c, n
			}
		}
		if bestN == 0 {
			ex.Notes = append(ex.Notes, "Catégorie : aucune occurrence comptée (voir colonne « compté »).")
		} else {
			ex.Notes = append(ex.Notes, fmt.Sprintf("Catégorie : « %s » n'atteint pas le poids requis (récence) ou trop proche d'une autre → l'IA décide.", best))
		}
	} else {
		ex.Notes = append(ex.Notes, fmt.Sprintf("Catégorie retenue : « %s » (%d votes).", cat, catN))
	}
	if ex.MatchedDestination != "" {
		ex.Notes = append(ex.Notes, fmt.Sprintf("Destination retenue : « %s ».", ex.MatchedDestination))
	}
	if len(ex.MatchedTags) > 0 {
		ex.Notes = append(ex.Notes, "Tags repris : "+strings.Join(ex.MatchedTags, ", ")+".")
	}
	return ex, nil
}

// ─── Weighted, search-based auto-match ──────────────────────────────────────

const (
	autoMatchWindowDays = 731 // ±~24 months around the transaction date
	autoMatchMinWeight  = 1.5 // ≈ 2 recent occurrences, or ~3 older ones
	autoMatchTieMargin  = 1.3 // winner must beat the runner-up by this factor
)

func recencyWeight(entryDate, txnDate time.Time) float64 {
	if entryDate.IsZero() || txnDate.IsZero() {
		return 0.6
	}
	d := entryDate.Sub(txnDate)
	if d < 0 {
		d = -d
	}
	days := d.Hours() / 24
	switch {
	case days <= 92:
		return 1.0
	case days <= 366:
		return 0.6
	case days <= 731:
		return 0.3
	default:
		return 0.1
	}
}

// amountWeight scores how close two amounts are: ~exact counts full, further
// counts progressively less, beyond ~1.75× it's excluded (0). Unknown → neutral.
func amountWeight(a, b float64) float64 {
	if a <= 0 || b <= 0 {
		return 1.0
	}
	r := math.Max(a, b) / math.Min(a, b)
	switch {
	case r <= 1.02:
		return 1.0
	case r <= 1.10:
		return 0.8
	case r <= 1.25:
		return 0.5
	case r <= 1.75:
		return 0.2
	default:
		return 0.0
	}
}

func weightedVote(weights map[string]float64) (string, float64) {
	best, bestW, second := "", 0.0, 0.0
	for k, w := range weights {
		if w > bestW {
			second, best, bestW = bestW, k, w
		} else if w > second {
			second = w
		}
	}
	if bestW < autoMatchMinWeight {
		return "", bestW
	}
	if second > 0 && bestW < second*autoMatchTieMargin {
		return "", bestW
	}
	return best, bestW
}

func weightedCategory(history []classifier.HistoricalEntry, txnDate time.Time, amount float64) (string, float64) {
	w := map[string]float64{}
	for _, h := range history {
		aw := amountWeight(amount, h.Amount)
		if h.CategoryName == "" || aw == 0 {
			continue
		}
		w[h.CategoryName] += recencyWeight(h.Date, txnDate) * aw
	}
	return weightedVote(w)
}

func weightedDestination(history []classifier.HistoricalEntry, txnDate time.Time, amount float64) (string, float64) {
	w := map[string]float64{}
	for _, h := range history {
		aw := amountWeight(amount, h.Amount)
		if h.DestinationAccountID == "" || aw == 0 {
			continue
		}
		w[h.DestinationAccountID] += recencyWeight(h.Date, txnDate) * aw
	}
	return weightedVote(w)
}

func weightedTags(history []classifier.HistoricalEntry, txnDate time.Time, amount float64) []string {
	w := map[string]float64{}
	var order []string
	orig := map[string]string{}
	for _, h := range history {
		aw := amountWeight(amount, h.Amount)
		if aw == 0 {
			continue
		}
		seen := map[string]bool{}
		for _, t := range h.Tags {
			k := strings.ToLower(strings.TrimSpace(t))
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			if _, ok := orig[k]; !ok {
				orig[k] = t
				order = append(order, k)
			}
			w[k] += recencyWeight(h.Date, txnDate) * aw
		}
	}
	var out []string
	for _, k := range order {
		if w[k] >= autoMatchMinWeight {
			out = append(out, orig[k])
		}
	}
	return out
}

// merchantHistory fetches (and briefly memoizes) same-merchant history via
// Firefly's indexed search, keeping categorized non-generic withdrawals within
// ±autoMatchWindowDays of the transaction date.
func (p *Pipeline) merchantHistory(ctx context.Context, gkey string, txnDate time.Time) []classifier.HistoricalEntry {
	if gkey == "" {
		return nil
	}
	p.histMu.Lock()
	memo, ok := p.histMemo[gkey]
	fresh := ok && time.Since(memo.at) < 2*time.Minute
	p.histMu.Unlock()

	var raw []classifier.HistoricalEntry
	if fresh {
		raw = memo.entries
	} else {
		txns, err := p.firefly.SearchWithdrawals(ctx, gkey)
		if err != nil {
			slog.Warn("merchant history search failed", "error", err)
			return nil
		}
		for _, t := range txns {
			if len(t.Splits) == 0 {
				continue
			}
			s := t.Splits[0]
			if classifier.GroupKey(s.DestinationName, s.Description) != gkey {
				continue
			}
			if s.CategoryName == "" || classifier.IsGenericAccountName(s.DestinationName) {
				continue
			}
			amt, _ := strconv.ParseFloat(strings.TrimSpace(s.Amount), 64)
			var d time.Time
			if len(s.Date) >= 10 {
				d, _ = time.Parse("2006-01-02", s.Date[:10])
			}
			raw = append(raw, classifier.HistoricalEntry{
				TransactionID: t.ID, DestinationName: s.DestinationName, Description: s.Description,
				CategoryName: s.CategoryName, DestinationAccountID: s.DestinationID,
				Amount: math.Abs(amt), Tags: classifier.SemanticTags(s.Tags), Date: d,
			})
		}
		p.histMu.Lock()
		p.histMemo[gkey] = memoEntry{entries: raw, at: time.Now()}
		p.histMu.Unlock()
	}

	var out []classifier.HistoricalEntry
	for _, h := range raw {
		if !txnDate.IsZero() && !h.Date.IsZero() {
			d := h.Date.Sub(txnDate)
			if d < 0 {
				d = -d
			}
			if d.Hours()/24 > autoMatchWindowDays {
				continue
			}
		}
		out = append(out, h)
	}
	return out
}

// InvalidateHistory forces the history cache to refetch from Firefly on next use
// (call after a manual category/destination/tag change so auto-match stays fresh).
func (p *Pipeline) InvalidateHistory() {
	if p.cache != nil {
		p.cache.Invalidate()
	}
	p.histMu.Lock()
	p.histMemo = map[string]memoEntry{}
	p.histMu.Unlock()
}

// SimilarTxn is a transaction sharing a merchant with the reference one.
type SimilarTxn struct {
	ID          string   `json:"id"`
	Date        string   `json:"date"`
	Description string   `json:"description"`
	Amount      float64  `json:"amount"`
	Category    string   `json:"category"`
	Destination string   `json:"destination"`
	Tags        []string `json:"tags"`
}

// SimilarTransactions returns other withdrawals that share the merchant key of
// the given transaction (for bulk apply). Lookback ~730 days.
func (p *Pipeline) SimilarTransactions(ctx context.Context, transactionID string) ([]SimilarTxn, error) {
	txns, err := p.firefly.GetTransactionsByIDs(ctx, []string{transactionID})
	if err != nil || len(txns) == 0 || len(txns[0].Splits) == 0 {
		return nil, fmt.Errorf("transaction not found")
	}
	s := txns[0].Splits[0]
	key := classifier.GroupKey(s.DestinationName, s.Description)
	if key == "" {
		return nil, nil
	}
	all, err := p.firefly.SearchWithdrawals(ctx, key)
	if err != nil {
		return nil, err
	}
	var out []SimilarTxn
	for _, t := range all {
		if t.ID == transactionID || len(t.Splits) == 0 {
			continue
		}
		sp := t.Splits[0]
		if classifier.GroupKey(sp.DestinationName, sp.Description) != key {
			continue
		}
		amt, _ := strconv.ParseFloat(strings.TrimSpace(sp.Amount), 64)
		date := sp.Date
		if len(date) >= 10 {
			date = date[:10]
		}
		out = append(out, SimilarTxn{
			ID: t.ID, Date: date, Description: sp.Description, Amount: math.Abs(amt),
			Category: sp.CategoryName, Destination: sp.DestinationName, Tags: classifier.SemanticTags(sp.Tags),
		})
	}
	return out, nil
}
