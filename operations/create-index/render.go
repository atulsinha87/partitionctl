package createindex

import (
	"fmt"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// This file produces [protocol.Node.RenderedSQL]: a NON-AUTHORITATIVE human
// preview for the reviewer of the plan file (FR-PLANFILE-7). Nothing here is
// sent to a server.
//
// The DDL itself comes from [protocol.Preview], which is [protocol.Render] with
// a terminator — the same function the executor sends from. This operation
// therefore assembles no SQL of its own, which is what makes the preview a
// preview rather than a second implementation that could drift from what runs
// (T2). The only thing added on top is the ownership marker, which the executor
// issues after the statement and which the artifact would otherwise not show.

// preview fills in a node's RenderedSQL from its own typed parameters,
// including the ownership-marker statement the executor issues after it (G2,
// FR-PLANFILE-7).
//
// The run and plan fields of the previewed marker read "pending" because
// neither exists yet: a plan is not bound to a run until `execute` opens one,
// and the digest cannot name itself. The executor substitutes the real values.
func preview(n protocol.Node) protocol.Node {
	n.RenderedSQL = protocol.Preview(&n)
	stmt, ok, err := protocol.RenderMarkerStatement(&n, protocol.Marker{
		Run: "pending", Plan: "pending", Op: string(protocol.OpCreateIndex), At: "pending",
	}, protocol.Marker{}, protocol.MarkerAbsent)
	if err != nil || !ok {
		return n
	}
	n.RenderedSQL += "\n" + stmt + ";"
	return n
}

// renderAssertComment previews the precondition node.
func renderAssertComment(n int) string {
	return fmt.Sprintf("-- catalog.assert: %d precondition(s) checked before any DDL is issued", n)
}

// renderLeafVerifyComment previews the pre-attach check on one leaf index.
func renderLeafVerifyComment(child protocol.ObjectName) string {
	return "-- verify " + child.Quoted() + " is valid, ready and live before attaching it"
}

// renderFinalVerifyComment previews the terminal verification.
func renderFinalVerifyComment(parentIndex protocol.ObjectName, leaves int) string {
	return fmt.Sprintf("-- verify %s is valid and has %d attached leaf index(es)",
		parentIndex.Quoted(), leaves)
}

// renderWaitComment previews a pacing node.
func renderWaitComment(seconds int) string {
	if seconds <= 0 {
		return "-- no pause"
	}
	return fmt.Sprintf("-- pause %ds before the next partition (FR-ORD-3)", seconds)
}
