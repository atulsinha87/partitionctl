// Package protocol defines the PartitionCTL plan format: the contract every
// other package compiles against.
//
// It owns four things and nothing else:
//
//  1. The plan artifact ([Plan], [Node], [NodeParams]) and its versioning
//     ([PlanFormatVersion], [CheckFormatVersion]) — FR-PLANFILE-1, FR-PLANFILE-8,
//     NFR-COMPAT-3.
//  2. The two integrity mechanisms that guard the gap between `plan` and
//     `execute`: the plan digest ([Plan.ComputeDigest], FR-PLANFILE-2) and the
//     topology fingerprint ([TopologyInput.Fingerprint], FR-PLANFILE-4).
//  3. The node vocabulary ([NodeKind]), the destructive-action authorization
//     modes ([AuthorizationMode]) and the node lifecycle ([NodeState]) — TRD
//     §7.2.2, §7.2.9, §10.2.
//  4. The pure, database-free helpers that must be identical everywhere:
//     identifier quoting ([QuoteIdentifier], NFR-SEC-4) and deterministic child
//     index naming ([ChildIndexName], FR-PLAN-11, FR-PLAN-13).
//
// The package imports only the standard library, opens no connections, and
// contains no planning, dispatch, retry or state logic.
//
// # The node vocabulary is sealed
//
// [NodeParams] has an unexported method, so the nine kinds in [AllNodeKinds]
// are the complete set. This is deliberate: TRD §7.2.2 makes the executor's
// node vocabulary "a deliberate, versioned engine contract", and adding a kind
// requires a [PlanFormatVersion] bump. Construct params with the concrete types
// or with [NewParams]; do not try to implement [NodeParams] from another
// package.
//
// # Canonical form
//
// The digest and the fingerprint are both SHA-256 over a *canonical*
// serialization, so that two structurally equal values hash identically in
// every process and on every run. The canonical form is JSON with these
// additional rules:
//
//   - Object keys are sorted ascending by their UTF-8 byte sequence.
//   - No insignificant whitespace anywhere.
//   - Integers are emitted in shortest decimal form; -0 normalizes to 0.
//     Non-integers use Go's shortest round-trip float formatting. NaN and
//     infinity are rejected. (The schema itself contains no float fields.)
//   - Strings escape only `"`, `\` and the C0 controls, using the short forms
//     \b \f \n \r \t where they exist and \u00xx otherwise. Every other rune is
//     emitted literally as UTF-8. Invalid UTF-8 normalizes to U+FFFD.
//   - Timestamps are [Timestamp] values, always normalized to UTC and formatted
//     RFC 3339 with trailing fractional zeros trimmed, so the same instant in
//     two zones produces the same bytes.
//
// # Trust boundary
//
// Structured node parameters are the authoritative execution input
// (FR-PLANFILE-6). [Node.RenderedSQL] is a non-authoritative human preview: the
// executor MUST ignore it and re-render from [Node.Params] (FR-PLANFILE-7, T2).
// Identifiers reach SQL only through [QuoteIdentifier] / [QuoteQualifiedName].
// The two exceptions are [IndexColumn.Expression] and [IndexDefinition.Where],
// which are operator-authored SQL fragments that cannot be structured without a
// SQL parser; they are checked for well-formedness only, and are covered by the
// plan-file review step (G2) rather than by quoting.
package protocol
