package io.github.atulsinha87.partitionctl.liquibase;

import io.github.atulsinha87.partitionctl.liquibase.precondition.IndexAbsentGatePrecondition;
import io.github.atulsinha87.partitionctl.liquibase.precondition.IndexGatePrecondition;
import io.github.atulsinha87.partitionctl.liquibase.precondition.PartitionedIndexPrecondition;
import io.github.atulsinha87.partitionctl.liquibase.precondition.ReindexGatePrecondition;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.w3c.dom.Document;
import org.w3c.dom.Element;
import org.w3c.dom.Node;
import org.w3c.dom.NodeList;

import javax.xml.parsers.DocumentBuilder;
import javax.xml.parsers.DocumentBuilderFactory;
import java.io.InputStream;
import java.util.Arrays;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Set;
import java.util.TreeSet;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * An XSD attribute whose name does not match the Java property binds to <b>null</b>, silently. For
 * a precondition that failure is particularly nasty: the gate still runs, still queries, and still
 * returns an ordinary-looking verdict computed from a null. A mistyped {@code index} would report
 * "no index named public.null exists" — indistinguishable, to a reader, from a real absence. A
 * mistyped {@code requireValidParent} would read as "not set", so the strictness an adopter asked
 * for would quietly not happen and every deploy would pass.
 */
class PreconditionXsdBindingTest {

    private static final String XSD_RESOURCE =
            "www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl.xsd";

    private static final List<String> GATE_ELEMENTS = Arrays.asList(
            "partitionctlIndexGate", "partitionctlIndexAbsentGate", "partitionctlReindexGate");

    @Test
    @DisplayName("partitionctlIndexGate: XSD attributes and Java setters agree exactly")
    void indexGateBinds() throws Exception {
        assertBinds("partitionctlIndexGate", IndexGatePrecondition.class);
    }

    @Test
    @DisplayName("partitionctlIndexAbsentGate: XSD attributes and Java setters agree exactly")
    void absentGateBinds() throws Exception {
        assertBinds("partitionctlIndexAbsentGate", IndexAbsentGatePrecondition.class);
    }

    @Test
    @DisplayName("partitionctlReindexGate: XSD attributes and Java setters agree exactly")
    void reindexGateBinds() throws Exception {
        assertBinds("partitionctlReindexGate", ReindexGatePrecondition.class);
    }

    @Test
    @DisplayName("the three identifying attributes are use=\"required\" on every gate")
    void identifiersAreRequired() throws Exception {
        for (String element : GATE_ELEMENTS) {
            assertEquals(new TreeSet<String>(Arrays.asList("schemaName", "tableName", "indexName")),
                    new TreeSet<String>(attributeNamesOf(element, true)),
                    element + ": a missing identifier must be a parse error, not a null field");
        }
    }

    @Test
    @DisplayName("requireValidParent is optional and xsd:boolean, so a typo'd value fails at parse")
    void requireValidParentIsOptionalAndTyped() throws Exception {
        assertTrue(attributeNamesOf("partitionctlIndexGate", false).contains("requireValidParent"));
        assertTrue(!attributeNamesOf("partitionctlIndexGate", true).contains("requireValidParent"),
                "defaulting to strict would make an L3 tree fail every deploy forever");
        assertEquals("xsd:boolean", typeOf("partitionctlIndexGate", "requireValidParent"));
    }

    @Test
    @DisplayName("only partitionctlIndexGate takes requireValidParent")
    void theOtherGatesDoNotTakeRequireValidParent() throws Exception {
        // The reindex gate composes with the index gate instead of duplicating the attribute,
        // and there is no parent left for the absent gate to have a flag on.
        assertTrue(!attributeNamesOf("partitionctlIndexAbsentGate", false)
                .contains("requireValidParent"));
        assertTrue(!attributeNamesOf("partitionctlReindexGate", false)
                .contains("requireValidParent"));
    }

    @Test
    @DisplayName("the gates declare no child elements: they read the catalog, they are not authored")
    void gatesTakeNoChildElements() throws Exception {
        for (String name : GATE_ELEMENTS) {
            Element element = elementDeclaration(name);
            Node complexType = element.getElementsByTagNameNS("*", "complexType").item(0);
            NodeList children = complexType.getChildNodes();
            for (int i = 0; i < children.getLength(); i++) {
                Node node = children.item(i);
                if (node.getNodeType() != Node.ELEMENT_NODE) {
                    continue;
                }
                assertEquals("attribute", node.getLocalName(),
                        name + " must declare attributes only, found " + node.getLocalName());
            }
        }
    }

