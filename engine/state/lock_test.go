package state

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

func TestLockKeyAdvisoryKeys(t *testing.T) {
	// Determinism: the same key must produce the same pair every time, or two
	// processes disagree about what they are excluding each other from.
	k := testKey()
	c1, o1 := k.AdvisoryKeys()
	c2, o2 := k.AdvisoryKeys()
	if c1 != c2 || o1 != o2 {
		t.Fatalf("AdvisoryKeys is not deterministic: (%d,%d) then (%d,%d)", c1, o1, c2, o2)
	}
	if c1 != advisoryClassID {
		t.Errorf("classid = %d, want the fixed %d", c1, advisoryClassID)
	}

	// Injectivity across the part boundary: length framing is what stops
	// database "a" table "b.c" colliding with database "a/b" table "c".
	tests := []struct {
		name string
		a, b LockKey
	}{
		{
			name: "database boundary",
			a:    LockKey{Database: "a", Table: protocol.NewObjectName("b", "c")},
			b:    LockKey{Database: "ab", Table: protocol.NewObjectName("", "c")},
		},
		{
			name: "schema boundary",
			a:    LockKey{Database: "d", Table: protocol.NewObjectName("pub", "lic_orders")},
			b:    LockKey{Database: "d", Table: protocol.NewObjectName("public", "_orders")},
		},
		{
			name: "different table",
			a:    LockKey{Database: "d", Table: protocol.NewObjectName("public", "orders")},
			b:    LockKey{Database: "d", Table: protocol.NewObjectName("public", "events")},
		},
		{
			name: "different database",
			a:    LockKey{Database: "d1", Table: protocol.NewObjectName("public", "orders")},
			b:    LockKey{Database: "d2", Table: protocol.NewObjectName("public", "orders")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, oa := tc.a.AdvisoryKeys()
			_, ob := tc.b.AdvisoryKeys()
			if oa == ob {
				t.Errorf("%s and %s hash to the same objid %d", tc.a, tc.b, oa)
			}
		})
	}
}

func TestLockKeyStringAndValidate(t *testing.T) {
	tests := []struct {
		name    string
		key     LockKey
		wantStr string
		wantErr bool
	}{
		{
			name:    "qualified",
			key:     LockKey{Database: "appdb", Table: protocol.NewObjectName("public", "orders")},
			wantStr: "appdb/public.orders",
		},
		{
			name:    "no database",
			key:     LockKey{Table: protocol.NewObjectName("public", "orders")},
			wantStr: "public.orders",
		},
		{
			name:    "no table is invalid",
			key:     LockKey{Database: "appdb"},
			wantErr: true,
		},
		{
			name:    "an over-long table name is invalid",
			key:     LockKey{Table: protocol.NewObjectName("public", string(make([]byte, 64)))},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.key.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got := tc.key.String(); got != tc.wantStr {
				t.Errorf("String() = %q, want %q", got, tc.wantStr)
			}
		})
	}
}

// AC-10: two concurrent attempts against the same target, exactly one proceeds
// and the other exits non-zero with a clear message.
func TestFileStoreLockExcludes(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-lock")
	key := run.LockKey()

	first, err := s.TryLock(ctx, key)
	if err != nil {
		t.Fatalf("first TryLock: %v", err)
	}
	_, contended := s.TryLock(ctx, key)
	if !errors.Is(contended, protocol.ErrLockHeld) {
		t.Fatalf("second TryLock = %v, want ErrLockHeld", contended)
	}
	if !strings.Contains(contended.Error(), string(run.RunID)) {
		t.Errorf("the message does not name the holding run (FR-LOCK-2): %v", contended)
	}
	if got := protocol.ExitCodeFor(contended); got != protocol.ExitLockHeld {
		t.Errorf("exit code = %d, want %d", got, protocol.ExitLockHeld)
	}

	// A different target is unaffected.
	other := LockKey{Database: "appdb", Table: protocol.NewObjectName("public", "events")}
	l2, err2 := s.TryLock(ctx, other)
	if err2 != nil {
		t.Fatalf("TryLock on a different target: %v", err2)
	}
	if err := l2.Unlock(ctx); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	// Releasing lets the next caller in.
	if err := first.Unlock(ctx); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	third, err := s.TryLock(ctx, key)
	if err != nil {
		t.Fatalf("TryLock after release: %v", err)
	}
	if err := third.Unlock(ctx); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	// Unlocking twice is a no-op.
	if err := third.Unlock(ctx); err != nil {
		t.Fatalf("second Unlock: %v", err)
	}
}

// A lock file has no session to end, so a holder that dies must not block the
// target forever: the TTL is what an expired claim is reclaimed against.
func TestFileStoreLockStaleTakeover(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	s, err := OpenFileStore(t.TempDir(), FileOptions{
		Clock: c.Clock(), Holder: "dead-process", LockTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	key := testKey()

	dead, err := s.TryLock(ctx, key)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}

	// Still live: no takeover.
	c.Advance(10 * time.Second)
	if _, err := s.TryLock(ctx, key); !errors.Is(err, protocol.ErrLockHeld) {
		t.Fatalf("TryLock while the claim is live = %v, want ErrLockHeld", err)
	}

	// Refresh extends it, which is the heartbeat's job (FR-LOCK-3).
	if err := dead.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	c.Advance(25 * time.Second)
	if _, err := s.TryLock(ctx, key); !errors.Is(err, protocol.ErrLockHeld) {
		t.Fatalf("TryLock after a refresh = %v, want ErrLockHeld", err)
	}

	// Stop refreshing: the claim expires and the next caller reclaims it.
	c.Advance(31 * time.Second)
	taken, err := s.TryLock(ctx, key)
	if err != nil {
		t.Fatalf("TryLock after expiry: %v", err)
	}
	if held, _ := dead.Held(ctx); held {
		t.Error("the displaced holder still believes it holds the lock")
	}
	if err := dead.Refresh(ctx); !errors.Is(err, protocol.ErrLockHeld) {
		t.Errorf("the displaced holder's Refresh = %v, want ErrLockHeld", err)
	}
	// The displaced holder must not remove the new holder's lock file.
	if err := dead.Unlock(ctx); err != nil {
		t.Fatalf("displaced Unlock: %v", err)
	}
	if held, err := taken.Held(ctx); err != nil || !held {
		t.Fatalf("the new holder lost its lock to the displaced one: %v, %v", held, err)
	}
}

func TestFileStoreLockHeldReportsRevocation(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	key := testKey()
	lock, err := s.TryLock(ctx, key)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	if held, err := lock.Held(ctx); err != nil || !held {
		t.Fatalf("Held = %v, %v; want true", held, err)
	}
	// An operator removing the lock file out of band is observable.
	if err := os.Remove(s.lockPath(key)); err != nil {
		t.Fatalf("remove lock file: %v", err)
	}
	if held, err := lock.Held(ctx); err != nil || held {
		t.Fatalf("Held = %v, %v; want false after the file was removed", held, err)
	}
}

func TestFileStoreLockRejectsAnInvalidKey(t *testing.T) {
	s, _ := newFileStore(t)
	if _, err := s.TryLock(context.Background(), LockKey{Database: "appdb"}); err == nil {
		t.Fatal("want an error for a key with no table")
	}
}
