package dropindex

import (
	"fmt"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// The previews written into Node.RenderedSQL.
//
// They are for the human reading the committed plan file, not for the database:
// the executor renders the statement it actually issues from the node's typed
// parameters, so nothing here can change what runs. That separation is why a
// preview is allowed to carry a comment the real statement does not, which is
// how FR-DROP-5 gets its blast-radius line into rendered_sql.

// renderAssertComment previews the precondition node.
func renderAssertComment(n int) string {
	return fmt.Sprintf(
		"-- catalog.assert: %d preconditions (relkind, role membership, index exists, "+
			"index is partitioned, index is not constraint-backed)", n)
}

// dropPartitionedPreamble is the FR-DROP-5 blast-radius disclosure that sits
// above the one statement this operation is gated on. The statement itself is
// rendered by [planner.Preview] from the node's params, so only the comment is
// assembled here.
//
// The wording is measured, not inferred. PostgreSQL acquires the
// AccessExclusiveLocks one relation at a time and holds each one while it waits
// for the next, so blocking starts at the first acquisition and continues
// through every subsequent wait — including on the path where the statement
// ultimately aborts having changed nothing. Observed on PG 17.10: with a
// session holding AccessShareLock on the LAST partition, pg_locks shows the
// dropping backend already granted AccessExclusiveLock on the parent and every
// earlier partition while it is still merely waiting, and reads against those
// earlier partitions time out.
//
// lock_timeout bounds each acquisition attempt separately, not the statement,
// so the worst case per attempt is about (leafCount + 1) x lock_timeout of
// escalating tree-wide blocking. The texts used to say the locks were "taken
// simultaneously" and held "until it commits", which reads as a single bounded
// stall that either succeeds or vanishes.
func dropPartitionedPreamble(p *protocol.DropPartitionedParams) string {
	return fmt.Sprintf(
		"-- AccessExclusiveLock on %s and on all %d leaf partition(s), acquired one relation at a\n"+
			"-- time and held cumulatively: blocking begins at the first acquisition and continues\n"+
			"-- through every later wait, including if the statement aborts (FR-DROP-5).\n"+
			"-- lock_timeout bounds each acquisition separately, so the worst case per attempt is\n"+
			"-- about %d x lock_timeout, and each retry repeats the whole escalation.\n"+
			"-- Not online: PostgreSQL rejects DROP INDEX CONCURRENTLY on a partitioned index (TRD §7.2.10)",
		p.Parent.Quoted(), p.LeafCount, p.LeafCount+1)
}

// renderFinalVerifyComment previews the terminal verification.
func renderFinalVerifyComment(index protocol.ObjectName, children int) string {
	return fmt.Sprintf("-- verify %s absent from pg_index, and %d generated leaf index name(s) with it",
		index.Quoted(), children)
}
