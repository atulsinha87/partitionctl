// Command partitionctl is PartitionCTL's command-line interface: partition-aware
// online schema evolution for PostgreSQL.
//
// # Registering a driver
//
// No package in this repository imports a PostgreSQL driver. That is deliberate
// and it is what keeps the engine offline-testable: engine/planner,
// engine/executor, engine/state and engine/verifier all speak to database/sql
// interfaces, so every one of them, and the CLI above them, is unit-tested with
// in-memory fakes and no server (HANDOFF §3).
//
// The consequence is that this binary connects to nothing until a driver is
// registered, and registering one is this file's only job. It registers lib/pq,
// so `--driver postgres` works out of the box; `partitionctl plan` lists the
// drivers it can see when asked for one it cannot.
//
// To add another, add a blank import below alongside the lib/pq one — for
// example:
//
//	import _ "github.com/jackc/pgx/v5/stdlib" // registers "pgx", needs Go 1.25+
//
// and select it with --driver, PARTITIONCTL_DRIVER, or `driver:` in the
// configuration file. Everything that does not need the target — `render`, and
// `status` and `cancel` against a file state store — works with no driver at
// all, which is the same property AC-25 asserts for an unreachable database.
package main

import (
	"context"
	"os"

	"github.com/atulsinha87/partitionctl/adapters/cli"

	// The one driver import in the tree, and it lives here rather than under
	// engine/ or adapters/ for the reason given above. lib/pq rather than pgx
	// because pgx v5 requires Go 1.25 and this module targets 1.22.
	_ "github.com/lib/pq" // registers "postgres"
)

func main() {
	os.Exit(cli.Main(context.Background(), os.Args[1:]))
}
