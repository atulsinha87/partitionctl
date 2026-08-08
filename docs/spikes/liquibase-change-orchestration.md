# Liquibase custom **Change** orchestration: empirical spike (M4 Q1–Q3)

Status: **COMPLETE. Q1, Q2, Q3 answered (all pass), plus Q4–Q7.**
Run 2026-08-09. Every claim below is backed by a command actually run against a
real PostgreSQL 17.10 container (`partitionctl-pocspike`, port 5434) and a real
Liquibase 4.33.0 driven through `liquibase-maven-plugin:update`. Nothing here
is from documentation or forum posts.

Working tree (start here):
```
/private/tmp/claude-502/-Users-atulsinha-ClaudeProjects-Pg-Partition/06788eb5-b48b-42a7-83d1-4980352d2143/scratchpad/poc/
  ext/     the extension module (jar) — builds with `mvn clean install`
  runner/  liquibase-maven-plugin harness; `mvn liquibase:update -Dchangelog=changelogs/<f>.xml`
```

## Headline verdict

**VIABLE, with caveats.** All three go/no-go questions pass, proven end to end
against a real database:

- **Q1 — YES.** A custom `AbstractChange` runs the full parent-ONLY /
  `CREATE INDEX CONCURRENTLY`-per-leaf / `ATTACH` choreography, and the parent
  index auto-validates on the final ATTACH. `runInTransaction="false"` on the
  changeset is necessary and sufficient. A control run with default
  transaction handling fails with the exact error the plan feared, so the flag
  is doing the work.
- **Q2 — YES.** `((JdbcConnection) database.getConnection()).getUnderlyingConnection()`
  inside `generateStatements` gives a normal `java.sql.Connection`. A
  4-partition table emitted 9 statements and a 7-partition table emitted 15,
  from the same class with nothing hard-coded.
- **Q3 — YES, re-discovery works.** A failed changeset is **never written to
  `DATABASECHANGELOG`**, so the next `liquibase update` retries it, calls
  `generateStatements` again, and re-discovery emits only the remaining work
  (9 statements became 4 after a failure at leaf 3 of 4). Under
  `runInTransaction="false"` the already-completed indexes survive; under the
  default they are all rolled back.

Q4–Q7 were also reached: nested `<column>` binding works (with an XSD caveat),
`SET statement_timeout`/`lock_timeout` do take effect across emitted statements,
the checksum covers the XML and not the generated SQL, and operator output
during a long run is one line per changeset — effectively silence.

The caveats, in order of how much they will cost:

0. **An interrupted CIC can make the next run report success while leaving a
   permanently invalid index.** Found while testing resume; it is worse than
   anything Q1–Q3 turned up and is written out in full at the end of Q3.
   Discovery MUST check `pg_index.indisvalid` per child index, and the run MUST
   verify the parent index is valid at the end. Without both, a broken outcome
   is recorded as `EXECUTED` and can never be repaired by re-running.
1. **`generateStatements` is called about 7 times per update, not once.**
   Discovery must be memoised or a 400-partition catalog sweep runs 7 times,
   and the method must be free of side effects.
2. **After a changeset succeeds it is skipped forever.** Partitions added
   later are never revisited by the extension. PostgreSQL happens to cover
   *newly created* partitions itself once the parent index is valid, but with
   its own index names — so coverage detection must key on `pg_inherits` from
   the parent index OID, never on the child index name.
3. **`runInTransaction="false"` is load-bearing twice over** — once for CIC to
   run at all, once for partial progress to survive a failure. Getting it
   wrong in either direction is silent until it isn't.
4. Every changeset carrying this Change must set it. There is no way for the
   extension to force it from Java; it is the adopter's XML that decides. A
   `validate()` check that inspects `getChangeSet().isRunInTransaction()` and
   errors out is strongly advised.

---

## Q1 — Can a custom Change execute `CREATE INDEX CONCURRENTLY`?

### Q1a — ordinary table: **YES, with `runInTransaction="false"`. Confirmed.**

A minimal `AbstractChange` (`PocSimpleConcurrentIndexChange`, tag
`pocSimpleConcurrentIndex`) returning one `RawSqlStatement` holding
`CREATE INDEX CONCURRENTLY`, registered via
`META-INF/services/liquibase.change.Change`.

Changelog `changelogs/q1a-simple-notx.xml`:
```xml
<changeSet id="q1a-notx" author="poc" runInTransaction="false">
    <ext:pocSimpleConcurrentIndex schemaName="public" tableName="ord"
                                  indexName="ord_created_at_cic" columnName="created_at"/>
</changeSet>
```

```
$ MAVEN_OPTS=-Duser.timezone=UTC mvn liquibase:update -Dchangelog=changelogs/q1a-simple-notx.xml
[POC] generateStatements emitting: CREATE INDEX CONCURRENTLY "ord_created_at_cic" ON "public"."ord" ("created_at")
[INFO] Running Changeset: changelogs/q1a-simple-notx.xml::q1a-notx::poc
[INFO] pocSimpleConcurrentIndex: created ord_created_at_cic on public.ord
[INFO] ChangeSet changelogs/q1a-simple-notx.xml::q1a-notx::poc ran successfully in 6ms
[INFO] BUILD SUCCESS
```

