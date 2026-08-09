package executor

import (
	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// Render produces the statement the executor will send for a node.
//
// It is [protocol.Render] and nothing else. The renderer lives in
// engine/protocol so that the executor, the three operation planners and
// `render` all emit the same bytes from the same code: rendered_sql in the
// reviewed artifact is a preview of *this* function's output, and a separate
// implementation on either side would let the artifact and the run disagree
// (FR-PLANFILE-7, T2).
//
// [protocol.Node.RenderedSQL] is never read. Callers should gate on
// [protocol.NodeKind.IssuesDDL]: the three kinds that issue no DDL render as
// the empty string with a nil error.
func Render(n *protocol.Node) (string, error) { return protocol.Render(n) }

// paramsOf recovers a node's concrete params type. [protocol.Node.Validate]
// already guarantees Params.Kind() == Kind, so a failure here means the plan
// bypassed validation.
func paramsOf[T protocol.NodeParams](n *protocol.Node) (T, error) {
	p, ok := n.Params.(T)
	if !ok {
		var zero T
		return zero, protocol.ErrInvalidPlan.Detailf(
			"node %q of kind %q carries params of type %T", n.ID, n.Kind, n.Params)
	}
	return p, nil
}
