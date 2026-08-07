package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// Directive A.5.1's adopt-then-drop row, executed. The object exists because a
// statement ran and the process died before the COMMENT, so it carries no
// marker and is ours only by the claim the dead run still holds. The marker is
// written onto it first, which is what keeps the destruction auditable once the
// claim is gone.
func TestAdoptionMarksTheObjectBeforeDroppingIt(t *testing.T) {
	h := newHarness()
	h.cfg.AllowAdoption = true
	const index = "public.orders_created_at_idx_orders_2026_03"
	h.store.claim(obj(t, index), "run-crashed")

	if _, err := h.run(t, cleanupPlan(t, provenanceAuth(t, index))); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stmts := h.sql.statementsFor("nDrop")
	if len(stmts) != 2 {
		t.Fatalf("the drop node issued %d statements, want the marker and then the drop", len(stmts))
	}
	if !strings.HasPrefix(stmts[0].SQL, "COMMENT ON INDEX ") {
		t.Fatalf("the first statement is not the adoption marker:\n%s", stmts[0].SQL)
	}
	if !strings.HasPrefix(stmts[1].SQL, "DROP INDEX CONCURRENTLY ") {
		t.Fatalf("the second statement is not the drop:\n%s", stmts[1].SQL)
	}
	m, status := parseMarkerLiteral(t, stmts[0].SQL)
	if status != protocol.MarkerOurs {
		t.Fatalf("the adoption wrote a marker this binary cannot read back: %v", status)
	}
	if m.Run != "run-1" {
		t.Errorf("the adoption marker names run %q, want the adopting run", m.Run)
	}

	// The audit trail records which of the two witnesses authorized it.
	if len(h.store.authz) != 1 {
		t.Fatalf("recorded %d authorizations, want one", len(h.store.authz))
	}
	if got := h.store.authz[0].Evidence["source"]; got != "claim" {
		t.Errorf("evidence source = %q, want \"claim\"", got)
	}
}

// FR-CLI-9: adoption is `resume`'s alone. `execute` finds the same live state
// and refuses, naming the command that may proceed.
func TestAdoptionIsRefusedWhenTheCallerIsNotResume(t *testing.T) {
	h := newHarness() // AllowAdoption defaults to false
	const index = "public.orders_created_at_idx_orders_2026_03"
	h.store.claim(obj(t, index), "run-crashed")

	_, err := h.run(t, cleanupPlan(t, provenanceAuth(t, index)))
	if !errors.Is(err, protocol.ErrAuthorizationUnsatisfied) {
		t.Fatalf("error = %v, want ErrAuthorizationUnsatisfied", err)
	}
	if !strings.Contains(err.Error(), "resume") {
		t.Errorf("the refusal does not name the command that may proceed: %v", err)
	}
	if h.sql.execCount() != 0 {
		t.Fatal("a statement ran after adoption was refused")
	}
	if h.sql.markCount() != 0 {
		t.Fatal("the executor stamped its marker onto an object it was not allowed to adopt")
	}
	if len(h.store.authz) != 0 {
		t.Fatal("an authorization was recorded for a refused adoption (INV-2)")
	}
}

// A marker-authorized drop is a catalog fact, so it needs no adoption and
// `execute` may perform it. Refusing it was the deadlock in the old design: a
// re-plan after a crash produced a node `execute` refused and `resume` could
// not reach, because the digest had changed.
func TestAMarkedObjectNeedsNoAdoption(t *testing.T) {
	h := newHarness() // AllowAdoption false, as under `execute`
	const index = "public.orders_created_at_idx_orders_2026_03"
	h.catalog.mark(t, obj(t, index), "run-earlier")

	if _, err := h.run(t, cleanupPlan(t, provenanceAuth(t, index))); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stmts := h.sql.statementsFor("nDrop")
	if len(stmts) != 1 || !strings.HasPrefix(stmts[0].SQL, "DROP INDEX CONCURRENTLY ") {
		t.Fatalf("the drop node issued %d statements: %+v", len(stmts), stmts)
	}
}