Verified in Postgres that the index is real **and valid**:
```
$ docker exec -i partitionctl-pocspike psql -U postgres -d postgres
SELECT c.relname, i.indisvalid, i.indisready FROM pg_class c
  JOIN pg_index i ON i.indexrelid=c.oid WHERE c.relname='ord_created_at_cic';
      relname       | indisvalid | indisready
--------------------+------------+------------
 ord_created_at_cic | t          | t
```

### The control run — `runInTransaction` is the decisive factor, proven

Same Change, same jar, same database, **only** difference is
`runInTransaction` omitted (`changelogs/q1a-simple-tx.xml`):

```
$ MAVEN_OPTS=-Duser.timezone=UTC mvn liquibase:update -Dchangelog=changelogs/q1a-simple-tx.xml
[ERROR] liquibase.exception.MigrationFailedException: Migration failed for changeset changelogs/q1a-simple-tx.xml::q1a-tx::poc:
[ERROR]      Reason: liquibase.exception.DatabaseException: ERROR: CREATE INDEX CONCURRENTLY cannot run inside a transaction block [Failed SQL: (0) CREATE INDEX CONCURRENTLY "ord_val_cic_tx" ON "public"."ord" ("val")]
[INFO] BUILD FAILURE
```

So the failure mode the plan feared is real and reproducible, and
`runInTransaction="false"` is exactly and sufficiently what avoids it. There
is no additional flag, connection property or `Database` API call needed.

After the failed control run, nothing was recorded and nothing was left behind:
```
SELECT id, author, exectype FROM databasechangelog;
    id    | author | exectype
----------+--------+----------
 q1a-notx | poc    | EXECUTED     <-- only the successful run

SELECT relname FROM pg_class WHERE relname='ord_val_cic_tx';
 relname
---------
(0 rows)
```

### Sharp edge found immediately: `generateStatements` is called SEVEN times

The `[POC]` stdout marker printed **7 times** for a single changeset in one
`liquibase:update`: 4 times before the lock is even acquired (parse /
checksum / validation phases), then 3 more inside `Running Changeset`.

```
[POC] generateStatements emitting: ...   (x4, before "Successfully acquired change log lock")
[INFO] Successfully acquired change log lock
[INFO] Running Changeset: ...
[POC] generateStatements emitting: ...   (x3)
[INFO] ChangeSet ... ran successfully
```

This matters a lot at 400 partitions: the discovery query will be executed
**once per call**, so a naive implementation runs the catalog sweep 7 times per
update. Implementers must memoise discovery per `(Change instance, Database)`
or accept 7x the catalog load. It also means `generateStatements` **must be
free of side effects** — it is not called once.

### Q1b — partitioned parent, full choreography: **YES. Confirmed end to end.**

Table `person`, 4 range partitions, 4000 rows. Changelog
`changelogs/q2-discover.xml`, one changeset, `runInTransaction="false"`:

```xml
<changeSet id="q2-discover" author="poc" runInTransaction="false">
    <ext:createPartitionedTableIndex indexName="idx_personaddress"
                                     schemaName="public" tableName="person">
        <ext:column name="address" descending="true"/>
    </ext:createPartitionedTableIndex>
</changeSet>
```

```
$ MAVEN_OPTS=-Duser.timezone=UTC mvn liquibase:update -Dchangelog=changelogs/q2-discover.xml
[POC] DISCOVERY: parentIndexExists=false leafCount=4
[POC]   leaf public.person_p01 childIdxExists=false attached=false
[POC]   leaf public.person_p02 childIdxExists=false attached=false
[POC]   leaf public.person_p03 childIdxExists=false attached=false
[POC]   leaf public.person_p04 childIdxExists=false attached=false
[POC] EMITTING 9 statements for 4 leaves
[POC]   > CREATE INDEX "idx_personaddress" ON ONLY "public"."person" ("address" DESC)
[POC]   > CREATE INDEX CONCURRENTLY "idx_personaddress_person_p01" ON "public"."person_p01" ("address" DESC)
[POC]   > ALTER INDEX "public"."idx_personaddress" ATTACH PARTITION "public"."idx_personaddress_person_p01"
[POC]   > CREATE INDEX CONCURRENTLY "idx_personaddress_person_p02" ON "public"."person_p02" ("address" DESC)
[POC]   > ALTER INDEX "public"."idx_personaddress" ATTACH PARTITION "public"."idx_personaddress_person_p02"
[POC]   > CREATE INDEX CONCURRENTLY "idx_personaddress_person_p03" ON "public"."person_p03" ("address" DESC)
[POC]   > ALTER INDEX "public"."idx_personaddress" ATTACH PARTITION "public"."idx_personaddress_person_p03"
[POC]   > CREATE INDEX CONCURRENTLY "idx_personaddress_person_p04" ON "public"."person_p04" ("address" DESC)
[POC]   > ALTER INDEX "public"."idx_personaddress" ATTACH PARTITION "public"."idx_personaddress_person_p04"
[INFO] ChangeSet changelogs/q2-discover.xml::q2-discover::poc ran successfully in 45ms
[INFO] BUILD SUCCESS
```

