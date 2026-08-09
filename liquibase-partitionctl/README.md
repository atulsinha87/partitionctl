# liquibase-partitionctl

Create, reindex and drop indexes on **partitioned** PostgreSQL tables from inside
`liquibase update`, without blocking writes.

`CREATE INDEX CONCURRENTLY` is rejected outright on a partitioned parent. The documented
workaround is a per-partition choreography — build the parent `ON ONLY`, build one index
concurrently per leaf, attach each one — that has to be generated from the live catalog because
the statement list depends on how many partitions exist today. This extension does that, and
resumes correctly when a run is interrupted.

---

## Before you start: what is not ready

**This artifact is not published anywhere.** `io.github.atulsinha87` returns HTTP 404 on Maven
Central — the whole groupId, not just this artifact. The version is `0.1.0-SNAPSHOT`, and Maven
Central never accepts `-SNAPSHOT` versions. The `pom.xml` also has none of the plumbing Central
requires (no `maven-gpg-plugin`, no source or javadoc jar, no `<scm>`, no `<developers>`, no
`distributionManagement`).

So the coordinate below **will not resolve** from Maven Central today. The only install path that
works is building from source:

```bash
git clone <this repository>
cd liquibase-partitionctl && mvn clean install     # installs 0.1.0-SNAPSHOT into your ~/.m2
```

Everything else in this document is exercised and true. Licensing is Apache 2.0 (`LICENSE` at the
repository root); the copyright holder line in `NOTICE` is still a placeholder the project owner
must fill in.

---

## Quickstart

### 1. Put the extension on Liquibase's classpath

It goes inside the **`liquibase-maven-plugin`'s own `<dependencies>`**, not your project's. This
is the single most common way to get a "no declaration can be found" error.

```xml
<plugin>
  <groupId>org.liquibase</groupId>
  <artifactId>liquibase-maven-plugin</artifactId>
  <version>4.33.0</version>
  <configuration>
    <changeLogFile>changelogs/changelog.xml</changeLogFile>
    <url>jdbc:postgresql://localhost:5432/mydb</url>
    <username>myuser</username>
    <password>mypassword</password>
  </configuration>
  <dependencies>
    <dependency>
      <groupId>org.postgresql</groupId>
      <artifactId>postgresql</artifactId>
      <version>42.7.4</version>
    </dependency>
    <dependency>
      <groupId>io.github.atulsinha87</groupId>
      <artifactId>liquibase-partitionctl</artifactId>
      <version>0.1.0-SNAPSHOT</version>
    </dependency>
  </dependencies>
</plugin>
```

`liquibase-core` is a `provided` dependency of this jar, so it never drags in a second copy. The
jar contains **zero** Liquibase classes and no transitive dependencies at all.

### 2. Copy this changelog

Every line of the header matters. Both namespace declarations, **both** `xsi:schemaLocation`
pairs, `runInTransaction="false"` and `runAlways="true"`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<databaseChangeLog
    xmlns="http://www.liquibase.org/xml/ns/dbchangelog"
    xmlns:ext="http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl"
    xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
    xsi:schemaLocation="http://www.liquibase.org/xml/ns/dbchangelog
                        http://www.liquibase.org/xml/ns/dbchangelog/dbchangelog-latest.xsd
                        http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl
                        http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl.xsd">

  <changeSet author="you" id="1" runInTransaction="false" runAlways="true">
    <ext:createPartitionedTableIndex indexName="idx_orders_created"
                                     schemaName="public"
                                     tableName="orders">
      <column name="created_at" descending="true"/>
      <column name="customer_id"/>
    </ext:createPartitionedTableIndex>
  </changeSet>

</databaseChangeLog>
```

Nothing is fetched over the network. Liquibase strips the protocol from that second
`schemaLocation` URL and resolves it as a **classpath resource**, which the jar provides.

### 3. Run it

```bash
mvn liquibase:update
```

### What success looks like

One line per partition, printed **while** the run happens, at the default log level:

```
[INFO] Running Changeset: changelogs/changelog.xml::1::you
[INFO] [partitionctl] parent index public.idx_orders_created ON ONLY -- invalid until the final ATTACH
[INFO] [partitionctl] [ 1/12] public.orders_2024_01  build + attach
[INFO] [partitionctl] [ 2/12] public.orders_2024_02  build + attach
...
[INFO] [partitionctl] [12/12] public.orders_2024_12  build + attach
[INFO] [partitionctl] verifying public.idx_orders_created over 12 leaf partition(s)
[INFO] partitionctl: public.idx_orders_created VALID, 12 of 12 leaf partitions covered
[INFO] BUILD SUCCESS
```

Verify it yourself from the catalog rather than from that log:

```sql
SELECT count(*) FILTER (WHERE ix.indisvalid) AS valid_children,
       (SELECT indisvalid FROM pg_index WHERE indexrelid = 'public.idx_orders_created'::regclass)
         AS parent_valid
  FROM pg_inherits ii
  JOIN pg_index ix ON ix.indexrelid = ii.inhrelid
 WHERE ii.inhparent = 'public.idx_orders_created'::regclass;
