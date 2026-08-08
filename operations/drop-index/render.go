package dropindex

import (
	"fmt"

	"github.com/atulsinha/partitionctl/engine/protocol"
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

// renderDropConcurrently previews the online removal of one unattached orphan
// leaf index. Unattached is what makes it online: an attached child index is a
// dependency of its partitioned parent and DROP INDEX CONCURRENTLY is rejected
// on it.
func renderDropConcurrently(p *protocol.DropConcurrentlyParams) string {
	return "DROP INDEX CONCURRENTLY " + p.Index.Quoted() + ";"
}

// renderDropPartitioned previews the one statement the operation is gated on,
// with the lock it takes stated above it (FR-DROP-5).
func renderDropPartitioned(p *protocol.DropPartitionedParams) string {
	return fmt.Sprintf(
		"-- AccessExclusiveLock on %s and on all %d leaf partition(s), taken simultaneously (FR-DROP-5)\n"+
			"-- Not online: PostgreSQL rejects DROP INDEX CONCURRENTLY on a partitioned index (TRD §7.2.10)\n"+
			"DROP INDEX %s;",
		p.Parent.Quoted(), p.LeafCount, p.Index.Quoted())
}

// renderFinalVerifyComment previews the terminal verification.
func renderFinalVerifyComment(index protocol.ObjectName, children int) string {
	return fmt.Sprintf("-- verify %s absent from pg_index, and %d generated leaf index name(s) with it",
		index.Quoted(), children)
}
