# m4-e2e — the integration harness

The whole plugin exercised together: **create → gate → reindex → drop**, from one changelog,
against a real PostgreSQL 17 with 12 RANGE partitions and 1,200,000 rows.

Kept in the repo rather than in a session directory because this project has already lost one
experiment tree (`lbspike/`) that way.

```bash
make lb-e2e        # build, load the fixture, run all four stages with catalog checks between
make lb-progress   # just the create stage, timestamped, to judge the progress output
make lb-preview    # liquibase updateSQL for the whole changelog, no database changes
make lb-inspect    # read the tree straight from pg_catalog
make lb-clean      # remove the container and build output
```

It runs its own container (`partitionctl-lb`, port **5559**) so it can never disturb the Go
CLI's `partitionctl-pg` on 5432. `MAVEN_OPTS=-Duser.timezone=UTC` is set by `run.sh`; without
it the JDBC connect fails on macOS with `invalid value for parameter "TimeZone"`.

## Every verdict comes from the catalog

`e2e.sh` never reads a verdict out of Liquibase's log. Each stage's answer is a `psql` query
against `pg_class` / `pg_index` / `pg_inherits` / `obj_description`. A green log line over a
broken tree is the specific failure this project has shipped before.

## What each file proves

| File | Proves |
|---|---|
| `changelogs/e2e.xml` | the four operations compose in one changelog, driven stage by stage with `-Dliquibase.toTag` |
| `changelogs/shape.xml` | `unique`, `using` and `where`, singly and all three together, reach the parent **and** all 12 children — a tree that reaches 12/12 attached could not have a shape mismatch, because `ATTACH` refuses one |
| `changelogs/control-unmarked.xml` + `unmark.sql` | **negative control for the ownership marker.** Strip the `COMMENT ON INDEX` markers from a built tree and the drop refuses, leaving all 13 indexes in place and writing no `DATABASECHANGELOG` row |
| `changelogs/control-shape-change.xml` | the honest limitation: pointing a *new* changeset at an existing index with a different `where` emits **zero** statements. Coverage keys on `pg_inherits`, never on the index definition |
| `fixture.sql` | 12 monthly partitions, 100,000 rows each, so `CREATE INDEX CONCURRENTLY` is real work rather than a catalog no-op |
| `inspect.sql` | validity, attachment, uniqueness, access method, predicate, marker and `relfilenode` for every child index |

## The seam that had to be measured

`createPartitionedTableIndex` stamps each child index with a `COMMENT`;
`reindexPartitionedTableIndex` replaces that index's storage; `dropPartitionedTableIndex`
refuses without the comment. Whether the marker survives a reindex was therefore load-bearing
and not something to assume.

Measured on 17.10: it survives **both** leaf-level and parent-level
`REINDEX INDEX CONCURRENTLY`, even though the `relfilenode` changes each time. Stage 3 of
`e2e.sh` asserts exactly that — 12 rebuilt, 12 still marked — so a regression would fail the
walkthrough rather than surface later as an unexplained refusal.