The deliberately-INVALID parent index really does get auto-validated on the
final ATTACH — this is the whole point of the choreography and it holds:

```
SELECT c.relname, c.relkind, i.indisvalid FROM pg_class c JOIN pg_index i ON i.indexrelid=c.oid
  WHERE c.relname LIKE 'idx_personaddress%' ORDER BY 1;
           relname            | relkind | indisvalid
------------------------------+---------+------------
 idx_personaddress            | I       | t          <-- parent, VALID
 idx_personaddress_person_p01 | i       | t
 idx_personaddress_person_p02 | i       | t
 idx_personaddress_person_p03 | i       | t
 idx_personaddress_person_p04 | i       | t

SELECT count(*) AS attached FROM pg_inherits ii JOIN pg_class p ON p.oid=ii.inhparent
  WHERE p.relname='idx_personaddress';
 attached
----------
        4
```

And the `DESC` from the nested element actually reached PostgreSQL:
```
SELECT indexdef FROM pg_indexes WHERE indexname='idx_personaddress_person_p01';
 CREATE INDEX idx_personaddress_person_p01 ON public.person_p01 USING btree (address DESC)
```

**Q1 verdict: the design is not killed. A custom Change can run the full
CIC choreography, provided every changeset carrying one is marked
`runInTransaction="false"`.**

---

## Q2 — Can the Change discover partitions at runtime? **YES. Confirmed.**

### Exact API path to the JDBC connection

Inside `generateStatements(Database database)`, identical to the precondition
path proven in the earlier spike:

```java
DatabaseConnection dbc = database.getConnection();
if (!(dbc instanceof JdbcConnection)) { throw new UnexpectedLiquibaseException(...); }
Connection c = ((JdbcConnection) dbc).getUnderlyingConnection();   // java.sql.Connection
```
`liquibase.database.jvm.JdbcConnection#getUnderlyingConnection()` is public.
This is the same connection the changeset's own DDL runs on — no second
credential, no separate datasource, no subprocess. `PreparedStatement` with
bind parameters works normally against it.

### Discovery query used (handles multi-level partitioning)

Recursive over `pg_inherits`, so sub-partitioned trees resolve to true leaves
(`relkind='r'`; intermediate partitioned tables are `relkind='p'` and are
correctly not treated as leaves). It also reports, per leaf, whether the child
index already exists and whether it is already attached — that is what makes
re-discovery/resume possible (Q3).

```sql
WITH RECURSIVE tree AS (
  SELECT c.oid, c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = ? AND c.relname = ?
  UNION ALL
  SELECT c2.oid, c2.relkind FROM tree t
  JOIN pg_inherits i ON i.inhparent = t.oid
  JOIN pg_class c2 ON c2.oid = i.inhrelid
)
SELECT n.nspname AS leaf_schema, c.relname AS leaf_name,
       (ci.oid IS NOT NULL)      AS child_idx_exists,
       (ii.inhrelid IS NOT NULL) AS child_idx_attached
FROM tree t
JOIN pg_class c ON c.oid = t.oid
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_class ci ON ci.relname = ? || '_' || c.relname AND ci.relnamespace = n.oid
LEFT JOIN pg_inherits ii ON ii.inhrelid = ci.oid
WHERE t.relkind = 'r'
ORDER BY c.relname;
```

### The statement array is genuinely variable-length, derived from the catalog

Two different tables, same Change class, same jar, same run mechanism:

| table | leaves discovered | statements emitted |
|---|---|---|
| `person` | 4 | **9**  (= 1 parent + 4x2) |
| `person2` | 7 | **15** (= 1 parent + 7x2) |

```
$ mvn liquibase:update -Dchangelog=changelogs/q2-seven.xml
[POC] DISCOVERY: parentIndexExists=false leafCount=7
[POC] EMITTING 15 statements for 7 leaves
[INFO] ChangeSet changelogs/q2-seven.xml::q2-seven::poc ran successfully in 56ms
[INFO] BUILD SUCCESS
```

Nothing about the count is authored in the changelog. The changelog names only
schema/table/index and one column.

**Caveat carried over from Q1a:** because `generateStatements` is called ~7
times per update, this discovery query runs ~7 times unless memoised.

## Q3 — Failure and resume: **re-discovery WORKS. Liquibase does not fight it.**

This is the single most important result in the POC, so it is written out in
full. Setup: table `q3a`, 4 range partitions, 4000 rows. The Change was given
`poisonLeafOrdinal="3"`, which replaces the 3rd leaf's `CREATE INDEX
CONCURRENTLY` with one naming a column that does not exist. Changeset carried
`runInTransaction="false"`.

### Run 1 — fails on leaf 3 of 4

```
[POC] EMITTING 9 statements for 4 leaves
...
[ERROR] ChangeSet changelogs/q3-poison-notx.xml::q3-poison::poc encountered an exception.
liquibase.exception.DatabaseException: ERROR: column "NO_SUCH_COLUMN_POISON" does not exist [Failed SQL: (0) CREATE INDEX CONCURRENTLY "idx_q3a_q3a_p03" ON "public"."q3a_p03" ("NO_SUCH_COLUMN_POISON")]
[INFO] BUILD FAILURE
```

**Is the changeset written to `DATABASECHANGELOG`? NO.**
```
SELECT id, author, filename, exectype, orderexecuted FROM databasechangelog ORDER BY orderexecuted;
     id      | author |            filename            | exectype | orderexecuted
