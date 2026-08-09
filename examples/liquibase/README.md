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

## Run it against your own database

```bash
mvn liquibase:update \
  -Ddb.url=jdbc:postgresql://your-host:5432/your-db \
  -Ddb.username=you \
  -Ddb.password=secret
```

Then edit `changelogs/changelog.xml` to name your schema, table, index and columns.

On macOS, set `MAVEN_OPTS=-Duser.timezone=UTC` if your system timezone is not UTC — the JDBC
connect otherwise fails with `invalid value for parameter "TimeZone"`.

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
