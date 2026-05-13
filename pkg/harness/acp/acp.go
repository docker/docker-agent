// Package acp provides the shared ACP (Agent Client Protocol) harness base
// for docker-agent. It implements acp.Client and translates ACP SessionNotification
// updates into canonical harness events.
//
// Concrete adapters (copilot, openclaw) embed BaseAdapter and supply only the
// subprocess invocation details.
package acp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/harness"
)

// Config holds ACP adapter-specific configuration shared by all ACP adapters.
type Config struct {
	Command string   `json:"command,omitempty" yaml:"command,omitempty"`
	Args    []string `json:"args,omitempty" yaml:"args,omitempty"`
}

// BaseAdapter provides the shared ACP client implementation.
// Concrete adapters embed this and implement Name(), Capabilities(), and binaryArgs().
type BaseAdapter struct {
	// BinaryName is the default binary name (e.g. "copilot", "openclaw").
	BinaryName string
	// DefaultArgs are the default arguments to pass to the binary.
	DefaultArgs []string
}

// RunACP implements harness.ACPAdapter.
func (b *BaseAdapter) RunACP(ctx context.Context, req harness.SubSessionRequest, callbacks harness.ACPCallbacks) {
	if err := b.runACP(ctx, req, callbacks); err != nil {
		req.Events.Emit(harness.RunError{
			RunID:   req.RunID,
			Code:    harness.ErrCodeHarnessCrashed,
			Message: err.Error(),
			At:      time.Now(),
		})
	}
}

// Run implements harness.HarnessAdapter. ACP adapters should always be called
// via RunACP; this method exists for interface compliance and logs a warning.
func (b *BaseAdapter) Run(ctx context.Context, req harness.SubSessionRequest) {
	slog.Warn("ACP adapter Run() called without ACPCallbacks; use RunACP instead",
		"adapter", b.BinaryName)
	req.Events.Emit(harness.RunError{
		RunID:   req.RunID,
		Code:    harness.ErrCodeCapabilityMismatch,
		Message: "ACP adapter requires ACPCallbacks; call RunACP instead of Run",
		At:      time.Now(),
	})
}

