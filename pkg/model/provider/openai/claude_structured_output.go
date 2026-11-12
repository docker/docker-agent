package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// claudeSchemaInstruction builds the prompt instruction that repeats a
// structured-output schema in machine-readable form so a model produces raw
// JSON even when the endpoint drops the request's response_format field. The
// schema's name and description are included when set: they carry intent the
// bare schema does not.
func claudeSchemaInstruction(structuredOutput *latest.StructuredOutput) (string, error) {
	schemaJSON, err := json.Marshal(structuredOutput.Schema)
	if err != nil {
		return "", fmt.Errorf("failed to serialize structured output schema %q: %w", structuredOutput.Name, err)
	}

	var sb strings.Builder
	sb.WriteString("Your entire reply MUST be a single JSON object that validates against this JSON schema:")
	if structuredOutput.Name != "" {
		fmt.Fprintf(&sb, "\nSchema name: %s", structuredOutput.Name)
	}
	if structuredOutput.Description != "" {
		fmt.Fprintf(&sb, "\nSchema description: %s", structuredOutput.Description)
	}
	fmt.Fprintf(&sb, "\n\n%s\n\n", schemaJSON)
	sb.WriteString("Output raw JSON only: no Markdown formatting, no code fences, no explanations, and no text before or after the JSON object.")
	return sb.String(), nil
}

// withClaudeSchemaInstruction returns messages carrying the structured-output
// schema instruction as leading system content when structured output is
// configured and this client serves a Claude-family model. All other
// requests are returned unchanged.
//
// OpenAI-compatible endpoints fronting Claude (GitHub Copilot, LiteLLM-style
// gateways, ...) may silently ignore response_format.json_schema, and Claude
// then answers with Markdown prose that fails strict JSON parsing (issue
// #3840). Repeating the schema in the prompt makes the model emit raw JSON
// regardless; response_format is still sent as best effort.
//
// The instruction is merged into an existing leading plain-text system
// message, or prepended as a new system message otherwise: system content
// must stay at the beginning because some OpenAI-compatible backends only
// accept a system message at index 0 (see shouldMergeConsecutiveMessages).
// The caller-owned slice is never mutated: changes land on a fresh copy.
func (c *Client) withClaudeSchemaInstruction(ctx context.Context, messages []chat.Message) []chat.Message {
	structuredOutput := c.ModelOptions.StructuredOutput()
	if structuredOutput == nil {
		return messages
	}
	// The nil store skips the models.dev lookup: this check runs on the
	// request hot path and must never load or fetch the catalog.
	// modelinfo's name-pattern fallback recognizes the bare and
	// provider-qualified Claude IDs this fix targets (explicit Claude
	// models behind OpenAI-compatible endpoints).
	if !modelinfo.IsClaude(ctx, nil, c.ID()) {
		return messages
	}

	instruction, err := claudeSchemaInstruction(structuredOutput)
	if err != nil {
		// The request stays valid without the fallback; response_format is
		// still sent.
		slog.WarnContext(ctx, "Skipping structured output schema instruction for Claude model", "error", err)
		return messages
	}

	slog.DebugContext(ctx, "Injecting structured output schema instruction for Claude model",
		"model", c.ModelConfig.Model, "name", structuredOutput.Name)

	if len(messages) > 0 && messages[0].Role == chat.MessageRoleSystem && len(messages[0].MultiContent) == 0 {
		out := slices.Clip(slices.Clone(messages))
		out[0].Content += "\n\n" + instruction
		return out
	}

	out := make([]chat.Message, 0, len(messages)+1)
	out = append(out, chat.Message{Role: chat.MessageRoleSystem, Content: instruction})
	return append(out, messages...)
}
