# Liquibase custom-precondition mechanism: empirical spike

Status: **works, with two sharp edges that will burn the M4 implementers if not documented.**

This was a blocking spike. Nothing below is copied from documentation or forum
posts. Every claim is backed by a command that was actually run against a real
PostgreSQL 17 container and a real Liquibase 4.33.0 jar, on 2026-08-07. The
working spike tree (jar builds, JDBC-checked precondition, five changelogs
covering pass/fail/validation-error/silent-binding-bug/no-schema-location) is
at:

```
/private/tmp/claude-502/-Users-atulsinha-ClaudeProjects-Pg-Partition/e32f5896-13fb-4d7f-a572-632997207f53/scratchpad/lbspike/
```

M4 implementers should start from that tree (`ext/` is the extension module,
`runner/` is a `liquibase-maven-plugin`-based harness that drives real
`liquibase:update` runs). It is intact and buildable as of this writing.

## Headline verdict

**The namespaced-XSD approach works exactly as the prior research pass
assumed, end to end, with real `liquibase update` runs, both PASS and
HALT-on-FAIL outcomes proven.** No fallback to `<customPrecondition
className="...">` is needed.

Two things are *not* obvious from the docs and cost real time here:

1. XSD validation is genuinely enforced (not decorative) if you declare
   `xsi:schemaLocation` for your namespace — a missing required attribute
   throws a real `cvc-complex-type.4` `SAXParseException` and halts parsing.
   But **if the attribute name in your XSD doesn't match your Java
   property name exactly, there is no error at all** — the value silently
   binds to `null` and your precondition just evaluates against `null`
   input. See Q4.
2. Maven's incremental compiler will silently reuse stale bytecode from a
   previous `--release` setting if you only touch `pom.xml` and not the
   `.java` file. `mvn clean install`, always, when changing compiler flags.
   Cost about 10 minutes here before being caught by inspecting the actual
   class file bytecode version in the built jar.

---

## Q1 — Does a custom precondition with its own XML namespace actually load and evaluate?

Yes, on all four counts: it loaded, the tag parsed, the precondition
evaluated a real SQL check, and a FAILED precondition halted the changeset
under `onFail="HALT"`.

**Setup.** One precondition class, `com.partitionctl.liquibase.PartitionctlIndexGatePrecondition`,
tag name `partitionctlIndexGate`, attributes `schema`/`table`/`index`,
packaged as `partitionctl-liquibase-ext.jar` with:
- `META-INF/services/liquibase.precondition.Precondition` naming the class
- `www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl.xsd` declaring
  `targetNamespace="http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl"`

Driven via `liquibase-maven-plugin:4.33.0`'s `update` goal (real
`liquibase update`, not a hand-rolled `Liquibase.update()` call) against
`jdbc:postgresql://localhost:5433/postgres` (a disposable `postgres:17`
container, port 5433, separate from the shared `partitionctl-pg` container on
5432). A real table `orders` with a real index `orders_created_at_idx` was
created first:

```
$ docker exec -i partitionctl-lbspike psql -U postgres -d postgres
CREATE TABLE orders (id bigint, created_at timestamptz, val text);
CREATE INDEX orders_created_at_idx ON orders (created_at);
```

**PASS run** (`changelogs/pass.xml`, index exists):

```xml
<preConditions onFail="HALT">
    <ext:partitionctlIndexGate schema="public" table="orders" index="orders_created_at_idx"/>
</preConditions>
<createTable tableName="spike_pass_marker">...
```

```
$ mvn liquibase:update -Dchangelog=changelogs/pass.xml
...
[INFO] Table spike_pass_marker created
[INFO] ChangeSet changelogs/pass.xml::1::spike ran successfully in 14ms
...
[INFO] BUILD SUCCESS
```
Verified in Postgres: `spike_pass_marker` exists.

**FAIL run** (`changelogs/fail.xml`, index name that does not exist,
`onFail="HALT"`):

