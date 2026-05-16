package codemode

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
)

func toolToJsDoc(tool tools.Tool) string {
	var doc strings.Builder

	doc.WriteString(toComment(&tool))
	fmt.Fprintf(&doc, "function %s(args: Input): Output { ... }\n", tool.Name)

	return doc.String()
}

func toComment(tool *tools.Tool) string {
	var comment strings.Builder

	inputSchema, _ := json.MarshalIndent(tool.Parameters, " * ", "  ")
	outputSchema, _ := json.MarshalIndent(tool.OutputSchema, " * ", "  ")

	comment.WriteString("\n/**\n")
	for line := range strings.SplitSeq(tool.Description, "\n") {
		comment.WriteString(" * " + strings.TrimSpace(line) + "\n")
	}
	comment.WriteString(" * \n")
	comment.WriteString(" * @param args - Input object containing the parameters.\n")
	comment.WriteString(" * @returns Output - The result of the function execution.\n")
	comment.WriteString(" *\n")
	comment.WriteString(" * Where Input follows the following JSON schema:\n")
	comment.WriteString(" * " + string(inputSchema) + "\n")
	comment.WriteString(" *\n")
	comment.WriteString(" * And Output follows the following JSON schema:\n")
	comment.WriteString(" * " + string(outputSchema) + "\n")
	comment.WriteString(" */\n")

	return comment.String()
}
