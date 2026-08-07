package protocol

import (
	"strings"
	"unicode/utf8"
)

// MaxIdentifierBytes is PostgreSQL's NAMEDATALEN - 1: the longest identifier
// the server stores without silently truncating it.
const MaxIdentifierBytes = 63

// QuoteIdentifier renders s as a quoted PostgreSQL identifier (NFR-SEC-4).
//
// It always quotes, never conditionally: unconditional quoting preserves case,
// makes reserved words safe, and removes the "is this identifier bare-safe?"
// judgement from every call site. Inside double quotes PostgreSQL processes no
// backslash escapes, so a doubled `"` is the only escape required and no input
// can break out of the quotes.
//
// The one byte that is not representable is NUL. QuoteIdentifier is a pure
// transform and does not police its input; call [ValidateIdentifier] first, or
// rely on [Plan.Validate], which validates every identifier in every node
// before the plan is executable.
func QuoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// QuoteLiteral renders s as a single-quoted PostgreSQL string literal.
//
// Doubling the quote is the whole escape: PostgreSQL processes no backslash
// escapes inside a standard string literal when standard_conforming_strings is
// on, which it is by default on every version NFR-COMPAT-1 covers. It is used
// for index storage-parameter values and for the ownership marker text.
func QuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// QuoteQualifiedName renders a dotted, fully quoted name from its parts, for
// example ("public", "orders") -> "public"."orders". Empty parts are skipped,
// so an unqualified name is rendered bare-schema-less rather than as
// ""."orders".
func QuoteQualifiedName(parts ...string) string {
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		quoted = append(quoted, QuoteIdentifier(p))
	}
	return strings.Join(quoted, ".")
}

// QuoteMaybeQualified quotes a name that may be written as `schema.name` or as
// a bare `name`, producing `"schema"."name"` or `"name"`.
//
// It exists for the catalogs that are schema-qualified but are not relations:
// pg_opclass and pg_collation. An operator on managed PostgreSQL routinely
// needs `extensions.gin_trgm_ops`, because pg_trgm is installed into its own
// schema and is not on the search path. Quoting that whole string as one
// identifier produces `"extensions.gin_trgm_ops"`, which is a legal identifier
// that no opclass can ever be named, so the statement is syntactically valid
// and fails against the live server with 42704.
//
// A name with more than one dot is quoted whole rather than guessed at: the
// caller validated it, and inventing a split here would be worse than an
// honest failure.
func QuoteMaybeQualified(s string) string {
	i := strings.IndexByte(s, '.')
	if i <= 0 || i == len(s)-1 || strings.Count(s, ".") != 1 {
		return QuoteIdentifier(s)
	}
	return QuoteIdentifier(s[:i]) + "." + QuoteIdentifier(s[i+1:])
}

// ValidateMaybeQualified checks a name that may carry one schema qualifier.
func ValidateMaybeQualified(s string) error {
	if strings.Count(s, ".") == 0 {
		return ValidateIdentifier(s)
	}
	o, err := ParseObjectName(s)
	if err != nil {
		return err
	}
	return o.Validate()
}

// ValidateSimpleIdentifier is [ValidateIdentifier] plus a rejection of the dot.
//
// It is for names drawn from catalogs that have no namespace at all, where a
// qualified name is not merely unusual but unresolvable: pg_am is the case in
// point, so `extensions.gin` can never name an access method.
func ValidateSimpleIdentifier(s string) error {
	if err := ValidateIdentifier(s); err != nil {
		return err
	}
	if strings.Contains(s, ".") {
		return ErrInvalidIdentifier.Detailf(
			"%q is schema-qualified, but this name comes from a catalog with no namespace, "+
				"so it cannot resolve", s)
	}
	return nil
}

// ValidateIdentifier reports whether s can be a PostgreSQL identifier. It
// rejects the empty string, anything longer than [MaxIdentifierBytes] (which
// the server would silently truncate, breaking resume correlation), invalid
// UTF-8, and NUL bytes.
func ValidateIdentifier(s string) error {
	if s == "" {
		return ErrInvalidIdentifier.Detailf("identifier is empty")
	}
	if len(s) > MaxIdentifierBytes {
		return ErrInvalidIdentifier.Detailf(
			"identifier %q is %d bytes; PostgreSQL truncates above %d", s, len(s), MaxIdentifierBytes)
	}
	if !utf8.ValidString(s) {
		return ErrInvalidIdentifier.Detailf("identifier is not valid UTF-8")
	}
	if strings.IndexByte(s, 0) >= 0 {
		return ErrInvalidIdentifier.Detailf("identifier %q contains a NUL byte", s)
	}
	return nil
}

// validateSQLFragment checks an operator-authored SQL fragment (an index
// expression or a partial-index predicate) for well-formedness only.
//
// These fragments cannot be structured without a SQL parser, so they are not
// identifier-quoted. They are covered by the plan-file review step (G2): they
// appear verbatim in the artifact a human approves. See the package doc's trust
// boundary note.
func validateSQLFragment(field, s string) error {
	if s == "" {
		return ErrInvalidPlan.Detailf("%s is empty", field)
	}
	if !utf8.ValidString(s) {
		return ErrInvalidPlan.Detailf("%s is not valid UTF-8", field)
	}
	if strings.IndexByte(s, 0) >= 0 {
		return ErrInvalidPlan.Detailf("%s contains a NUL byte", field)
	}
	return nil
}