-------------+--------+--------------------------------+----------+---------------
 q1a-notx    | poc    | changelogs/q1a-simple-notx.xml | EXECUTED |             1
 q2-discover | poc    | changelogs/q2-discover.xml     | EXECUTED |             2
 q2-seven    | poc    | changelogs/q2-seven.xml        | EXECUTED |             3
(3 rows)
```
`q3-poison` is absent. A failed changeset leaves **no** row — not an
`EXECUTED` row, not a `FAILED` row, nothing.

**Do the statements that already succeeded survive? YES, all of them.**
```
SELECT c.relname, c.relkind, i.indisvalid FROM pg_class c JOIN pg_index i ON i.indexrelid=c.oid
  WHERE c.relname LIKE 'idx_q3a%' ORDER BY 1;
     relname     | relkind | indisvalid
-----------------+---------+------------
 idx_q3a         | I       | f          <-- parent, still deliberately INVALID
 idx_q3a_q3a_p01 | i       | t
 idx_q3a_q3a_p02 | i       | t

SELECT ch.relname FROM pg_inherits ii JOIN pg_class p ON p.oid=ii.inhparent
  JOIN pg_class ch ON ch.oid=ii.inhrelid WHERE p.relname='idx_q3a';
 idx_q3a_q3a_p01
 idx_q3a_q3a_p02
```
The two completed leaves are built **and attached**. The parent index survives
in its invalid state, which is exactly the correct intermediate state.

The changelog lock is released cleanly on failure — no manual
`releaseLocks` needed:
```
SELECT id, locked, lockedby FROM databasechangeloglock;
 id | locked | lockedby
----+--------+----------
  1 | f      |
```

### Run 2 — re-running retries the changeset, and re-discovery narrows the work

Identical changelog, unchanged, re-run:

```
$ mvn liquibase:update -Dchangelog=changelogs/q3-poison-notx.xml
[INFO] Running Changeset: changelogs/q3-poison-notx.xml::q3-poison::poc
[POC] DISCOVERY: parentIndexExists=true leafCount=4
[POC]   leaf public.q3a_p01 childIdxExists=true  attached=true
[POC]   leaf public.q3a_p02 childIdxExists=true  attached=true
[POC]   leaf public.q3a_p03 childIdxExists=false attached=false
[POC]   leaf public.q3a_p04 childIdxExists=false attached=false
[POC] EMITTING 4 statements for 4 leaves
[POC]   > CREATE INDEX CONCURRENTLY "idx_q3a_q3a_p03" ON "public"."q3a_p03" ("NO_SUCH_COLUMN_POISON")
[POC]   > ALTER INDEX "public"."idx_q3a" ATTACH PARTITION "public"."idx_q3a_q3a_p03"
[POC]   > CREATE INDEX CONCURRENTLY "idx_q3a_q3a_p04" ON "public"."q3a_p04" ("address")
[POC]   > ALTER INDEX "public"."idx_q3a" ATTACH PARTITION "public"."idx_q3a_q3a_p04"
```

**9 statements on run 1, 4 statements on run 2.** Liquibase re-ran the
changeset (because it was never recorded), called `generateStatements` again,
and our re-discovery emitted only the outstanding work. **This is the resume
strategy and it works, unmodified, with no Liquibase-side cooperation
required.**

### Run 3 — poison removed, resume completes

```
[POC] EMITTING 4 statements for 4 leaves
[POC]   > CREATE INDEX CONCURRENTLY "idx_q3a_q3a_p03" ON "public"."q3a_p03" ("address")
[POC]   > ALTER INDEX "public"."idx_q3a" ATTACH PARTITION "public"."idx_q3a_q3a_p03"
[POC]   > CREATE INDEX CONCURRENTLY "idx_q3a_q3a_p04" ON "public"."q3a_p04" ("address")
[POC]   > ALTER INDEX "public"."idx_q3a" ATTACH PARTITION "public"."idx_q3a_q3a_p04"
```
```
SELECT id, exectype, orderexecuted FROM databasechangelog ORDER BY orderexecuted;
 q3-poison   | EXECUTED |             4      <-- recorded only now, on success

SELECT c.relname, i.indisvalid FROM pg_class c JOIN pg_index i ON i.indexrelid=c.oid
  WHERE c.relname LIKE 'idx_q3a%';
 idx_q3a         | t     <-- parent auto-validated on the final ATTACH
 idx_q3a_q3a_p01 | t
 idx_q3a_q3a_p02 | t
 idx_q3a_q3a_p03 | t
 idx_q3a_q3a_p04 | t
