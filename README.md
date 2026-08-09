# PartitionCTL

Partition-aware online schema evolution for PostgreSQL.

`CREATE INDEX CONCURRENTLY` is **rejected outright** on a partitioned parent table:

```
ERROR:  cannot create index on partitioned table "orders" concurrently
```

The documented workaround is a per-partition choreography — build the parent index `ON ONLY`,
build one index concurrently per leaf, then `ATTACH` each one, at which point PostgreSQL
validates the parent on the final attach. It has to be generated from the live catalog, because
the statement list depends on how many partitions exist today. On a table with a few hundred
partitions that is several hundred order-sensitive statements, any one of which can fail and
leave the tree in a state the next run has to understand.

This repository does that for you, two different ways.

---

## Two independent products. Pick one.

They share no code, do not interoperate, and neither is a wrapper around the other.

### 1. The Liquibase extension — `liquibase-partitionctl/`

One Maven dependency and one changeset. The whole operation happens inside `liquibase update`:
no binary to install, no second set of credentials, no step outside your existing migration
pipeline.

**→ [liquibase-partitionctl/README.md](liquibase-partitionctl/README.md)** — quickstart, full
attribute reference, troubleshooting and limitations.

It must be `<pluginRepositories>`, **not** `<repositories>`. The extension goes inside the
liquibase-maven-plugin's own `<dependencies>`, and plugin dependencies are resolved from plugin
repositories only — with `<repositories>` Maven never contacts JitPack and fails saying the
artifact is not in Central.

```xml
<pluginRepositories>
  <pluginRepository>
    <id>jitpack.io</id>
    <url>https://jitpack.io</url>
  </pluginRepository>
</pluginRepositories>
```

```xml
<!-- inside the liquibase-maven-plugin's OWN <dependencies>, not your project's -->
<dependency>
  <groupId>com.github.atulsinha87.partitionctl</groupId>
  <artifactId>liquibase-partitionctl</artifactId>
  <version>v0.1.2</version>
</dependency>
```

**[examples/liquibase/](examples/liquibase/)** is a complete, runnable consumer of exactly this
coordinate — `make lb-adopter` runs it against a throwaway 12-partition database and verifies the
result from `pg_catalog`.

Building from source works too, and needs no repository entry:

```bash
git clone https://github.com/atulsinha87/partitionctl.git
cd partitionctl && mvn clean install
```

### 2. The Go CLI — `engine/`, `operations/`, `adapters/`, `cmd/`

A standalone `plan` → `execute` → `verify` loop with a reviewable plan artifact, a state store
and resume. Use it when you want the migration to be an artifact you inspect and approve before
it runs, independent of Liquibase.

```bash
make build
build/partitionctl plan -spec examples/local/orders-idx.json -o build/migration.plan
build/partitionctl render build/migration.plan     # the SQL runbook, offline
build/partitionctl execute build/migration.plan
build/partitionctl verify -end-state build/migration.plan
```

---

## What it does, and what it costs

| Operation | Blocking? |
|---|---|
| Create an index over every partition | **No.** One `CREATE INDEX CONCURRENTLY` per leaf, attached as it goes |
| Reindex every partition | **No.** `REINDEX INDEX CONCURRENTLY` per leaf |
| Drop a partitioned index | **Yes, unavoidably.** See below |

**Dropping is the one operation that is not online.** `DROP INDEX` on a partitioned parent takes
`AccessExclusiveLock` on the parent *and every leaf*, simultaneously. PostgreSQL offers no
`ALTER INDEX … DETACH PARTITION`, so attached children cannot be detached and dropped one at a
time — the inverse of `ATTACH` does not exist. Both tools therefore refuse to drop without an
explicit confirmation flag, and say what the lock will cost before they take it.

## Requirements

- **PostgreSQL** with declarative partitioning. Verified on 14.23 and 17.10.
- **Liquibase extension:** Java 8 or newer at runtime; `liquibase-core` 4.x (`provided`, so the
  jar contains zero Liquibase classes and no transitive dependencies at all). Build with JDK 17.
- **Go CLI:** Go 1.22 or newer.
- Single-level partitioning. Sub-partitioned tables are **refused, not silently mishandled** —
  `ALTER INDEX … ATTACH PARTITION` only accepts an index on a direct partition.

## Status

`v0.1.2`, the first working release. Both products have been exercised against a live PostgreSQL 17.10
with 12 partitions and 1.2M rows, through the whole create → gate → reindex → drop cycle, with
every verdict read from `pg_catalog` rather than from either tool's own log — including
`SIGKILL` mid-flight and a `lock_timeout` abort, both of which resumed and finished.

Known limitations are documented plainly in
[liquibase-partitionctl/README.md](liquibase-partitionctl/README.md); they are real constraints
of PostgreSQL and Liquibase, not a to-do list.

## Verifying a checkout

```bash
gofmt -l . && go vet ./... && go build ./... && go test ./... -count=1
```

```bash
mvn clean install
```

Live database work needs Docker:

```bash
make db-reset && make demo-all     # the Go CLI, end to end
make lb-e2e                        # the extension: create, gate, reindex, drop
```

On macOS the Liquibase Maven plugin needs `MAVEN_OPTS=-Duser.timezone=UTC`, or the JDBC connect
fails on a non-UTC system timezone. The harness scripts set it.

## Licence

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
