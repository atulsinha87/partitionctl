package executor

import (
	"fmt"
	"math"
	"time"
)

// RetryPolicy bounds retries of a retryable failure (FR-EXEC-4).
//
// Backoff is exponential with jitter. Jitter matters more than it looks: at
// 400 leaf partitions a retry storm behind one long transaction would otherwise
// have every attempt land in lockstep on the same lock.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts for one node in one run,
	// including the first. It must be at least 1.
	//
	// The budget is per run invocation, not cumulative across resumes: a run
	// that dies mid-backoff and is resumed gets a fresh budget, while the
	// cumulative count still accrues in the state store for the operator to see.
	MaxAttempts int

	// BaseDelay is the delay before the second attempt. Each subsequent delay
	// doubles until MaxDelay.
	BaseDelay time.Duration

	// MaxDelay caps the delay. It must be at least BaseDelay.
	MaxDelay time.Duration

	// Jitter is the fraction of each delay that is randomized away, in [0, 1].
	// 0 disables jitter; 1 makes the delay uniform over (0, delay].
	Jitter float64
}

// DefaultRetryPolicy is the policy used when [Config.Retry] is left zero.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 5,
		BaseDelay:   1 * time.Second,
		MaxDelay:    60 * time.Second,
		Jitter:      0.5,
	}
}

// Validate checks the policy is usable.
func (p RetryPolicy) Validate() error {
	if p.MaxAttempts < 1 {
		return fmt.Errorf("retry: max_attempts is %d, want at least 1", p.MaxAttempts)
	}
	if p.BaseDelay < 0 {
		return fmt.Errorf("retry: base_delay is negative")
	}
	if p.MaxDelay < 0 {
		return fmt.Errorf("retry: max_delay is negative")
	}
	if p.MaxDelay < p.BaseDelay {
		return fmt.Errorf("retry: max_delay %s is below base_delay %s", p.MaxDelay, p.BaseDelay)
	}
	if p.Jitter < 0 || p.Jitter > 1 {
		return fmt.Errorf("retry: jitter is %v, want a fraction in [0, 1]", p.Jitter)
	}
	return nil
}

// Delay returns the un-jittered backoff before the given attempt number.
// Attempt 1 is the first try and has no delay; attempt n waits
// BaseDelay * 2^(n-2), capped at MaxDelay.
func (p RetryPolicy) Delay(attempt int) time.Duration {
	if attempt <= 1 || p.BaseDelay <= 0 {
		return 0
	}
	d := p.BaseDelay
	for i := 2; i < attempt; i++ {
		if d >= p.MaxDelay {
			return p.MaxDelay
		}
		d *= 2
	}
	if d > p.MaxDelay {
		return p.MaxDelay
	}
	return d
}

// JitteredDelay applies jitter to [RetryPolicy.Delay] using r, a value in
// [0, 1) drawn by the caller. The result lies in
// ((1-Jitter) * Delay, Delay] and is never negative, which is the bound the
// tests assert.
func (p RetryPolicy) JitteredDelay(attempt int, r float64) time.Duration {
	d := p.Delay(attempt)
	if d <= 0 || p.Jitter <= 0 {
		return d
	}
	if r < 0 || math.IsNaN(r) {
		r = 0
	}
	if r >= 1 {
		r = math.Nextafter(1, 0)
	}
	out := float64(d) * (1 - p.Jitter*r)
	if out < 0 {
		return 0
	}
	return time.Duration(out)
}