```

**A second run is a no-op.** It emits exactly one statement, the verification block, and no DDL.
That is what makes `runAlways="true"` safe.

### What failure looks like

Failures are loud, and they say what was and was not done:

```
[ERROR] partitionctl: public.orders is a MULTI-LEVEL partitioned table -- 2 of its partition(s)
        are themselves partitioned. ... Nothing was executed.
```

A failed changeset is **not written to DATABASECHANGELOG at all** — not even as a failed marker.
So fixing the cause and re-running retries it, and re-discovery emits only the work still
outstanding. There is no state file to clean up and no `--force` to remember.

---

## Why `runInTransaction="false"` and `runAlways="true"`

**`runInTransaction="false"` is mandatory.** The change refuses to run without it. One flag, two
properties: `CREATE INDEX CONCURRENTLY` cannot run inside a transaction block at all, and partial
progress has to survive a failure for the resume-by-re-running design to work.

**`runAlways="true"` is strongly recommended.** A changeset that succeeded is never re-run, so
partitions created *after* it ran would silently go unindexed. With `runAlways` the extension
re-discovers on every deploy and covers new partitions automatically — and does nothing, in about
15 ms, when there is nothing to do.

One consequence to plan for: `runAlways` means a changeset pointed at a table that is later
**dropped** fails on every deploy forever. Pair it with a precondition so a retired table
degrades to a skip instead of a permanent red build:

```xml
<changeSet author="you" id="1" runInTransaction="false" runAlways="true">
  <preConditions onFail="MARK_RAN">
    <tableExists schemaName="public" tableName="orders"/>
  </preConditions>
  <ext:createPartitionedTableIndex indexName="idx_orders_created"
                                   schemaName="public" tableName="orders">
    <column name="created_at"/>
  </ext:createPartitionedTableIndex>