```

Note that the changelog XML was **edited** between run 2 and run 3
(`poisonLeafOrdinal="3"` removed), which changes the changeset checksum — and
nothing complained. Because the changeset was never recorded, there is no
stored checksum to validate against. **Checksum drift is not a problem for the
failure/resume path.** (It is a problem after success — see the trap below.)

### Does `runInTransaction="false"` change the answers? YES, decisively — for survival.

Direct A/B, same Change class, same poison on leaf 3, on two identical
4-partition tables. To make the comparison possible at all the choreography
was switched to non-concurrent (`concurrently="false"`), since plain CIC
cannot run in a transaction:

| | `q3b` — default (transactional) | `q3c` — `runInTransaction="false"` |
|---|---|---|
| Statements emitted | 9 | 9 |
| Outcome | BUILD FAILURE | BUILD FAILURE |
| Row in `DATABASECHANGELOG` | **none** | **none** |
| Indexes surviving | **none** | **parent (invalid) + 2 leaf indexes, attached** |

```
=== q3b (DEFAULT transaction) survivors ===
 relname | indisvalid
---------+------------
(0 rows)                    <-- everything rolled back

=== q3c (runInTransaction=false) survivors ===
     relname     | indisvalid
-----------------+------------
 idx_q3c         | f
 idx_q3c_q3c_p01 | t
 idx_q3c_q3c_p02 | t
```

So:
- **Changelog bookkeeping is identical either way** — a failed changeset is
  never recorded, and is always retried on the next run.
- **Partial progress only survives under `runInTransaction="false"`.** Under
  the default, all completed work is rolled back and every re-run starts from
  zero — which at 400 partitions means a failure at partition 399 throws away
  everything.

This is a happy alignment: the flag CIC *requires* (Q1) is the same flag
resume *requires*. There is no tension between them.

### The trap: after SUCCESS, the changeset is skipped forever

Once recorded, a re-run does not call `generateStatements` at all:
```
$ mvn liquibase:update -Dchangelog=changelogs/q3-poison-notx.xml
Run:                          0
Previously run:               1
Filtered out:                 0
[INFO] BUILD SUCCESS
```
(no `[POC] DISCOVERY` line — the Change is never instantiated for execution.)

A 5th partition was then created and the changelog re-run:
```
$ docker exec -i partitionctl-pocspike psql -U postgres -d postgres \
    -c "CREATE TABLE q3a_p05 PARTITION OF q3a FOR VALUES FROM ('2019-01-01') TO ('2020-01-01');"
$ mvn liquibase:update -Dchangelog=changelogs/q3-poison-notx.xml
Run:                          0
Previously run:               1
[INFO] BUILD SUCCESS
```
The Change did not run. **Liquibase's own bookkeeping gives you no coverage
guarantee for partitions added after the changeset succeeded** — the changeset
is done, forever, and re-discovery never gets a chance.

In this particular case PostgreSQL rescued it, which implementers must not
mistake for the extension working:
```
SELECT indexname, indexdef FROM pg_indexes WHERE tablename='q3a_p05';
 q3a_p05_address_idx | CREATE INDEX q3a_p05_address_idx ON public.q3a_p05 USING btree (address)
```
Because the parent partitioned index was **valid**, `CREATE TABLE ... PARTITION
OF` auto-created and auto-attached a matching child index — with PostgreSQL's
own generated name, not ours. Two consequences:
1. Coverage of *newly created* partitions is handled by PostgreSQL for free,
   but the index name will not follow our naming convention. **Discovery must
   therefore key "is this leaf covered?" off `pg_inherits` from the parent
   index OID, not off the child index name.** The POC's name-based check
   (`indexName || '_' || leafname`) is wrong for this case and would report a
   covered leaf as uncovered.
2. Partitions brought in with `ALTER TABLE ... ATTACH PARTITION` carrying
   existing rows are the dangerous case — PostgreSQL builds the child index
   non-concurrently there. That is a PostgreSQL behavior, not a Liquibase one,
   but it belongs in the docs.

If a changeset must re-check coverage on every run, it needs `runAlways="true"`
(not tested here) and the Change must then be a genuine no-op when there is
nothing to do.

### The worst finding in the POC: an interrupted CIC makes the *next* run report success while leaving a permanently unusable index

This was not on the question list. It was found while testing resume and it is
more dangerous than anything Q1–Q3 turned up.

An interrupted `CREATE INDEX CONCURRENTLY` leaves an **invalid** child index
behind, with exactly the name the naming convention would produce. Simulated
deterministically with a statement timeout on a 900k-row partition:

```
SET statement_timeout='60ms';
CREATE INDEX CONCURRENTLY "idx_q9_q9_p01" ON q9_p01 (address);
ERROR:  canceling statement due to statement timeout

SELECT c.relname, i.indisvalid, i.indisready FROM pg_class c JOIN pg_index i ON i.indexrelid=c.oid
  WHERE c.relname LIKE 'idx_q9%';
    relname    | indisvalid | indisready
---------------+------------+------------
 idx_q9_q9_p01 | f          | f
