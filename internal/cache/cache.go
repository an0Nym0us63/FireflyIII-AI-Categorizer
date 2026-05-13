package cache

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/openaccountants/firefly-iii-ai-categorize/internal/classifier"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/firefly"
)

// Cache holds a TTL-backed, in-memory copy of categorized transactions fetched
// from Firefly III. It is the single source of truth for classification history,
// so manual edits made in Firefly are picked up after the TTL expires.
type Cache struct {
	mu      sync.RWMutex
	entries []classifier.HistoricalEntry

	fetchedAt time.Time
	ttl       time.Duration
	lookback  int
	client    *firefly.Client
}

func New(client *firefly.Client, ttl time.Duration, lookbackDays int) *Cache {
	return &Cache{
		ttl:      ttl,
		lookback: lookbackDays,
		client:   client,
	}
}

// GetHistory returns up to limit past classifications whose GroupKey matches
// groupKey, most-recent first. Silently returns nil on refresh errors (the
// classifier still works, just without context).
func (c *Cache) GetHistory(ctx context.Context, groupKey string, limit int) []classifier.HistoricalEntry {
	if groupKey == "" {
		return nil
	}
	if err := c.ensureFresh(ctx); err != nil {
		slog.Error("history cache refresh failed", "error", err)
		return nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	var matches []classifier.HistoricalEntry
	for i := len(c.entries) - 1; i >= 0 && len(matches) < limit; i-- {
		if c.entries[i].GroupKey == groupKey {
			matches = append(matches, c.entries[i])
		}
	}
	return matches
}

// Append adds a freshly classified entry without waiting for TTL expiry.
// This ensures later jobs in the same batch benefit from earlier results.
func (c *Cache) Append(entry classifier.HistoricalEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, entry)
}

// Invalidate forces a re-fetch on the next GetHistory call.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetchedAt = time.Time{}
}

func (c *Cache) ensureFresh(ctx context.Context) error {
	// Fast path: check under read lock.
	c.mu.RLock()
	fresh := c.ttl > 0 && !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < c.ttl
	c.mu.RUnlock()
	if fresh {
		return nil
	}

	// Slow path: acquire write lock and double-check.
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ttl > 0 && !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < c.ttl {
		return nil
	}

	slog.Info("refreshing history cache from Firefly III")
	txns, err := c.client.GetCategorizedWithdrawals(ctx, c.lookback)
	if err != nil {
		return err
	}

	entries := make([]classifier.HistoricalEntry, 0, len(txns))
	for _, txn := range txns {
		for _, s := range txn.Splits {
			amount, _ := strconv.ParseFloat(s.Amount, 64)
			entries = append(entries, classifier.HistoricalEntry{
				TransactionID:   txn.ID,
				DestinationName: s.DestinationName,
				Description:     s.Description,
				CategoryName:    s.CategoryName,
				GroupKey:        classifier.GroupKey(s.DestinationName, s.Description),
				Amount:          amount,
			})
		}
	}

	c.entries = entries
	c.fetchedAt = time.Now()
	slog.Info("history cache refreshed", "entries", len(entries))
	return nil
}
