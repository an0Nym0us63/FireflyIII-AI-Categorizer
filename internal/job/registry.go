package job

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Event is emitted on every job state change and consumed by SSE subscribers.
type Event struct {
	Type string `json:"type"` // "created" | "updated"
	Job  *Job   `json:"job"`
}

// Registry is a thread-safe in-memory store for Jobs with pub/sub support.
type Registry struct {
	mu   sync.RWMutex
	jobs map[string]*Job

	subMu       sync.Mutex
	subscribers []chan Event
}

func NewRegistry() *Registry {
	return &Registry{jobs: make(map[string]*Job)}
}

func (r *Registry) Create(transactionID, batchID, destinationName, description string, amount *float64) *Job {
	j := &Job{
		ID:              uuid.New().String(),
		BatchID:         batchID,
		Status:          StatusQueued,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
		TransactionID:   transactionID,
		DestinationName: destinationName,
		Description:     description,
		Amount:          amount,
	}
	r.mu.Lock()
	r.jobs[j.ID] = j
	r.mu.Unlock()
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
