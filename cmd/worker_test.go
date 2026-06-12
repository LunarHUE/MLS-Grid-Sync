package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/sync"
)

// stubWorker drives runWorkerLoop with scripted RunResults so the
// flag/accumulator semantics can be tested without a Service.
type stubWorker struct {
	results []sync.RunResult
	errs    []error
	calls   int
}

func (s *stubWorker) Run(_ context.Context) (sync.RunResult, error) {
	i := s.calls
	s.calls++
	var r sync.RunResult
	if i < len(s.results) {
		r = s.results[i]
	}
	var err error
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return r, err
}

func (s *stubWorker) WorkerID() string { return "stub-worker" }

func TestRunWorkerLoop_OnceExitsAfterOneCycle(t *testing.T) {
	stub := &stubWorker{
		results: []sync.RunResult{
			{Worked: true, Succeeded: 7, Retrying: 1, PermanentlyFailed: 0},
		},
	}
	err := runWorkerLoop(context.Background(), stub, workerLoopOpts{once: true})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("expected exactly 1 cycle, got %d", stub.calls)
	}
}

func TestRunWorkerLoop_OnceWithErrorReturnsError(t *testing.T) {
	wantErr := errors.New("cycle 1 bomb")
	stub := &stubWorker{errs: []error{wantErr}}
	err := runWorkerLoop(context.Background(), stub, workerLoopOpts{once: true})
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped error, got %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("expected exactly 1 cycle on error, got %d", stub.calls)
	}
}

func TestRunWorkerLoop_MaxJobsTerminalCountAcrossCycles(t *testing.T) {
	// 50 + 50 = 100 terminal outcomes (succeeded + permanently_failed).
	// Loop must exit after cycle 2 with --max-jobs 100.
	stub := &stubWorker{
		results: []sync.RunResult{
			{Worked: true, Succeeded: 40, PermanentlyFailed: 10}, // cycle 1: +50
			{Worked: true, Succeeded: 45, PermanentlyFailed: 5},  // cycle 2: +50 (total 100, hit bound)
			{Worked: true, Succeeded: 99},                        // cycle 3: must NOT run
		},
	}
	err := runWorkerLoop(context.Background(), stub, workerLoopOpts{maxJobs: 100})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if stub.calls != 2 {
		t.Errorf("expected 2 cycles to satisfy max-jobs 100, got %d", stub.calls)
	}
}

func TestRunWorkerLoop_MaxJobsRetryingDoesNotCount(t *testing.T) {
	// 99 succeeded + 50 retrying transitions = 99 terminal. Bound (100)
	// NOT reached. Loop continues to cycle 2.
	stub := &stubWorker{
		results: []sync.RunResult{
			{Worked: true, Succeeded: 99, Retrying: 50},
			{Worked: true, Succeeded: 1}, // +1 → 100 → exit
		},
	}
	err := runWorkerLoop(context.Background(), stub, workerLoopOpts{maxJobs: 100})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if stub.calls != 2 {
		t.Errorf("retrying must not satisfy bound; expected 2 cycles, got %d", stub.calls)
	}
}

func TestRunWorkerLoop_MaxJobsPriorRowsDontCount(t *testing.T) {
	// Critical invariant from the plan: in-process counting only.
	// "Prior test rows" (rows the table already has succeeded for some
	// other reason) MUST NOT count toward the bound. The stub emits
	// zero terminal outcomes for the first cycle → loop must continue,
	// not exit thinking the bound was already met by pre-existing state.
	stub := &stubWorker{
		results: []sync.RunResult{
			{Worked: true, Succeeded: 0}, // pretends DB already has 1000 succeeded
			{Worked: true, Succeeded: 10},
			{Worked: true, Succeeded: 90},
		},
	}
	err := runWorkerLoop(context.Background(), stub, workerLoopOpts{maxJobs: 100})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	// Cycle 1 (0) + cycle 2 (10) + cycle 3 (90) = 100 — three cycles.
	if stub.calls != 3 {
		t.Errorf("expected 3 cycles (in-process count), got %d", stub.calls)
	}
}

func TestRunWorkerLoop_UnlimitedRunsUntilCancel(t *testing.T) {
	// maxJobs=0 and once=false → infinite loop. Cancel the context to
	// stop, verify the loop honors it.
	ctx, cancel := context.WithCancel(context.Background())
	stub := &stubWorker{
		results: []sync.RunResult{
			{Worked: true, Succeeded: 1},
			{Worked: true, Succeeded: 1},
		},
	}
	// After 2 cycles, cancel.
	go func() {
		for stub.calls < 2 {
			// busy-spin until at least 2 cycles
		}
		cancel()
	}()
	err := runWorkerLoop(ctx, stub, workerLoopOpts{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
