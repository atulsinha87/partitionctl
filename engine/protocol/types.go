package protocol

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// ObjectName is a schema-qualified PostgreSQL object name. It is the only way
// an identifier enters the plan; nothing in the format carries a pre-joined
// dotted string.
type ObjectName struct {
	// Schema is the containing schema. Empty means unqualified, which is
	// legal in a specification but should be resolved by the planner before it
	// reaches a plan.
	Schema string `json:"schema,omitempty"`
	// Name is the object's own name.
	Name string `json:"name"`
}

// NewObjectName is a convenience constructor.
func NewObjectName(schema, name string) ObjectName {
	return ObjectName{Schema: schema, Name: name}
}

// String renders the *identity* form, schema.name, unquoted. Use it for log
// fields, audit rows and provenance keys. Never interpolate it into SQL: use
// [ObjectName.Quoted].
func (o ObjectName) String() string {
	if o.Schema == "" {
		return o.Name
	}
	return o.Schema + "." + o.Name
}

// Quoted renders the SQL form, fully double-quoted (NFR-SEC-4).
func (o ObjectName) Quoted() string { return QuoteQualifiedName(o.Schema, o.Name) }

// IsZero reports whether the name is entirely unset.
func (o ObjectName) IsZero() bool { return o.Schema == "" && o.Name == "" }

// Validate checks both parts against [ValidateIdentifier]. Schema may be empty;
// Name may not.
func (o ObjectName) Validate() error {
	if o.Schema != "" {
		if err := ValidateIdentifier(o.Schema); err != nil {
			return err
		}
	}
	return ValidateIdentifier(o.Name)
}

// ParseObjectName parses "schema.name" or "name". It is a convenience for CLI
// flag parsing, deliberately strict: it does not accept quoted identifiers, and
// more than one dot is an error rather than a guess.
func ParseObjectName(s string) (ObjectName, error) {
	switch n := strings.Count(s, "."); n {
	case 0:
		o := ObjectName{Name: s}
		return o, o.Validate()
	case 1:
		i := strings.IndexByte(s, '.')
		o := ObjectName{Schema: s[:i], Name: s[i+1:]}
		// A dot was written, so both halves were meant. An empty one is a typo,
		// not an unqualified name.
		if o.Schema == "" {
			return ObjectName{}, ErrInvalidIdentifier.Detailf("%q has an empty schema", s)
		}
		return o, o.Validate()
	default:
		return ObjectName{}, ErrInvalidIdentifier.Detailf(
			"%q has %d dots; expected \"schema.name\" or \"name\"", s, n)
	}
}

// Timestamp is a time that serializes canonically: always UTC, always RFC 3339
// with trailing fractional zeros trimmed.
//
// This normalization is load-bearing. Two Timestamps for the same instant in
// different zones produce identical JSON and therefore identical plan digests
// (FR-PLANFILE-2). Compare instants with ts.Time.Equal, not with ==, because
// == also compares the location pointer.
type Timestamp struct {
	time.Time
}

// NewTimestamp wraps t.
func NewTimestamp(t time.Time) Timestamp { return Timestamp{Time: t} }

// Now returns the current time as a Timestamp.
func Now() Timestamp { return Timestamp{Time: time.Now()} }

// Canonical returns the normalized textual form: UTC, RFC 3339, trailing
// fractional zeros trimmed.
func (ts Timestamp) Canonical() string {
	return ts.Time.UTC().Format(time.RFC3339Nano)
}

// String returns the canonical form.
func (ts Timestamp) String() string { return ts.Canonical() }

// MarshalJSON emits the canonical form as a JSON string.
func (ts Timestamp) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(ts.Canonical())), nil
}

// UnmarshalJSON accepts any RFC 3339 timestamp, in any offset, and normalizes
// it to UTC. JSON null decodes to the zero Timestamp.
func (ts *Timestamp) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		ts.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return ErrInvalidPlan.Detailf("timestamp is not a JSON string: %v", err)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return ErrInvalidPlan.Detailf("timestamp %q is not RFC 3339: %v", s, err)
	}
	ts.Time = t.UTC()
	return nil
}
