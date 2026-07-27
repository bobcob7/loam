package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// recordingRunner is a runner whose Run blocks until ctx is canceled, then
// records that it returned -- enough to prove multiRunner starts every
// member and waits for all of them.
type recordingRunner struct {
	started  atomic.Bool
	finished atomic.Bool
}

func (r *recordingRunner) Run(ctx context.Context) {
	r.started.Store(true)
	<-ctx.Done()
	r.finished.Store(true)
}

// TestMultiRunner_RunsEveryMemberAndWaitsForAll proves multiRunner starts
// every member concurrently and does not return until ALL of them have --
// not just the first, and not before any of them.
func TestMultiRunner_RunsEveryMemberAndWaitsForAll(t *testing.T) {
	t.Parallel()
	a := &recordingRunner{}
	b := &recordingRunner{}
	m := multiRunner{a, b}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	// Give both members a moment to actually start before canceling --
	// this also proves both were started, not just one.
	for !a.started.Load() || !b.started.Load() {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("multiRunner.Run did not return after ctx was canceled")
	}
	assert.True(t, a.finished.Load(), "every member's Run must have returned before multiRunner.Run returns")
	assert.True(t, b.finished.Load(), "every member's Run must have returned before multiRunner.Run returns")
}

// TestMultiRunner_EmptyIsANoOp proves a multiRunner with zero members
// returns immediately rather than blocking forever.
func TestMultiRunner_EmptyIsANoOp(t *testing.T) {
	t.Parallel()
	var m multiRunner
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an empty multiRunner must return immediately")
	}
}

// TestMultiRunner_OneSlowMemberStillBlocksTheWholeGroup proves the "wait
// for ALL, not just the first to finish" contract concretely: a WaitGroup
// misuse (e.g. only waiting on the first member) would let this test's
// second, deliberately-slower member's completion race the outer
// goroutine's own bookkeeping -- asserting via a shared counter, not
// timing, is what makes this deterministic.
func TestMultiRunner_OneSlowMemberStillBlocksTheWholeGroup(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var finishOrder []string
	fast := runnerFunc(func(ctx context.Context) {
		<-ctx.Done()
		mu.Lock()
		finishOrder = append(finishOrder, "fast")
		mu.Unlock()
	})
	slow := runnerFunc(func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		finishOrder = append(finishOrder, "slow")
		mu.Unlock()
	})
	m := multiRunner{fast, slow}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("multiRunner.Run did not return")
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"fast", "slow"}, finishOrder, "multiRunner.Run must not return until the SLOWEST member has also finished")
}

// runnerFunc adapts a plain func(context.Context) to the runner interface,
// for this test file's own fixtures.
type runnerFunc func(ctx context.Context)

func (f runnerFunc) Run(ctx context.Context) { f(ctx) }
