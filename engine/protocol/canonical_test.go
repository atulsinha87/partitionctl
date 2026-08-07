package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCanonicalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"null", `null`, `null`},
		{"true", `true`, `true`},
		{"false", `false`, `false`},
		{"empty object", `{}`, `{}`},
		{"empty array", `[]`, `[]`},
		{"whitespace stripped", "{\n  \"a\" : 1 ,\n  \"b\": 2\n}", `{"a":1,"b":2}`},
		{"keys sorted", `{"b":1,"a":2,"C":3,"_":4}`, `{"C":3,"_":4,"a":2,"b":1}`},
		{"nested keys sorted", `{"z":{"y":1,"x":2}}`, `{"z":{"x":2,"y":1}}`},
		{"array order preserved", `[3,1,2]`, `[3,1,2]`},

		// Numbers normalize to a single spelling.
		{"integer", `1`, `1`},
		{"negative zero", `-0`, `0`},
		{"leading zero exponent", `0e0`, `0`},
		{"integral float", `1.0`, `1`},
		{"exponent integer", `1e3`, `1000`},
		{"large int64", `9223372036854775807`, `9223372036854775807`},
		{"beyond int64", `18446744073709551615`, `18446744073709551615`},
		{"fraction", `0.1`, `0.1`},
		{"fraction padded", `0.10`, `0.1`},

		// Strings escape only what JSON requires.
		{"plain string", `"abc"`, `"abc"`},
		{"quote", `"a\"b"`, `"a\"b"`},
		{"backslash", `"a\\b"`, `"a\\b"`},
		{"newline", `"a\nb"`, `"a\nb"`},
		{"tab", `"a\tb"`, `"a\tb"`},
		{"carriage return", `"a\rb"`, `"a\rb"`},
		{"backspace", `"a\bb"`, `"a\bb"`},
		{"form feed", `"a\fb"`, `"a\fb"`},
		{"other control uses \\u00xx", "\"a\\u0001b\"", `"a\u0001b"`},
		{"control escape is lowercased", "\"\\u001F\"", `"\u001f"`},
		{"escaped ascii is unescaped", "\"\\u0041\"", `"A"`},
		{"html chars stay literal", `"<script>&"`, `"<script>&"`},
		{"line separator stays literal", "\"\\u2028\"", "\"\u2028\""},
		{"astral plane", `"🐘"`, `"🐘"`},
		{"del is not escaped", "\"\\u007f\"", "\"\u007f\""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonicalJSON([]byte(tc.in))
			if err != nil {
				t.Fatalf("canonicalJSON(%s): %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Fatalf("canonicalJSON(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// Canonicalization must be idempotent: the canonical form of a canonical form
// is itself. Without this, a value's digest could depend on how many times it
// had been through the pipeline.
func TestCanonicalJSONIsIdempotent(t *testing.T) {
	inputs := []string{
		`{"b":1,"a":[1,2,{"z":null,"y":true}]}`,
		`{"s":"tab\tquote\"emoji🐘","n":1.0,"neg":-0}`,
		`[[[[[]]]]]`,
	}
	for _, in := range inputs {
		once, err := canonicalJSON([]byte(in))
		if err != nil {
			t.Fatalf("canonicalJSON(%s): %v", in, err)
		}
		twice, err := canonicalJSON(once)
		if err != nil {
			t.Fatalf("canonicalJSON(%s): %v", once, err)
		}
		if string(once) != string(twice) {
			t.Fatalf("not idempotent: %s -> %s -> %s", in, once, twice)
		}
	}
}

func TestCanonicalJSONDropsTopLevelKeys(t *testing.T) {
	got, err := canonicalJSON([]byte(`{"a":1,"digest":"x","b":2}`), "digest")
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	if string(got) != `{"a":1,"b":2}` {
		t.Fatalf("got %s", got)
	}

	// Only the top level: a nested key of the same name survives.
	got, err = canonicalJSON([]byte(`{"n":{"digest":"x"}}`), "digest")
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	if string(got) != `{"n":{"digest":"x"}}` {
		t.Fatalf("got %s", got)
	}
}

func TestCanonicalJSONRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"malformed", `{`},
		{"trailing content", `{} {}`},
		{"trailing garbage", `1 2`},
		{"deep nesting", strings.Repeat("[", maxCanonicalDepth+5) + strings.Repeat("]", maxCanonicalDepth+5)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := canonicalJSON([]byte(tc.in)); err == nil {
				t.Fatalf("canonicalJSON(%q) accepted bad input", tc.in)
			}
		})
	}
}

// Invalid UTF-8 must normalize rather than pass through, so that two byte
// sequences that mean the same thing hash the same.
func TestCanonicalStringNormalizesInvalidUTF8(t *testing.T) {
	got := canonicalString("bad \xff bytes")
	want := "\"bad � bytes\""
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if canonicalString("bad � bytes") != got {
		t.Fatal("invalid bytes and an explicit U+FFFD did not converge")
	}
}

func canonicalString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	out, err := canonicalJSON(b)
	if err != nil {
		panic(err)
	}
	return string(out)
}

func TestFindDuplicateKey(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantKey string
		wantDup bool
	}{
		{"clean object", `{"a":1,"b":2}`, "", false},
		{"same key different objects", `{"n":{"a":1},"m":{"a":2}}`, "", false},
		{"same key in array elements", `[{"a":1},{"a":2}]`, "", false},
		{"string value equal to a key", `{"a":"a","b":"a"}`, "", false},
		{"top level duplicate", `{"a":1,"a":2}`, "a", true},
		{"nested duplicate", `{"n":{"a":1,"a":2}}`, "a", true},
		{"duplicate after nested object", `{"a":{"x":1},"b":2,"a":3}`, "a", true},
		{"duplicate after array", `{"a":[1,2],"a":3}`, "a", true},
		{"duplicate deep in array of objects", `{"n":[{"x":{"y":1,"y":2}}]}`, "y", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, dup := findDuplicateKey([]byte(tc.in))
			if dup != tc.wantDup || key != tc.wantKey {
				t.Fatalf("findDuplicateKey(%s) = (%q, %v), want (%q, %v)", tc.in, key, dup, tc.wantKey, tc.wantDup)
			}
		})
	}
}
