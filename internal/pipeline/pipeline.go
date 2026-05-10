package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/openaccountants/firefly-iii-ai-categorize/internal/cache"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/classifier"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/firefly"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/job"
)

const categoryTTL = 5 * time.Minute

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
	history := p.cache.GetHistory(ctx, gkey, p.contextN)

	result, err := p.classifier.Classify(ctx, classifier.Request{
		Categories:      clCats,
		DestinationName: j.DestinationName,
		Description:     j.Description,
		Amount:          j.Amount,
		History:         history,
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

	// Immediately add to history cache so subsequent jobs benefit from this result.
	if result.Category != "" {
		p.cache.Append(classifier.HistoricalEntry{
			DestinationName: j.DestinationName,
			Description:     j.Description,
			CategoryName:    result.Category,
			GroupKey:        classifier.GroupKey(j.DestinationName, j.Description),
		})
	}

	slog.Info("transaction classified",
		"id", transactionID,
		"outcome", result.Outcome,
		"category", result.Category,
	)
	return nil
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
