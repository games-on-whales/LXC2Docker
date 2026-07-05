package lxc

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/image"
)

// Concurrent pulls of the SAME ref must collapse to a single underlying pull,
// with every caller sharing its result. Regression for the skopeo pile-up: an
// orchestrator re-issued the pull on each reconcile tick before the first
// finished, and the copies raced into one OCI store path, corrupted each other
// near completion, and a large image never landed.
func TestPullOCICoalescesSameRef(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	m := &Manager{
		pullImpl: func(_ *image.ResolvedImage, _ PullOpts) error {
			atomic.AddInt32(&calls, 1)
			<-release // hold the in-flight pull so the others must coalesce onto it
			return nil
		},
	}

	const n = 8
	r := &image.ResolvedImage{Ref: "ghcr.io/example/img:latest"}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = m.pullOCI(r, PullOpts{})
		}(i)
	}
	// Let every goroutine enter Do and coalesce onto the (blocked) leader.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("underlying pull ran %d times, want 1 (concurrent same-ref pulls must coalesce)", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d got error: %v", i, err)
		}
	}
}

// Different refs must NOT coalesce: they pull in parallel.
func TestPullOCIParallelForDifferentRefs(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	gate := make(chan struct{})
	m := &Manager{
		pullImpl: func(_ *image.ResolvedImage, _ PullOpts) error {
			atomic.AddInt32(&calls, 1)
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			<-gate
			mu.Lock()
			inFlight--
			mu.Unlock()
			return nil
		},
	}

	const n = 4
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = m.pullOCI(&image.ResolvedImage{Ref: fmt.Sprintf("ghcr.io/example/img:%d", i)}, PullOpts{})
		}(i)
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != n {
		t.Fatalf("underlying pull ran %d times, want %d (different refs must not coalesce)", got, n)
	}
	if maxInFlight < 2 {
		t.Fatalf("max concurrent pulls = %d, want >=2 (different refs should run in parallel)", maxInFlight)
	}
}