```
$ mvn liquibase:update -Dchangelog=changelogs/fail.xml
...
[ERROR] liquibase.exception.LiquibaseException: liquibase.exception.MigrationFailedException: Migration failed for changeset changelogs/fail.xml::2::spike:
[ERROR]      Reason: 
[ERROR]           changelogs/fail.xml : Index 'index_that_does_not_exist' not found on public.orders (checked live via pg_indexes on the changelog's own JDBC connection)
[ERROR] : Preconditions Failed
...
[INFO] BUILD FAILURE
```
Verified in Postgres: `spike_fail_marker` was **not** created — the
`createTable` inside the changeset never ran. `onFail="HALT"` genuinely
halted execution, and the exception message is the exact string our
`check()` method built, proving it's our code that ran, not some built-in
fallback.

Both runs were repeated after switching to the final `--release 8`-compiled
jar (see Q5) with identical results (see Q5 section) — the pass/fail
behavior is not an artifact of a particular bytecode target.

**Extra evidence that the XSD is not decorative.** Removing the required
`index` attribute produces a real schema-validation failure, not a silent
pass-through:

```
$ mvn liquibase:update -Dchangelog=changelogs/bad-attr.xml
[ERROR] cvc-complex-type.4: Attribute 'index' must appear on element 'ext:partitionctlIndexGate'.
...
[ERROR] liquibase.exception.ChangeLogParseException: Error parsing line 13 column 72 of changelogs/bad-attr.xml: cvc-complex-type.4: Attribute 'index' must appear on element 'ext:partitionctlIndexGate'.
```

And deliberately shipping a jar with the XSD resource missing (packaging
mistake) produces exactly the "resolved locally, never fetched over network"
behavior the prior research pass assumed:

```
[ERROR] liquibase.exception.ChangeLogParseException: liquibase.parser.core.xml.XSDLookUpException: Unable to resolve xml entity http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl.xsd. liquibase.secureParsing is set to 'true' which does not allow remote lookups. Check for spelling or capitalization errors and missing extensions such as liquibase-commercial in your XSD definition. Or, set it to 'false' to allow remote lookups of xsd files. If you are using a changelog with custom change types, ensure you have the appropriate database extension on the classpath.
```
`liquibase.secureParsing` defaults to `true`; it never fell back to the
network in any run here.

**Mechanism, confirmed by decompiling `liquibase-core-4.33.0.jar` (not
guessed):**
- `liquibase.precondition.PreconditionFactory` keys registered preconditions
  by **tag local name only** (`Map<String, Class<? extends Precondition>>`),
  populated via `ServiceLoader` over `liquibase.precondition.Precondition`.
  The XML **namespace does not participate in dispatch** — only the local
  element name does. This means namespace collisions across different
  vendors' extensions are not possible to detect via namespace; tag name
  uniqueness is what actually matters.
- The base Liquibase changelog XSD (`dbchangelog-latest.xsd`) has an
  `<xsd:any namespace="##other" processContents="lax" minOccurs="0"
  maxOccurs="unbounded"/>` inside the `PreConditionChildren` group. This is
  *why* an arbitrary namespaced element is even syntactically legal inside
  `<preConditions>` — `processContents="lax"` means: validate against a
  schema for that namespace **if one can be found**, but don't require it.
  Confirmed empirically: `changelogs/no-schemalocation.xml` (no
  `xsi:schemaLocation` entry for our namespace at all, only the built-in
  dbchangelog one) still parsed, still ran the precondition check, and still
  passed. Declaring `xsi:schemaLocation` for your namespace is what buys you
  the attribute-completeness validation from Q1's `cvc-complex-type.4` test
  above — it is optional for function, valuable for safety.
