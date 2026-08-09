# Contributing to PartitionCTL

Contributions are welcome. This document is short on ceremony and long on the two things that
actually get pull requests merged here: **what you must run**, and **what you must not change**.

---

## The bar: run it, and say what it printed

This project's central lesson, learned expensively and more than once, is that **a green test
suite is weak evidence**. Its own history:

- 579 passing tests and four reviewers, over an `execute --dry-run` path that was broken for
  *every* input the tool could produce. One real run found it, plus three more bugs, in twenty
  minutes.
- 40 passing tests over a shipped XSD containing `--` inside an XML comment, which XML forbids.
  The schema could never have loaded; the jar would have failed on first use.
- Two releases, `v0.1.0` and `v0.1.1`, that both passed `mvn clean install` locally and both
  failed on publish — for two different build-environment reasons.

Every one of those lived in a *seam*: between components, or between the code and its environment.
Unit tests with fakes cannot look there.

So, in your pull request description:

- State what you ran and what it printed. Paste the output.
- Keep **"written"** and **"verified"** visibly separate. "I added a test for this" and "I ran it
  against PostgreSQL and here is the catalog output" are different claims — say which one you have.
- If a change depends on how PostgreSQL or Liquibase behaves, **run the experiment** rather than
  quoting documentation. `make db-reset` plus a `psql -c` settles most questions in under a minute,
  and `\h ALTER INDEX` settles a grammar question in four seconds. Two load-bearing designs in this
  repository were built on documentation summaries that turned out to be wrong.

Read verdicts from `pg_catalog`, not from a tool's own log. A green log line over a broken tree is
precisely the failure this project has shipped before.

---

## Verifying a change

The first two need nothing installed but a toolchain. The rest need Docker.

```bash
gofmt -l . && go vet ./... && go build ./... && go test ./... -count=1
```
```bash
./mvnw clean install          # 217 tests
```

Run the gate that matches what you touched:

| You changed | Run |
|---|---|
| anything | both commands above |
| Go CLI behaviour | `make db-reset && make demo-all` |
| extension behaviour | `make lb-e2e` |
| packaging, versions, or the published coordinate | `make lb-adopter` |

`make lb-adopter` is the only gate that exercises the **published** artifact. `make lb-e2e`
resolves the extension from your local `~/.m2`, so it passes even when what is on JitPack is
broken — which is exactly how two releases got out. Use it before any release.

Build the Java side with **JDK 17**. The jar targets Java 8 bytecode (`--release 8`) and recent
javac versions have been dropping old `--release` targets. Use `./mvnw`, not `mvn`: the wrapper
pins Maven 3.9.9, and an older Maven silently changes the compiler plugin's behaviour.

On macOS set `MAVEN_OPTS=-Duser.timezone=UTC` if your system timezone is not UTC, or the JDBC
connect fails with `invalid value for parameter "TimeZone"`.

---

## Constraints that are not negotiable

A pull request that breaks one of these will be declined regardless of how good it looks.

- **Go 1.22 floor.** pgx v5 requires Go 1.25, which is why `lib/pq` is used. Do not swap the
  driver to "modernise" it.
- **Nothing under `engine/`, `operations/` or `adapters/` may import a database driver.** Only
  `cmd/partitionctl/main.go` imports `lib/pq`. That single rule is what keeps the whole tree
  unit-testable offline against in-memory fakes.
- **The CLI uses stdlib `flag`, not cobra.**
- **Go's `flag` package stops parsing at the first non-flag argument**, so every flag must precede
  a positional. `execute <plan> -dry-run` silently passes `-dry-run` as a second positional.
- **Java:** `liquibase-core` 4.33.0 at `provided` scope, `--release 8`, groupId
  `io.github.atulsinha87`. The built jar must contain **zero** entries under `liquibase/` — shipping
  Liquibase classes would let this jar override the host's own version.

### The two products are independent by decision

The Go CLI and the Liquibase extension share no code, no SQL, and no naming. They do not
interoperate, and an adopter picks one. This looks like an obvious thing to fix and it is not:
a shared SQL catalog, a naming specification and a cross-language conformance test were all
designed, and then deliberately deleted, because users pick one path and stay on it.

Pull requests that make the two share code, or that add a conformance layer between them, will be
declined. M4 did not edit a single `.go` file, and extension work should not either.

### Measured facts that override older documents

The original spec (`docs/TRD.md`) is not in this repository — see below — and it is wrong on these
points. They were measured against real servers, on 14.23 and 17.10:

- `REINDEX INDEX CONCURRENTLY` and `REINDEX TABLE CONCURRENTLY` **do work** on partitioned
  relations. The spec says they do not.
- `CREATE INDEX ON ONLY <parent>` takes **`ShareLock`** on the parent table, not
  `ShareUpdateExclusive`.
- `ALTER INDEX ... ATTACH PARTITION` takes **`AccessExclusiveLock`** on the child index.
- **`ALTER INDEX ... DETACH PARTITION` does not exist.** `ATTACH` has no inverse. Any workflow
  built on it is fiction — `\h ALTER INDEX` on 17.10 confirms in four seconds.

Do not "fix" code to match the spec on these points.

---

## Releasing

Only relevant to maintainers, and it has bitten twice, so it is written down.

1. Bump the version in `pom.xml`, `liquibase-partitionctl/pom.xml`,
   `examples/liquibase/pom.xml`, `docs/experiments/poc-trees/m4-e2e/pom.xml`, and both READMEs.
2. `./mvnw clean install` and the Go gate.
3. Tag `vX.Y.Z` and push the tag.
4. Wait for `https://jitpack.io/api/builds/com.github.atulsinha87/partitionctl/vX.Y.Z` to report
   `"status": "ok"`. Read the build log if it does not.
5. `make lb-adopter`.

**JitPack caches one build result per version, permanently.** A failed build means that version is
spent: re-pointing the tag does *not* retrigger it, the API keeps reporting the original commit.
Cut the next version instead. Never move a published tag.

---

## What is intentionally not in this repository

Handover notes, the TRD, milestone plans, adversarial review rounds, experiment write-ups and the
proof-of-concept harnesses are all gitignored. They record how the work was driven, not how either
product is used, and none is needed to build, run, adopt or contribute.

`docs/experiments/poc-trees/m4-e2e/` is the deliberate exception, because `make lb-e2e` executes
it. If you find a dangling reference to a missing document, the fix is to restate the fact inline
or drop the reference — not to re-add the document.

---

## Pull requests

- Keep the commit history readable. Commit messages here explain *why*, at length, and that is a
  feature — the reasoning is expected to survive in the log.
- One logical change per pull request.
- Update the adopter-facing docs in the same change: `liquibase-partitionctl/README.md` for
  extension behaviour, `examples/liquibase/` if the consumer-side setup changes.
- CI runs the two offline gates on every pull request. The Docker-backed gates are yours to run
  locally; say in the description which ones you ran.

By contributing you agree that your contributions are licensed under the Apache License 2.0, the
same terms as the project.
