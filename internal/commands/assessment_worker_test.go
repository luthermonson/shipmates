package commands

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestAssessmentWorkerRunsOffSchedulerAndJoins(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	w := newAssessmentWorker(context.Background(), func(ctx context.Context, active func() bool) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-ctx.Done()
		if active() {
			t.Error("worker remained active after cancellation")
		}
		return ctx.Err()
	})
	w.Start()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("assessment worker did not start")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("assessment calls = %d, want one in-flight worker call", calls.Load())
	}
}

func TestAssessmentWorkerPanicIsContainedAndCloseIsIdempotent(t *testing.T) {
	w := newAssessmentWorker(context.Background(), func(context.Context, func() bool) error { panic("controlled adviser failure") })
	w.Start()
	deadline := time.After(time.Second)
	select {
	case <-w.done:
	case <-deadline:
		t.Fatal("panicking worker did not terminate")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAssessmentWorkerErrorProducesObservableIndeterminateOutcome(t *testing.T) {
	outcomes := make(chan [2]string, 1)
	w := newLeasedAssessmentWorker(context.Background(), func(context.Context, func() bool) error {
		return context.DeadlineExceeded
	}, nil, func(state, code string) error {
		outcomes <- [2]string{state, code}
		return nil
	})
	w.Start()
	select {
	case got := <-outcomes:
		if got != [2]string{"indeterminate", "worker_error"} {
			t.Fatalf("outcome=%v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("worker outcome not persisted")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAssessmentWorkerFencePreventsPublicationAfterClose(t *testing.T) {
	var activeAtCommit atomic.Bool
	w := newAssessmentWorker(context.Background(), func(ctx context.Context, active func() bool) error {
		<-ctx.Done()
		activeAtCommit.Store(active())
		return nil
	})
	w.Start()
	// Give the worker a bounded opportunity to enter the controlled operation.
	time.Sleep(40 * time.Millisecond)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if activeAtCommit.Load() {
		t.Fatal("stale generation remained publishable")
	}
}

func TestAssessmentWorkerSuccessfulOutcomeIsRecordedOnce(t *testing.T) {
	var outcomes atomic.Int32
	w := newLeasedAssessmentWorker(context.Background(), nil, nil, func(string, string) error {
		outcomes.Add(1)
		return nil
	})
	w.finishOutcome("completed", "assessment_terminal")
	w.Start()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if outcomes.Load() != 1 {
		t.Fatalf("outcome callback count=%d, want one", outcomes.Load())
	}
}

func TestAssessmentWorkerCancellationAfterEntryIsIndeterminate(t *testing.T) {
	entered := make(chan struct{})
	outcomes := make(chan [2]string, 1)
	w := newLeasedAssessmentWorker(context.Background(), func(ctx context.Context, active func() bool) error {
		<-ctx.Done()
		if active() {
			t.Fatal("cancelled worker remained publishable")
		}
		return ctx.Err()
	}, func() error {
		close(entered)
		return nil
	}, func(state, code string) error {
		outcomes <- [2]string{state, code}
		return nil
	})
	w.Start()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-outcomes:
		if got != [2]string{"indeterminate", "worker_cancelled"} {
			t.Fatalf("outcome=%v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("missing indeterminate outcome")
	}
}
