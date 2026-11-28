package agui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
)

const maxIDLength = 256

var knownRoles = []types.Role{
	types.RoleDeveloper, types.RoleSystem, types.RoleAssistant, types.RoleUser,
	types.RoleTool, types.RoleActivity, types.RoleReasoning,
}

// userMessage is a user message of the request, reduced to its text.
type userMessage struct {
	id   string
	text string
}

// runInput is a validated RunAgentInput.
type runInput struct {
	threadID string
	runID    string
	// newMessages are the user messages that end the client history.
	newMessages []userMessage
}

// decodeRunInput reads and validates a RunAgentInput. Fields this server does
// not implement are rejected rather than ignored.
func decodeRunInput(body io.Reader) (*runInput, error) {
	dec := json.NewDecoder(body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if dec.More() {
		return nil, errors.New("invalid JSON body: unexpected data after the RunAgentInput object")
	}
	if trimmed := strings.TrimSpace(string(raw)); !strings.HasPrefix(trimmed, "{") {
		return nil, errors.New("request body must be a RunAgentInput object")
	}

	var in types.RunAgentInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("invalid RunAgentInput: %w", err)
	}

	if err := validateID("threadId", in.ThreadID); err != nil {
		return nil, err
	}
	if err := validateID("runId", in.RunID); err != nil {
		return nil, err
	}
	if len(in.Tools) > 0 {
		return nil, errors.New("tools: frontend tools are not supported")
	}
	if !isEmptyValue(in.State) {
		return nil, errors.New("state: shared state is not supported")
	}
	if len(in.Resume) > 0 {
		return nil, errors.New("resume: interrupts are not supported")
	}

	messages := make([]userMessage, 0, len(in.Messages))
	for i, m := range in.Messages {
		if m.ID == "" {
			return nil, fmt.Errorf("messages[%d]: id is required", i)
		}
		if !slices.Contains(knownRoles, m.Role) {
			return nil, fmt.Errorf("messages[%d]: unknown role %q", i, m.Role)
		}
		if m.Role != types.RoleUser {
			// The server-side session is the source of truth for history.
			messages = messages[:0]
			continue
		}
		text, err := userText(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		messages = append(messages, userMessage{id: m.ID, text: text})
	}
	if len(messages) == 0 {
		return nil, errors.New("messages: the history must end with a user message")
	}

	return &runInput{threadID: in.ThreadID, runID: in.RunID, newMessages: messages}, nil
}

func validateID(field, value string) error {
	switch {
	case strings.TrimSpace(value) == "":
		return fmt.Errorf("%s is required", field)
	case len(value) > maxIDLength:
		return fmt.Errorf("%s is longer than %d bytes", field, maxIDLength)
	}
	return nil
}

// isEmptyValue reports whether a decoded JSON value carries no data.
func isEmptyValue(v any) bool {
	switch value := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(value) == 0
	case []any:
		return len(value) == 0
	case string:
		return value == ""
	}
	return false
}

// userText extracts the text of a user message. Content is either a string or
// a list of parts; only text parts are supported.
func userText(content any) (string, error) {
	var text string
	switch value := content.(type) {
	case string:
		text = value
	case []any:
		var parts []string
		for i, item := range value {
			part, ok := item.(map[string]any)
			if !ok {
				return "", fmt.Errorf("content[%d] is not an object", i)
			}
			if kind, _ := part["type"].(string); kind != types.InputContentTypeText {
				return "", fmt.Errorf("content[%d]: only text content is supported", i)
			}
			partText, ok := part["text"].(string)
			if !ok {
				return "", fmt.Errorf("content[%d]: text must be a string", i)
			}
			parts = append(parts, partText)
		}
		text = strings.Join(parts, "\n")
	case nil:
	default:
		return "", errors.New("content must be a string or a list of text parts")
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("user message content is empty")
	}
	return text, nil
}
