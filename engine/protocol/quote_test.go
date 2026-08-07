package protocol

import (
	"errors"
	"strings"
	"testing"
)

func TestQuoteIdentifier(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "orders", `"orders"`},
		{"mixed case preserved", "Orders", `"Orders"`},
		{"reserved word", "select", `"select"`},
		{"leading digit", "2026_orders", `"2026_orders"`},
		{"empty", "", `""`},
		{"embedded quote", `a"b`, `"a""b"`},
		{"leading quote", `"ab`, `"""ab"`},
		{"trailing quote", `ab"`, `"ab"""`},
		{"only a quote", `"`, `""""`},
		{"already doubled", `a""b`, `"a""""b"`},
		{"backslash is literal", `a\b`, `"a\b"`},
		{"backslash quote", `a\"b`, `"a\""b"`},
		{"dot is not a separator", "a.b", `"a.b"`},
		{"space", "a b", `"a b"`},
		{"newline", "a\nb", "\"a\nb\""},
		{"unicode", "commandé", `"commandé"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := QuoteIdentifier(tc.in); got != tc.want {
				t.Fatalf("QuoteIdentifier(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// T2: nothing an attacker can put in an identifier may escape the quotes.
func TestQuoteIdentifierResistsInjection(t *testing.T) {
	attacks := []string{
		`orders"; DROP TABLE users; --`,
		`orders"); DROP TABLE users; --`,
		`"; DROP TABLE users; --`,
		`orders" ON users; DROP INDEX "x`,
		`a" OR "1"="1`,
		`orders'; DROP TABLE users; --`,
		`orders/*comment*/`,
		`orders--comment`,
		`orders" WITH (fillfactor=1) ; SELECT "`,
		"orders\"\n; DROP TABLE users; --",
		`\"; DROP TABLE users; --`,
		`orders\`,
	}
	for _, attack := range attacks {
		got := QuoteIdentifier(attack)

		if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
			t.Fatalf("QuoteIdentifier(%q) = %s is not wrapped in quotes", attack, got)
		}
		// The body must contain no odd-length run of quotes, which is what an
		// escape out of the literal requires.
		body := got[1 : len(got)-1]
		i := 0
		for i < len(body) {
			if body[i] != '"' {
				i++
				continue
			}
			run := 0
			for i < len(body) && body[i] == '"' {
				run++
				i++
			}
			if run%2 != 0 {
				t.Fatalf("QuoteIdentifier(%q) = %s has an unbalanced quote run of %d", attack, got, run)
			}
		}
		// And the round trip must recover the original exactly.
		if back := strings.ReplaceAll(body, `""`, `"`); back != attack {
			t.Fatalf("QuoteIdentifier(%q) does not round-trip: got %q", attack, back)
		}
	}
}

func TestQuoteQualifiedName(t *testing.T) {
	tests := []struct {
		name  string
		parts []string
		want  string
	}{
		{"schema and name", []string{"public", "orders"}, `"public"."orders"`},
		{"empty schema skipped", []string{"", "orders"}, `"orders"`},
		{"empty name skipped", []string{"public", ""}, `"public"`},
		{"all empty", []string{"", ""}, ``},
		{"three parts", []string{"db", "public", "orders"}, `"db"."public"."orders"`},
		{"dot inside a part", []string{"pub.lic", "orders"}, `"pub.lic"."orders"`},
		{"injection in schema", []string{`p"; DROP SCHEMA x; --`, "orders"}, `"p""; DROP SCHEMA x; --"."orders"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := QuoteQualifiedName(tc.parts...); got != tc.want {
				t.Fatalf("QuoteQualifiedName(%q) = %s, want %s", tc.parts, got, tc.want)
			}
		})
	}
}

func TestValidateIdentifier(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"plain", "orders", false},
		{"at the limit", strings.Repeat("a", MaxIdentifierBytes), false},
		{"unicode within the limit", strings.Repeat("é", 31), false},
		{"embedded quote is legal", `a"b`, false},
		{"empty", "", true},
		{"one byte over", strings.Repeat("a", MaxIdentifierBytes+1), true},
		{"unicode over the byte limit", strings.Repeat("é", 32), true},
		{"nul byte", "a\x00b", true},
		{"invalid utf8", "a\xffb", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateIdentifier(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateIdentifier(%q) = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidIdentifier) {
				t.Fatalf("error %v is not ErrInvalidIdentifier", err)
			}
		})
	}
}

func TestObjectName(t *testing.T) {
	o := NewObjectName("public", "orders")
	if got := o.String(); got != "public.orders" {
		t.Errorf("String() = %q", got)
	}
	if got := o.Quoted(); got != `"public"."orders"` {
		t.Errorf("Quoted() = %s", got)
	}
	if o.IsZero() {
		t.Error("IsZero() on a populated name")
	}
	if !(ObjectName{}).IsZero() {
		t.Error("IsZero() false on the zero value")
	}

	bare := ObjectName{Name: "orders"}
	if got := bare.String(); got != "orders" {
		t.Errorf("String() = %q", got)
	}
	if got := bare.Quoted(); got != `"orders"` {
		t.Errorf("Quoted() = %s", got)
	}

	if err := (ObjectName{Name: ""}).Validate(); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("Validate() on an empty name: %v", err)
	}
	if err := (ObjectName{Schema: "a\x00b", Name: "orders"}).Validate(); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("Validate() on a NUL schema: %v", err)
	}
}

func TestParseObjectName(t *testing.T) {
	tests := []struct {
		in      string
		want    ObjectName
		wantErr bool
	}{
		{"orders", ObjectName{Name: "orders"}, false},
		{"public.orders", ObjectName{Schema: "public", Name: "orders"}, false},
		{"db.public.orders", ObjectName{}, true},
		{"", ObjectName{}, true},
		{".orders", ObjectName{}, true},
		{"public.", ObjectName{}, true},
	}
	for _, tc := range tests {
		got, err := ParseObjectName(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("ParseObjectName(%q) = (%v, %v), wantErr %v", tc.in, got, err, tc.wantErr)
		}
		if err == nil && got != tc.want {
			t.Fatalf("ParseObjectName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Plan.Validate is the backstop that keeps an unquotable identifier from ever
// reaching the executor, which is what lets QuoteIdentifier stay a pure
// transform.
func TestPlanValidateRejectsUnquotableIdentifiers(t *testing.T) {
	for _, bad := range []string{"a\x00b", strings.Repeat("a", 64), ""} {
		p := samplePlan(t)
		p.Nodes[3].Params.(*CreateConcurrentlyParams).Index.Name = bad
		if err := p.Validate(); err == nil {
			t.Fatalf("Validate accepted the identifier %q", bad)
		}
	}
}
