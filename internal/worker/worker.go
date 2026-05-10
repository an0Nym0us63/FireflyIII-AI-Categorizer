package worker

import (
	"context"
	"log/slog"
	"sync"
)

// Task is a unit of work submitted to a Pool.
type Task struct {
	JobID   string
	Execute func(ctx context.Context) error
}

// Pool is a bounded goroutine pool that processes Tasks from a channel.
// Tasks always run with context.Background() so that HTTP request cancellations
// do not abort queued classification work.
type Pool struct {
	tasks       chan Task
	concurrency int
	wg          sync.WaitGroup
}

func NewPool(concurrency, queueSize int) *Pool {
	return &Pool{
		tasks:       make(chan Task, queueSize),
		concurrency: concurrency,
	}
}

func (p *Pool) Start() {
	for i := 0; i < p.concurrency; i++ {
		p.wg.Add(1)
		go p.run()
	}
}

func (p *Pool) Submit(t Task) {
	p.tasks <- t
}

// Shutdown stops accepting new tasks and waits for all queued tasks to finish.
func (p *Pool) Shutdown() {
	close(p.tasks)
	p.wg.Wait()
}

func (p *Pool) run() {
	defer p.wg.Done()
	for task := range p.tasks {
		if err := task.Execute(context.Background()); err != nil {
			slog.Error("task failed", "job_id", task.JobID, "error", err)
		}
	}
}
