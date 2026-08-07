package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// DigestPrefix names the hash algorithm inside the digest string, so a future
// algorithm change is visible in the artifact rather than implied by length.
const DigestPrefix = "sha256:"

// CanonicalBody returns the canonical serialization the digest is computed
// over: the whole plan except the digest field itself (FR-PLANFILE-2).
//
// It is exported because it is the thing to diff when two processes disagree
// about a digest.
func (p *Plan) CanonicalBody() ([]byte, error) {
	body := *p
	body.Digest = ""
	raw, err := json.Marshal(&body)
	if err != nil {
		return nil, ErrInvalidPlan.Detailf("digest: %v", err)
	}
	return canonicalJSON(raw, "digest")
}

// ComputeDigest returns the digest of the plan's current content, without
// modifying the plan.
//
// Two plans equal in content produce an identical digest in every process and
// on every run: map iteration order, time-zone offsets and JSON encoder
// escaping choices are all normalized away by the canonical form.
func (p *Plan) ComputeDigest() (string, error) {
	body, err := p.CanonicalBody()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// Seal computes the digest and stores it in the plan. The planner calls it once
// the graph is final; nothing may change afterwards.
func (p *Plan) Seal() error {
	d, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	p.Digest = d
	return nil
}

// VerifyDigest recomputes the digest and compares it with the recorded one
// (FR-PLANFILE-3, AC-2, T1). It returns an error matching [ErrDigestMismatch],
// which maps to exit code 10.
func (p *Plan) VerifyDigest() error {
	if p.Digest == "" {
		return ErrDigestMismatch.Detailf("plan %q carries no digest; it was never sealed", p.PlanID)
	}
	want, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	if want != p.Digest {
		return ErrDigestMismatch.Detailf(
			"plan %q: file records %s, contents hash to %s", p.PlanID, p.Digest, want)
	}
	return nil
}