```

The Change was then run against that table. Re-discovery saw
`childIdxExists=true` for `q9_p01` (name matches), skipped the rebuild, and
emitted only the ATTACH:

```
[POC] EMITTING 4 statements for 2 leaves
[POC]   > CREATE INDEX "idx_q9" ON ONLY "public"."q9" ("address")
[POC]   > ALTER INDEX "public"."idx_q9" ATTACH PARTITION "public"."idx_q9_q9_p01"
[POC]   > CREATE INDEX CONCURRENTLY "idx_q9_q9_p02" ON "public"."q9_p02" ("address")
[POC]   > ALTER INDEX "public"."idx_q9" ATTACH PARTITION "public"."idx_q9_q9_p02"
```

**PostgreSQL accepted the ATTACH of an invalid index without complaint**, and
the result is:

```
    relname    | relkind | indisvalid | indisready
---------------+---------+------------+------------
 idx_q9        | I       | f          | t          <-- parent PERMANENTLY INVALID
 idx_q9_q9_p01 | i       | f          | f          <-- invalid, but ATTACHED
 idx_q9_q9_p02 | i       | t          | t

SELECT ch.relname FROM pg_inherits ii ... WHERE p.relname='idx_q9';
 idx_q9_q9_p01
 idx_q9_q9_p02      <-- both attached, so no further ATTACH will ever auto-validate the parent
```

And Liquibase recorded it as a clean success:
```
SELECT id, exectype FROM databasechangelog WHERE id LIKE 'q9%';
      id       | exectype
---------------+----------
 q9-visibility | EXECUTED
```

So: **BUILD SUCCESS, changeset EXECUTED, and an index the planner will never
use.** Because the changeset is now recorded, re-running can never repair it
(Q3 trap). Silent, permanent, and indistinguishable from success in every
Liquibase-visible signal.

Mandatory mitigations for the real implementation:
1. Discovery must select `pg_index.indisvalid` (and `indisready`) per child
   index. A child that exists but is invalid must be treated as **absent**:
   `DROP INDEX CONCURRENTLY` it, then rebuild — never ATTACH it.
2. After the final ATTACH, verify the parent's `indisvalid` and fail the
   changeset if it is false. Otherwise a broken outcome is recorded as done.
   A `partitionctlIndexGate`-style precondition, or a trailing verification
   statement, is the natural place for this.

A different flavour of the same trap: if the leftover index has a *different*
definition (e.g. a leftover UNIQUE index where a plain one is wanted), the
ATTACH fails loudly instead, which is the better outcome:
```
ERROR: cannot attach index "idx_q9_q9_p01" as a partition of index "idx_q9"
  Detail: The index definitions do not match. [Failed SQL: (0) ALTER INDEX "public"."idx_q9" ATTACH PARTITION "public"."idx_q9_q9_p01"]
```
Loud failure when definitions differ, silent corruption when they match but the
child is invalid.

---

## Q4 — Nested `<column>` elements: works, but the owner's exact syntax needs a permissive XSD

Implement `ChangeWithColumns<ColumnConfig>`; `AbstractChange.load()` binds the
nested elements by reflection, exactly as `CreateIndexChange` does. No
`load()`/`ParsedNode` override needed.

```java
public class CreatePartitionedTableIndexChange extends AbstractChange
        implements ChangeWithColumns<ColumnConfig> {
    private List<ColumnConfig> columns = new ArrayList<>();
    public List<ColumnConfig> getColumns() { return columns; }
    public void setColumns(List<ColumnConfig> columns) { this.columns = columns; }
    public void addColumn(ColumnConfig column) { this.columns.add(column); }
}
```
`ColumnConfig` already carries `getDescending()`/`setDescending(Boolean)`, so
`descending="true"` is free.

**The catch.** The owner's target XML uses an *unprefixed* `<column>`:
```xml
<createPartitionedTableIndex indexName="idx_personaddress" ...>
    <column descending="true" name="address"/>
</createPartitionedTableIndex>
```
An unprefixed child inherits the document's default namespace, which is the
base dbchangelog namespace — so with `elementFormDefault="qualified"` and an
explicit `<xsd:element name="column">` in our schema, it is rejected:

```
[ERROR] cvc-complex-type.2.4.a: Invalid content was found starting with element
'{"http://www.liquibase.org/xml/ns/dbchangelog":column}'. One of
'{"http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl":column}' is expected.
[ERROR] liquibase.exception.ChangeLogParseException: Error parsing line 12 column 55
of changelogs/q4-unprefixed.xml: cvc-complex-type.2.4.a: ...
```

Fix — declare the child particle as `xsd:any` with lax processing:
```xml
<xsd:sequence>
    <xsd:any namespace="##any" processContents="lax" minOccurs="1" maxOccurs="unbounded"/>
