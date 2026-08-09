package io.github.atulsinha87.partitionctl.liquibase;

import io.github.atulsinha87.partitionctl.liquibase.change.DropPartitionedTableIndexChange;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.w3c.dom.Document;
import org.w3c.dom.Element;
import org.w3c.dom.Node;
import org.w3c.dom.NodeList;

import javax.xml.parsers.DocumentBuilder;
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
 * with no error of any kind. For this change that failure is not cosmetic: a
 * {@code confirmExclusiveLock} that never reaches the Java would leave the acknowledgement
 * permanently false and the drop permanently refused, or — if the default were inverted — would
 * silently drop the tree without one.
 */
class DropXsdBindingTest {

    private static final String XSD_RESOURCE =
            "www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl.xsd";

    @Test
    @DisplayName("dropPartitionedTableIndex: XSD attributes and Java setters agree exactly")
    void attributesMatchSetters() throws Exception {
        Set<String> xsd = attributeNamesOf("dropPartitionedTableIndex", false);
        Set<String> java = settablePropertiesOf(DropPartitionedTableIndexChange.class);

        assertEquals(new TreeSet<String>(java), new TreeSet<String>(xsd),
                "a mismatch binds to null with no error. XSD=" + new TreeSet<String>(xsd)
                        + " Java=" + new TreeSet<String>(java));
    }

    @Test
    @DisplayName("only the three identifying attributes are required")
    void requiredAttributes() throws Exception {
        Set<String> required = attributeNamesOf("dropPartitionedTableIndex", true);
        assertEquals(new TreeSet<String>(java.util.Arrays.asList(
                "schemaName", "tableName", "indexName")), new TreeSet<String>(required));
    }

    @Test
    @DisplayName("confirmExclusiveLock is optional in the schema, enforced against live state")
    void confirmationIsNotSchemaRequired() throws Exception {
        // Marking it required here would demand it on runs that take no exclusive lock at all:
        // a leftovers-only cleanup is fully online, and a re-run with nothing left to drop
        // touches nothing. The change refuses at plan time instead, when a tree is really there.
        assertTrue(attributeNamesOf("dropPartitionedTableIndex", false)
                .contains("confirmExclusiveLock"));
        assertTrue(!attributeNamesOf("dropPartitionedTableIndex", true)
                .contains("confirmExclusiveLock"));
    }

    @Test
    @DisplayName("confirmExclusiveLock is xsd:boolean, so a typo'd value fails at parse time")
    void confirmationIsTyped() throws Exception {
        assertEquals("xsd:boolean", typeOf("dropPartitionedTableIndex", "confirmExclusiveLock"));
        assertEquals("xsd:integer", typeOf("dropPartitionedTableIndex", "exclusiveRetries"));
    }

    @Test
    @DisplayName("the drop element declares no child elements: the objects are discovered")
    void noChildElements() throws Exception {
        Element element = elementDeclaration("dropPartitionedTableIndex");
        Node complexType = element.getElementsByTagNameNS("*", "complexType").item(0);
        NodeList children = complexType.getChildNodes();
        for (int i = 0; i < children.getLength(); i++) {
            Node node = children.item(i);
            if (node.getNodeType() != Node.ELEMENT_NODE) {
                continue;
            }
            assertEquals("attribute", node.getLocalName(),
                    "the statement list is discovered from the catalog, never authored, so there "
                            + "is nothing for a child element to say");
        }
    }

    @Test
    @DisplayName("the drop change is registered with the ServiceLoader")
    void registeredWithTheServiceLoader() throws Exception {
        InputStream in = getClass().getClassLoader()
                .getResourceAsStream("META-INF/services/liquibase.change.Change");
        assertNotNull(in);
        StringBuilder text = new StringBuilder();
        int c;
        while ((c = in.read()) != -1) {
            text.append((char) c);
        }
        in.close();
        assertTrue(text.toString().contains(DropPartitionedTableIndexChange.class.getName()),
                "without this line Liquibase never sees the change and reports the element as "
                        + "unknown: " + text);
    }

    // ------------------------------------------------------------------ helpers

    private Document xsd() throws Exception {
        DocumentBuilderFactory factory = DocumentBuilderFactory.newInstance();
        factory.setNamespaceAware(true);
        DocumentBuilder builder = factory.newDocumentBuilder();
        try (InputStream in = getClass().getClassLoader().getResourceAsStream(XSD_RESOURCE)) {
            assertNotNull(in, "the XSD must ship at " + XSD_RESOURCE);
            return builder.parse(in);
        }
    }

    private Element elementDeclaration(String name) throws Exception {
        NodeList elements = xsd().getDocumentElement().getElementsByTagNameNS("*", "element");
        for (int i = 0; i < elements.getLength(); i++) {
            Element element = (Element) elements.item(i);
            if (name.equals(element.getAttribute("name"))
                    && "schema".equals(element.getParentNode().getLocalName())) {
                return element;
            }
        }
        throw new AssertionError("no top-level element declaration named " + name);
    }

    private Set<String> attributeNamesOf(String elementName, boolean requiredOnly) throws Exception {
        Set<String> names = new LinkedHashSet<String>();
        for (Element attribute : attributesOf(elementName)) {
            if (requiredOnly && !"required".equals(attribute.getAttribute("use"))) {
                continue;
            }
            names.add(attribute.getAttribute("name"));
        }
        return names;
    }

    private String typeOf(String elementName, String attributeName) throws Exception {
        for (Element attribute : attributesOf(elementName)) {
            if (attributeName.equals(attribute.getAttribute("name"))) {
                return attribute.getAttribute("type");
            }
        }
        throw new AssertionError("no attribute " + attributeName + " on " + elementName);
    }

    private java.util.List<Element> attributesOf(String elementName) throws Exception {
        Element element = elementDeclaration(elementName);
        Node complexType = element.getElementsByTagNameNS("*", "complexType").item(0);
        java.util.List<Element> attributes = new java.util.ArrayList<Element>();
        NodeList children = complexType.getChildNodes();
        for (int i = 0; i < children.getLength(); i++) {
            Node node = children.item(i);
            if (node.getNodeType() == Node.ELEMENT_NODE && "attribute".equals(node.getLocalName())) {
                attributes.add((Element) node);
            }
        }
        return attributes;
    }

    private Set<String> settablePropertiesOf(Class<?> type) {
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