- XSD resolution: `liquibase.parser.core.xml.LiquibaseEntityResolver`
  (`org.xml.sax.ext.EntityResolver2`) takes the `systemId` from
  `xsi:schemaLocation`, strips the `https?://` protocol prefix
  (`http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl.xsd` →
  `www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl.xsd`), and resolves
  it first via `ClassLoader.getResource(path)`, falling back to
  `Scope.getCurrentScope().getResourceAccessor().get(path)`. If neither
  finds it and `liquibase.secureParsing=true` (the default), it throws
  `XSDLookUpException` rather than touching the network. This is the exact
  mechanism, and the exact jar-relative path convention, the prior research
  pass assumed — confirmed by bytecode, not doc copy.

## Q2 — Exact Liquibase version, and does the ServiceLoader interface path move across 4.x?

Pinned to **`org.liquibase:liquibase-core:4.33.0`** (latest stable 4.x on
Maven Central as of this spike; 5.0.x also exists on Central but was not the
target — see below).

`liquibase.precondition.Precondition` is the correct ServiceLoader key, and
it does not move. Verified by downloading and inspecting
`META-INF/services/liquibase.precondition.Precondition` and
`liquibase/precondition/{Precondition,AbstractPrecondition}.class` across
five 4.x releases spanning the entire line, plus one 5.x release:

```
$ for v in 4.0.0 4.9.0 4.20.0 4.29.2 4.33.0 5.0.3; do
    unzip -l liquibase-core-$v.jar | grep -E \
      "precondition/Precondition\.class|precondition/AbstractPrecondition\.class|META-INF/services/liquibase.precondition.Precondition$"
  done
```
Every version (4.0.0 through 5.0.3) has all three entries at the identical
path, and `Precondition.class` / `AbstractPrecondition.class` are the exact
same byte size (837 / 538 bytes) in every single one of the six jars
inspected — i.e. this corner of the API has not been touched since Liquibase
4.0.0 (2020) through the newest 5.0.3 (2026). This is the most stable part of
the extension surface; there is no version-skew risk here.

## Q3 — How does the precondition get a live JDBC connection?

Confirmed path, and confirmed it executes a real `SELECT` inside `check()`:

