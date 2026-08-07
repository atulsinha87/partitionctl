package state

import (
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// DefaultLeaseTTL is the lease lifetime when a caller does not choose one.
// FR-LOCK-3 makes the heartbeat interval configurable; the TTL should be a
// small multiple of it so a slow checkpoint does not look like a dead process.
const DefaultLeaseTTL = 60 * time.Second

// DefaultHeartbeatInterval is the suggested heartbeat period for
// [DefaultLeaseTTL]. It is advisory: the executor owns the timer.
const DefaultHeartbeatInterval = 15 * time.Second

// Lease is the liveness signal that makes an abandoned run detectable
// (FR-STATE-7, FR-LOCK-3, INV-4).
//
// It is not a mutual-exclusion primitive. The advisory lock is (FR-LOCK-1).
// The lease answers a different question: "is the process that opened this run
// still alive?", which the advisory lock cannot answer after the holding
// session has already gone away.
type Lease struct {
	RunID RunID `json:"run_id"`

	// Holder identifies the process, conventionally host/pid. It is the
	// fencing token: a heartbeat from a different holder is refused.
	Holder string `json:"holder"`

	AcquiredAt  protocol.Timestamp `json:"acquired_at"`
	HeartbeatAt protocol.Timestamp `json:"heartbeat_at"`

	// TTLSeconds is how long after the last heartbeat the lease stays valid.
	TTLSeconds int `json:"ttl_seconds"`
}

// TTL returns the lease lifetime as a duration.
func (l Lease) TTL() time.Duration { return time.Duration(l.TTLSeconds) * time.Second }

// ExpiresAt returns the instant after which the lease is expired.
func (l Lease) ExpiresAt() time.Time { return l.HeartbeatAt.Time.Add(l.TTL()) }

// Expired reports whether the lease has expired as of now. A lease with a
// non-positive TTL is treated as expired, so a malformed record fails toward
// "the holder might be dead" rather than "the holder is definitely alive".
func (l Lease) Expired(now time.Time) bool {
	if l.TTLSeconds <= 0 {
		return true
	}
	return !now.Before(l.ExpiresAt())
}

// IsZero reports whether the lease is unset.
func (l Lease) IsZero() bool { return l.RunID == "" && l.Holder == "" }

func normalizeTTL(ttl time.Duration) (int, error) {
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	secs := int(ttl / time.Second)
	if secs <= 0 {
		return 0, protocol.ErrFailure.Detailf("lease ttl %s rounds down to zero seconds", ttl)
	}
	return secs, nil
}
