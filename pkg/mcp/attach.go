package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime/remote"
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

	client, err := remote.NewClient(addr)
	if err != nil {
		return nil, fmt.Errorf("client for %s: %w", addr, err)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "docker-agent (attached)",
		Version: version.Version,
	}, &mcp.ServerOptions{
		// Override the SDK default that advertises deprecated MCP logging.
		Capabilities: &mcp.ServerCapabilities{},
	})

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

// ResolveAttachSession returns the session ID an attach client should drive for
// the run at addr. recorded is the session ID stored in the run's discovery
// record; explicit reports whether the caller named that session explicitly
// (rather than discovering the run by pid, address, or "latest").
//
// A run's discovery record keeps naming the session it started with, but under
// --listen that initial tab can be closed while the run (and other tabs) stay
// alive. Attaching to the closed ID would make every send fail with "session
// not found or not running", so:
//
//   - when the recorded session is still attached, it is used;
//   - an explicitly named session that is no longer attached fails with a clear
//     error listing the live sessions instead of being silently redirected to
//     another one;
//   - a fresh run-level discovery resolves the live session when there is
//     exactly one, and otherwise fails clearly listing the candidates.
func ResolveAttachSession(ctx context.Context, addr, recorded string, explicit bool) (string, error) {
	client, err := remote.NewClient(addr)
	if err != nil {
		return "", fmt.Errorf("client for %s: %w", addr, err)
	}
	active, err := client.GetActiveSessions(ctx)
	if err != nil {
		return "", fmt.Errorf("listing attached sessions of %s: %w", addr, err)
	}
	live := make([]string, 0, len(active))
	for _, sess := range active {
		if sess.ID == recorded {
			return recorded, nil
		}
		live = append(live, sess.ID)
	}

	if explicit {
		return "", fmt.Errorf("session %s is no longer attached to the run at %s (its tab was closed); live sessions: %s", recorded, addr, formatSessionIDs(live))
	}
	switch len(live) {
	case 0:
		return "", fmt.Errorf("the run at %s has no attached session: its initial session %s was closed and no other tab is open", addr, recorded)
	case 1:
		return live[0], nil
	default:
		return "", fmt.Errorf("the run's initial session %s is no longer attached and the run has several live sessions; select one explicitly with --attach <session-id>: %s", recorded, formatSessionIDs(live))
	}
}

func formatSessionIDs(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

// StartAttachStdio runs the attach MCP server over stdio.
func StartAttachStdio(ctx context.Context, addr, sessionID string) error {
	server, err := AttachServer(ctx, addr, sessionID)
	if err != nil {
		return err
	}
	return server.Run(ctx, &mcp.StdioTransport{})
}
