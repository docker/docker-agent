package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/version"
)

type SendInput struct {
	Message  string `json:"message" jsonschema:"the message to send"`
	FollowUp bool   `json:"followup,omitempty" jsonschema:"queue as end-of-turn follow-up instead of mid-turn steer"`
}

type SendOutput struct {
	Status string `json:"status"`
}

type TranscriptInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"max number of recent messages to return (0 = all)"`
}

type TranscriptOutput struct {
	Title    string        `json:"title"`
	Messages []TranscriptM `json:"messages"`
}

type TranscriptM struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AttachServer wires an MCP server to a running docker-agent run accessible
// via its HTTP control plane. It exposes a small toolset (send, transcript)
// that other agents can use to drive the live session.
func AttachServer(ctx context.Context, addr, sessionID string) (*mcp.Server, error) {
	if addr == "" || sessionID == "" {
		return nil, errors.New("attach: addr and sessionID are required")
	}

	client, err := runtime.NewClient(addr)
	if err != nil {
		return nil, fmt.Errorf("client for %s: %w", addr, err)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "docker-agent (attached)",
		Version: version.Version,
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "send",
		Description: fmt.Sprintf("Send a message to the running docker-agent session %s.", sessionID),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SendInput) (*mcp.CallToolResult, SendOutput, error) {
		msgs := []api.Message{{Content: in.Message}}
		var err error
		if in.FollowUp {
			err = client.FollowUpSession(ctx, sessionID, msgs)
		} else {
			err = client.SteerSession(ctx, sessionID, msgs)
		}
		if err != nil {
			return nil, SendOutput{}, err
		}
		return nil, SendOutput{Status: "queued"}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "transcript",
		Description: fmt.Sprintf("Read the transcript of the running docker-agent session %s.", sessionID),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in TranscriptInput) (*mcp.CallToolResult, TranscriptOutput, error) {
		sess, err := client.GetSession(ctx, sessionID)
		if err != nil {
			return nil, TranscriptOutput{}, err
		}

		out := TranscriptOutput{Title: sess.Title}
		messages := sess.Messages
		if in.Limit > 0 && len(messages) > in.Limit {
			messages = messages[len(messages)-in.Limit:]
		}
		for _, m := range messages {
			out.Messages = append(out.Messages, TranscriptM{
				Role:    string(m.Message.Role),
				Content: m.Message.Content,
			})
		}
		return nil, out, nil
	})

	slog.DebugContext(ctx, "MCP attach server ready", "addr", addr, "session_id", sessionID)
	return server, nil
}

// StartAttachStdio runs the attach MCP server over stdio.
func StartAttachStdio(ctx context.Context, addr, sessionID string) error {
	server, err := AttachServer(ctx, addr, sessionID)
	if err != nil {
		return err
	}
	return server.Run(ctx, &mcp.StdioTransport{})
}
