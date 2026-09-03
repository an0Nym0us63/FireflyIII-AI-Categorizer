package job

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Store persists jobs as JSON blobs. Implemented by the aidb package (which
// deals in strings to avoid an import cycle).
type Store interface {
	SaveJobJSON(id, data string) error
	LoadJobsJSON(limit int) ([]string, error)
}

// Event is emitted on every job state change and consumed by SSE subscribers.
type Event struct {
	Type string `json:"type"` // "created" | "updated"
	Job  *Job   `json:"job"`
}

// Registry is a thread-safe store for Jobs with pub/sub, optionally persisted.
type Registry struct {
	mu   sync.RWMutex
	jobs map[string]*Job

	byTxn map[string]string // transaction ID -> job ID (one job per transaction)

	subMu       sync.Mutex
	subscribers []chan Event

	store Store
}

func NewRegistry() *Registry {
	return &Registry{jobs: make(map[string]*Job), byTxn: make(map[string]string)}
}

// Attach wires a persistence store and loads previously saved jobs into memory.
func (r *Registry) Attach(s Store) {
	if s == nil {
		return
	}
	r.store = s
	datas, err := s.LoadJobsJSON(5000)
	if err != nil {
		return
	}
	r.mu.Lock()
	for _, d := range datas {
		var j Job
		if json.Unmarshal([]byte(d), &j) == nil && j.ID != "" {
			jj := j
			r.jobs[j.ID] = &jj
			if jj.TransactionID != "" {
				r.byTxn[jj.TransactionID] = jj.ID
			}
		}
	}
	r.mu.Unlock()
}

func (r *Registry) persist(j *Job) {
	if r.store == nil {
		return
	}
	if b, err := json.Marshal(j); err == nil {
		_ = r.store.SaveJobJSON(j.ID, string(b))
	}
}

func (r *Registry) Create(transactionID, batchID, destinationName, description string, amount *float64) *Job {
	now := time.Now()

	r.mu.Lock()
	// Reuse the existing job for this transaction (reset it) rather than piling
	// up duplicates when a transaction is reprocessed.
	if transactionID != "" {
		if id, ok := r.byTxn[transactionID]; ok {
			if j := r.jobs[id]; j != nil {
				j.BatchID = batchID
				j.Status = StatusQueued
				j.CreatedAt = now
				j.UpdatedAt = now
				j.DestinationName = destinationName
				j.Description = description
				j.Amount = amount
				j.Outcome = ""
				j.Category = ""
				j.Reason = ""
				j.Assumption = ""
				j.RawPrompt = ""
				j.RawResponse = ""
				j.DestinationAccount = ""
				j.DestinationAction = ""
				j.Tags = nil
				j.TagsAssumed = nil
				j.Error = ""
				r.mu.Unlock()
				r.persist(j)
				r.publish(Event{Type: "created", Job: j})
				return j
			}
		}
	}

	j := &Job{
		ID:              uuid.New().String(),
		BatchID:         batchID,
		Status:          StatusQueued,
		CreatedAt:       now,
		UpdatedAt:       now,
		TransactionID:   transactionID,
		DestinationName: destinationName,
		Description:     description,
		Amount:          amount,
	}
	r.jobs[j.ID] = j
	if transactionID != "" {
		r.byTxn[transactionID] = j.ID
	}
	r.mu.Unlock()
	r.persist(j)
	r.publish(Event{Type: "created", Job: j})
	return j
}

func (r *Registry) SetInProgress(id string) {
	r.update(id, func(j *Job) { j.Status = StatusInProgress })
}

func (r *Registry) SetFinished(id, outcome, category, reason, assumption, rawPrompt, rawResponse, destAccount, destAction string, tags, tagsAssumed []string) {
	r.update(id, func(j *Job) {
		j.Status = StatusFinished
		j.Outcome = outcome
		j.Category = category
		j.Reason = reason
		j.Assumption = assumption
		j.RawPrompt = rawPrompt
		j.RawResponse = rawResponse
		j.DestinationAccount = destAccount
		j.DestinationAction = destAction
		j.Tags = tags
		j.TagsAssumed = tagsAssumed
	})
}

func (r *Registry) SetFailed(id, errMsg string) {
	r.update(id, func(j *Job) {
		j.Status = StatusFailed
		j.Error = errMsg
	})
}

// UpdateTagsByTxn reflects a tag validation on the matching jobs: applied tags
// move from suggested to applied, rejected ones are removed.
func (r *Registry) UpdateTagsByTxn(txnID string, applied, rejected []string) {
	resolved := map[string]bool{}
	for _, n := range applied {
		resolved[strings.ToLower(strings.TrimSpace(n))] = true
	}
	for _, n := range rejected {
		resolved[strings.ToLower(strings.TrimSpace(n))] = true
	}
	var updated []*Job
	r.mu.Lock()
	for _, j := range r.jobs {
		if j.TransactionID != txnID {
			continue
		}
		var newAssumed []string
		for _, t := range j.TagsAssumed {
			if !resolved[strings.ToLower(strings.TrimSpace(t))] {
				newAssumed = append(newAssumed, t)
			}
		}
		j.TagsAssumed = newAssumed
		for _, a := range applied {
			has := false
			for _, t := range j.Tags {
				if strings.EqualFold(t, a) {
					has = true
					break
				}
			}
			if !has {
				j.Tags = append(j.Tags, a)
			}
		}
		j.UpdatedAt = time.Now()
		updated = append(updated, j)
	}
	r.mu.Unlock()
	for _, j := range updated {
		r.persist(j)
		r.publish(Event{Type: "updated", Job: j})
	}
}

// MarkReviewedByTxn marks any jobs for a transaction as reviewed, so the Jobs
// list reflects a later human review.
func (r *Registry) MarkReviewedByTxn(txnID string) {
	var updated []*Job
	r.mu.Lock()
	for _, j := range r.jobs {
		if j.TransactionID == txnID {
			j.Outcome = "REVIEWED"
			j.UpdatedAt = time.Now()
			updated = append(updated, j)
		}
	}
	r.mu.Unlock()
	for _, j := range updated {
		r.persist(j)
		r.publish(Event{Type: "updated", Job: j})
	}
}

func (r *Registry) Get(id string) (*Job, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	j, ok := r.jobs[id]
	return j, ok
}

func (r *Registry) List() []*Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	jobs := make([]*Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		jobs = append(jobs, j)
	}
	return jobs
}

