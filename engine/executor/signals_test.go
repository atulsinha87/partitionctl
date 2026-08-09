package executor

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// TestStopOnSignalsTreatsBothSignalsAlike proves FR-EXEC-8 against real
// signals. signal.NotifyContext installs its handler before anything is sent,
// so neither signal reaches the default disposition and the test process
// survives.
func TestStopOnSignalsTreatsBothSignalsAlike(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		sig := sig
		t.Run(sig.String(), func(t *testing.T) {
			ctx, stop := StopOnSignals(context.Background())
			defer stop()

			if ctx.Err() != nil {
				t.Fatalf("context is already done: %v", ctx.Err())
			}
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatalf("kill: %v", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("%s did not cancel the context", sig)
			}
		})
	}
}

// TestSignalStopsSchedulingWithoutFailingTheRun ties the signal path to the
// dispatch loop: an already-cancelled context stops the run at the first node
// boundary, with no DDL and no failure.
func TestSignalStopsSchedulingWithoutFailingTheRun(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := h.executor(t).Run(ctx, "run-1", createChainPlan(t))
	if err != nil {
		t.Fatalf("a signalled run is not a failure, got %v", err)
	}
	if !res.Cancelled || res.CancelReason != StopSignal {
		t.Fatalf("Result = %+v, want Cancelled with reason %q", res, StopSignal)
	}
	if h.sql.execCount() != 0 {
		t.Fatal("statements ran after the signal")
	}
	if res.Remaining != res.Total {
		t.Fatalf("Remaining = %d, want all %d nodes still to do", res.Remaining, res.Total)
	}
	if got := h.store.stateOf("n1"); got != protocol.NodePending {
		t.Fatalf("n1 is %s, want PENDING so resume can adopt it", got)
	}
}
