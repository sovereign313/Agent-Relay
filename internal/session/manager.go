package session

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrQueueFull = errors.New("session queue is full")
	ErrClosed    = errors.New("session manager is closed")
)

type Key struct {
	ChatID    int64
	ProjectID string
}

type Job struct {
	ID          int64
	Key         Key
	ProjectPath string
	Prompt      string
}

type Processor func(context.Context, Job)

type Status struct {
	Working   bool
	CurrentID int64
	Queued    int
}

type Manager struct {
	ctx       context.Context
	cancel    context.CancelFunc
	queueSize int
	process   Processor

	mu           sync.Mutex
	workers      map[Key]*worker
	projectGates map[string]chan struct{}
	closed       bool
	wg           sync.WaitGroup
}

type worker struct {
	queue        []Job
	wake         chan struct{}
	working      bool
	currentID    int64
	activeCancel context.CancelFunc
}

func NewManager(parent context.Context, queueSize int, process Processor) *Manager {
	ctx, cancel := context.WithCancel(parent)
	return &Manager{
		ctx:          ctx,
		cancel:       cancel,
		queueSize:    queueSize,
		process:      process,
		workers:      make(map[Key]*worker),
		projectGates: make(map[string]chan struct{}),
	}
}

func (m *Manager) Enqueue(job Job) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false, ErrClosed
	}
	current, ok := m.workers[job.Key]
	if !ok {
		current = &worker{wake: make(chan struct{}, 1)}
		m.workers[job.Key] = current
		m.wg.Add(1)
		go m.runWorker(current)
	}
	if len(current.queue) >= m.queueSize {
		return true, ErrQueueFull
	}
	queued := current.working || len(current.queue) > 0
	current.queue = append(current.queue, job)
	select {
	case current.wake <- struct{}{}:
	default:
	}
	return queued, nil
}

func (m *Manager) Cancel(key Key) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.workers[key]
	if !ok || !current.working || current.activeCancel == nil {
		return false
	}
	current.activeCancel()
	return true
}

func (m *Manager) CancelChat(chatID int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	cancelled := 0
	for key, current := range m.workers {
		if key.ChatID != chatID || !current.working || current.activeCancel == nil {
			continue
		}
		current.activeCancel()
		cancelled++
	}
	return cancelled
}

func (m *Manager) ClearQueue(key Key) []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.workers[key]
	if !ok || len(current.queue) == 0 {
		return nil
	}
	removed := append([]Job(nil), current.queue...)
	current.queue = nil
	return removed
}

func (m *Manager) Status(key Key) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.workers[key]
	if !ok {
		return Status{}
	}
	return Status{
		Working:   current.working,
		CurrentID: current.currentID,
		Queued:    len(current.queue),
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.cancel()
	for _, current := range m.workers {
		select {
		case current.wake <- struct{}{}:
		default:
		}
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *Manager) runWorker(current *worker) {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-current.wake:
		}

		for {
			m.mu.Lock()
			if m.ctx.Err() != nil {
				m.mu.Unlock()
				return
			}
			if len(current.queue) == 0 {
				m.mu.Unlock()
				break
			}
			job := current.queue[0]
			current.queue = current.queue[1:]
			jobContext, cancel := context.WithCancel(m.ctx)
			current.working = true
			current.currentID = job.ID
			current.activeCancel = cancel
			gate := m.projectGateLocked(job.ProjectPath)
			m.mu.Unlock()

			select {
			case <-jobContext.Done():
				m.process(jobContext, job)
			case <-gate:
				m.process(jobContext, job)
				gate <- struct{}{}
			}

			cancel()
			m.mu.Lock()
			current.working = false
			current.currentID = 0
			current.activeCancel = nil
			m.mu.Unlock()
		}
	}
}

func (m *Manager) projectGateLocked(path string) chan struct{} {
	gate, ok := m.projectGates[path]
	if ok {
		return gate
	}
	gate = make(chan struct{}, 1)
	gate <- struct{}{}
	m.projectGates[path] = gate
	return gate
}
