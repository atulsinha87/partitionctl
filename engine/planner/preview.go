package planner

import (
	"github.com/atulsinha/partitionctl/engine/protocol"
)

// Preview fills in a node's [protocol.Node.RenderedSQL]: the NON-AUTHORITATIVE
// human preview a reviewer reads in the committed plan file (FR-PLANFILE-7).
// Nothing it returns is sent to a server.
//
// # Why every operation must route through this
//
// The DDL comes from [protocol.Preview], which is [protocol.Render] with a
// terminator — the same function the executor sends from — so an operation that
// uses this assembles no SQL of its own and its preview cannot drift from what
// runs (T2). An operation that hand-writes the string instead has a second
// implementation of every statement, and the tree had two of them: drop-index
// and reindex-index both carried private `renderDropConcurrently` functions
// whose header comment promised they were "kept byte-identical to
// engine/executor/render.go", a file that had since shrunk to a 32-line
// forward and contained no SQL to be identical to.
//
// # The marker statement, and why omitting it was the real bug
//
// The executor issues a COMMENT ON INDEX after the primary statement for every
// kind that writes an ownership marker ([protocol.NodeKind.ClaimsOwnership]),
// and that statement mutates catalog metadata the destructive-action table
// later reads as proof of ownership. reindex-index's hand-written preview
// showed only the REINDEX. A reviewer diffing a 12-partition reindex artifact —
// which is what the digest, the tamper guard and the whole review-then-execute
// story are built around — approved 12 statements while 12 further statements
// that rewrite provenance on production indexes were not in front of them.
//
// The run and plan fields read "pending" because neither exists yet: a plan is
// not bound to a run until `execute` opens one, and a digest cannot name
// itself. The executor substitutes the real values.
//
// # What the preview cannot know
//
// The prior marker on the object is a live catalog fact, so this passes
// [protocol.MarkerAbsent]. For a rewrite kind the executor may therefore emit a
// COMMENT whose *contents* differ from this preview — it preserves the creation
// facts already on the object — or, over a foreign comment, emit none at all.
// What the preview is answering is the question the reviewer is actually
// asking: does this node write to the catalog beyond its DDL, and to what
// object. That answer is exact.
func Preview(n protocol.Node, op protocol.Operation) protocol.Node {
	n.RenderedSQL = protocol.Preview(&n)
	stmt, ok, err := protocol.RenderMarkerStatement(&n, protocol.Marker{
		Run: "pending", Plan: "pending", Op: string(op), At: "pending",
	}, protocol.Marker{}, protocol.MarkerAbsent)
	if err != nil || !ok {
		return n
	}
	n.RenderedSQL += "\n" + stmt + ";"
	return n
}
