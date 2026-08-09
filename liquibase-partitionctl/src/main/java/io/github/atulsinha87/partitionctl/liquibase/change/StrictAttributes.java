package io.github.atulsinha87.partitionctl.liquibase.change;

import liquibase.parser.core.ParsedNode;
import liquibase.parser.core.ParsedNodeException;

import java.util.ArrayList;
import java.util.Collection;
import java.util.List;
import java.util.Set;
import java.util.TreeSet;

/**
 * Refuses an attribute or child element the target change does not define.
 *
 * <h2>The defect this exists to prevent</h2>
 * A misspelled attribute binds to {@code null} in <b>total silence</b>. Liquibase reports
 * nothing, {@code validate()} sees a field that was simply not set, and the change proceeds with
 * its default. Measured on PostgreSQL 17.10 with liquibase-maven-plugin 4.33.0, a changeset
 * asking for {@code uniq="true" usin="brin" wher="status &lt;&gt; 'archived'"}:
 * <pre>
 * BUILD SUCCESS -- partitionctl: public.idx_noxsd VALID, 7 of 7 leaf partitions covered
 * relname   | indisunique | amname | predicate
 * idx_noxsd | f           | btree  |
 * </pre>
 * A non-unique, full btree index, shipped and recorded as EXECUTED where a unique partial brin
 * index was asked for. Worse, it is not correctable afterwards: coverage keys on
 * {@code pg_inherits}, never on the index definition, so a corrected changeset aimed at the same
 * index emits <b>zero</b> statements and leaves the wrong index in place.
 *
 * <h2>Why the XSD is not enough</h2>
 * partitionctl.xsd does declare every attribute and {@code use="required"} where it applies, and
 * when it is enforced a typo is a hard parse failure ({@code cvc-complex-type.3.2.2: Attribute
 * 'uniq' is not allowed}). But it is only enforced when the adopter lists the extension schema in
 * their {@code xsi:schemaLocation}. Declaring {@code xmlns:ext} alone is enough to make the
 * element bind and <b>not</b> enough to make it validated — a one-line omission Liquibase never
 * complains about, and the measurement above is exactly that changelog.
 *
 * <p>So the check lives here instead, in {@code load()}, where it runs on the parsed node before
 * any binding happens and cannot be switched off by how the changelog was written.
 *
 * <h2>What the parser gives us</h2>
 * Measured: for {@code <ext:createPartitionedTableIndex schemaName="public" uniq="true">
 * <column .../></ext:createPartitionedTableIndex>}, {@code parsedNode.getChildren()} is
 * {@code [schemaName, uniq, column]} — attributes and child elements alike, all with a null
 * namespace, and the misspelling is right there. Both child forms normalise to the same name, so
 * {@code <column/>} and {@code <ext:column/>} are indistinguishable here and both are accepted.
 */
final class StrictAttributes {

    private StrictAttributes() {
    }

    /**
     * @param parsedNode    the node Liquibase is about to bind
     * @param elementName   the element's name, for the message
     * @param known         {@code getSerializableFields()} — every attribute the change defines
     * @param childElements names of legal child elements, which arrive as children too
     */
    static void rejectUnknown(ParsedNode parsedNode, String elementName,
                              Collection<String> known, String... childElements)
            throws ParsedNodeException {
        Set<String> allowed = new TreeSet<String>(known);
        for (String child : childElements) {
            allowed.add(child);
        }

        List<String> unknown = new ArrayList<String>();
        for (ParsedNode child : parsedNode.getChildren()) {
            String name = child.getName();
            if (name != null && !allowed.contains(name) && !unknown.contains(name)) {
                unknown.add(name);
            }
        }
        if (unknown.isEmpty()) {
            return;
        }

        StringBuilder message = new StringBuilder("partitionctl: <").append(elementName)
                .append("> does not accept ")
                .append(unknown.size() == 1 ? "the attribute " : "the attributes ");
        for (int i = 0; i < unknown.size(); i++) {
            if (i > 0) {
                message.append(", ");
            }
            String name = unknown.get(i);
            message.append('"').append(name).append('"');
            String near = closest(name, allowed);
            if (near != null) {
                message.append(" (did you mean \"").append(near).append("\"?)");
            }
        }
        message.append(". Nothing was executed. Left unchecked, a misspelled attribute binds to "
                + "null in silence and the change runs with the default instead -- uniq=\"true\" "
                + "builds a NON-unique index and reports success. Accepted on this element: ")
                .append(allowed).append(".");
        throw new ParsedNodeException(message.toString());
    }

    /**
     * The nearest allowed name, so the message can say "did you mean". Null when nothing is close
     * enough — a wrong guess is worse than none.
     *
     * <p>Two kinds of near-miss actually happen, and edit distance alone only catches one.
     * A slip is short: {@code uniq}/{@code unique}, {@code usin}/{@code using},
     * {@code lockTimout}/{@code lockTimeout}. A carry-over from another element is a clean
     * truncation: {@code table}/{@code tableName}, {@code index}/{@code indexName}, which is 4 and
     * 5 edits away and would never be suggested on distance. So a prefix relationship counts as a
     * match outright.
     */
    private static String closest(String candidate, Set<String> allowed) {
        String lower = candidate.toLowerCase();
        for (String option : allowed) {
            String optionLower = option.toLowerCase();
            if (optionLower.startsWith(lower) || lower.startsWith(optionLower)) {
                return option;
            }
        }
        String best = null;
        int bestDistance = Integer.MAX_VALUE;
        for (String option : allowed) {
            int distance = editDistance(lower, option.toLowerCase());
            if (distance < bestDistance) {
                bestDistance = distance;
                best = option;
            }
        }
        return bestDistance <= Math.max(2, candidate.length() / 3) ? best : null;
    }

    private static int editDistance(String a, String b) {
        int[] previous = new int[b.length() + 1];
        int[] current = new int[b.length() + 1];
        for (int j = 0; j <= b.length(); j++) {
            previous[j] = j;
        }
        for (int i = 1; i <= a.length(); i++) {
            current[0] = i;
            for (int j = 1; j <= b.length(); j++) {
                int substitute = previous[j - 1] + (a.charAt(i - 1) == b.charAt(j - 1) ? 0 : 1);
                current[j] = Math.min(Math.min(current[j - 1] + 1, previous[j] + 1), substitute);
            }
            int[] swap = previous;
            previous = current;
            current = swap;
        }
        return previous[b.length()];
    }
}
