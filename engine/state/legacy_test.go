package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// writeLegacyProv lays down the artefact a schema-version-1 file store wrote:
// runs/<run>/prov/<n>.json, with the object it proved was created.
func writeLegacyProv(t *testing.T, dir, run, database string, object protocol.ObjectName) {
	t.Helper()
	provDir := filepath.Join(dir, "runs", run, "prov")
	if err := os.MkdirAll(provDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	rec := map[string]any{
		"provenance_id": run + ":prov:00000001",
		"run_id":        run,
		"database":      database,
		"object":        object,
		"object_kind":   "index",
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(provDir, "00000001.json"), b, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// The wreckage a version-1 run left behind carries no marker and no claim, so
// resume halts on it. The record naming it is sitting in the run directory
// resume just read, and the operator has to be told that rather than sent to
// fix by hand something the tool can see.
func TestLegacyProvenanceRunFindsAVersion1Record(t *testing.T) {
	dir := t.TempDir()
	object := protocol.NewObjectName("public", "orders_resume_idx_orders_2026_01")
	writeLegacyProv(t, dir, "run-20260101T000000Z-aaaa", "appdb", object)

	got, ok := LegacyProvenanceRun(dir, "appdb", object)
	if !ok {
		t.Fatal("a version-1 provenance record naming the index was not found")
	}
	if got != "run-20260101T000000Z-aaaa" {
		t.Errorf("run = %q, want the run that wrote the record", got)
	}
}

// Scoped by database for the same reason claims are: a file store holds state
// for several targets, and a record from another database says nothing here.
func TestLegacyProvenanceRunIsScopedByDatabase(t *testing.T) {
	dir := t.TempDir()
	object := protocol.NewObjectName("public", "orders_idx_p1")
	writeLegacyProv(t, dir, "run-other", "otherdb", object)

	if _, ok := LegacyProvenanceRun(dir, "appdb", object); ok {
		t.Error("a record from another database was reported as relevant")
	}
	if _, ok := LegacyProvenanceRun(dir, "otherdb", object); !ok {
		t.Error("the record was not found for its own database")
	}
}

// Every way of having no answer is silence, never an error: this runs on the
// halt path, where a failing diagnostic must not replace the real message.
func TestLegacyProvenanceRunIsSilentWhenThereIsNothingToSay(t *testing.T) {
	object := protocol.NewObjectName("public", "orders_idx_p1")

	empty := t.TempDir()
	writeLegacyProv(t, empty, "run-1", "appdb", protocol.NewObjectName("public", "some_other_idx"))

	tests := []struct {
		name string
		dir  string
	}{
		{name: "no state dir", dir: ""},
		{name: "directory does not exist", dir: filepath.Join(t.TempDir(), "nope")},
		{name: "no runs at all", dir: t.TempDir()},
		{name: "records name other objects", dir: empty},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := LegacyProvenanceRun(tc.dir, "appdb", object); ok {
				t.Error("reported a legacy record that is not there")
			}
		})
	}
}

// A store this build wrote has no prov/ directory at all, so an operator who
// never ran a version-1 binary sees the halt message unchanged.
func TestLegacyProvenanceRunSaysNothingForAModernStore(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "runs", "run-modern", "nodes"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, ok := LegacyProvenanceRun(dir, "appdb", protocol.NewObjectName("public", "idx")); ok {
		t.Error("a store with no provenance records reported one")
	}
}
