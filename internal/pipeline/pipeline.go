package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

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

	// Short-lived category cache to avoid re-fetching on every job in a batch.
	catMu      sync.RWMutex
	catCache   []firefly.Category
	catFetched time.Time
}

func New(
	fc *firefly.Client,
	cl classifier.Classifier,
	ca *cache.Cache,
	reg *job.Registry,
	contextN int,
) *Pipeline {
	return &Pipeline{
		firefly:    fc,
		classifier: cl,
		cache:      ca,
		registry:   reg,
		contextN:   contextN,
	}
}

// Run executes the full classification pipeline for a queued job.
func (p *Pipeline) Run(ctx context.Context, j *job.Job, transactionID string, splits []firefly.Split) error {
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

	// Fetch enough history for both the match check and the AI prompt.
	lookupLimit := historyMatchLookup
	if p.contextN > lookupLimit {
		lookupLimit = p.contextN
	}
	history := excludeTransaction(p.cache.GetHistory(ctx, gkey, lookupLimit), transactionID)

	// Skip the AI call when history gives a high-confidence answer.
	if matchCat, matchCount := tryHistoryMatch(history, j.Amount); matchCat != "" {
		categoryID := ""
		for _, c := range categories {
			if c.Name == matchCat {
				categoryID = c.ID
				break
			}
		}
		reason := fmt.Sprintf("Auto-matched from %d previous transactions with the same payee.", matchCount)
		outcome := firefly.UpdateOutcome{
			Outcome:    string(classifier.Classified),
			Category:   matchCat,
			CategoryID: categoryID,
			Reason:     reason,
		}
		if err := p.firefly.UpdateTransaction(ctx, transactionID, splits, outcome); err != nil {
			p.registry.SetFailed(j.ID, err.Error())
			return fmt.Errorf("update transaction: %w", err)
		}
		p.registry.SetFinished(j.ID, string(classifier.Classified), matchCat, reason, "", "", "")
		p.cache.Append(classifier.HistoricalEntry{
			TransactionID:   transactionID,
			DestinationName: j.DestinationName,
			Description:     j.Description,
			CategoryName:    matchCat,
			GroupKey:        gkey,
			Amount:          derefAmount(j.Amount),
		})
		slog.Info("transaction history-matched",
			"id", transactionID,
			"category", matchCat,
			"matches", matchCount,
		)
		return nil
	}

	// Trim history to the configured context limit for the AI prompt.
	promptHistory := history
	if len(promptHistory) > p.contextN {
		promptHistory = promptHistory[:p.contextN]
	}

	result, err := p.classifier.Classify(ctx, classifier.Request{
		Categories:      clCats,
		DestinationName: j.DestinationName,
		Description:     j.Description,
		Amount:          j.Amount,
		History:         promptHistory,
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

	if err := p.firefly.UpdateTransaction(ctx, transactionID, splits, outcome); err != nil {
		p.registry.SetFailed(j.ID, err.Error())
		return fmt.Errorf("update transaction: %w", err)
	}

	p.registry.SetFinished(
		j.ID,
		string(result.Outcome),
		result.Category,
		result.Reason,
		result.Assumption,
		result.RawPrompt,
		result.RawResponse,
	)

	if result.Category != "" {
		p.cache.Append(classifier.HistoricalEntry{
			TransactionID:   transactionID,
			DestinationName: j.DestinationName,
			Description:     j.Description,
			CategoryName:    result.Category,
			GroupKey:        gkey,
			Amount:          derefAmount(j.Amount),
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
