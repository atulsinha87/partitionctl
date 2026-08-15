# PartitionCTL

Partition-aware online schema evolution for PostgreSQL. It exists because `CREATE INDEX CONCURRENTLY` is rejected on a partitioned parent, and the documented workaround is an 800+ statement choreography that no shell script survives.

**Two independent products.** They do not interoperate and share no code:

1. **Go CLI** (`engine/`, `operations/`, `adapters/cli/`, `cmd/`) — plan/execute/verify, standalone.
2. **Liquibase plugin** (`liquibase-partitionctl/`) — a Maven dependency that does the work end to end inside `liquibase update`. **This is the deliverable.**

---

## Read these first

| Document | When |
|---|---|
| [README.md](README.md) | What this is, which of the two products to pick |
| [CONTRIBUTING.md](CONTRIBUTING.md) | The verification bar, the non-negotiable constraints, and the release recipe |
| [liquibase-partitionctl/README.md](liquibase-partitionctl/README.md) | The adopter-facing quickstart, attribute reference and troubleshooting |
| [examples/liquibase/](examples/liquibase/) | A runnable consumer of the published coordinate. `make lb-adopter` runs it |
| `docs/experiments/poc-trees/m4-e2e/` | The live end-to-end harness. `make lb-e2e` runs it |

### Working documents, on the author's disk only — not in this repository

Everything else under `docs/` is **gitignored**: `M4-HANDOFF.md` (**start here if you have it**),
`HANDOFF.md`, `M4-PLAN.md`, `TRD.md`, `M2-M3-DIRECTIVE.md`, `M2-M3-FOUNDATION.md`,
`REPAIR-REPORT.md`, `REVIEW-ROUND1.md`, `REVIEW-ROUND2.md`, `NEW-MACHINE-PROMPT.md`,
`LAPTOP-TRANSFER.md`, `workflows/`, the `experiments/*.md` write-ups and every `poc-trees/`
harness except `m4-e2e`.

They are handover, planning and research records — how the work was driven, not how either
product is used. **If this is a fresh clone they are not present, and nothing in the build needs
them.** The measured facts that outrank the spec are restated below so they survive without the
documents.

### The spec is wrong about two things

The TRD (`docs/TRD.md`, on the author's disk only) is authoritative for the Go CLI but wrong
here. Both corrections were measured against real servers:

- It says `REINDEX CONCURRENTLY` is unsupported on partitioned relations. **It works**, on 14.23 and 17.10.
- Its lock table is wrong twice: `CREATE INDEX ON ONLY` takes `ShareLock` on the parent table, and `ALTER INDEX … ATTACH PARTITION` takes `AccessExclusiveLock` on the child index.

Do not "fix" code to match the TRD on these points.

---

## Verifying the tree

```bash
gofmt -l . && go vet ./... && go build ./... && go test ./... -count=1
```

```bash
mvn clean install                                  # from the root, or from liquibase-partitionctl/
```

Build with **JDK 17**. The jar targets Java 8 bytecode (`--release 8`) and recent javac releases
have been dropping old `--release` targets — a JDK 26 default was rejected in favour of 17, which
is the newest verified to build this tree.

Live database work needs Docker:

```bash
make db-reset      # PostgreSQL 17, 12 RANGE partitions, ~1.05M rows
make demo-all      # full scripted walkthrough, every command echoed
make lb-e2e        # the plugin: create, gate, reindex, drop, catalog-checked at each stage
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

## Nothing is published without the owner approving the diff

**Commit freely. Do not push, tag, or open anything outward-facing until the owner has seen the
complete diff and said yes.**

A push to a public repository is irreversible in the way that matters: the content is disclosed,
and may be forked, indexed or cached. Reverting afterwards does not undo publication. Verifying a
change and getting agreement on it are different things, and doing the first well is not a
substitute for the second.

- Commit locally as much as you like, then stop and show the diff — `git show --stat` plus the
  substantive hunks, or a file-by-file walk when it is large.
- A request to fix or build something is **not** authorisation to publish the result.
- This covers pushes, tags, GitHub issues, releases and repository settings. Approval for one does
  not carry to the next.

## Checkpointing

Before compacting context or pausing work, use the `smart-compact` skill. It stops background work, verifies the build by running it, commits, updates the handoff, and refreshes this index. The skill lives in `.claude/`, which is gitignored, so it is present on the author's machine and not in a fresh clone.