</xsd:sequence>
```
After rebuilding with that XSD, the owner's exact unprefixed syntax parses and
binds correctly:
```
[POC] EMITTING 5 statements for 2 leaves
[POC]   > CREATE INDEX "idx_q4" ON ONLY "public"."q4" ("address" DESC)
[POC]   > CREATE INDEX CONCURRENTLY "idx_q4_q4_p01" ON "public"."q4_p01" ("address" DESC)
...
```
The `DESC` proves `descending="true"` bound through despite the namespace
mismatch — Liquibase's `ParsedNode` binding keys on the local element name and
ignores the namespace, consistent with the precondition finding in
`liquibase-mechanism.md`.

**Trade-off to decide deliberately:** `xsd:any`+`lax` buys the friendly syntax
but gives up per-attribute validation on the nested element, which is exactly
the protection that Q4 of the earlier spike identified as valuable. Attributes
on the *parent* element are still validated.

## Q5 — `SET lock_timeout` / `SET statement_timeout` between statements: **they take effect. Confirmed.**

A probe Change emitting two or three statements in one array, changeset
`runInTransaction="false"`:

`mode="tight"` — `SET statement_timeout='50ms'` then `SELECT pg_sleep(1)`:
```
[INFO] ERROR: Exception Primary Reason:  ERROR: canceling statement due to statement timeout
[INFO] BUILD FAILURE
```
`mode="reset"` — `SET statement_timeout='50ms'`, `SET statement_timeout=0`,
`SELECT pg_sleep(1)`:
```
[INFO] ChangeSet changelogs/q5-reset.xml::q5-reset::poc ran successfully in 1006ms
[INFO] BUILD SUCCESS
```

The 1006ms confirms the sleep actually ran to completion after the reset. So
statements from one `SqlStatement[]` all execute on the same session, in
order, and `SET` persists across them. Emitting `SET lock_timeout = '15min'`
and `SET statement_timeout = 0` as the first two statements works exactly as
the plan assumed.

## Q6 — Checksum covers the XML, not the generated SQL

**Adding a partition does NOT change the checksum and does NOT trigger
validation failure.** After `q4-unprefixed` had succeeded, a third partition
was added and the unchanged changelog re-run:
```
$ docker exec ... psql -c "CREATE TABLE q4_p03 PARTITION OF q4 FOR VALUES FROM ('2017-01-01') TO ('2018-01-01');"
$ mvn liquibase:update -Dchangelog=changelogs/q4-unprefixed.xml
Run:                          0
Previously run:               1
[INFO] BUILD SUCCESS
```
No checksum error — and also no work done, per the Q3 trap.

**Editing the XML DOES trigger it.** Changing `descending="true"` to
`descending="false"` in the same changeset:
```
[ERROR] liquibase.exception.ValidationFailedException: Validation Failed:
     1 changesets check sum
          changelogs/q4-unprefixed.xml::q4-unprefixed::poc was: 9:ff4bd99375eee72d28cf414c0a33b71b but is now: 9:4dbcfbea5063982d7210333c26394d88
[INFO] BUILD FAILURE
```

So the checksum is computed over the serialized change definition, not over
what `generateStatements` returns. Practical consequence: **catalog drift is
invisible to Liquibase's integrity checking.** Nothing fails when the partition
set changes; the changeset is simply considered done. Any drift/coverage
guarantee has to be built by us (a precondition, or a `runAlways` no-op
Change), it does not come from the checksum.

## Q7 — Operator visibility during a long run: essentially silent

Default log level, one changeset emitting 7 statements across 3 partitions —
this is the **entire** output between lock acquisition and completion:
```
[INFO] Successfully acquired change log lock
[INFO] Using deploymentId: 6220174108
[INFO] Reading from databasechangelog
[INFO] Running Changeset: changelogs/q7-visibility.xml::q7-visibility::poc
[INFO] createPartitionedTableIndex: idx_q7 across all partitions of public.q7
[INFO] ChangeSet changelogs/q7-visibility.xml::q7-visibility::poc ran successfully in 42ms
```
One line per changeset. **No per-statement output at all.** Raising the level
does not help: `-Dliquibase.logLevel=DEBUG` produced 87 lines total, of which
**zero** matched `CREATE INDEX` or `ALTER INDEX`.

That means a 400-partition run that takes hours prints one line, then nothing,
then one line. For adoption this is the weakest point of the design. Note that
the `getConfirmationMessage()` string is printed *before* the statements run,
not after, so it cannot report what actually happened.

Two mitigations exist, both partial:
- The extension's own `System.out` writes do reach the console (all the `[POC]`
  lines above are ours). But `generateStatements` runs *before* execution, so
  it can only announce the plan, not report progress through it.
- `liquibase:updateSQL` is a genuine preview and is the replacement for
  `partitionctl render`. It ran discovery and wrote the full plan to
  `target/liquibase/migrate.sql`:
```
CREATE INDEX "idx_q8" ON ONLY "public"."q8" ("address");
CREATE INDEX CONCURRENTLY "idx_q8_q8_p01" ON "public"."q8_p01" ("address");
ALTER INDEX "public"."idx_q8" ATTACH PARTITION "public"."idx_q8_q8_p01";
CREATE INDEX CONCURRENTLY "idx_q8_q8_p02" ON "public"."q8_p02" ("address");
ALTER INDEX "public"."idx_q8" ATTACH PARTITION "public"."idx_q8_q8_p02";
```

Real per-statement progress would need a custom `SqlStatement` /
`SqlGenerator` pair, or a `ChangeExecListener` — neither was tested here.

---

## What to build

Everything below is running code from the POC tree, not a sketch.

### `pom.xml`
Unchanged from `liquibase-mechanism.md` — `liquibase-core:4.33.0`, `provided`
scope, `maven.compiler.release` 8, no shade/assembly plugin. Always
`mvn clean install`.

### ServiceLoader registration
`src/main/resources/META-INF/services/liquibase.change.Change`:
```
com.partitionctl.liquibase.CreatePartitionedTableIndexChange
```
(Changes register under `liquibase.change.Change`; preconditions keep their own
`liquibase.precondition.Precondition` file. Both can ship in one jar — this POC
does exactly that.)

### XSD
`src/main/resources/www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl.xsd`
```xml
<xsd:element name="createPartitionedTableIndex">
    <xsd:complexType>
        <xsd:sequence>
            <!-- ##any + lax so the unprefixed <column .../> form works (Q4) -->
            <xsd:any namespace="##any" processContents="lax"
                     minOccurs="1" maxOccurs="unbounded"/>
        </xsd:sequence>
        <xsd:attribute name="schemaName" type="xsd:string" use="required"/>
        <xsd:attribute name="tableName"  type="xsd:string" use="required"/>
        <xsd:attribute name="indexName"  type="xsd:string" use="required"/>
    </xsd:complexType>
