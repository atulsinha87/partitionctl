package state

import (
	"context"
	"crypto/sha256"
	"encoding/binary"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// advisoryClassID is the first half of the PostgreSQL two-int advisory lock
// key. It is a fixed constant so that PartitionCTL's locks never collide with
// another application's, whatever it hashes into the second half.
//
// The value is ASCII "PCT1" read big-endian, which keeps it recognizable in
// pg_locks output.
const advisoryClassID int32 = 0x50435431

// lockKeyDomain separates advisory-lock hashing from every other hash in the
// system, so a target's lock key can never coincide with a digest or a child
// index name tag.
const lockKeyDomain = "partitionctl.advisorylock.v1"

// LockKey identifies what a run holds exclusive rights over: one partitioned
// table in one database (FR-LOCK-1).
//
// The key is the target table and not the plan digest on purpose. Two different
// plans against the same table must exclude each other; that is what AC-10
// asserts.
type LockKey struct {
	// Database is the target database name. It is part of the key because a
	// file store can hold state for more than one database, and because the
	// same table name means different things in different databases.
	Database string `json:"database,omitempty"`

	// Table is the partitioned parent table.
	Table protocol.ObjectName `json:"table"`
}

// String renders the key's identity form, for log fields and error messages.
func (k LockKey) String() string {
	if k.Database == "" {
		return k.Table.String()
	}
	return k.Database + "/" + k.Table.String()
}

// Validate checks the key's identifiers.
func (k LockKey) Validate() error {
	if k.Table.IsZero() {
		return protocol.ErrInvalidIdentifier.Detailf("advisory lock key has no table")
	}
	return k.Table.Validate()
}

// AdvisoryKeys returns the (classid, objid) pair for pg_try_advisory_lock.
//
// classid is [advisoryClassID]; objid is the first four bytes of a
// domain-separated, length-framed SHA-256 over the key's parts, read as a
// big-endian signed 32-bit integer. Length framing keeps the hash injective
// over the parts: without it, database "a" table "b.c" and database "a/b" table
// "c" would collide.
//
// It is a pure function of the key, so two processes agree without
// coordinating, and it is identical for both store implementations, which is
// what lets the file store's lock mean the same thing as the database's.
func (k LockKey) AdvisoryKeys() (classID, objID int32) {
	h := sha256.New()
	writeLengthPrefixed(h, []byte(lockKeyDomain))
	writeLengthPrefixed(h, []byte(k.Database))
	writeLengthPrefixed(h, []byte(k.Table.Schema))
	writeLengthPrefixed(h, []byte(k.Table.Name))
	sum := h.Sum(nil)
	return advisoryClassID, int32(binary.BigEndian.Uint32(sum[:4]))
}

// writeLengthPrefixed writes an 8-byte big-endian length followed by b. It is
// the same framing discipline the protocol package uses for its digests, and it
// is what makes a multi-part hash injective over its parts.
func writeLengthPrefixed(h interface{ Write([]byte) (int, error) }, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(b)
}

// Lock is a held, session-scoped advisory lock (FR-LOCK-1).
//
// It is an interface rather than a struct because holding one is proof, and
// proof is what the orphan-recovery helpers take as an argument: [FindOrphans]
// and [AdoptOrphan] cannot be called without one, which is how the
// advisory-lock half of INV-4 stops being a convention.
type Lock interface {
	// Key returns what this lock covers.
	Key() LockKey

	// Held reports whether the lock is still held by this handle. A file
	// store's lock can be revoked by an operator deleting the lock file; a SQL
	// store's session can be terminated. A caller that cares checks.
	Held(ctx context.Context) (bool, error)

	// Refresh renews the lock's claim, and should be called from the same
	// timer that heartbeats the lease (FR-LOCK-3).
	//
	// It exists because the two implementations lose a dead holder's lock in
	// different ways. A PostgreSQL session-level advisory lock is released by
	// the server when the session ends, so [SQLStore]'s Refresh is a no-op. A
	// lock file has no session, so [FileStore] gives it a TTL that Refresh
	// extends; without that, a SIGKILLed process would block its target
	// forever.
	Refresh(ctx context.Context) error

	// Unlock releases the lock. It is idempotent.
	Unlock(ctx context.Context) error
}

// Locker acquires advisory locks (FR-LOCK-1, FR-LOCK-2).
type Locker interface {
	// TryLock takes the session-level advisory lock for key without waiting.
	//
	// It returns an error matching [protocol.ErrLockHeld] when the lock is
	// held, and the message names the holding run where the store can
	// determine it (FR-LOCK-2, AC-10).
	TryLock(ctx context.Context, key LockKey) (Lock, error)
}

// checkLock validates that a supplied lock is usable proof for key.
func checkLock(ctx context.Context, lock Lock, key LockKey) error {
	if lock == nil {
		return protocol.ErrLockHeld.Detailf(
			"orphan recovery requires the advisory lock for %s to be held first (FR-LOCK-1, INV-4)", key)
	}
	if got := lock.Key(); got != key {
		return protocol.ErrLockHeld.Detailf(
			"the supplied advisory lock covers %s, not %s", got, key)
	}
	held, err := lock.Held(ctx)
	if err != nil {
		return err
	}
	if !held {
		return protocol.ErrLockHeld.Detailf("the advisory lock for %s is no longer held", key)
	}
	return nil
}
