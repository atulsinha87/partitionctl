package reindexindex

import (
	"fmt"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// The renderers here produce Node.RenderedSQL: the preview a reviewer reads in
// the plan file. The executor re-renders from Params at dispatch and ignores
// these strings (FR-PLANFILE-7), so they are documentation, not instructions.
// They are kept byte-identical to engine/executor/render.go for the two kinds
// that issue DDL, and a comment is used for the kinds that issue none.

func renderReindexConcurrently(p *protocol.ReindexConcurrentlyParams) string {
	return "REINDEX INDEX CONCURRENTLY " + p.Index.Quoted() + ";"
}

func renderDropConcurrently(p *protocol.DropConcurrentlyParams) string {
	return "DROP INDEX CONCURRENTLY " + p.Index.Quoted() + ";"
}

func renderAssertComment(n int) string {
	return fmt.Sprintf(
		"-- catalog.assert: %d preconditions (relkind, strategy, depth, no DEFAULT partition, "+
			"role membership, index exists, index is partitioned, leaves attached)", n)
}

// renderLeafVerifyComment previews the per-leaf verification. Attachment is
// checked as well as validity because surviving the internal swap is the exact
// property the spike had to establish (FR-REIDX-6).
func renderLeafVerifyComment(child, parentIndex protocol.ObjectName) string {
	return "-- verify " + child.Quoted() + ": indisvalid AND indisready AND indislive, and still attached to " +
		parentIndex.Quoted()
}

func renderFinalVerifyComment(parentIndex protocol.ObjectName, leaves int) string {
	return fmt.Sprintf("-- verify %s: indisvalid, %d leaf index(es), and no _ccnew/_ccold left on any leaf",
		parentIndex.Quoted(), leaves)
}

func renderWaitComment(seconds int) string {
	return fmt.Sprintf("-- wait %ds: planner-emitted pacing between leaf index rebuilds", seconds)
}
