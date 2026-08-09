package io.github.atulsinha87.partitionctl.liquibase.catalog;

import liquibase.database.Database;
import liquibase.database.core.PostgresDatabase;
import liquibase.database.jvm.JdbcConnection;

import java.lang.reflect.InvocationHandler;
import java.lang.reflect.Method;
import java.lang.reflect.Proxy;
import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * A JDBC connection whose {@code ResultSet} is a list of maps, so the discovery classes can be
 * driven without a database.
 *
 * <p>Why this exists: roughly 640 lines across {@link PartitionDiscovery},
 * {@link DropTargetDiscovery} and {@link GateInspection} are hand-assembled SQL plus
 * {@code ResultSet} column mapping, and until now every one of those lines was covered only by the
 * live end-to-end run. That run exercises the happy path well and the error and edge paths not at
 * all -- a null column, a leaf appearing twice, a driver throwing mid-iteration. Those are exactly
 * the seams this project's own history says defects live in.
 *
 * <p>Implemented with {@link Proxy} rather than a mocking library because the module's only test
 * dependency is JUnit, and {@code ResultSet} has some two hundred methods of which these tests
 * need six. Anything not stubbed throws, so a mapping change that starts reading a new column
 * fails loudly here instead of returning a silent null.
 */
final class FakeJdbc {

    private final List<Map<String, Object>> rows = new ArrayList<Map<String, Object>>();
    private SQLException failOnQuery;
    private SQLException failOnRow;
    private int failAtRow = -1;

    /** The SQL the code under test actually prepared, for asserting the query is parameterised. */
    String preparedSql;
    /** Parameter index to bound value, so tests can assert what was bound where. */
    final Map<Integer, String> boundParameters = new LinkedHashMap<Integer, String>();

    static FakeJdbc returning() {
        return new FakeJdbc();
    }

    /** Adds a row as alternating column name and value: {@code row("a", 1, "b", null)}. */
    FakeJdbc row(Object... columnsAndValues) {
        if (columnsAndValues.length % 2 != 0) {
            throw new IllegalArgumentException("row() takes alternating name and value");
        }
        Map<String, Object> row = new LinkedHashMap<String, Object>();
        for (int i = 0; i < columnsAndValues.length; i += 2) {
            row.put((String) columnsAndValues[i], columnsAndValues[i + 1]);
        }
        rows.add(row);
        return this;
    }

    /** The driver throws when the query is executed. */
    FakeJdbc failingOnQuery(String message) {
        this.failOnQuery = new SQLException(message);
        return this;
    }

    /** The driver throws part-way through iteration, after {@code afterRows} successful rows. */
    FakeJdbc failingAfter(int afterRows, String message) {
        this.failAtRow = afterRows;
        this.failOnRow = new SQLException(message);
        return this;
    }

    /** A PostgresDatabase wired to this fake, which is what the discovery classes take. */
    Database database() {
        return new PostgresDatabase() {
            @Override
            public liquibase.database.DatabaseConnection getConnection() {
                return new JdbcConnection(connection());
            }
        };
    }

    Connection connection() {
        return (Connection) Proxy.newProxyInstance(
                getClass().getClassLoader(), new Class<?>[]{Connection.class}, new InvocationHandler() {
                    @Override
                    public Object invoke(Object proxy, Method method, Object[] args) throws Throwable {
                        String name = method.getName();
                        if ("prepareStatement".equals(name)) {
                            preparedSql = (String) args[0];
                            return statement();
                        }
                        return defaultFor(method, name);
                    }
                });
    }

    private PreparedStatement statement() {
        return (PreparedStatement) Proxy.newProxyInstance(
                getClass().getClassLoader(), new Class<?>[]{PreparedStatement.class}, new InvocationHandler() {
                    @Override
                    public Object invoke(Object proxy, Method method, Object[] args) throws Throwable {
                        String name = method.getName();
                        if ("setString".equals(name)) {
                            boundParameters.put((Integer) args[0], (String) args[1]);
                            return null;
                        }
                        if ("executeQuery".equals(name)) {
                            if (failOnQuery != null) {
                                throw failOnQuery;
                            }
                            return resultSet();
                        }
                        return defaultFor(method, name);
                    }
                });
    }

    private ResultSet resultSet() {
        return (ResultSet) Proxy.newProxyInstance(
                getClass().getClassLoader(), new Class<?>[]{ResultSet.class}, new InvocationHandler() {
                    private int cursor = -1;

                    @Override
                    public Object invoke(Object proxy, Method method, Object[] args) throws Throwable {
                        String name = method.getName();
                        if ("next".equals(name)) {
                            if (failOnRow != null && cursor + 1 == failAtRow) {
                                throw failOnRow;
                            }
                            cursor++;
                            return cursor < rows.size();
                        }
                        if ("getString".equals(name) || "getBoolean".equals(name)
                                || "getInt".equals(name) || "getObject".equals(name)) {
                            Map<String, Object> row = rows.get(cursor);
                            String column = (String) args[0];
                            if (!row.containsKey(column)) {
                                throw new SQLException("the code read a column the fixture does not "
                                        + "declare: " + column + " (present: " + row.keySet() + ")");
                            }
                            Object value = row.get(column);
                            if ("getBoolean".equals(name)) {
                                return value != null && (Boolean) value;
                            }
                            if ("getInt".equals(name)) {
                                return value == null ? 0 : ((Number) value).intValue();
                            }
                            return value;
                        }
                        return defaultFor(method, name);
                    }
                });
    }

    /** close/isClosed and the like are no-ops; anything else is a mapping change worth failing on. */
    private static Object defaultFor(Method method, String name) throws SQLException {
        if ("close".equals(name) || "setQueryTimeout".equals(name) || "setFetchSize".equals(name)) {
            return null;
        }
        if ("isClosed".equals(name)) {
            return Boolean.FALSE;
        }
        if ("toString".equals(name)) {
            return "FakeJdbc";
        }
        if ("hashCode".equals(name)) {
            return System.identityHashCode(method);
        }
        if ("equals".equals(name)) {
            return Boolean.FALSE;
        }
        Class<?> returnType = method.getReturnType();
        if (returnType == boolean.class) {
            return Boolean.FALSE;
        }
        if (returnType == int.class) {
            return 0;
        }
        if (returnType == void.class) {
            return null;
        }
        throw new SQLException("FakeJdbc has no stub for " + method.getDeclaringClass().getSimpleName()
                + "." + name + "; add one deliberately rather than returning a silent null");
    }
}