</changeSet>
```

---

## Reference

### `<ext:createPartitionedTableIndex>`

Builds an index over every leaf partition. Requires `runInTransaction="false"`.

| Attribute | Required | Default | What it does |
|---|---|---|---|
| `schemaName` | yes | — | Schema of the partitioned table. |
| `tableName` | yes | — | The partitioned table (`relkind = 'p'`). |
| `indexName` | yes | — | Name of the partitioned parent index. Child names are derived from it and clipped to 63 bytes; a derivation that would collide fails loudly before anything runs. |
| `unique` | no | `false` | `CREATE UNIQUE INDEX`. PostgreSQL requires every partitioning column to be included and names the missing one if not. |
| `using` | no | `btree` | Access method: `btree`, `gin`, `gist`, `brin`, `hash`, `spgist`. Emitted as a quoted identifier, so it is **case-sensitive** — write it lowercase. |
| `where` | no | none | Partial-index predicate, without the `WHERE` keyword. **Raw SQL** — see the security note below. |
| `lockTimeout` | no | `15min` | `lock_timeout` for each `CREATE INDEX CONCURRENTLY`. That statement takes only `ShareUpdateExclusiveLock`, so waiting is cheap and the value is generous. |
| `attachLockTimeout` | no | `30s` | `lock_timeout` for `ALTER INDEX ... ATTACH PARTITION`, which takes `AccessExclusiveLock` on the child index. Short, because a queued exclusive request blocks everything behind it. |
| `paceSeconds` | no | none | Seconds to sleep between partitions, to spread I/O on a large table. |

Child elements: one or more `<column name="..." descending="true|false"/>`. Both `<column/>` and
`<ext:column/>` work.

**The index shape is honoured at build time only.** Coverage is decided from `pg_inherits` — is
this leaf attached to this parent index — and never from the index definition, because that is what
makes a partition PostgreSQL indexed itself count as covered. Consequence: pointing a *new*
changeset at an existing index with a different `where` or `unique` emits **zero** statements and
leaves the original in place. Changing the shape of an index that exists means dropping it and
building it again.

**`where` is raw SQL and is not escaped.** It cannot be: a predicate is an arbitrary expression, so
there is no parameter to bind it to. It is concatenated verbatim into `CREATE INDEX`, exactly as
the body of a Liquibase `<sql>` tag is. Anyone who can edit a changelog can already run arbitrary
SQL through Liquibase, so this opens no new door — but a changelog **generated** by string
concatenation from untrusted input would carry that injection straight through, and nothing here
would catch it.

### `<ext:reindexPartitionedTableIndex>`

Rebuilds every leaf index of an existing partitioned index with `REINDEX INDEX CONCURRENTLY`, one
partition at a time. Requires `runInTransaction="false"`.

| Attribute | Required | Default | What it does |
|---|---|---|---|
| `schemaName` | yes | — | Schema of the partitioned table. |
| `tableName` | yes | — | The partitioned table. |
| `indexName` | yes | — | The existing partitioned index to rebuild. |
| `lockTimeout` | no | `15min` | `lock_timeout` for each rebuild and for dropping leftovers. |
| `paceSeconds` | no | none | Seconds to sleep after each partition that was actually rebuilt. |

There is no `attachLockTimeout`: the rebuild swaps the index in place and the `pg_inherits`
attachment survives, so no `ATTACH` is ever emitted.

**This is not idempotent, and cannot be.** "Was reindexed" is not observable in the catalog — a
rebuilt index differs from its predecessor only in `relfilenode` — and there is no state store. So
a re-run after an interruption rebuilds every partition that has no `_ccold` beside it, including
ones the previous attempt already finished. That is wasted work, not damage. Do **not** put
`runAlways="true"` on a reindex changeset.

### `<ext:dropPartitionedTableIndex>`

Drops a partitioned index, every child index attached to it, and any free-standing leftover an
interrupted build left behind. Requires `runInTransaction="false"`. Do **not** use `runAlways`: a
drop is a one-shot intent.

| Attribute | Required | Default | What it does |
|---|---|---|---|
| `schemaName` | yes | — | Schema of the partitioned table. |
| `tableName` | yes | — | The partitioned table. Checked against the index's real owner, so a wrong `tableName` is refused rather than dropping the right index by accident. |
| `indexName` | yes | — | The partitioned index to drop. |
| `confirmExclusiveLock` | see below | `false` | Acknowledges that dropping an attached tree takes `AccessExclusiveLock` on the table **and every leaf at once**. Required whenever an attached tree is present. |
| `lockTimeout` | no | `15min` | `lock_timeout` for `DROP INDEX CONCURRENTLY` on unattached leftovers, which is fully online. |
| `exclusiveLockTimeout` | no | `5s` | `lock_timeout` for **one lock acquisition** during the exclusive drop. |
| `exclusiveRetries` | no | `5` | Attempts at the exclusive drop, with doubling backoff. A failed attempt releases every lock it took before backing off. |
| `exclusiveTotalTimeout` | no | `5min` | Hard ceiling on the whole retry loop, as `statement_timeout`. |

**Read `exclusiveLockTimeout` and `exclusiveTotalTimeout` together.** `exclusiveLockTimeout` bounds
one lock acquisition, not the statement. `DROP INDEX` on a partitioned parent takes one
`AccessExclusiveLock` per leaf plus two, **holding each while it waits for the next**, so waits add
up: one attempt can hold the table for up to `exclusiveLockTimeout × (leaves + 2)`. Measured on
17.10 with 8 leaves and two contended, at the 5 s default, the drop held the table for 6.9 seconds.
`exclusiveTotalTimeout` is the only bound that is not per-lock. When it fires the statement is
cancelled, and because the retry loop is one transaction the whole thing rolls back — nothing is
dropped, nothing is recorded, and re-running retries.

`confirmExclusiveLock` is required by the *change* when an attached tree is present, not by the
schema, so a leftovers-only cleanup (fully online) and a re-run with nothing left to drop both work
without it.

**Ownership marker.** `createPartitionedTableIndex` stamps every child index it builds with a
`COMMENT ON INDEX` beginning `partitionctl owner=liquibase`, and the drop refuses to destroy a tree
that carries no such evidence anywhere. This is **evidence, not authorisation** — anyone who can run
the changeset can already run `DROP INDEX` by hand, and anyone who can write a comment can forge the
marker. It stops the accident: a copied changelog, a typo'd `indexName`, a collision with an index
the DBA team built. A tree built by the separate **Go CLI** carries a different marker and is
deliberately refused.

### Preconditions

Three read-only gates. All take `schemaName`, `tableName` and `indexName`, all required — the same
names the changes use, and the same names Liquibase's own `<indexExists>` uses.

| Element | Passes when |
|---|---|
| `<ext:partitionctlIndexGate>` | The partitioned index exists and covers every leaf with a valid child index. Optional `requireValidParent="true"` (default `false`) also demands `indisvalid` on the parent itself. |
| `<ext:partitionctlIndexAbsentGate>` | The partitioned index is gone **and** no orphaned child index survives on any leaf. |
| `<ext:partitionctlReindexGate>` | The tree is healthy **and** no `_ccnew`/`_ccold` leftover from an interrupted `REINDEX CONCURRENTLY` remains. |

`requireValidParent` defaults to `false` on purpose. Once an invalid child index has been attached,
the parent's `indisvalid = false` is permanent — no form of `REINDEX` clears it — and the index is
still fully usable. Defaulting to true would fail every deploy forever with no repair path.

```xml
<changeSet author="you" id="99" runInTransaction="false">
  <preConditions onFail="MARK_RAN">
    <ext:partitionctlIndexGate schemaName="public" tableName="orders"
                               indexName="idx_orders_created"/>
  </preConditions>
  <sql>ANALYZE public.orders;</sql>
