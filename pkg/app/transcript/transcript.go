package transcript

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func PlainText(sess *session.Session) string {
	var builder strings.Builder

	messages := sess.GetAllMessages()
	for i := range messages {
		msg := messages[i]

		if msg.Implicit {
			continue
		}

		switch msg.Message.Role {
		case chat.MessageRoleUser:
			writeUserMessage(&builder, msg)
		case chat.MessageRoleAssistant:
			writeAssistantMessage(&builder, msg)
		case chat.MessageRoleTool:
			writeToolMessage(&builder, msg)
		}
	}

	return strings.TrimSpace(builder.String())
}

func writeUserMessage(builder *strings.Builder, msg session.Message) {
	builder.WriteString("\n## User\n")

	// When MultiContent is present, render text and attachment chips.
	// The text part reflects what was actually sent to the model (the user's
	// original typed text, plus any dimension notes from image resizing and
	// inlined file headers from ReadFileForInline). This is intentional: a
	// transcript should show what the model received, not just what the user
	// typed into the prompt box. Callers wanting just the raw typed text
	// should read msg.Message.Content directly.
	if len(msg.Message.MultiContent) > 0 {
		for _, part := range msg.Message.MultiContent {
			switch part.Type {
			case chat.MessagePartTypeText:
				if part.Text != "" {
					fmt.Fprintf(builder, "\n%s\n", part.Text)
				}
			case chat.MessagePartTypeDocument:
				if part.Document != nil {
					doc := part.Document
					switch {
					case chat.IsImageMimeType(doc.MimeType):
						size := doc.Size
						if size == 0 {
							size = int64(len(doc.Source.InlineData))
						}
						fmt.Fprintf(builder, "\n[image: %s (%s, %s)]\n", doc.Name, doc.MimeType, formatBytes(size))
					case doc.Source.InlineText != "":
						fmt.Fprintf(builder, "\n[attachment: %s (%s)]\n", doc.Name, doc.MimeType)
					default:
						size := doc.Size
						if size == 0 {
							size = int64(len(doc.Source.InlineData))
						}
						fmt.Fprintf(builder, "\n[attachment: %s (%s, %s)]\n", doc.Name, doc.MimeType, formatBytes(size))
					}
				}
			// Note: superseded types kept for backward-compat with stored sessions.
			case chat.MessagePartTypeImageURL:
				if part.ImageURL != nil {
					fmt.Fprintf(builder, "\n[image: %s]\n", part.ImageURL.URL[:min(len(part.ImageURL.URL), 60)])
				}
			case chat.MessagePartTypeFile:
				if part.File != nil {
					fmt.Fprintf(builder, "\n[file: %s]\n", part.File.Path)
				}
			}
		}
		return
	}
	fmt.Fprintf(builder, "\n%s\n", msg.Message.Content)
}

// formatBytes returns a human-readable byte size string (e.g. "1.2 MB").
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func writeAssistantMessage(builder *strings.Builder, msg session.Message) {
	builder.WriteString("\n## Assistant")
	if msg.AgentName != "" {
		fmt.Fprintf(builder, " (%s)", msg.AgentName)
	}
	builder.WriteString("\n\n")

	if msg.Message.ReasoningContent != "" {
		builder.WriteString("### Reasoning\n\n")
		builder.WriteString(msg.Message.ReasoningContent)
		builder.WriteString("\n\n")
	}

	if msg.Message.Content != "" {
		builder.WriteString(msg.Message.Content)
		builder.WriteString("\n")
	}

	if len(msg.Message.ToolCalls) > 0 {
		builder.WriteString("\n### Tool Calls\n\n")
		for _, toolCall := range msg.Message.ToolCalls {
			fmt.Fprintf(builder, "- **%s**", toolCall.Function.Name)
			if toolCall.ID != "" {
				fmt.Fprintf(builder, " (ID: %s)", toolCall.ID)
			}

			builder.WriteString("\n")
			toJSONString(builder, toolCall.Function.Arguments)
			builder.WriteString("\n")
		}
		builder.WriteString("\n")
	}
}

func writeToolMessage(builder *strings.Builder, msg session.Message) {
	builder.WriteString("### Tool Result")
	if msg.Message.ToolCallID != "" {
		fmt.Fprintf(builder, " (ID: %s)", msg.Message.ToolCallID)
	}
	fmt.Fprintf(builder, "\n\n")

	toJSONString(builder, msg.Message.Content)
	builder.WriteString("\n")
}

func toJSONString(builder *strings.Builder, in string) {
	var content any
	if err := json.Unmarshal([]byte(in), &content); err == nil {
		if formatted, err := json.MarshalIndent(content, "", "  "); err == nil {
			builder.WriteString("```json\n")
			builder.Write(formatted)
			builder.WriteString("\n```\n")
		} else {
			builder.WriteString(in)
			builder.WriteString("\n")
		}
	} else {
		if in != "" {
			builder.WriteString(in)
			builder.WriteString("\n")
		}
	}
}
