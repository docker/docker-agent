package prompts

import (
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/rag/types"
)

// BuildRerankDocumentsPrompt formats the user prompt with query and numbered documents.
// It includes metadata (source path and custom metadata) for better context.
func BuildRerankDocumentsPrompt(query string, documents []types.Document) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Query:\n%s\n\n", query)
	fmt.Fprintf(&b, "Documents:\n")

	for i, doc := range documents {
		// Format document with metadata for better context
		fmt.Fprintf(&b, "[%d]", i)

		// Add metadata if present
		if doc.SourcePath != "" || len(doc.Metadata) > 0 {
			fmt.Fprintf(&b, " (")
			var parts []string

			if doc.SourcePath != "" {
				parts = append(parts, "source: "+doc.SourcePath)
			}

			// Add relevant metadata
			for key, value := range doc.Metadata {
				parts = append(parts, fmt.Sprintf("%s: %s", key, value))
			}

			fmt.Fprintf(&b, "%s", strings.Join(parts, ", "))
			fmt.Fprintf(&b, ")")
		}

		fmt.Fprintf(&b, ":\n%s\n\n", doc.Content)
	}

	return b.String()
}

// BuildRerankSystemPrompt constructs the reranking system prompt.
// It supports full override via providerOpts["rerank_prompt"] or builds a default
// prompt with optional criteria and provider-specific JSON format instructions.
func BuildRerankSystemPrompt(
	documents []types.Document,
	criteria string,
	providerOpts map[string]any,
	jsonFormatInstruction string,
) string {
	// Check for full override first
	if providerOpts != nil {
		if v, ok := providerOpts["rerank_prompt"]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				// Full override mode - user takes complete control
				return s
			}
		}
	}

	// Default mode: construct from base + criteria + format instructions
	systemPrompt := `You are a reranking model.
Given a search query and a list of documents, you assign each document a relevance score between 0 and 1.
Higher scores mean more relevant.`

	// Inject user-provided criteria if specified
	if criteria != "" {
		systemPrompt += "\n\n" + criteria
	}

	// Append scoring instructions with explicit count and format requirements
	systemPrompt += fmt.Sprintf(`

You MUST carefully evaluate each document's relevance and assign DIFFERENT scores to reflect varying degrees of relevance.
Not all documents are equally relevant - differentiate between them.
%s

IMPORTANT: You have been given %d documents, so you MUST return exactly %d scores in the "scores" array.
Each score must be a number between 0 and 1, where 1 is most relevant.`,
		jsonFormatInstruction, len(documents), len(documents))

	return systemPrompt
}