</changeSet>
```

### Rollback

There is **no automatic inverse**. `mvn liquibase:rollback` on a create changeset with no rollback
block fails with `RollbackImpossibleException: No inverse to ...CreatePartitionedTableIndexChange
created`, and correctly leaves the index in place.

A hand-written rollback block works end to end:

```xml
<changeSet author="you" id="1" runInTransaction="false">
  <ext:createPartitionedTableIndex indexName="idx_orders_created"
                                   schemaName="public" tableName="orders">
    <column name="created_at"/>
  </ext:createPartitionedTableIndex>
  <rollback>
    <ext:dropPartitionedTableIndex indexName="idx_orders_created"
                                   schemaName="public" tableName="orders"
                                   confirmExclusiveLock="true"/>
  </rollback>
</changeSet>
```

`confirmExclusiveLock="true"` is not optional there — without it the rollback refuses.

---

## Troubleshooting

Every error below is verbatim from a real run.

### `Invalid content was found starting with element '...createPartitionedTableIndex'`

```
liquibase.exception.ChangeLogParseException: ... cvc-complex-type.2.4.a: Invalid content was
found starting with element '{"http://www.liquibase.org/xml/ns/dbchangelog":
createPartitionedTableIndex}'. One of '{...}' is expected.
```

where `{...}` is every built-in Liquibase element name. Measured: 7,436 characters of output, and
the error never names this extension, never says a namespace is missing, and never points at the
XSD.

**Cause:** the `xmlns:ext` namespace declaration is missing, so the element landed in the plain
`dbchangelog` namespace. **Fix:** add both lines from the quickstart header — the `xmlns:ext`
declaration *and* the second `xsi:schemaLocation` pair.

This is what you get from pasting a bare `<createPartitionedTableIndex .../>` snippet into a stock
changelog, so it is the first error most people will see.

### `does not accept the attribute "uniq" (did you mean "unique"?)`

```
partitionctl: <createPartitionedTableIndex> does not accept the attributes "uniq" (did you mean
"unique"?), "usin" (did you mean "using"?). Nothing was executed. Left unchecked, a misspelled
attribute binds to null in silence and the change runs with the default instead: uniq="true"
builds a NON-unique index and reports success. Accepted on this element: [...].
```

**Cause:** a typo. **Fix:** the message names it and suggests the right spelling.

Worth knowing *why* this check exists. If the extension XSD is not listed in your
`xsi:schemaLocation` — declaring `xmlns:ext` alone is enough to make the element bind — then XSD
validation never runs on it, and before this check a misspelled attribute was **silently
discarded**. Measured: a changelog asking for `uniq="true" usin="brin" wher="status <> 'archived'"`
reported `BUILD SUCCESS` and built a non-unique, full, btree index. And it was not correctable
afterwards, because coverage keys on `pg_inherits` and a corrected changeset emits zero statements.
The extension now refuses unknown attributes itself, whether or not you referenced the XSD — but
reference it anyway, because it also catches misspelled *elements*.

### `requires runInTransaction="false"`

```
liquibase.exception.ValidationFailedException: Validation Failed:
     1 changes have validation failures
          createPartitionedTableIndex requires runInTransaction="false" on the changeSet
          (CREATE INDEX CONCURRENTLY cannot run inside a transaction block, and partial
          progress must survive a failure for resume to work)
