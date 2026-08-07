package executor

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

func TestRetryPolicyDelayDoublesAndCaps(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 8, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 0, want: 0},
		{attempt: 1, want: 0}, // the first try is not a retry
		{attempt: 2, want: 100 * time.Millisecond},
		{attempt: 3, want: 200 * time.Millisecond},
		{attempt: 4, want: 400 * time.Millisecond},
		{attempt: 5, want: 800 * time.Millisecond},
		{attempt: 6, want: time.Second}, // capped
		{attempt: 50, want: time.Second},
	}
	for _, tc := range tests {
		if got := p.Delay(tc.attempt); got != tc.want {
			t.Errorf("Delay(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}

func TestRetryPolicyDelayIsZeroWithoutABaseDelay(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 3, BaseDelay: 0, MaxDelay: time.Second}
	for attempt := 1; attempt <= 5; attempt++ {
		if got := p.Delay(attempt); got != 0 {
			t.Fatalf("Delay(%d) = %s, want 0", attempt, got)
		}
	}
}

func TestJitteredDelayStaysWithinItsBounds(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 10, BaseDelay: time.Second, MaxDelay: 30 * time.Second, Jitter: 0.5}
	r := rand.New(rand.NewSource(1))

	for attempt := 2; attempt <= 10; attempt++ {
		base := p.Delay(attempt)
		low := time.Duration(float64(base) * (1 - p.Jitter))
		for i := 0; i < 2000; i++ {
			got := p.JitteredDelay(attempt, r.Float64())
			if got < 0 {
				t.Fatalf("attempt %d: negative delay %s", attempt, got)
			}
			if got > base {
				t.Fatalf("attempt %d: delay %s exceeds the un-jittered %s", attempt, got, base)
			}
			if got < low {
				t.Fatalf("attempt %d: delay %s is below the jitter floor %s", attempt, got, low)
			}
			if got > p.MaxDelay {
				t.Fatalf("attempt %d: delay %s exceeds max_delay %s", attempt, got, p.MaxDelay)
			}
		}
	}
}

func TestJitteredDelayHandlesDegenerateDraws(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: time.Second, Jitter: 1}
	tests := []struct {
		name string
		r    float64
		want time.Duration
	}{
		{"zero draw keeps the full delay", 0, time.Second},
		{"a draw of one is clamped below one", 1, 0},
		{"a draw above one is clamped", 7, 0},
		{"a negative draw is treated as zero", -3, time.Second},
		{"NaN is treated as zero", math.NaN(), time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.JitteredDelay(2, tc.r)
			if got < 0 {
				t.Fatalf("negative delay %s", got)
			}
			// A clamped draw of one lands just under the full delay rather than
			// exactly at zero, so compare loosely at that end.
			if tc.want == 0 {
				if got > time.Millisecond {
					t.Fatalf("JitteredDelay = %s, want approximately 0", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("JitteredDelay = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestJitteredDelayIsExactWithoutJitter(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, BaseDelay: 250 * time.Millisecond, MaxDelay: time.Minute, Jitter: 0}
	for attempt := 1; attempt <= 5; attempt++ {
		if got, want := p.JitteredDelay(attempt, 0.99), p.Delay(attempt); got != want {
			t.Fatalf("attempt %d: JitteredDelay = %s, want %s with jitter disabled", attempt, got, want)
		}
	}
}

func TestRetryPolicyValidate(t *testing.T) {
	tests := []struct {
		name    string
		policy  RetryPolicy
		wantErr bool
	}{
		{"default", DefaultRetryPolicy(), false},
		{"single attempt", RetryPolicy{MaxAttempts: 1}, false},
		{"zero attempts", RetryPolicy{MaxAttempts: 0}, true},
		{"negative attempts", RetryPolicy{MaxAttempts: -1}, true},
		{"negative base", RetryPolicy{MaxAttempts: 3, BaseDelay: -time.Second, MaxDelay: time.Second}, true},
		{"negative max", RetryPolicy{MaxAttempts: 3, MaxDelay: -time.Second}, true},
		{"max below base", RetryPolicy{MaxAttempts: 3, BaseDelay: time.Minute, MaxDelay: time.Second}, true},
		{"jitter above one", RetryPolicy{MaxAttempts: 3, Jitter: 1.5}, true},
		{"jitter below zero", RetryPolicy{MaxAttempts: 3, Jitter: -0.1}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestDefaultRetryPolicyIsBounded(t *testing.T) {
	p := DefaultRetryPolicy()
	if err := p.Validate(); err != nil {
		t.Fatalf("the default policy is invalid: %v", err)
	}
	// The total time a node can spend backing off is finite and knowable, which
	// is what "bounded by a configurable maximum attempt count" means.
	var total time.Duration
	for attempt := 2; attempt <= p.MaxAttempts; attempt++ {
		total += p.Delay(attempt)
	}
	if total > 2*time.Minute {
		t.Fatalf("default policy backs off for up to %s, which is too long to be a default", total)
	}
}