func (b *BaseAdapter) runACP(ctx context.Context, req harness.SubSessionRequest, callbacks harness.ACPCallbacks) error {
	binary := b.BinaryName
	args := b.DefaultArgs

	cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec
	cmd.Dir = req.WorkingDir
	cmd.Env = buildEnv(req)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("acp stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("acp stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("acp stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("acp start %q: %w", binary, err)
	}

	// Drain stderr.
	go drainStderr(stderr)

	// Build the ACP client.
	client := &acpClient{
		runID:     req.RunID,
		events:    req.Events,
		callbacks: callbacks,
	}

	conn := acpsdk.NewClientSideConnection(client, stdin, stdout)
	conn.SetLogger(slog.Default())

	// Initialize the ACP session.
	_, err = conn.Initialize(ctx, acpsdk.InitializeRequest{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		ClientCapabilities: acpsdk.ClientCapabilities{
			Fs: acpsdk.FileSystemCapabilities{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
		},
	})
	if err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("acp initialize: %w", err)
	}

	// Create a new session.
	sessResp, err := conn.NewSession(ctx, acpsdk.NewSessionRequest{
		Cwd: req.WorkingDir,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("acp new session: %w", err)
	}

	// Emit RunStart now that we have a session ID.
	req.Events.Emit(harness.RunStart{
		RunID:        req.RunID,
		HarnessRunID: string(sessResp.SessionId),
		At:           time.Now(),
	})
	client.sessionID = string(sessResp.SessionId)

	// Send the prompt.
	_, err = conn.Prompt(ctx, acpsdk.PromptRequest{
		SessionId: sessResp.SessionId,
		Prompt:    []acpsdk.ContentBlock{acpsdk.TextBlock(req.Task)},
	})
	if err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("acp prompt: %w", err)
	}

	// Emit RunEnd on success.
	req.Events.Emit(harness.RunEnd{
		RunID:        req.RunID,
		HarnessRunID: string(sessResp.SessionId),
		StopReason:   "success",
		At:           time.Now(),
	})

	_ = cmd.Process.Kill()
	return nil
}

// buildEnv constructs the environment for the ACP subprocess.
func buildEnv(req harness.SubSessionRequest) []string {
	env := os.Environ()
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func drainStderr(r interface{ Read([]byte) (int, error) }) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			slog.Debug("acp stderr", "data", string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

// --- acpClient implements acp.Client ---

type acpClient struct {
	runID     string
	sessionID string
	events    harness.EventSink
	callbacks harness.ACPCallbacks
}

// SessionUpdate translates ACP session notifications to canonical harness events.
func (c *acpClient) SessionUpdate(_ context.Context, params acpsdk.SessionNotification) error {
	now := time.Now()
	u := params.Update

	switch {
	case u.AgentMessageChunk != nil:
		chunk := u.AgentMessageChunk
		if chunk.Content.Text != nil {
			msgID := c.runID
			if chunk.MessageId != nil {
				msgID = *chunk.MessageId
			}
			c.events.Emit(harness.TextDelta{
				MessageID: msgID,
				Delta:     chunk.Content.Text.Text,
				At:        now,
			})
		}

	case u.AgentThoughtChunk != nil:
		chunk := u.AgentThoughtChunk
		if chunk.Content.Text != nil {
			msgID := c.runID
			if chunk.MessageId != nil {
				msgID = *chunk.MessageId
			}
			c.events.Emit(harness.ReasoningDelta{
				MessageID: msgID,
				Delta:     chunk.Content.Text.Text,
				At:        now,
			})
		}

	case u.ToolCall != nil:
		tc := u.ToolCall
		switch tc.Status {
		case acpsdk.ToolCallStatusPending, acpsdk.ToolCallStatusInProgress, "":
			c.events.Emit(harness.ToolCallStart{
				ToolCallID: string(tc.ToolCallId),
				ToolName:   tc.Title,
				At:         now,
			})
		case acpsdk.ToolCallStatusCompleted, acpsdk.ToolCallStatusFailed:
			c.events.Emit(harness.ToolCallEnd{
				ToolCallID: string(tc.ToolCallId),
				At:         now,
			})
		}

	case u.ToolCallUpdate != nil:
		tcu := u.ToolCallUpdate
		if tcu.Status != nil && *tcu.Status == acpsdk.ToolCallStatusCompleted {
			result := ""
			if tcu.RawOutput != nil {
				if s, ok := tcu.RawOutput.(string); ok {
					result = s
				}
			}
			c.events.Emit(harness.ToolCallResult{
				ToolCallID: string(tcu.ToolCallId),
				Result:     result,
				At:         now,
			})
		}
	}

	return nil
}

// RequestPermission handles ACP permission requests via the PermissionRequester callback.
func (c *acpClient) RequestPermission(ctx context.Context, params acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
	if c.callbacks.Permission == nil {
		// Auto-allow if no permission requester configured.
		if len(params.Options) > 0 {
			return acpsdk.RequestPermissionResponse{
				Outcome: acpsdk.RequestPermissionOutcome{
					Selected: &acpsdk.RequestPermissionOutcomeSelected{
						OptionId: params.Options[0].OptionId,
					},
				},
			}, nil
		}
		return acpsdk.RequestPermissionResponse{
			Outcome: acpsdk.RequestPermissionOutcome{
				Cancelled: &acpsdk.RequestPermissionOutcomeCancelled{},
			},
		}, nil
	}

	title := ""
	if params.ToolCall.Title != nil {
		title = *params.ToolCall.Title
	}
	var options []string
	for _, o := range params.Options {
		options = append(options, string(o.Kind))
	}

	allowed, _, err := c.callbacks.Permission.Request(ctx, "", title, title, options)
	if err != nil {
		return acpsdk.RequestPermissionResponse{
			Outcome: acpsdk.RequestPermissionOutcome{
				Cancelled: &acpsdk.RequestPermissionOutcomeCancelled{},
			},
		}, nil
	}

	if allowed && len(params.Options) > 0 {
		// Find an allow option.
		for _, o := range params.Options {
			if o.Kind == acpsdk.PermissionOptionKindAllowOnce || o.Kind == acpsdk.PermissionOptionKindAllowAlways {
				return acpsdk.RequestPermissionResponse{
					Outcome: acpsdk.RequestPermissionOutcome{
						Selected: &acpsdk.RequestPermissionOutcomeSelected{OptionId: o.OptionId},
					},
				}, nil
			}
		}
	}

	return acpsdk.RequestPermissionResponse{
		Outcome: acpsdk.RequestPermissionOutcome{
			Cancelled: &acpsdk.RequestPermissionOutcomeCancelled{},
		},
	}, nil
}

// ReadTextFile delegates to the ToolExecutor.
func (c *acpClient) ReadTextFile(ctx context.Context, params acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
	if c.callbacks.ToolExecutor == nil {
		return acpsdk.ReadTextFileResponse{}, fmt.Errorf("no ToolExecutor configured")
	}
	// Simple implementation: read the file directly.
	data, err := os.ReadFile(params.Path)
	if err != nil {
		return acpsdk.ReadTextFileResponse{}, err
	}
	return acpsdk.ReadTextFileResponse{Content: string(data)}, nil
}

// WriteTextFile delegates to the ToolExecutor.
func (c *acpClient) WriteTextFile(ctx context.Context, params acpsdk.WriteTextFileRequest) (acpsdk.WriteTextFileResponse, error) {
	if c.callbacks.ToolExecutor == nil {
		return acpsdk.WriteTextFileResponse{}, fmt.Errorf("no ToolExecutor configured")
	}
	if err := os.WriteFile(params.Path, []byte(params.Content), 0o644); err != nil {
		return acpsdk.WriteTextFileResponse{}, err
	}
	return acpsdk.WriteTextFileResponse{}, nil
}

// Terminal methods -- stub implementations for v1.
func (c *acpClient) CreateTerminal(_ context.Context, _ acpsdk.CreateTerminalRequest) (acpsdk.CreateTerminalResponse, error) {
	return acpsdk.CreateTerminalResponse{TerminalId: "stub-terminal"}, nil
}

func (c *acpClient) KillTerminal(_ context.Context, _ acpsdk.KillTerminalRequest) (acpsdk.KillTerminalResponse, error) {
	return acpsdk.KillTerminalResponse{}, nil
}

func (c *acpClient) TerminalOutput(_ context.Context, _ acpsdk.TerminalOutputRequest) (acpsdk.TerminalOutputResponse, error) {
	return acpsdk.TerminalOutputResponse{Output: "", Truncated: false}, nil
}

func (c *acpClient) ReleaseTerminal(_ context.Context, _ acpsdk.ReleaseTerminalRequest) (acpsdk.ReleaseTerminalResponse, error) {
	return acpsdk.ReleaseTerminalResponse{}, nil
}

func (c *acpClient) WaitForTerminalExit(_ context.Context, _ acpsdk.WaitForTerminalExitRequest) (acpsdk.WaitForTerminalExitResponse, error) {
	return acpsdk.WaitForTerminalExitResponse{}, nil
}