</xsd:element>
```
Attribute names must match the Java property names exactly — a mismatch binds
to `null` silently (proven in `liquibase-mechanism.md` Q4).

### Changeset XML (the proven pattern)
```xml
<changeSet id="1" author="x" runInTransaction="false">
    <ext:createPartitionedTableIndex indexName="idx_personaddress"
                                     schemaName="public" tableName="person">
        <column descending="true" name="address"/>
    </ext:createPartitionedTableIndex>
</changeSet>
```
`runInTransaction="false"` is **mandatory**, for two independent reasons (Q1
and Q3). The Change should refuse to run without it.

### The Change class
The full working class is
`ext/src/main/java/com/partitionctl/liquibase/CreatePartitionedTableIndexChange.java`
in the POC tree. Its load-bearing parts:

```java
@DatabaseChange(name = "createPartitionedTableIndex",
                description = "...", priority = ChangeMetaData.PRIORITY_DEFAULT)
public class CreatePartitionedTableIndexChange extends AbstractChange
        implements ChangeWithColumns<ColumnConfig> {

    @Override
    public String getSerializedObjectNamespace() {
        return "http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl";
    }

    // MUST be true: the statement list depends on live catalog state.
    @Override
    public boolean generateStatementsVolatile(Database database) { return true; }

    @Override
    public SqlStatement[] generateStatements(Database database) {
        Connection c = ((JdbcConnection) database.getConnection()).getUnderlyingConnection();
        // 1. does the parent index already exist?  2. enumerate leaves + their index state
        // 3. build a variable-length List<SqlStatement>, skipping already-done work
        // 4. return list.toArray(new SqlStatement[0]);
    }
}
```

The POC's own known defects, to fix in the real implementation:
1. Leaf coverage is detected by child-index **name**
   (`indexName || '_' || leafname`). Must instead be detected via `pg_inherits`
   from the parent index OID, or PostgreSQL-auto-created child indexes are
   misreported as missing (Q3 trap).
2. No memoisation — discovery runs on all ~7 `generateStatements` calls (Q1).
3. No `NAMEDATALEN` (63-byte) truncation on generated child index names.
4. `validate()` does not assert `getChangeSet().isRunInTransaction() == false`.
   It should.
5. Invalid leftovers from an interrupted CIC are not detected — **tested, and
   it silently produces a permanently invalid index recorded as `EXECUTED`**
   (full evidence at the end of Q3). Discovery must select
   `pg_index.indisvalid`/`indisready` per child, treat an invalid child as
   absent, `DROP INDEX CONCURRENTLY` it and rebuild; and the change must verify
   the parent index is valid before allowing the changeset to succeed. This is
   the single most important fix on this list.

### How to reproduce
```
docker run -d --name partitionctl-pocspike -e POSTGRES_PASSWORD=pw -p 5434:5432 postgres:17
cd <poc>/ext    && mvn clean install
cd <poc>/runner && MAVEN_OPTS=-Duser.timezone=UTC \
                   mvn liquibase:update -Dchangelog=changelogs/q2-discover.xml
```
Changelogs in `runner/changelogs/`: `q1a-simple-notx.xml`, `q1a-simple-tx.xml`,
`q2-discover.xml`, `q2-seven.xml`, `q3-poison-notx.xml`, `q3-nonconc-tx.xml`,
`q3-nonconc-notx.xml`, `q4-unprefixed.xml`, `q5-tight.xml`, `q5-reset.xml`,
`q7-visibility.xml`, `q8-preview.xml`, `q9-leftover.xml`.

The Postgres fixtures each test used (`ord`, `person`, `person2`, `q3a`–`q3c`,
`q4`, `q7`–`q9`) are created by the `psql` snippets quoted inline above; the
container itself is disposable.

`MAVEN_OPTS=-Duser.timezone=UTC` is required on this machine — without it the
PostgreSQL JDBC driver fails to connect with
`FATAL: invalid value for parameter "TimeZone"` (`Asia/Calcutta` vs
`Asia/Kolkata`), same as noted in `liquibase-mechanism.md`.
