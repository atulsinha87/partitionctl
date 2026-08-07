package state

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLeaseExpiry(t *testing.T) {
	l := Lease{HeartbeatAt: tsAt(baseTime), TTLSeconds: 60}

	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{name: "at the heartbeat", now: baseTime, want: false},
		{name: "one second before the deadline", now: baseTime.Add(59 * time.Second), want: false},
		{name: "exactly at the deadline is expired", now: baseTime.Add(60 * time.Second), want: true},
		{name: "after the deadline", now: baseTime.Add(90 * time.Second), want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := l.Expired(tc.now); got != tc.want {
				t.Errorf("Expired(%s) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}

	// A malformed TTL must fail toward "the holder might be dead", never
	// toward "the holder is definitely alive".
	t.Run("a non-positive ttl is expired", func(t *testing.T) {
		for _, ttl := range []int{0, -1} {
			bad := Lease{HeartbeatAt: tsAt(baseTime), TTLSeconds: ttl}
			if !bad.Expired(baseTime) {
				t.Errorf("ttl %d: Expired = false, want true", ttl)
			}
		}
	})
}

func TestFileStoreLeaseLifecycle(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-lease")

	lease := mustLease(t, s, run.RunID, "holder-a", 30*time.Second)
	if lease.Holder != "holder-a" || lease.TTLSeconds != 30 {
		t.Fatalf("lease = %+v", lease)
	}

	// A second holder is refused while the first is live.
	if _, err := s.AcquireLease(ctx, run.RunID, "holder-b", 30*time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("AcquireLease by a second holder = %v, want ErrLeaseLost", err)
	}

	// Heartbeat extends it, and the acquisition time does not move.
	c.Advance(10 * time.Second)
	beat, err := s.Heartbeat(ctx, run.RunID, "holder-a")
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !beat.AcquiredAt.Time.Equal(lease.AcquiredAt.Time) {
		t.Errorf("AcquiredAt moved on heartbeat")
	}
	if !beat.HeartbeatAt.Time.After(lease.HeartbeatAt.Time) {
		t.Errorf("HeartbeatAt did not advance")
	}

	// A heartbeat from the wrong holder is the fencing signal.
	if _, err := s.Heartbeat(ctx, run.RunID, "holder-b"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Heartbeat by the wrong holder = %v, want ErrLeaseLost", err)
	}

	// Once expired, anyone may take it: that is what makes an abandoned run
	// adoptable (INV-4).
	c.Advance(31 * time.Second)
	if got, err := s.GetLease(ctx, run.RunID); err != nil {
		t.Fatalf("GetLease: %v", err)
	} else if !got.Expired(c.Now()) {
		t.Fatal("lease should be expired")
	}
	taken, err := s.AcquireLease(ctx, run.RunID, "holder-b", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireLease after expiry: %v", err)
	}
	if taken.Holder != "holder-b" {
		t.Errorf("holder = %q, want holder-b", taken.Holder)
	}

	// The displaced holder can no longer heartbeat or release.
	if _, err := s.Heartbeat(ctx, run.RunID, "holder-a"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("displaced heartbeat = %v, want ErrLeaseLost", err)
	}
	if err := s.ReleaseLease(ctx, run.RunID, "holder-a"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("displaced release = %v, want ErrLeaseLost", err)
	}

	if err := s.ReleaseLease(ctx, run.RunID, "holder-b"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if _, err := s.GetLease(ctx, run.RunID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetLease after release = %v, want ErrNotFound", err)
	}
	// Releasing again is a no-op, not an error.
	if err := s.ReleaseLease(ctx, run.RunID, "holder-b"); err != nil {
		t.Fatalf("second ReleaseLease: %v", err)
	}
}

func TestFileStoreLeaseRenewalByTheSameHolder(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-renew")

	first := mustLease(t, s, run.RunID, "holder-a", 30*time.Second)
	c.Advance(5 * time.Second)
	second, err := s.AcquireLease(ctx, run.RunID, "holder-a", 45*time.Second)
	if err != nil {
		t.Fatalf("re-acquire by the same holder: %v", err)
	}
	if !second.AcquiredAt.Time.Equal(first.AcquiredAt.Time) {
		t.Errorf("AcquiredAt = %s, want the original %s", second.AcquiredAt, first.AcquiredAt)
	}
	if second.TTLSeconds != 45 {
		t.Errorf("ttl = %d, want 45", second.TTLSeconds)
	}
}

func TestFileStoreLeaseRequiresARun(t *testing.T) {
	s, _ := newFileStore(t)
	if _, err := s.AcquireLease(context.Background(), "no-such-run", "h", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestNormalizeTTL(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want int
	}{
		{name: "zero takes the default", in: 0, want: int(DefaultLeaseTTL / time.Second)},
		{name: "negative takes the default", in: -time.Second, want: int(DefaultLeaseTTL / time.Second)},
		{name: "whole seconds", in: 90 * time.Second, want: 90},
		{name: "sub-second rounds down and is refused", in: 500 * time.Millisecond, want: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeTTL(tc.in)
			if tc.want < 0 {
				if err == nil {
					t.Fatalf("want an error for %s", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeTTL(%s): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}
