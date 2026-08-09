package io.github.atulsinha87.partitionctl.liquibase;

import io.github.atulsinha87.partitionctl.liquibase.change.ReindexPartitionedTableIndexChange;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.w3c.dom.Document;
import org.w3c.dom.Element;
import org.w3c.dom.Node;
import org.w3c.dom.NodeList;

import javax.xml.parsers.DocumentBuilderFactory;
import java.io.InputStream;
import java.lang.reflect.Method;
import java.lang.reflect.Modifier;
import java.util.LinkedHashSet;
import java.util.Set;
import java.util.TreeSet;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * An XSD attribute whose name does not match the Java property binds to <b>null</b>, silently,
 * with no error of any kind. This is the test that catches it for
 * {@code <ext:reindexPartitionedTableIndex>}: it reads the shipped XSD out of the classpath and
 * compares its attribute names against the setters the class actually declares.
 */
class ReindexXsdBindingTest {

    private static final String XSD_RESOURCE =
            "www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl.xsd";
    private static final String ELEMENT = "reindexPartitionedTableIndex";

    @Test
    @DisplayName("XSD attributes and Java setters agree exactly")
    void attributesMatchSetters() throws Exception {
        Set<String> xsd = attributeNames(false);
        Set<String> java = settableProperties(ReindexPartitionedTableIndexChange.class);

        assertEquals(new TreeSet<String>(java), new TreeSet<String>(xsd),
                "a mismatch binds to null with no error. XSD=" + new TreeSet<String>(xsd)
                        + " Java=" + new TreeSet<String>(java));
    }

    @Test
    @DisplayName("schemaName, tableName and indexName are required; the tuning knobs are not")
    void requiredAttributes() throws Exception {
        Set<String> required = attributeNames(true);
        assertTrue(required.contains("schemaName"), required.toString());
        assertTrue(required.contains("tableName"), required.toString());
        assertTrue(required.contains("indexName"), required.toString());
        assertEquals(3, required.size(), "everything else must stay optional: " + required);
    }

    @Test
    @DisplayName("the element takes no child elements at all")
    void noChildElements() throws Exception {
        Node complexType = elementDeclaration()
                .getElementsByTagNameNS("*", "complexType").item(0);
        NodeList children = complexType.getChildNodes();
        for (int i = 0; i < children.getLength(); i++) {
            Node node = children.item(i);
            if (node.getNodeType() != Node.ELEMENT_NODE) {
                continue;
            }
            assertEquals("attribute", node.getLocalName(),
                    "reindex takes no <column> list: the columns are whatever the index already has");
        }
    }

    @Test
    @DisplayName("the change is registered with the ServiceLoader")
    void registeredWithServiceLoader() throws Exception {
        InputStream in = getClass().getClassLoader()
                .getResourceAsStream("META-INF/services/liquibase.change.Change");
        assertNotNull(in);
        StringBuilder text = new StringBuilder();
        int c;
        while ((c = in.read()) != -1) {
            text.append((char) c);
        }
        in.close();
        assertTrue(text.toString().contains(ReindexPartitionedTableIndexChange.class.getName()),
                "without this line Liquibase never sees the change and the element does not resolve");
    }

    // ------------------------------------------------------------------ helpers

    private Element elementDeclaration() throws Exception {
        DocumentBuilderFactory factory = DocumentBuilderFactory.newInstance();
        factory.setNamespaceAware(true);
        Document document;
        try (InputStream in = getClass().getClassLoader().getResourceAsStream(XSD_RESOURCE)) {
            assertNotNull(in, "the XSD must ship at " + XSD_RESOURCE);
            document = factory.newDocumentBuilder().parse(in);
        }
        NodeList elements = document.getDocumentElement().getElementsByTagNameNS("*", "element");
        for (int i = 0; i < elements.getLength(); i++) {
            Element element = (Element) elements.item(i);
            if (ELEMENT.equals(element.getAttribute("name"))
                    && "schema".equals(element.getParentNode().getLocalName())) {
                return element;
            }
        }
        throw new AssertionError("no top-level element declaration named " + ELEMENT);
    }

    private Set<String> attributeNames(boolean requiredOnly) throws Exception {
        Node complexType = elementDeclaration()
                .getElementsByTagNameNS("*", "complexType").item(0);
        Set<String> names = new LinkedHashSet<String>();
        NodeList children = complexType.getChildNodes();
        for (int i = 0; i < children.getLength(); i++) {
            Node node = children.item(i);
            if (node.getNodeType() != Node.ELEMENT_NODE || !"attribute".equals(node.getLocalName())) {
                continue;
            }
            Element attribute = (Element) node;
            if (requiredOnly && !"required".equals(attribute.getAttribute("use"))) {
                continue;
            }
            names.add(attribute.getAttribute("name"));
        }
        return names;
    }

    /** Bean properties the class declares itself, not inherited ones. */
    private Set<String> settableProperties(Class<?> type) {
        Set<String> names = new LinkedHashSet<String>();
        for (Method method : type.getDeclaredMethods()) {
            if (!method.getName().startsWith("set")
                    || method.getParameterCount() != 1
                    || method.getName().length() < 4
                    || !Modifier.isPublic(method.getModifiers())) {
                continue;
            }
            names.add(Character.toLowerCase(method.getName().charAt(3))
                    + method.getName().substring(4));
        }
        return names;
    }
}