```java
DatabaseConnection dbConnection = database.getConnection();
Connection rawConnection = ((JdbcConnection) dbConnection).getUnderlyingConnection();
try (PreparedStatement ps = rawConnection.prepareStatement(
        "SELECT 1 FROM pg_indexes WHERE schemaname = ? AND tablename = ? AND indexname = ?")) {
    ps.setString(1, schema);
    ps.setString(2, table);
    ps.setString(3, index);
    try (ResultSet rs = ps.executeQuery()) {
        found = rs.next();
    }
}
```
`database` is the `liquibase.database.Database` instance passed into
`check(Database, DatabaseChangeLog, ChangeSet, ChangeExecListener)` by
Liquibase itself — this is the same connection the changeset's own DDL runs
on, authenticated once by Liquibase's own `-url/-username/-password`
(here, the maven plugin's `<url>/<username>/<password>` config). No second
credential, no subprocess. Confirmed by the FAIL-case output above: the
`pg_indexes` lookup against the live connection is what produced "Index
'index_that_does_not_exist' not found on public.orders" — that string only
appears if the SQL actually ran and actually returned zero rows.

`javap` on `liquibase.database.jvm.JdbcConnection` confirms `public
java.sql.Connection getUnderlyingConnection();` is the real, public method —
not something inferred from docs.

## Q4 — Attribute binding

Confirmed via `javap` and empirically: plain JavaBean getters/setters, no
`load()` override needed for simple attributes.

`AbstractPrecondition extends AbstractLiquibaseSerializable implements
Precondition`. `AbstractLiquibaseSerializable.load(ParsedNode,
ResourceAccessor)` is already implemented and uses reflection to match each
`ParsedNode` child (i.e. each XML attribute) to a bean setter by name — the
built-in `TableExistsPrecondition` (decompiled for comparison) has exactly
`private String tableName; getTableName()/setTableName()` and does **not**
override `load()`. Our class follows the same pattern:

```java
private String schema;
private String table;
private String index;
public String getSchema()  { return schema; }
public void   setSchema(String schema) { this.schema = schema; }
// ... table, index identical shape
```

Two things beyond "just add getters/setters":
- `getSerializedObjectNamespace()` (declared on `LiquibaseSerializable`,
  left abstract by both `AbstractLiquibaseSerializable` and
  `AbstractPrecondition`) **must** be overridden or the class won't compile
  — first compile attempt failed with `is not abstract and does not
  override abstract method getSerializedObjectNamespace()`. We return our
  own namespace string.
- **Attribute names in the XSD must match the Java property name exactly,
  case-sensitively, and there is no error if they don't.** Proved this by
  building a second variant of the jar where the XSD declared
  `indexName` while the Java class kept `setIndex(String)`/`index`. Result:
  XSD validation passed (self-consistent schema), `check()` ran, and the
  precondition failed with **`Index 'null' not found on public.orders`** —
  the value was silently dropped during binding, no exception, no warning,
  just a `null` field. This is the sharpest edge in the whole mechanism and
  the one most likely to cost the next person time: a typo'd XSD attribute
  name doesn't fail loudly, it fails as "works fine, always evaluates false
  in production."

## Q5 — Minimum Java release

**`--release 8`** compiles cleanly against `liquibase-core:4.33.0` (only an
"obsolete" warning, not an error); `--release 7` is rejected outright by
JDK 22's `javac` (`error: release version 7 not supported`) so 8 is also the
practical floor imposed by current tooling, not just by Liquibase.

```
$ javac --release 7  -cp liquibase-core-4.33.0.jar ... → error: release version 7 not supported
$ javac --release 8  -cp liquibase-core-4.33.0.jar ... → 3 warnings, exit 0
$ javac --release 9,11,17 -cp liquibase-core-4.33.0.jar ... → exit 0, no warnings
```

Liquibase itself: every class file inspected in
`liquibase-core-4.33.0.jar` (`Liquibase.class`,
`command/core/UpdateCommandStep.class`, `Scope.class`,
`precondition/AbstractPrecondition.class`) is bytecode **major version 52 =
Java 8**. Liquibase 4.33.0's own jar targets Java 8, so there is no reason
for the extension to target anything newer.

**Gotcha that cost real time:** after changing `maven.compiler.release` from
11 to 8 in the pom and running `mvn install`, the packaged jar's class file
was still major version 55 (Java 11) — Maven's incremental compiler only
checks source-file timestamps, not whether compiler flags changed, so it
silently reused the stale Java-11-compiled `.class` file. Only caught by
inspecting the actual bytecode major-version byte in the built jar. Fix:
`mvn clean install` any time a `--release`/`source`/`target` change is made.
The final jar was rebuilt with `mvn clean install` and reverified at major
version 52, and the full pass/fail Postgres run was repeated end-to-end
against that jar with identical results.

## Q6 — Smallest honest `pom.xml`

Yes, `liquibase-core` must be (and was) `provided` scope: the host
(Liquibase CLI, the `liquibase-maven-plugin`, or whatever embeds Liquibase)
supplies it at runtime, and our jar must not bundle or shade it. Confirmed
by inspecting the built jar (`ext/target/partitionctl-liquibase-ext.jar`,
18 entries) — it contains only our one class, the XSD, the ServiceLoader
file, and Maven's own bookkeeping metadata; zero `liquibase/**` classes are
bundled, exactly as `provided` scope should behave.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0"
         xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
         xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 http://maven.apache.org/xsd/maven-4.0.0.xsd">
  <modelVersion>4.0.0</modelVersion>

  <groupId>com.partitionctl</groupId>
  <artifactId>partitionctl-liquibase-ext</artifactId>
  <version>0.0.1-SPIKE</version>
  <packaging>jar</packaging>

  <properties>
    <maven.compiler.release>8</maven.compiler.release>
    <project.build.sourceEncoding>UTF-8</project.build.sourceEncoding>
    <liquibase.version>4.33.0</liquibase.version>
  </properties>

  <dependencies>
    <!-- provided: the host application (Liquibase CLI / maven plugin / our runner)
         supplies this at runtime. Do not let this leak into compile/runtime scope
         or the jar will bundle liquibase-core and can version-clash with the host. -->
    <dependency>
      <groupId>org.liquibase</groupId>
      <artifactId>liquibase-core</artifactId>
      <version>${liquibase.version}</version>
      <scope>provided</scope>
    </dependency>
  </dependencies>

  <build>
    <finalName>partitionctl-liquibase-ext</finalName>
  </build>
</project>
```
This is the exact `ext/pom.xml` used in every run above. Nothing else was
needed — no shade plugin, no assembly plugin, no extra dependencies. A JDBC
driver is not needed in the extension jar either: the host (Liquibase
CLI/maven-plugin/our runner) already has one on its classpath to open the
connection our precondition reuses.

---

## What to build (copy-paste starting point for M4)

### ServiceLoader registration
`src/main/resources/META-INF/services/liquibase.precondition.Precondition`:
```
com.partitionctl.liquibase.PartitionctlIndexGatePrecondition
```
(one fully-qualified class name per line, one line per precondition class)

### XSD skeleton
`src/main/resources/www.liquibase.org/xml/ns/dbchangelog-ext/<yourname>.xsd`
— **the classpath path after the protocol-stripped URL is what
`LiquibaseEntityResolver` looks up; get this path wrong and you get
`XSDLookUpException`, not a helpful "file not found."**
```xml
<?xml version="1.0" encoding="UTF-8"?>
<xsd:schema xmlns:xsd="http://www.w3.org/2001/XMLSchema"
            targetNamespace="http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl"
            xmlns:ext="http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl"
            elementFormDefault="qualified">

    <xsd:element name="partitionctlIndexGate">
        <xsd:complexType>
            <xsd:attribute name="schema" type="xsd:string" use="required"/>
            <xsd:attribute name="table" type="xsd:string" use="required"/>
            <xsd:attribute name="index" type="xsd:string" use="required"/>
        </xsd:complexType>
    </xsd:element>

</xsd:schema>
```
Attribute names here must match the Java property names **exactly** (Q4).

### Minimal working precondition class
(the full file used and proven in this spike; copy as a template)
```java
package com.partitionctl.liquibase;

import liquibase.changelog.ChangeSet;
import liquibase.changelog.DatabaseChangeLog;
import liquibase.changelog.visitor.ChangeExecListener;
import liquibase.database.Database;
import liquibase.database.DatabaseConnection;
import liquibase.database.jvm.JdbcConnection;
import liquibase.exception.PreconditionErrorException;
import liquibase.exception.PreconditionFailedException;
import liquibase.exception.ValidationErrors;
import liquibase.exception.Warnings;
import liquibase.precondition.AbstractPrecondition;
import liquibase.precondition.FailedPrecondition;

import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.util.ArrayList;
import java.util.List;

public class PartitionctlIndexGatePrecondition extends AbstractPrecondition {

    private String schema;
    private String table;
    private String index;

    public String getSchema() { return schema; }
    public void setSchema(String schema) { this.schema = schema; }
    public String getTable() { return table; }
    public void setTable(String table) { this.table = table; }
    public String getIndex() { return index; }
    public void setIndex(String index) { this.index = index; }

    @Override
    public String getName() {
        return "partitionctlIndexGate";
    }

    @Override
    public String getSerializedObjectNamespace() {
        return "http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl";
    }

    @Override
    public Warnings warn(Database database) {
        return new Warnings();
    }

    @Override
    public ValidationErrors validate(Database database) {
        ValidationErrors errors = new ValidationErrors();
        if (schema == null || schema.isEmpty()) errors.addError("partitionctlIndexGate: 'schema' attribute is required");
        if (table == null || table.isEmpty())   errors.addError("partitionctlIndexGate: 'table' attribute is required");
        if (index == null || index.isEmpty())   errors.addError("partitionctlIndexGate: 'index' attribute is required");
        return errors;
    }

    @Override
    public void check(Database database, DatabaseChangeLog changeLog, ChangeSet changeSet, ChangeExecListener execListener)
            throws PreconditionFailedException, PreconditionErrorException {
        boolean found;
        try {
            DatabaseConnection dbConnection = database.getConnection();
            if (!(dbConnection instanceof JdbcConnection)) {
                throw new PreconditionErrorException(
                        new IllegalStateException("partitionctlIndexGate requires a JdbcConnection, got " + dbConnection.getClass()),
                        changeLog, this);
            }
            Connection rawConnection = ((JdbcConnection) dbConnection).getUnderlyingConnection();
            String sql = "SELECT 1 FROM pg_indexes WHERE schemaname = ? AND tablename = ? AND indexname = ?";
            try (PreparedStatement ps = rawConnection.prepareStatement(sql)) {
                ps.setString(1, schema);
                ps.setString(2, table);
                ps.setString(3, index);
                try (ResultSet rs = ps.executeQuery()) {
                    found = rs.next();
                }
            }
        } catch (PreconditionErrorException e) {
            throw e;
        } catch (Exception e) {
            throw new PreconditionErrorException(e, changeLog, this);
        }

        if (!found) {
            List<FailedPrecondition> failures = new ArrayList<>();
            failures.add(new FailedPrecondition(
                    "Index '" + index + "' not found on " + schema + "." + table
                            + " (checked live via pg_indexes on the changelog's own JDBC connection)",
                    changeLog, this));
            throw new PreconditionFailedException(failures);
        }
    }
}
```

### Changelog usage (proven pattern)
```xml
<databaseChangeLog
        xmlns="http://www.liquibase.org/xml/ns/dbchangelog"
        xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
        xmlns:ext="http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl"
        xsi:schemaLocation="http://www.liquibase.org/xml/ns/dbchangelog
            http://www.liquibase.org/xml/ns/dbchangelog/dbchangelog-4.29.xsd
            http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl
            http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl.xsd">

    <changeSet id="1" author="you">
        <preConditions onFail="HALT">
            <ext:partitionctlIndexGate schema="public" table="orders" index="orders_created_at_idx"/>
        </preConditions>
        <!-- your DDL -->
    </changeSet>
</databaseChangeLog>
```
Always declare `xsi:schemaLocation` for the custom namespace — it's what
turns a typo'd/missing attribute into a loud `cvc-complex-type.*` parse
error instead of a silent `null` (Q4). The `dbchangelog-4.29.xsd` version
number in the base namespace's schemaLocation should track whatever
Liquibase version you pin; it isn't required (Liquibase resolves the base
schema itself regardless) but keeps IDEs happy.

## Things that would cost the next person time (recap)

1. `mvn clean` before trusting any `--release`/compiler-flag change —
   Maven's staleness check is timestamp-based and won't recompile on a pom
   edit alone.
2. Postgres JDBC + a JVM default timezone the driver's mapping table
   doesn't recognize (`Asia/Calcutta` vs `Asia/Kolkata` on this machine)
   throws `FATAL: invalid value for parameter "TimeZone"` on connect. Fixed
   with `MAVEN_OPTS=-Duser.timezone=UTC` (or `-Duser.timezone=UTC` on
   whatever JVM launches Liquibase). Unrelated to the extension mechanism
   but will trip up anyone running this locally.
3. XSD attribute-name/Java-property-name mismatches fail silently (Q4) —
   write a unit test that asserts non-null fields after `load()`, or eyeball
   every attribute name against the setter twice.
4. Namespace URIs do not disambiguate preconditions from each other — only
   the tag's local name does (`PreconditionFactory` keys on name only).
   Pick a tag name unlikely to collide with any other vendor's extension;
   the namespace is not a safety net here.