    @Test
    @DisplayName("every gate class is registered with the ServiceLoader")
    void allThreeAreRegistered() throws Exception {
        String registered = resourceText("META-INF/services/liquibase.precondition.Precondition");
        for (Class<?> gate : Arrays.asList(IndexGatePrecondition.class,
                IndexAbsentGatePrecondition.class, ReindexGatePrecondition.class)) {
            assertTrue(registered.contains(gate.getName()),
                    gate.getName() + " is not in META-INF/services/liquibase.precondition."
                            + "Precondition, so Liquibase will never see its tag");
        }
    }

    @Test
    @DisplayName("getName() matches the XSD element name: PreconditionFactory dispatches on it alone")
    void tagNamesMatchTheElementNames() throws Exception {
        // liquibase.precondition.PreconditionFactory keys registered preconditions by the tag's
        // LOCAL NAME only -- the XML namespace does not participate in dispatch. A getName() that
        // disagreed with the element name would leave the tag unresolvable.
        assertEquals("partitionctlIndexGate", new IndexGatePrecondition().getName());
        assertEquals("partitionctlIndexAbsentGate", new IndexAbsentGatePrecondition().getName());
        assertEquals("partitionctlReindexGate", new ReindexGatePrecondition().getName());
        for (String name : GATE_ELEMENTS) {
            assertNotNull(elementDeclaration(name));
        }
    }

    @Test
    @DisplayName("every gate is instantiable with a public no-arg constructor")
    void gatesAreServiceLoadable() throws Exception {
        for (Class<?> gate : Arrays.asList(IndexGatePrecondition.class,
                IndexAbsentGatePrecondition.class, ReindexGatePrecondition.class)) {
            Object instance = gate.getDeclaredConstructor().newInstance();
            assertTrue(instance instanceof PartitionedIndexPrecondition);
            assertEquals(PartitionedIndexPrecondition.NAMESPACE,
                    ((PartitionedIndexPrecondition) instance).getSerializedObjectNamespace());
        }
    }

    // ------------------------------------------------------------------ helpers

    private void assertBinds(String elementName, Class<?> type) throws Exception {
        Set<String> xsd = attributeNamesOf(elementName, false);
        Set<String> java = XsdBindingTest.settablePropertiesOf(type);
        assertEquals(new TreeSet<String>(java), new TreeSet<String>(xsd),
                elementName + ": a mismatch binds to null with no error. XSD="
                        + new TreeSet<String>(xsd) + " Java=" + new TreeSet<String>(java));
    }

    private String resourceText(String resource) throws Exception {
        InputStream in = getClass().getClassLoader().getResourceAsStream(resource);
        assertNotNull(in, "missing " + resource);
        StringBuilder text = new StringBuilder();
        int c;
        while ((c = in.read()) != -1) {
            text.append((char) c);
        }
        in.close();
        return text.toString();
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

    private Set<String> attributeNamesOf(String elementName, boolean requiredOnly)
            throws Exception {
        Element element = elementDeclaration(elementName);
        Node complexType = element.getElementsByTagNameNS("*", "complexType").item(0);
        Set<String> names = new LinkedHashSet<String>();
        NodeList children = complexType.getChildNodes();
        for (int i = 0; i < children.getLength(); i++) {
            Node node = children.item(i);
            if (node.getNodeType() != Node.ELEMENT_NODE
                    || !"attribute".equals(node.getLocalName())) {
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

    private String typeOf(String elementName, String attributeName) throws Exception {
        Element element = elementDeclaration(elementName);
        Node complexType = element.getElementsByTagNameNS("*", "complexType").item(0);
        NodeList children = complexType.getChildNodes();
        for (int i = 0; i < children.getLength(); i++) {
            Node node = children.item(i);
            if (node.getNodeType() != Node.ELEMENT_NODE
                    || !"attribute".equals(node.getLocalName())) {
                continue;
            }
            Element attribute = (Element) node;
            if (attributeName.equals(attribute.getAttribute("name"))) {
                return attribute.getAttribute("type");
            }
        }
        throw new AssertionError(elementName + " has no attribute " + attributeName);
    }
}
