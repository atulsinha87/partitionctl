package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/atulsinha/partitionctl/engine/planner"
	"github.com/atulsinha/partitionctl/engine/protocol"
)

// cmdPlan implements `plan --spec <file> -o <plan>` (FR-CLI-2).
//
// It reads the catalog, validates topology and role membership, computes the
// remaining work and emits the graph with its digest and fingerprint. It opens
// no write transaction and issues no DDL (FR-PLAN-8, AC-1): the only database
// access is through [planner.CatalogReader], and the host proves the session is
// read-only before it plans.
func (a *App) cmdPlan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	setFlags := globalFlags(fs)

	specPath := fs.String("spec", "", "specification file (JSON); required")
	out := fs.String("o", "", "path to write the plan artifact to; required")
	force := fs.Bool("force", false, "overwrite an existing plan file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *specPath == "" {
		return protocol.ErrFailure.Detailf("--spec is required")
	}
	if *out == "" {
		return protocol.ErrFailure.Detailf("-o is required: `plan` writes the artifact to a caller-specified path (FR-CLI-2)")
	}
	if _, err := os.Stat(*out); err == nil && !*force {
		return protocol.ErrFailure.Detailf(
			"%s already exists; pass --force to overwrite. A plan is a reviewed artifact, so replacing one is deliberate", *out)
	}

	cfg, err := a.config(setFlags())
	if err != nil {
		return err
	}

	specFile, err := LoadSpecFile(*specPath)
	if err != nil {
		return protocol.ErrFailure.Detailf("reading specification %s: %v", *specPath, err)
	}
	now := protocol.NewTimestamp(a.Now())
	spec, err := specFile.Specification(cfg.Actor, now)
	if err != nil {
		return err
	}
	// Dispatch is a registry lookup, not a switch: wiring an operation is one
	// entry in operations.go and nothing else (NFR-EXT-1, AC-21). Resolving it
	// before opening a connection means an unknown operation costs no round
	// trip.
	op, discoverOptions, err := plannerFor(spec)
	if err != nil {
		return err
	}

	db, err := a.openDB(ctx, cfg)
	if err != nil {
		return err
	}
	if db != nil {
		defer func() { _ = db.Close() }()
	}
	tgt, err := a.openTarget(ctx, cfg, db)
	if err != nil {
		return err
	}
	defer tgt.close()

	// One read-only, repeatable-read snapshot for the whole planning pass: the
	// fingerprint, the index inspection and the privilege check must all
	// describe the same instant.
	read, release, err := tgt.snapshot(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = release() }()

	// The claim lookup is scoped by the database the *catalog* reports, never by
	// the connection configuration.
	//
	// Config.Dbname is a connection parameter: it is empty whenever the operator
	// connects through PARTITIONCTL_DSN, and it can differ from the real name
	// behind a pooler or an alias. The state store, meanwhile, stamps every run
	// with run.Target.Database, which the planner host takes from
	// current_database(). Scoping by anything else makes `plan` ask a different
	// question than `execute` and `resume` answer, and an empty scope matches a
	// run against *any* database, because a file state store deliberately holds
	// state for more than one target.
	database, err := read.CurrentDatabase(ctx)
	if err != nil {
		return err
	}

	// The claim source is optional at plan time and its absence is not neutral:
	// with no claim, the only thing that can authorize dropping an INVALID index
	// is the PartitionCTL ownership marker on the object itself, which is read
	// from the catalog and needs no state store at all (FR-PLAN-7, NFR-REL-3).
	// Opening the store therefore makes `plan` *less* restrictive, never more,
	// which is why a store that cannot be opened is a warning rather than a
	// failure.
	var claims planner.ClaimLookup
	if store, serr := a.openReadOnlyStore(ctx, cfg, db); serr == nil {
		defer func() { _ = store.Close() }()
		claims = claimLookup{store: store, database: database}
	} else {
		fmt.Fprintf(a.Stderr,
			"warning: no state store (%v); planning without claims, so an INVALID index is adoptable only "+
				"if it carries a PartitionCTL ownership marker (FR-PLAN-7)\n",
			serr)
	}

	host := &planner.Host{
		Catalog:         read,
		Claims:          claims,
		Now:             a.Now,
		DiscoverOptions: discoverOptions,
	}
	outcome, err := host.Run(ctx, op, spec)
	if err != nil {
		return err
	}

	data, err := protocol.EncodePlan(outcome.Plan)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(*out, data); err != nil {
		return protocol.ErrFailure.Detailf("writing plan %s: %v", *out, err)
	}

	a.reportPlan(outcome, *out)
	return nil
}

// reportPlan prints what a reviewer needs before approving the artifact.
func (a *App) reportPlan(outcome *planner.Outcome, path string) {
	p := outcome.Plan
	fmt.Fprintf(a.Stdout, "wrote %s\n", path)
	fmt.Fprintf(a.Stdout, "  plan id      %s\n", p.PlanID)
	fmt.Fprintf(a.Stdout, "  operation    %s\n", p.Operation)
	fmt.Fprintf(a.Stdout, "  target       %s", p.Target.Table)
	if p.Target.Index != nil {
		fmt.Fprintf(a.Stdout, " index %s", p.Target.Index)
	}
	if p.Target.Database != "" {
		fmt.Fprintf(a.Stdout, " in database %s", p.Target.Database)
	}
	fmt.Fprintln(a.Stdout)
	fmt.Fprintf(a.Stdout, "  role         %s\n", outcome.Role)
	fmt.Fprintf(a.Stdout, "  partitions   %d\n", outcome.Topology.LeafCount())
	fmt.Fprintf(a.Stdout, "  nodes        %d\n", len(p.Nodes))
	fmt.Fprintf(a.Stdout, "  estimate     %s (FR-PLAN-9; advisory, drives the ETA and nothing else)\n",
		humanSeconds(p.TotalEstimatedSeconds()))
	fmt.Fprintf(a.Stdout, "  digest       %s\n", p.Digest)
	fmt.Fprintf(a.Stdout, "  fingerprint  %s\n", p.TopologyFingerprint)

	byKind := map[protocol.NodeKind]int{}
	for i := range p.Nodes {
		byKind[p.Nodes[i].Kind]++
	}
	fmt.Fprintln(a.Stdout, "  graph:")
	for _, k := range protocol.AllNodeKinds() {
		if n := byKind[k]; n > 0 {
			lock := k.LockLevel()
			if lock == protocol.LockNone {
				fmt.Fprintf(a.Stdout, "    %-30s %4d  no lock\n", k, n)
				continue
			}
			fmt.Fprintf(a.Stdout, "    %-30s %4d  %s\n", k, n, lock)
		}
	}
	for _, note := range outcome.Notes {
		fmt.Fprintf(a.Stdout, "  note: %s\n", note)
	}
	if len(p.Confirmations) > 0 {
		for _, c := range p.Confirmations {
			fmt.Fprintf(a.Stdout, "  confirmed: %s by %s at %s\n", c.Flag, c.Actor, c.At)
		}
	}
	fmt.Fprintf(a.Stdout, "\nreview the artifact, then run: partitionctl execute %s\n", path)
}

// writeFileAtomic writes through a temporary file and a rename, so a crash
// mid-write cannot leave a truncated plan that still parses.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// humanSeconds renders a duration estimate without pretending to precision the
// estimate does not have.
func humanSeconds(sec int) string {
	switch {
	case sec <= 0:
		return "under a second"
	case sec < 60:
		return fmt.Sprintf("~%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("~%dm%ds", sec/60, sec%60)
	default:
		return fmt.Sprintf("~%dh%dm", sec/3600, (sec%3600)/60)
	}
}