// FR-PLAN-5 for reindex, and FR-LB-4's gate: the rebuild is recorded on the
// object, so "was this leaf already reindexed?" is a catalog question. The
// creation facts survive, because a reindex must not erase who built the index.
func TestReindexStampsTheMarkerWithoutErasingItsHistory(t *testing.T) {
	h := newHarness()
	leaf := obj(t, "public.orders_created_at_idx_orders_2026_03")
	original, err := protocol.FormatMarker(protocol.Marker{
		Run: "run-original", Plan: "sha256:original", Op: string(protocol.OpCreateIndex),
		Role: protocol.MarkerRoleLeaf, Parent: "public.orders_created_at_idx",
		At: "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("FormatMarker: %v", err)
	}
	h.catalog.setComment(leaf, original)

	plan := newPlan(t,
		node("n1", protocol.KindIndexReindexConcurrently, &protocol.ReindexConcurrentlyParams{
			Index:       leaf,
			Relation:    objPtr(t, "public.orders_2026_03"),
			ParentIndex: objPtr(t, "public.orders_created_at_idx"),
		}),
	)
	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stmts := h.sql.statementsFor("n1")
	if len(stmts) != 2 {
		t.Fatalf("issued %d statements, want the REINDEX and its marker rewrite", len(stmts))
	}
	m, status := parseMarkerLiteral(t, stmts[1].SQL)
	if status != protocol.MarkerOurs {
		t.Fatalf("status = %v", status)
	}
	if m.Run != "run-original" || m.At != "2026-01-01T00:00:00Z" || m.Op != string(protocol.OpCreateIndex) {
		t.Errorf("the reindex erased who built the index: %+v", m)
	}
	if m.ReindexRun != "run-1" || m.Reindexed == "" {
		t.Errorf("the reindex was not recorded: %+v", m)
	}
}

// Never overwrite a human's comment. The rebuild already succeeded and the
// index is healthy; the leaf simply reads as un-reindexed on the next plan and
// is rebuilt again, which is wasteful, safe and convergent.
func TestReindexDoesNotOverwriteAForeignComment(t *testing.T) {
	h := newHarness()
	leaf := obj(t, "public.orders_created_at_idx_orders_2026_03")
	h.catalog.setComment(leaf, "quarterly reporting index, owned by analytics")

	plan := newPlan(t,
		node("n1", protocol.KindIndexReindexConcurrently, &protocol.ReindexConcurrentlyParams{
			Index: leaf, Relation: objPtr(t, "public.orders_2026_03"),
		}),
	)
	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stmts := h.sql.statementsFor("n1")
	if len(stmts) != 1 {
		t.Fatalf("issued %d statements; the rewrite must be skipped, not forced:\n%+v", len(stmts), stmts)
	}
}

// A plan that reindexes needs the catalog port, because the rewrite reads the
// marker already on the object. Refusing up front beats discovering it halfway
// through a 400-partition rebuild.
func TestReindexNeedsTheCatalogPort(t *testing.T) {
	h := newHarness()
	h.cfg.Catalog = nil
	plan := newPlan(t,
		node("n1", protocol.KindIndexReindexConcurrently, &protocol.ReindexConcurrentlyParams{
			Index: obj(t, "public.orders_idx_p1"),
		}),
	)
	if _, err := h.run(t, plan); !errors.Is(err, ErrMissingPort) {
		t.Fatalf("error = %v, want ErrMissingPort", err)
	}
	if h.sql.execCount() != 0 {
		t.Fatal("a statement ran before the missing port was noticed")
	}
}

// A dry run touches nothing, so it needs no ports at all — including the
// catalog the marker rewrite would otherwise require. This is the shape of the
// bug the M1 live run found: preflight demanded a port the dry run had
// correctly not been given.
func TestDryRunNeedsNoCatalogForTheNewKinds(t *testing.T) {
	e, err := New(Config{DryRun: true, LockTimeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plan := newPlan(t,
		node("n1", protocol.KindIndexReindexConcurrently, &protocol.ReindexConcurrentlyParams{
			Index: obj(t, "public.orders_idx_p1"),
		}),
	)
	if _, err := e.Run(context.Background(), "dry-run", plan); err != nil {
		t.Fatalf("dry run: %v", err)
	}
}