```

**Cause:** the attribute is missing. Liquibase validates the whole changelog before executing any
of it, so nothing at all ran — including earlier changesets. **Fix:** add
`runInTransaction="false"`.

### `at least one <column> is required`

The element name is misspelled. `<colum/>` binds to nothing and leaves the column list empty. With
the XSD referenced you get the better error first: `cvc-complex-type.2.4.c: The matching wildcard
is strict, but no declaration can be found for element 'colum'`.

### `is a MULTI-LEVEL partitioned table`

```
partitionctl: public.orders is a MULTI-LEVEL partitioned table -- 2 of its partition(s) are
themselves partitioned. ALTER INDEX ... ATTACH PARTITION only accepts an index on a DIRECT
partition of the table ... Nothing was executed.
```

**Cause:** sub-partitioning. `ATTACH PARTITION` resolves the child against the table's *direct*
partitions, so an index built on a grandchild can never be attached. **Fix:** use a plain
`CREATE INDEX` on the parent (PostgreSQL walks the whole tree itself, but holds a `ShareLock`
while it does), or point one changeset at each lowest-level partitioned table.

### `already exists as a partitioned index on public.other_table`

**Cause:** index names are unique per schema but say nothing about which table they belong to, so
this is usually a copy-pasted changelog with the `tableName` not updated. **Fix:** correct
`tableName`, or choose a different `indexName`.

### `refusing to drop ... Nothing in the catalog says this plugin built it`

**Cause:** the drop found no `partitionctl owner=liquibase` marker on the parent, on any attached
child, or on any leftover. Either something else built this index, or it predates the marker.
**Fix:** if you are sure, drop it with plain SQL. The message also refuses outright if the parent
carries a *foreign* comment — somebody wrote something there deliberately.

### `canceling statement due to statement timeout`

**Cause:** your `statement_timeout` is finite and something long ran under it. The extension lifts
`statement_timeout` around every concurrent index operation and every pacing sleep, and restores
your value immediately afterwards, so this should not happen on the extension's own statements.
**Fix:** if it does, report it — and note which statement the message names.

### `invalid value for parameter "TimeZone": "Asia/Calcutta"` (macOS)

The JDBC connection fails before Liquibase starts. Not related to this extension.
**Fix:** `export MAVEN_OPTS=-Duser.timezone=UTC`.

### The run was killed and now everything hangs

```
Waiting for changelog lock....
```

A killed JVM leaves `DATABASECHANGELOGLOCK.LOCKED = true`. **Fix:** `mvn liquibase:releaseLocks`,
then re-run. Your partial progress is intact and the re-run continues from it.

### `liquibase:updateToTag` does not exist

There is no such Maven goal. **Fix:** `mvn liquibase:update -Dliquibase.toTag=yourtag`.

---

## Known limitations

| | |
|---|---|
| **L1** | Timeout settings leak past a *failure*. Irreducible under `runInTransaction="false"` without a host-registered `ChangeExecListener`. Narrow in practice: a failed changeset halts the update, so there are no later changesets to leak into. It matters for pooled or embedded hosts and for `failOnError="false"`. |
| **L3** | Once an invalid child index has been attached, the parent's `indisvalid = false` is **permanent**. `REINDEX INDEX`, `REINDEX INDEX CONCURRENTLY`, `REINDEX TABLE` and detach/attach were all tried; none clears it. Only drop and rebuild. The index remains fully usable, so this is reported as a warning, never a failure. |
| **L4** | PostgreSQL 17 has no `ALTER INDEX ... DETACH PARTITION`. `ATTACH` has no inverse, so an attached child index cannot be dropped individually. |
| **L5** | Dropping an attached tree always takes `AccessExclusiveLock` on the parent and every leaf. There is no online alternative. |
| **L11** | No *automatic* rollback. A hand-written `<rollback>` block works — see above. |
| **L12** | Multi-level (sub-partitioned) tables are refused, not supported. |
| **L13** | Reindex is not idempotent; do not use `runAlways` with it. |

## Compatibility

Verified against **PostgreSQL 17.10** and **liquibase-core 4.33.0**, on JDK 22 with Maven 3.9.6.
The jar is compiled to **Java 8 bytecode** (`--release 8`, major version 52), matching Liquibase
4.33.0 throughout, and contains no Liquibase classes and no transitive dependencies.

`REINDEX INDEX CONCURRENTLY` on a partitioned index also works on PostgreSQL 14.23, contrary to
some documentation summaries.
