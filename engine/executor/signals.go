package executor

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// StopOnSignals returns a context that is cancelled on SIGINT or SIGTERM, and a
// function that releases the handler.
//
// Both signals are handled identically (FR-EXEC-8): they stop the scheduling of
// new nodes and nothing else. [Executor.Run] observes the cancellation at the
// next node boundary, so an in-flight CREATE INDEX CONCURRENTLY runs to
// completion rather than dying half-built (FR-CLI-10).
//
// A second signal is not special-cased here. os/signal restores the default
// disposition once the returned stop function runs, so an operator who wants a
// hard kill still has one, and is choosing the INVALID index that comes with it.
func StopOnSignals(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
