# PartitionCTL

Partition-aware online schema evolution for PostgreSQL. It exists because `CREATE INDEX CONCURRENTLY` is rejected on a partitioned parent, and the documented workaround is an 800+ statement choreography that no shell script survives.

**Two independent products.** They do not interoperate and share no code:

1. **Go CLI** (`engine/`, `operations/`, `adapters/cli/`, `cmd/`) — plan/execute/verify, standalone.
2. **Liquibase plugin** (`liquibase-partitionctl/`) — a Maven dependency that does the work end to end inside `liquibase update`. **This is the deliverable.**

---

## Read these first

| Document | When |
|---|---|
| [docs/M4-HANDOFF.md](docs/M4-HANDOFF.md) | **Start here.** Where work stopped, what is verified vs merely written, settled decisions |
| [docs/LAPTOP-TRANSFER.md](docs/LAPTOP-TRANSFER.md) | Moving the repo to another machine, and the prerequisites to re-establish |
| [docs/NEW-MACHINE-PROMPT.md](docs/NEW-MACHINE-PROMPT.md) | The first prompt to paste after a move — what a fresh session cannot infer |
| [liquibase-partitionctl/README.md](liquibase-partitionctl/README.md) | The adopter-facing quickstart, attribute reference and troubleshooting |
| [docs/M4-PLAN.md](docs/M4-PLAN.md) | The Liquibase plugin design, with every question answered by experiment |
| [docs/experiments/](docs/experiments/) | **Measured facts. These override the TRD wherever they disagree.** |
| [docs/TRD.md](docs/TRD.md) | The original spec. Authoritative for the Go CLI, **wrong in places** — see below |
| [docs/HANDOFF.md](docs/HANDOFF.md) | Go CLI working state (M1–M3) |

### The TRD is wrong about two things

Both measured against real servers, both recorded in `docs/experiments/v0.0-results.md`:

- It says `REINDEX CONCURRENTLY` is unsupported on partitioned relations. **It works**, on 14.23 and 17.10.
- Its lock table is wrong twice: `CREATE INDEX ON ONLY` takes `ShareLock` on the parent table, and `ALTER INDEX … ATTACH PARTITION` takes `AccessExclusiveLock` on the child index.

Do not "fix" code to match the TRD on these points.

---

## Verifying the tree

```bash
gofmt -l . && go vet ./... && go build ./... && go test ./... -count=1
```

```bash
cd liquibase-partitionctl && mvn clean install
```

Live database work needs Docker:

```bash
make db-reset      # PostgreSQL 17, 12 RANGE partitions, ~1.05M rows
make demo-all      # full scripted walkthrough, every command echoed
```

---

## Constraints that are not negotiable

- **Go 1.22.0.** pgx v5 needs Go 1.25, so it cannot be used.
- **Nothing under `engine/`, `operations/` or `adapters/` may import a database driver.** Only `cmd/partitionctl/main.go` imports `lib/pq`. That is what keeps the tree unit-testable offline with in-memory fakes.
- CLI uses stdlib `flag`, not cobra.
- Java: `liquibase-core` 4.33.0 at `provided` scope, `--release 8`, `io.github.atulsinha87`.
- Go's `flag` package stops parsing at the first non-flag argument, so every flag must precede a positional.

---

## Method note, learned twice the hard way

This project has shipped a wrong design twice by trusting a documentation summary over a short experiment. It has repeatedly been saved by running the thing instead: an end-to-end run found four seam bugs in twenty minutes that 579 unit tests and four adversarial reviewers missed.

`make db-reset` plus a `psql -c` settles most PostgreSQL questions in under a minute. `\h ALTER INDEX` settles a grammar question in four seconds. Prefer those to recall.

---

## Checkpointing

Before compacting context or pausing work, use the `smart-compact` skill (`.claude/skills/smart-compact/`). It stops background work, verifies the build by running it, commits, updates the handoff, and refreshes this index.