func (r *Registry) ListByBatch(batchID string) []*Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var jobs []*Job
	for _, j := range r.jobs {
		if j.BatchID == batchID {
			jobs = append(jobs, j)
		}
	}
	return jobs
}

// Subscribe returns a channel that receives all future job events.
func (r *Registry) Subscribe() chan Event {
	ch := make(chan Event, 64)
	r.subMu.Lock()
	r.subscribers = append(r.subscribers, ch)
	r.subMu.Unlock()
	return ch
}

// Unsubscribe removes the channel and closes it.
func (r *Registry) Unsubscribe(ch chan Event) {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	for i, sub := range r.subscribers {
		if sub == ch {
			r.subscribers = append(r.subscribers[:i], r.subscribers[i+1:]...)
			close(ch)
			return
		}
	}
}

func (r *Registry) update(id string, fn func(*Job)) {
	r.mu.Lock()
	j, ok := r.jobs[id]
	if ok {
		fn(j)
		j.UpdatedAt = time.Now()
	}
	r.mu.Unlock()

	if ok {
		r.persist(j)
		r.publish(Event{Type: "updated", Job: j})
	}
}

func (r *Registry) publish(e Event) {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	for _, ch := range r.subscribers {
		select {
		case ch <- e:
		default: // drop if subscriber is slow
		}
	}
}
