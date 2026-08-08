package createindex

import (
	"fmt"

	"github.com/atulsinha/partitionctl/engine/planner"
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

// preview delegates to [planner.Preview], which every operation shares so that
// no operation can assemble SQL of its own or forget the marker statement the
// executor issues after the DDL (FR-PLANFILE-7, G2).
func preview(n protocol.Node) protocol.Node {
	return planner.Preview(n, protocol.OpCreateIndex)
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
