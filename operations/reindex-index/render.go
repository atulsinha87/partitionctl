package reindexindex

import (
	"fmt"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// The renderers here produce Node.RenderedSQL: the preview a reviewer reads in
// the plan file. The executor re-renders from Params at dispatch and ignores
// these strings (FR-PLANFILE-7), so they are documentation, not instructions.
//
// The kinds that issue DDL do not appear here at all. They go through
// [planner.Preview], which renders from the same function the executor sends
// from and appends the ownership-marker COMMENT the executor issues after it.
// Hand-written copies used to live here and claimed to be "kept byte-identical
// to engine/executor/render.go"; that file no longer holds any SQL, and the
// copy for index.reindex_concurrently silently dropped the marker statement, so
// a reviewer approving a 12-partition reindex never saw the 12 COMMENT
// statements that rewrite provenance on production indexes. The kinds that
// issue no DDL keep their comment previews below.

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
