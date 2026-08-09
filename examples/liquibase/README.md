# Using liquibase-partitionctl from JitPack — a complete example

The smallest working consumer of the extension: one `pom.xml`, one changelog. The extension is
resolved from JitPack exactly as an adopter would resolve it — nothing here is built from this
repository's source, so this example doubles as the release gate: **if it runs, the published
artifact works.**

## Run it against the bundled demo database

From the repository root (needs Docker):

```bash
make lb-adopter
```

That starts a throwaway PostgreSQL 17 with a 12-partition, 1.2M-row `orders` table, runs
`mvn liquibase:update` in this directory, and then verifies the result from `pg_catalog` —
not from Liquibase's log.

Expected output, one line per partition, while it runs:

```
[INFO] [partitionctl] parent index public.idx_orders_created ON ONLY -- invalid until the final ATTACH
[INFO] [partitionctl] [ 1/12] public.orders_2024_01  build + attach
...
[INFO] [partitionctl] [12/12] public.orders_2024_12  build + attach
[INFO] partitionctl: public.idx_orders_created VALID, 12 of 12 leaf partitions covered
[INFO] BUILD SUCCESS
```

Run it a second time: it emits no DDL and finishes in milliseconds. That idempotence is what
makes `runAlways="true"` safe, and it is what covers partitions created after the first run.

## Run this example against your own database

No file edits needed — the connection is parameterised:

```bash
mvn liquibase:update \
  -Ddb.url=jdbc:postgresql://your-host:5432/your-db \
  -Ddb.username=you \
  -Ddb.password=secret
```

Then adapt `changelogs/changelog.xml` to your table — see the next section for exactly what to
change and what must stay.

On macOS, set `MAVEN_OPTS=-Duser.timezone=UTC` if your system timezone is not UTC — the JDBC
connect otherwise fails with `invalid value for parameter "TimeZone"`.

## Adapting this to your own project

### `pom.xml` — copy two blocks, verbatim

If you already have a Maven project with the liquibase-maven-plugin, you take exactly two
things from this example's pom:

1. **The `<pluginRepositories>` block**, as-is. It must be `pluginRepositories` — see the
   mistakes section below for why `<repositories>` fails misleadingly.

2. **The extension dependency, inside the liquibase-maven-plugin's own `<dependencies>`:**

   ```xml
   <dependency>
     <groupId>com.github.atulsinha87.partitionctl</groupId>
     <artifactId>liquibase-partitionctl</artifactId>
     <version>v0.1.2</version>
   </dependency>
   ```

Everything else in this example's pom — the `db.*` properties, the changelog path, the driver —
is scaffolding for the demo; keep whatever your project already has. What must hold in *your*
configuration:

| In your pom | Requirement |
|---|---|
| `<pluginRepositories>` | contains `https://jitpack.io` |
| Plugin's `<dependencies>` | the extension **and** a PostgreSQL JDBC driver |
| Coordinate | `com.github.atulsinha87.partitionctl` — the JitPack groupId, **not** the `io.github.atulsinha87` you will see inside the jar's own pom |
| Version | a git tag, `v`-prefix included: `v0.1.2` |

### `changelogs/changelog.xml` — what to change, what must stay

**Keep the header character-for-character.** Both namespace declarations and **both**
`xsi:schemaLocation` pairs. Liquibase resolves the second pair as a classpath resource served
from the extension jar — nothing is fetched over the network, and a trimmed header fails with
"no declaration can be found".

**Keep on every changeset that uses the extension:**

| Attribute | Why |
|---|---|
| `runInTransaction="false"` | **Required** — the change refuses to run without it. CIC cannot run in a transaction block, and this is also what lets partial progress survive a failure so a re-run resumes |
| `runAlways="true"` | Strongly recommended for `createPartitionedTableIndex` — without it, partitions added after the first run silently go unindexed. Do **not** use it with `reindexPartitionedTableIndex`, which is not idempotent |

**Change to match your table:**

| What | In this example | Yours |
|---|---|---|
| `author` / `id` | `example` / `orders-created-idx` | your changeset identity — Liquibase keys history on it, so pick once and never change it |
| `schemaName` | `public` | your schema |
| `tableName` | `orders` | the **partitioned parent**, not a partition |
| `indexName` | `idx_orders_created` | yours — the parent index. Child names are derived from it per partition and clipped to 63 bytes; a derivation that would collide fails loudly before anything runs |
| `<column>` elements | `created_at` desc, `customer_id` | your key — order matters, `descending` is per column |
| `paceSeconds` | `1` | optional; delay between partitions on a busy cluster. Omit for none. It is **not** bounded by your `statement_timeout` — the extension lifts the timeout around the sleep and restores it. Budget for total wall-clock instead: `paceSeconds` × partitions, against any CI or deploy job timeout |

## The two mistakes that cost adopters the most time

1. **JitPack must be a `<pluginRepository>`, not a `<repository>`.** The extension lives inside
   the liquibase-maven-plugin's own `<dependencies>`, which makes it a *plugin* dependency, and
   Maven resolves those from plugin repositories only. Under `<repositories>` Maven never
   contacts JitPack and fails with `Could not find artifact ... in central` — which reads like
   the artifact does not exist, not like a misplaced element.

2. **The extension goes in the plugin's `<dependencies>`, not the project's.** In your project's
   dependencies it is invisible to Liquibase, and every `<ext:...>` tag fails with
   "no declaration can be found".

For the full attribute reference, the reindex and drop changes, preconditions, rollback and
troubleshooting, see [liquibase-partitionctl/README.md](../../liquibase-partitionctl/README.md).
