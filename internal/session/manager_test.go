package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagerSerializesSameProjectAcrossChats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan int64, 2)
	release := make(chan struct{}, 2)
	var active atomic.Int32
	var maximum atomic.Int32

	manager := NewManager(ctx, 2, func(_ context.Context, job Job) {
		current := active.Add(1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		started <- job.Key.ChatID
		<-release
		active.Add(-1)
	})
	defer manager.Close()

	path := "/same/project"
	if _, err := manager.Enqueue(Job{Key: Key{ChatID: 1, ProjectID: "p"}, ProjectPath: path}); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, started); got != 1 {
		t.Fatalf("first chat = %d", got)
	}
	if _, err := manager.Enqueue(Job{Key: Key{ChatID: 2, ProjectID: "p"}, ProjectPath: path}); err != nil {
		t.Fatal(err)
	}
	select {
	case unexpected := <-started:
		t.Fatalf("second project started concurrently for chat %d", unexpected)
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	if got := receive(t, started); got != 2 {
		t.Fatalf("second chat = %d", got)
	}
	release <- struct{}{}
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent processors = %d, want 1", maximum.Load())
	}
}

func TestManagerBoundsQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	manager := NewManager(ctx, 1, func(context.Context, Job) {
		startOnce.Do(func() { close(started) })
		<-release
	})
	defer manager.Close()

	job := Job{Key: Key{ChatID: 1, ProjectID: "p"}, ProjectPath: "/project"}
	if _, err := manager.Enqueue(job); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := manager.Enqueue(job); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enqueue(job); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third enqueue error = %v, want ErrQueueFull", err)
	}
	removed := manager.ClearQueue(job.Key)
	if len(removed) != 1 {
		t.Fatalf("cleared jobs = %d, want 1", len(removed))
	}
	if _, err := manager.Enqueue(job); err != nil {
		t.Fatalf("enqueue after clear: %v", err)
	}
	close(release)
}

func receive(t *testing.T, channel <-chan int64) int64 {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for processor")
		return 0
	}
}
