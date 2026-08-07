package protocol

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
)

// maxCanonicalDepth bounds recursion while canonicalizing. Plan files come from
// disk and may be hostile; the deepest legitimate plan nests about six levels.
const maxCanonicalDepth = 100

// canonicalJSON rewrites JSON into PartitionCTL's canonical form. The rules are
// specified in the package doc; the properties that matter are that the output
// depends only on the *content* of the value, and that it is byte-identical in
// every process and on every run.
//
// Keys named in dropTopLevel are removed from the top-level object first. That
// is how the digest excludes itself (FR-PLANFILE-2).
func canonicalJSON(data []byte, dropTopLevel ...string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	// UseNumber keeps numeric literals as text, so nothing is silently
	// widened to float64 on the way through.
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, ErrInvalidPlan.Detailf("canonicalize: %v", err)
	}
	if dec.More() {
		return nil, ErrInvalidPlan.Detailf("canonicalize: trailing content after the top-level JSON value")
	}
	if len(dropTopLevel) > 0 {
		if obj, ok := v.(map[string]any); ok {
			for _, k := range dropTopLevel {
				delete(obj, k)
			}
		}
	}

	var buf bytes.Buffer
	buf.Grow(len(data))
	if err := writeCanonicalValue(&buf, v, 0); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// canonicalOf marshals v and canonicalizes the result.
func canonicalOf(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, ErrInvalidPlan.Detailf("canonicalize: %v", err)
	}
	return canonicalJSON(raw)
}

func writeCanonicalValue(buf *bytes.Buffer, v any, depth int) error {
	if depth > maxCanonicalDepth {
		return ErrInvalidPlan.Detailf("canonicalize: JSON nested deeper than %d levels", maxCanonicalDepth)
	}
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		return writeCanonicalNumber(buf, x)
	case string:
		writeCanonicalString(buf, x)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalValue(buf, e, depth+1); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// Ascending by UTF-8 byte sequence. This is what makes map iteration
		// order irrelevant to the digest.
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonicalValue(buf, x[k], depth+1); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return ErrInvalidPlan.Detailf("canonicalize: unsupported JSON value of type %T", v)
	}
	return nil
}

// writeCanonicalNumber normalizes a numeric literal. Integers are emitted in
// shortest decimal form, so "-0", "0" and "0e0" all become "0". Non-integers
// use Go's shortest round-trip float formatting, which is deterministic.
func writeCanonicalNumber(buf *bytes.Buffer, n json.Number) error {
	s := n.String()
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		buf.WriteString(strconv.FormatInt(i, 10))
		return nil
	}
	if u, err := strconv.ParseUint(s, 10, 64); err == nil {
		buf.WriteString(strconv.FormatUint(u, 10))
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return ErrInvalidPlan.Detailf("canonicalize: unrepresentable number %q", s)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return ErrInvalidPlan.Detailf("canonicalize: number %q is not finite", s)
	}
	// An integral float normalizes to the same text as the equivalent integer.
	if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		buf.WriteString(strconv.FormatInt(int64(f), 10))
		return nil
	}
	buf.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	return nil
}

// findDuplicateKey reports the first key that appears twice in the same JSON
// object, scanning the raw bytes.
//
// This is the one edit the digest cannot catch. Go's decoder takes last-wins on
// duplicate keys, and so does the canonicalizer, so both sides of
// [Plan.VerifyDigest] agree and the plan verifies — while a human reviewing the
// artifact may well read the *first* occurrence. That is exactly the
// review-versus-execute divergence T1 exists to prevent, so [DecodePlan]
// refuses such a file outright.
func findDuplicateKey(data []byte) (string, bool) {
	type frame struct {
		isObject bool
		seen     map[string]struct{}
		wantKey  bool
	}
	var stack []*frame

	// valueDone marks that a value completed in the enclosing object, so the
	// next scalar there is a key again.
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].isObject {
			stack[n-1].wantKey = true
		}
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err != nil {
			// Malformed or exhausted input: json.Unmarshal reports the real
			// error, this scan simply stops.
			return "", false
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, &frame{isObject: true, seen: map[string]struct{}{}, wantKey: true})
			case '[':
				stack = append(stack, &frame{})
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				valueDone()
			}
			continue
		}
		n := len(stack)
		if n > 0 && stack[n-1].isObject && stack[n-1].wantKey {
			key, _ := tok.(string)
			if _, dup := stack[n-1].seen[key]; dup {
				return key, true
			}
			stack[n-1].seen[key] = struct{}{}
			stack[n-1].wantKey = false
			continue
		}
		valueDone()
	}
}

const hexDigits = "0123456789abcdef"

// writeCanonicalString escapes only what JSON requires: `"`, `\` and the C0
// controls. Every other rune is emitted literally as UTF-8, so `<`, `&`,
// U+2028 and emoji all round-trip byte-for-byte instead of depending on any
// encoder's HTML-escaping mood. Invalid UTF-8 normalizes to U+FFFD.
func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				buf.WriteString(`\u00`)
				buf.WriteByte(hexDigits[(r>>4)&0xf])
				buf.WriteByte(hexDigits[r&0xf])
				continue
			}
			// WriteRune emits U+FFFD for an invalid rune, which is exactly the
			// deterministic normalization we want.
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}
