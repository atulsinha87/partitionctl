package io.github.atulsinha87.partitionctl.liquibase;

import io.github.atulsinha87.partitionctl.liquibase.change.CreatePartitionedTableIndexChange;
import io.github.atulsinha87.partitionctl.liquibase.precondition.IndexGatePrecondition;

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
import java.util.LinkedHashSet;
import java.util.Set;
import java.util.TreeSet;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * An XSD attribute whose name does not match the Java property name binds to <b>null</b>,
 * silently, with no error of any kind — the spike lost a day to exactly that. This test is
 * the thing that catches it: it reads the shipped XSD out of the classpath and compares its
 * attribute names against the setters actually declared on the classes.
 */
class XsdBindingTest {

    private static final String XSD_RESOURCE =
            "www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl.xsd";

    // ------------------------------------------------------------------ the XSD ships

    @Test
    @DisplayName("the XSD is on the classpath at the exact path Liquibase resolves")
    void xsdIsOnTheClasspath() throws Exception {
        InputStream in = getClass().getClassLoader().getResourceAsStream(XSD_RESOURCE);
        assertNotNull(in, "Liquibase strips the protocol from the xsi:schemaLocation URL and looks "
                + "the rest up as a classpath resource, so it must be at " + XSD_RESOURCE);
        in.close();
    }

    @Test
    @DisplayName("the ServiceLoader files name classes that exist and are the right type")
    void serviceLoaderFilesResolve() throws Exception {
        assertServiceFile("META-INF/services/liquibase.change.Change", liquibase.change.Change.class);
        assertServiceFile("META-INF/services/liquibase.precondition.Precondition",
                liquibase.precondition.Precondition.class);
    }

    // ------------------------------------------------------------------ binding

    @Test
    @DisplayName("createPartitionedTableIndex: XSD attributes and Java setters agree exactly")
    void createChangeAttributesMatchSetters() throws Exception {
        Set<String> xsd = attributeNamesOf("createPartitionedTableIndex");
        Set<String> java = settablePropertiesOf(CreatePartitionedTableIndexChange.class, "columns");

        assertEquals(new TreeSet<String>(java), new TreeSet<String>(xsd),
                "XSD attributes must match Java properties EXACTLY. A mismatch binds to null "
                        + "with no error. XSD=" + new TreeSet<String>(xsd)
                        + " Java=" + new TreeSet<String>(java));
    }

    @Test
    @DisplayName("partitionctlIndexGate: XSD attributes and Java setters agree exactly")
    void preconditionAttributesMatchSetters() throws Exception {
        Set<String> xsd = attributeNamesOf("partitionctlIndexGate");
        Set<String> java = settablePropertiesOf(IndexGatePrecondition.class);

        assertEquals(new TreeSet<String>(java), new TreeSet<String>(xsd));
    }

    @Test
    @DisplayName("the required attributes really are use=\"required\"")
    void requiredAttributesAreMarkedRequired() throws Exception {
        Set<String> required = requiredAttributeNamesOf("createPartitionedTableIndex");
        assertTrue(required.contains("schemaName"), required.toString());
        assertTrue(required.contains("tableName"), required.toString());
        assertTrue(required.contains("indexName"), required.toString());
        assertEquals(3, required.size(), "everything else must stay optional: " + required);
    }

    @Test
    @DisplayName("the child particle accepts both <column> and <ext:column>")
    void childParticleAcceptsBothColumnForms() throws Exception {
        Element element = elementDeclaration("createPartitionedTableIndex");
        Element choice = firstChild(element.getElementsByTagNameNS("*", "complexType").item(0),
                "choice");
        assertNotNull(choice, "an xsd:choice is what makes the prefixed form work as well as the "
                + "owner's unprefixed form");

        boolean hasOwnColumn = false;
        boolean hasOtherWildcard = false;
        NodeList children = choice.getChildNodes();
        for (int i = 0; i < children.getLength(); i++) {
            Node node = children.item(i);
            if (node.getNodeType() != Node.ELEMENT_NODE) {
                continue;
            }
            String local = node.getLocalName();
            if ("element".equals(local)
                    && "column".equals(((Element) node).getAttribute("name"))) {
                hasOwnColumn = true;
            }
            if ("any".equals(local)) {
                Element any = (Element) node;
                assertEquals("##other", any.getAttribute("namespace"));
                assertEquals("strict", any.getAttribute("processContents"),
                        "strict keeps the unknown-element check; lax loses it");
                hasOtherWildcard = true;
            }
        }
        assertTrue(hasOwnColumn, "no explicit <column> declaration -- <ext:column> would not parse");
        assertTrue(hasOtherWildcard, "no ##other wildcard -- the unprefixed <column> would not parse");
    }

    // ------------------------------------------------------------------ helpers

    private void assertServiceFile(String resource, Class<?> expectedType) throws Exception {
        InputStream in = getClass().getClassLoader().getResourceAsStream(resource);
        assertNotNull(in, "missing " + resource);
        StringBuilder text = new StringBuilder();
        int c;
        while ((c = in.read()) != -1) {
            text.append((char) c);
        }
        in.close();

        int named = 0;
        for (String line : text.toString().split("\\R")) {
            String trimmed = line.trim();
            if (trimmed.isEmpty() || trimmed.startsWith("#")) {
                continue;
            }
            Class<?> loaded = Class.forName(trimmed);
            assertTrue(expectedType.isAssignableFrom(loaded),
                    trimmed + " listed in " + resource + " is not a " + expectedType.getName());
            named++;
        }
        assertTrue(named > 0, resource + " names no implementations");
    }

    private Document xsd() throws Exception {
        DocumentBuilderFactory factory = DocumentBuilderFactory.newInstance();
        factory.setNamespaceAware(true);
        DocumentBuilder builder = factory.newDocumentBuilder();
        try (InputStream in = getClass().getClassLoader().getResourceAsStream(XSD_RESOURCE)) {
            assertNotNull(in);
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

    /** Attributes declared directly on the element's own complexType, not on nested children. */
    private Set<String> attributeNamesOf(String elementName) throws Exception {
        return attributeNamesOf(elementName, false);
    }

    private Set<String> requiredAttributeNamesOf(String elementName) throws Exception {
        return attributeNamesOf(elementName, true);
    }

    private Set<String> attributeNamesOf(String elementName, boolean requiredOnly) throws Exception {
        Element element = elementDeclaration(elementName);
        Node complexType = element.getElementsByTagNameNS("*", "complexType").item(0);
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

    private Element firstChild(Node parent, String localName) {
        NodeList children = parent.getChildNodes();
        for (int i = 0; i < children.getLength(); i++) {
            Node node = children.item(i);
            if (node.getNodeType() == Node.ELEMENT_NODE && localName.equals(node.getLocalName())) {
                return (Element) node;
            }
        }
        return null;
    }

    /** Bean properties declared by the class itself (not inherited), minus the exclusions. */
    private Set<String> settablePropertiesOf(Class<?> type, String... exclude) {
        Set<String> excluded = new LinkedHashSet<String>();
        for (String name : exclude) {
            excluded.add(name);
        }
        Set<String> names = new LinkedHashSet<String>();
        for (Method method : type.getDeclaredMethods()) {
            if (!method.getName().startsWith("set")
                    || method.getParameterCount() != 1
                    || method.getName().length() < 4
                    || !java.lang.reflect.Modifier.isPublic(method.getModifiers())) {
                continue;
            }
            String property = Character.toLowerCase(method.getName().charAt(3))
                    + method.getName().substring(4);
            if (!excluded.contains(property)) {
                names.add(property);
            }
        }
        return names;
    }
}
