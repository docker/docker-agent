package transcript

import (
	"testing"

	"gotest.tools/v3/golden"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSimple(t *testing.T) {
	sess := session.New(session.WithUserMessage("Hello"))
	content := PlainText(sess)
	golden.Assert(t, content, "simple.golden")
}

func TestAssistantMessage(t *testing.T) {
	sess := session.New(
		session.WithUserMessage("Hello"),
	)
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Hello to you too",
		},
	})
	content := PlainText(sess)
	golden.Assert(t, content, "assistant_message.golden")
}

func TestAssistantMessageWithReasoning(t *testing.T) {
	sess := session.New(
		session.WithUserMessage("Hello"),
	)
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:             chat.MessageRoleAssistant,
			Content:          "Hello to you too",
			ReasoningContent: "Hm....",
		},
	})
	content := PlainText(sess)
	golden.Assert(t, content, "assistant_message_with_reasoning.golden")
}

func TestToolCalls(t *testing.T) {
	sess := session.New(
		session.WithUserMessage("Hello"),
	)
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Hello to you too",
			ToolCalls: []tools.ToolCall{
				{
					Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`},
				},
			},
		},
	})

	sess.AddMessage(&session.Message{
		AgentName: "",
		Message: chat.Message{
			Role:    chat.MessageRoleTool,
			Content: ".\n..",
		},
	})
	content := PlainText(sess)

	golden.Assert(t, content, "tool_calls.golden")
}

// ─── Document attachment rendering ───────────────────────────────────────────

func TestUserMessageWithImageDocument(t *testing.T) {
	msg := session.UserMessage(
		"Check this image",
		chat.MessagePart{Type: chat.MessagePartTypeText, Text: "Check this image"},
		chat.MessagePart{
			Type: chat.MessagePartTypeDocument,
			Document: &chat.Document{
				Name:     "photo.jpg",
				MimeType: "image/jpeg",
				Size:     42000,
				Source:   chat.DocumentSource{InlineData: []byte{0xFF, 0xD8}},
			},
		},
	)
	sess := session.New()
	sess.AddMessage(msg)
	content := PlainText(sess)
	golden.Assert(t, content, "user_message_image_document.golden")
}

func TestUserMessageWithPDFDocument(t *testing.T) {
	msg := session.UserMessage(
		"Summarise this PDF",
		chat.MessagePart{Type: chat.MessagePartTypeText, Text: "Summarise this PDF"},
		chat.MessagePart{
			Type: chat.MessagePartTypeDocument,
			Document: &chat.Document{
				Name:     "report.pdf",
				MimeType: "application/pdf",
				Size:     251658, // ~245 KB
				Source:   chat.DocumentSource{InlineData: []byte{0x25, 0x50, 0x44, 0x46}},
			},
		},
	)
	sess := session.New()
	sess.AddMessage(msg)
	content := PlainText(sess)
	golden.Assert(t, content, "user_message_pdf_document.golden")
}

func TestUserMessageWithTextDocument(t *testing.T) {
	msg := session.UserMessage(
		"Review this file",
		chat.MessagePart{Type: chat.MessagePartTypeText, Text: "Review this file"},
		chat.MessagePart{
			Type: chat.MessagePartTypeDocument,
			Document: &chat.Document{
				Name:     "readme.md",
				MimeType: "text/markdown",
				Source:   chat.DocumentSource{InlineText: "# Hello World"},
			},
		},
	)
	sess := session.New()
	sess.AddMessage(msg)
	content := PlainText(sess)
	golden.Assert(t, content, "user_message_text_document.golden")
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{2 * 1024 * 1024, "2.0 MB"},
		{1536 * 1024, "1.5 MB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatBytes(tt.input)
			if got != tt.want {
				t.Errorf("formatBytes(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
