package mermaid

import "strings"

func parseFlowchart(doc *Document, statements []string) {
	var subgraphStack []int
	for _, statement := range statements {
		if id, label, ok := parseMermaidSubgraph(statement); ok {
			parentID := ""
			if len(subgraphStack) > 0 {
				parentID = doc.Subgraphs[subgraphStack[len(subgraphStack)-1]].ID
			}
			doc.Subgraphs = append(doc.Subgraphs, Subgraph{ID: id, Label: label, ParentID: parentID})
			subgraphStack = append(subgraphStack, len(doc.Subgraphs)-1)
			continue
		}
		if strings.EqualFold(strings.TrimSpace(statement), "end") {
			if len(subgraphStack) > 0 {
				subgraphStack = subgraphStack[:len(subgraphStack)-1]
			}
			continue
		}
		if mermaidFlowchartDirective(statement) {
			continue
		}
		cursor := mermaidCursor{input: statement}
		from, ok := cursor.readNode()
		if !ok {
			continue
		}
		addMermaidNode(doc, from.id, from.Label, from.shape)
		addMermaidSubgraphNode(doc, subgraphStack, from.id)

		parsedEdge := false
		for {
			label, ok := cursor.readFlowchartConnector()
			if !ok {
				break
			}
			to, ok := cursor.readNode()
			if !ok {
				break
			}
			addMermaidNode(doc, to.id, to.Label, to.shape)
			addMermaidSubgraphNode(doc, subgraphStack, to.id)
			doc.Edges = append(doc.Edges, Edge{From: from.id, To: to.id, Label: cleanMermaidLabel(label)})
			from = to
			parsedEdge = true
		}
		if !parsedEdge {
			addMermaidOrder(doc, from.id)
		}
	}
}

func parseMermaidSubgraph(statement string) (string, string, bool) {
	fields := strings.Fields(statement)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "subgraph") {
		return "", "", false
	}

	definition := strings.TrimSpace(statement[len(fields[0]):])
	cursor := mermaidCursor{input: definition}
	node, ok := cursor.readNode()
	if ok && cursor.eof() {
		return node.id, node.Label, true
	}

	label := cleanMermaidLabel(definition)
	id := strings.ReplaceAll(strings.ToLower(label), " ", "-")
	return id, label, id != ""
}

func addMermaidSubgraphNode(doc *Document, stack []int, id string) {
	if len(stack) == 0 {
		return
	}
	index := stack[len(stack)-1]
	doc.Subgraphs[index].Nodes = appendUniqueMermaid(doc.Subgraphs[index].Nodes, id)
}

func (c *mermaidCursor) readFlowchartConnector() (string, bool) {
	position := c.pos
	if _, ok := c.readEdgeOperator(); ok {
		label, _ := c.readPipeLabel()
		return label, true
	}

	c.pos = position
	c.skipSpace()
	if !c.hasPrefix("--") {
		return "", false
	}
	c.pos += 2
	labelStart := c.pos
	if end := strings.Index(c.input[c.pos:], "-->"); end >= 0 {
		c.pos += end
		label := c.input[labelStart:c.pos]
		c.pos += len("-->")
		return label, true
	}
	c.pos = position
	return "", false
}

func mermaidFlowchartDirective(statement string) bool {
	for field := range strings.FieldsSeq(statement) {
		switch strings.ToLower(field) {
		case "direction", "classdef", "class", "style", "click", "linkstyle":
			return true
		default:
			return false
		}
	}
	return true
}
