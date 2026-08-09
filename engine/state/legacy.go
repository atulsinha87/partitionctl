package state

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// LegacyProvenanceRun reports the run that holds a schema-version-1 provenance
// record naming object, in a file state directory written before this build.
//
// # Why this exists, and why it is a diagnostic rather than an ownership proof
//
// Version 1 proved ownership with a side-table record: `runs/<run>/prov/*.json`
// in a file store, a `provenance` table in a SQL store. This build proves it
// with an ownership marker on the object (COMMENT ON INDEX) or, in the crash
// window before that marker lands, a live claim from a node record naming the
// object. Version 1 wrote neither: it issued no COMMENT, and its node records
// carry no object field.
//
// The consequence for anyone upgrading mid-incident is sharp. The canonical
// AC-5 wreckage -- a CREATE INDEX CONCURRENTLY aborted at 55P03, leaving an
// INVALID unmarked index -- is exactly what `resume` exists to clear, and
// against version-1 state it now halts with "carries no PartitionCTL ownership
// marker and no run holds a live claim on it", while a record naming that exact
// index sits unread in the run directory it just opened.
//
// This does NOT make those records authorize anything, and that is deliberate.
// The whole point of moving provenance onto the object was that a side-table
// record outlives the object it describes: a completed run's record still names
// public.orders_idx_p1 long after somebody else created a different index under
// that name, and honouring it would authorize destroying theirs (AC-6,
// NFR-REL-3). Reinstating a reader would reopen precisely that hole. So the
// record is read for one purpose only -- to tell the operator what changed
// under them, instead of sending them to fix by hand something the tool can see
// and will not explain.
//
// It returns ok false for any unreadable or absent directory. A diagnostic that
// fails is a diagnostic that says nothing, never an error on the halt path.
func LegacyProvenanceRun(stateDir string, database string, object protocol.ObjectName) (RunID, bool) {
	if stateDir == "" || object.IsZero() {
		return "", false
	}
	runs, err := os.ReadDir(filepath.Join(stateDir, "runs"))
	if err != nil {
		return "", false
	}
	for _, r := range runs {
		if !r.IsDir() {
			continue
		}
		provDir := filepath.Join(stateDir, "runs", r.Name(), "prov")
		entries, err := os.ReadDir(provDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(provDir, e.Name()))
			if err != nil {
				continue
			}
			var rec struct {
				RunID    RunID               `json:"run_id"`
				Database string              `json:"database"`
				Object   protocol.ObjectName `json:"object"`
			}
			if err := json.Unmarshal(b, &rec); err != nil {
				continue
			}
			if rec.Object.Schema != object.Schema || rec.Object.Name != object.Name {
				continue
			}
			// Scoped by database for the same reason claims are: a file store
			// deliberately holds state for several targets, and a record from a
			// run against another database says nothing about this object.
			if database != "" && rec.Database != "" && rec.Database != database {
				continue
			}
			id := rec.RunID
			if id == "" {
				id = RunID(r.Name())
			}
			return id, true
		}
	}
	return "", false
}
