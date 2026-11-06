package mcp

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

func TestRemoteToolsetLogsDoNotExposeCredentials(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	const secret = "audit-secret-value"
	ts := NewRemoteToolset(
		"test",
		"https://user:"+secret+"@example.com/mcp?api_key="+secret,
		"streamable",
		map[string]string{"Authorization": "Bearer " + secret, "X-API-Key": secret},
		nil,
	)

	output := logs.String()
	assert.NotContains(t, output, secret)
	assert.NotContains(t, output, "user:")
	assert.Contains(t, output, "Authorization")
	assert.Contains(t, output, "X-API-Key")
	assert.Equal(t, "example.com", ts.logID)
}

// mockMCPClient is a test double for the mcpClient interface.
type mockMCPClient struct {
	callToolFn func(ctx context.Context, request *mcp.CallToolParams) (*mcp.CallToolResult, error)
}

func (m *mockMCPClient) Initialize(context.Context, *mcp.InitializeRequest) (*mcp.InitializeResult, error) {
	return &mcp.InitializeResult{}, nil
}

func (m *mockMCPClient) ListTools(context.Context, *mcp.ListToolsParams) iter.Seq2[*mcp.Tool, error] {
	return func(func(*mcp.Tool, error) bool) {}
}

func (m *mockMCPClient) CallTool(ctx context.Context, request *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return m.callToolFn(ctx, request)
}

func (m *mockMCPClient) ListPrompts(context.Context, *mcp.ListPromptsParams) iter.Seq2[*mcp.Prompt, error] {
	return func(func(*mcp.Prompt, error) bool) {}
}

func (m *mockMCPClient) GetPrompt(context.Context, *mcp.GetPromptParams) (*mcp.GetPromptResult, error) {
	return &mcp.GetPromptResult{}, nil
}

func (m *mockMCPClient) SetElicitationHandler(tools.ElicitationHandler) {}

func (m *mockMCPClient) SetOAuthSuccessHandler(func()) {}

func (m *mockMCPClient) SetManagedOAuth(bool) {}

func (m *mockMCPClient) SetUnmanagedOAuthRedirectURI(string) {}

func (m *mockMCPClient) SetToolListChangedHandler(func()) {}

func (m *mockMCPClient) SetPromptListChangedHandler(func()) {}

func (m *mockMCPClient) ServerAddress() string { return "mock://test" }

func (m *mockMCPClient) Wait() error { return nil }

func (m *mockMCPClient) Close(context.Context) error { return nil }

// reconnectableMockClient extends mockMCPClient with reconnect simulation.
type reconnectableMockClient struct {
	mockMCPClient

	mu     sync.Mutex
	waitCh chan struct{} // closed when Close is called, unblocking Wait
}

func newReconnectableMock() *reconnectableMockClient {
	return &reconnectableMockClient{
		waitCh: make(chan struct{}),
	}
}

func (m *reconnectableMockClient) Initialize(context.Context, *mcp.InitializeRequest) (*mcp.InitializeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.waitCh = make(chan struct{}) // fresh channel for each session
	return &mcp.InitializeResult{}, nil
}

func (m *reconnectableMockClient) Wait() error {
	m.mu.Lock()
	ch := m.waitCh
	m.mu.Unlock()
	<-ch
	return nil
}

func (m *reconnectableMockClient) Close(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Close the wait channel to unblock Wait().
	select {
	case <-m.waitCh:
	default:
		close(m.waitCh)
	}
	return nil
}

func TestToolsAndCallToolRoundTrip(t *testing.T) {
	t.Parallel()

	// Round-trip test: whatever name Tools() exposes, callTool() must
	// forward to the underlying server with the original (unprefixed)
	// name. This guards both flows (catalog-activated toolsets that
	// always have a name, and YAML-declared command/remote toolsets
	// that may or may not have one).
	tests := []struct {
		name           string
		toolsetName    string
		serverToolName string
		wantExposed    string
	}{
		{
			name:           "named toolset (e.g. mcp catalog server) prefixes and strips",
			toolsetName:    "github-official",
			serverToolName: "get_issue",
			wantExposed:    "github-official_get_issue",
		},
		{
			name:           "named non-catalog mcp server (YAML name set) prefixes and strips",
			toolsetName:    "my-mcp",
			serverToolName: "do_thing",
			wantExposed:    "my-mcp_do_thing",
		},
		{
			name:           "unnamed mcp toolset (no YAML name) does not prefix or strip",
			toolsetName:    "",
			serverToolName: "do_thing",
			wantExposed:    "do_thing",
		},
		{
			name:           "server tool name that already contains the toolset name as a substring",
			toolsetName:    "github",
			serverToolName: "github_status",
			wantExposed:    "github_github_status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var capturedName string
			mock := &mockMCPClient{
				callToolFn: func(_ context.Context, request *mcp.CallToolParams) (*mcp.CallToolResult, error) {
					capturedName = request.Name
					return &mcp.CallToolResult{
						Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
					}, nil
				},
			}
			// ListTools returns one tool with the server-side name.
			mock2 := &listToolsMock{mockMCPClient: *mock, toolName: tt.serverToolName}

			ts := newTestToolset(tt.toolsetName, "test", mock2)
			ts.markStartedForTesting()

			exposed, err := ts.Tools(t.Context())
			require.NoError(t, err)
			require.Len(t, exposed, 1)
			assert.Equal(t, tt.wantExposed, exposed[0].Name,
				"Tools() must expose the prefixed name to the model")

			// The model calls back using the exposed name.
			_, err = ts.callTool(t.Context(), tools.ToolCall{
				Function: tools.FunctionCall{
					Name:      exposed[0].Name,
					Arguments: `{}`,
				},
			}, tools.NopRuntime{})
			require.NoError(t, err)
			assert.Equal(t, tt.serverToolName, capturedName,
				"callTool() must forward the original (unprefixed) tool name to the server")
		})
	}
}

// listToolsMock extends mockMCPClient with a single tool returned by ListTools.
type listToolsMock struct {
	mockMCPClient

	toolName string
}

func (m *listToolsMock) ListTools(context.Context, *mcp.ListToolsParams) iter.Seq2[*mcp.Tool, error] {
	return func(yield func(*mcp.Tool, error) bool) {
		yield(&mcp.Tool{Name: m.toolName}, nil)
	}
}

func TestCallToolStripsToolsetNamePrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		toolsetName     string
		calledToolName  string
		wantForwardName string
	}{
		{
			name:            "prefix is stripped when toolset has a name",
			toolsetName:     "github",
			calledToolName:  "github_get_issue",
			wantForwardName: "get_issue",
		},
		{
			name:            "name with hyphens (mcp catalog id) is stripped",
			toolsetName:     "github-official",
			calledToolName:  "github-official_get_issue",
			wantForwardName: "get_issue",
		},
		{
			name:            "only the leading toolset prefix is stripped",
			toolsetName:     "github",
			calledToolName:  "github_github_get_issue",
			wantForwardName: "github_get_issue",
		},
		{
			name:            "unprefixed call is forwarded unchanged",
			toolsetName:     "github",
			calledToolName:  "get_issue",
			wantForwardName: "get_issue",
		},
		{
			name:            "unnamed toolset forwards as-is",
			toolsetName:     "",
			calledToolName:  "get_issue",
			wantForwardName: "get_issue",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var capturedName string

			ts := newTestToolset(tt.toolsetName, "test", &mockMCPClient{
				callToolFn: func(_ context.Context, request *mcp.CallToolParams) (*mcp.CallToolResult, error) {
					capturedName = request.Name
					return &mcp.CallToolResult{
						Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
					}, nil
				},
			})
			ts.markStartedForTesting()

			_, err := ts.callTool(t.Context(), tools.ToolCall{
				Function: tools.FunctionCall{
					Name:      tt.calledToolName,
					Arguments: `{}`,
				},
			}, tools.NopRuntime{})

			require.NoError(t, err)
			assert.Equal(t, tt.wantForwardName, capturedName)
		})
	}
}

func TestCallToolStripsNullArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		arguments    string
		expectedArgs map[string]any
	}{
		{
			name:         "all null values are stripped",
			arguments:    `{"dir": null, "pattern": null}`,
			expectedArgs: map[string]any{},
		},
		{
			name:         "only null values are stripped",
			arguments:    `{"dir": ".", "pattern": null, "extra": "value"}`,
			expectedArgs: map[string]any{"dir": ".", "extra": "value"},
		},
		{
			name:         "empty arguments stay empty",
			arguments:    `{}`,
			expectedArgs: map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var capturedArgs map[string]any

			ts := newTestToolset("test", "test", &mockMCPClient{
				callToolFn: func(_ context.Context, request *mcp.CallToolParams) (*mcp.CallToolResult, error) {
					if m, ok := request.Arguments.(map[string]any); ok {
						capturedArgs = m
					}
					return &mcp.CallToolResult{
						Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
					}, nil
				},
			})
			ts.markStartedForTesting()

			result, err := ts.callTool(t.Context(), tools.ToolCall{
				Function: tools.FunctionCall{
					Name:      "test_tool",
					Arguments: tt.arguments,
				},
			}, tools.NopRuntime{})

			require.NoError(t, err)
			assert.Equal(t, "ok", result.Output)
			assert.Equal(t, tt.expectedArgs, capturedArgs)
		})
	}
}

func TestProcessMCPContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		input          *mcp.CallToolResult
		wantOutput     string
		wantIsError    bool
		wantImages     []tools.MediaContent
		wantAudios     []tools.MediaContent
		wantDocuments  []tools.DocumentContent
		wantStructured any
	}{
		// --- text ---
		{
			name:       "text only",
			input:      callToolResult(&mcp.TextContent{Text: "hello"}),
			wantOutput: "hello",
		},
		{
			name:       "empty response",
			input:      &mcp.CallToolResult{},
			wantOutput: "no output",
		},
		{
			// FastMCP returns one TextContent per element of a list result.
			name:       "separate text blocks do not run together",
			input:      callToolResult(&mcp.TextContent{Text: "alpha.txt"}, &mcp.TextContent{Text: "beta.txt"}, &mcp.TextContent{Text: "gamma.txt"}),
			wantOutput: "alpha.txt\nbeta.txt\ngamma.txt",
		},
		{
			name:       "text block already ending in a newline is not doubled",
			input:      callToolResult(&mcp.TextContent{Text: "line one\n"}, &mcp.TextContent{Text: "line two"}),
			wantOutput: "line one\nline two",
		},
		{
			name:       "empty text block adds no separator",
			input:      callToolResult(&mcp.TextContent{Text: "a"}, &mcp.TextContent{Text: ""}, &mcp.TextContent{Text: "b"}),
			wantOutput: "a\nb",
		},
		{
			name:       "separate resource links do not run together",
			input:      callToolResult(&mcp.ResourceLink{Name: "a", URI: "file:///a"}, &mcp.ResourceLink{Name: "b", URI: "file:///b"}),
			wantOutput: "[a](file:///a)\n[b](file:///b)",
		},

		// --- images ---
		{
			name:       "image only",
			input:      callToolResult(&mcp.ImageContent{Data: []byte("imagedata"), MIMEType: "image/png"}),
			wantOutput: "no output",
			wantImages: []tools.MediaContent{{Data: "aW1hZ2VkYXRh", MimeType: "image/png"}},
		},
		{
			name:       "text and image",
			input:      callToolResult(&mcp.TextContent{Text: "Here is the screenshot"}, &mcp.ImageContent{Data: []byte("screenshot"), MIMEType: "image/jpeg"}),
			wantOutput: "Here is the screenshot",
			wantImages: []tools.MediaContent{{Data: "c2NyZWVuc2hvdA==", MimeType: "image/jpeg"}},
		},
		{
			name:        "error with image",
			input:       &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "error occurred"}, &mcp.ImageContent{Data: []byte("error"), MIMEType: "image/png"}}},
			wantOutput:  "error occurred",
			wantIsError: true,
			wantImages:  []tools.MediaContent{{Data: "ZXJyb3I=", MimeType: "image/png"}},
		},

		// --- audio ---
		{
			name:       "audio only",
			input:      callToolResult(&mcp.AudioContent{Data: []byte("audiodata"), MIMEType: "audio/wav"}),
			wantOutput: "no output",
			wantAudios: []tools.MediaContent{{Data: "YXVkaW9kYXRh", MimeType: "audio/wav"}},
		},
		{
			name:       "text and audio",
			input:      callToolResult(&mcp.TextContent{Text: "Here is the recording"}, &mcp.AudioContent{Data: []byte("recording"), MIMEType: "audio/mp3"}),
			wantOutput: "Here is the recording",
			wantAudios: []tools.MediaContent{{Data: "cmVjb3JkaW5n", MimeType: "audio/mp3"}},
		},
		{
			name:       "text image and audio",
			input:      callToolResult(&mcp.TextContent{Text: "multimedia"}, &mcp.ImageContent{Data: []byte("img"), MIMEType: "image/png"}, &mcp.AudioContent{Data: []byte("aud"), MIMEType: "audio/wav"}),
			wantOutput: "multimedia",
			wantImages: []tools.MediaContent{{Data: "aW1n", MimeType: "image/png"}},
			wantAudios: []tools.MediaContent{{Data: "YXVk", MimeType: "audio/wav"}},
		},

		// --- resource links ---
		{
			name:       "resource link with name",
			input:      callToolResult(&mcp.ResourceLink{Name: "my-doc", URI: "file:///path/to/doc.txt"}),
			wantOutput: "[my-doc](file:///path/to/doc.txt)",
		},
		{
			name:       "resource link without name",
			input:      callToolResult(&mcp.ResourceLink{URI: "file:///path/to/doc.txt"}),
			wantOutput: "file:///path/to/doc.txt",
		},
		{
			name:       "text and resource link",
			input:      callToolResult(&mcp.TextContent{Text: "See: "}, &mcp.ResourceLink{Name: "readme", URI: "file:///README.md"}),
			wantOutput: "See: [readme](file:///README.md)",
		},
		{
			name:       "resource link name with bracket is escaped",
			input:      callToolResult(&mcp.ResourceLink{Name: "doc]name", URI: "file:///doc.txt"}),
			wantOutput: `[doc\]name](file:///doc.txt)`,
		},
		{
			name:       "resource link URI with paren is escaped",
			input:      callToolResult(&mcp.ResourceLink{Name: "doc", URI: "file:///path(1)/doc.txt"}),
			wantOutput: "[doc](file:///path(1%29/doc.txt)",
		},

		// --- embedded resources ---
		{
			name: "embedded text resource",
			input: callToolResult(&mcp.EmbeddedResource{
				Resource: &mcp.ResourceContents{
					URI:      "file:///notes.txt",
					MIMEType: "text/plain",
					Text:     "hello world",
				},
			}),
			wantOutput: "hello world",
		},
		{
			name: "text ack and embedded text resource concatenate",
			input: callToolResult(
				&mcp.TextContent{Text: "downloaded "},
				&mcp.EmbeddedResource{
					Resource: &mcp.ResourceContents{
						URI:      "file:///notes.txt",
						MIMEType: "text/plain",
						Text:     "hello world",
					},
				},
			),
			wantOutput: "downloaded hello world",
		},
		{
			name: "embedded image resource routes to documents",
			input: callToolResult(&mcp.EmbeddedResource{
				Resource: &mcp.ResourceContents{
					URI:      "file:///image.png",
					MIMEType: "image/png",
					Blob:     []byte("PNGBYTES"),
				},
			}),
			wantOutput:    "no output",
			wantDocuments: []tools.DocumentContent{{Name: "image.png", URI: "file:///image.png", Data: "UE5HQllURVM=", MimeType: "image/png"}},
		},
		{
			name: "embedded text blob resource routes to documents",
			input: callToolResult(&mcp.EmbeddedResource{
				Resource: &mcp.ResourceContents{
					URI:      "file:///notes.md",
					MIMEType: "text/markdown",
					Blob:     []byte("# Notes"),
				},
			}),
			wantOutput:    "no output",
			wantDocuments: []tools.DocumentContent{{Name: "notes.md", URI: "file:///notes.md", Text: "# Notes", MimeType: "text/markdown"}},
		},
		{
			name: "embedded audio resource routes to audios",
			input: callToolResult(&mcp.EmbeddedResource{
				Resource: &mcp.ResourceContents{
					URI:      "file:///clip.wav",
					MIMEType: "audio/wav",
					Blob:     []byte("WAVBYTES"),
				},
			}),
			wantOutput: "no output",
			wantAudios: []tools.MediaContent{{Data: "V0FWQllURVM=", MimeType: "audio/wav"}},
		},
		{
			name: "text ack and embedded image resource",
			input: callToolResult(
				&mcp.TextContent{Text: "here you go"},
				&mcp.EmbeddedResource{
					Resource: &mcp.ResourceContents{
						URI:      "file:///image.jpg",
						MIMEType: "image/jpeg",
						Blob:     []byte("JPGBYTES"),
					},
				},
			),
			wantOutput:    "here you go",
			wantDocuments: []tools.DocumentContent{{Name: "image.jpg", URI: "file:///image.jpg", Data: "SlBHQllURVM=", MimeType: "image/jpeg"}},
		},
		{
			name: "embedded pdf resource routes to documents",
			input: callToolResult(&mcp.EmbeddedResource{
				Resource: &mcp.ResourceContents{
					URI:      "file:///doc.pdf",
					MIMEType: "application/pdf",
					Blob:     []byte("PDFBYTES"),
				},
			}),
			wantOutput:    "no output",
			wantDocuments: []tools.DocumentContent{{Name: "doc.pdf", URI: "file:///doc.pdf", Data: "UERGQllURVM=", MimeType: "application/pdf"}},
		},
		{
			name: "embedded unsupported blob still emits a marker",
			input: callToolResult(&mcp.EmbeddedResource{
				Resource: &mcp.ResourceContents{
					URI:      "file:///archive.zip",
					MIMEType: "application/zip",
					Blob:     []byte("ZIPBYTES"),
				},
			}),
			wantOutput: "[embedded resource file:///archive.zip (application/zip, 8 bytes)]",
		},
		{
			name: "embedded resource with nil contents is no-op",
			input: callToolResult(&mcp.EmbeddedResource{
				Resource: nil,
			}),
			wantOutput: "no output",
		},

		// --- structured content ---
		{
			name:           "structured content passed through",
			input:          &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}, StructuredContent: map[string]any{"status": "ok", "count": float64(42)}},
			wantOutput:     "done",
			wantStructured: map[string]any{"status": "ok", "count": float64(42)},
		},
		{
			name:       "nil structured content",
			input:      callToolResult(&mcp.TextContent{Text: "hello"}),
			wantOutput: "hello",
		},
		{
			name:           "structured content without text",
			input:          &mcp.CallToolResult{StructuredContent: map[string]any{"key": "value"}},
			wantOutput:     "no output",
			wantStructured: map[string]any{"key": "value"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := processMCPContent(tt.input)

			assert.Equal(t, tt.wantOutput, result.Output)
			assert.Equal(t, tt.wantIsError, result.IsError)

			if tt.wantImages != nil {
				assert.Equal(t, tt.wantImages, result.Images)
			} else {
				assert.Empty(t, result.Images)
			}
			if tt.wantAudios != nil {
				assert.Equal(t, tt.wantAudios, result.Audios)
			} else {
				assert.Empty(t, result.Audios)
			}
			if tt.wantDocuments != nil {
				assert.Equal(t, tt.wantDocuments, result.Documents)
			} else {
				assert.Empty(t, result.Documents)
			}
			assert.Equal(t, tt.wantStructured, result.StructuredContent)
		})
	}
}

// callToolResult is a helper to build a CallToolResult from content blocks.
func callToolResult(content ...mcp.Content) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: content}
}

func TestCallToolRecoversFromErrSessionMissing(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var callCount atomic.Int32

		mock := newReconnectableMock()
		mock.callToolFn = func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			n := callCount.Add(1)
			if n == 1 {
				// First call: simulate server restart by returning ErrSessionMissing.
				return nil, fmt.Errorf("tools/call: %w", mcp.ErrSessionMissing)
			}
			// Second call (after reconnect): succeed.
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "recovered"}},
			}, nil
		}

		ts := newTestToolset("test-server", "test-server", mock)
		require.NoError(t, ts.Start(t.Context()))
		t.Cleanup(func() { _ = ts.Stop(context.WithoutCancel(t.Context())) })

		result, err := ts.callTool(t.Context(), tools.ToolCall{
			Function: tools.FunctionCall{
				Name:      "test_tool",
				Arguments: `{"key": "value"}`,
			},
		}, tools.NopRuntime{})

		require.NoError(t, err)
		assert.Equal(t, "recovered", result.Output)
		assert.Equal(t, int32(2), callCount.Load(), "expected exactly 2 CallTool invocations (1 failed + 1 retry)")
	})
}

func TestCallToolTimeoutFires(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		mock := &mockMCPClient{
			callToolFn: func(ctx context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}

		ts := newTestToolset("test-server", "test-server", mock)
		ts.callTimeout = 50 * time.Millisecond
		ts.markStartedForTesting()

		start := time.Now()
		_, err := ts.callTool(t.Context(), tools.ToolCall{
			Function: tools.FunctionCall{Name: "test_tool", Arguments: `{}`},
		}, tools.NopRuntime{})
		elapsed := time.Since(start)

		require.Error(t, err)
		require.ErrorIs(t, err, tools.ErrCallTimeout, "expected ErrCallTimeout, got: %v", err)
		assert.Contains(t, err.Error(), "timed out")
		assert.Less(t, elapsed, 5*time.Second, "the call_timeout should have fired promptly")
		assert.Equal(t, lifecycle.StateReady, ts.State().State, "a fired call_timeout must not disturb the toolset's lifecycle state")
	})
}

func TestCallToolNoTimeoutWhenCallTimeoutUnset(t *testing.T) {
	t.Parallel()

	var sawDeadline bool
	mock := &mockMCPClient{
		callToolFn: func(ctx context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			_, sawDeadline = ctx.Deadline()
			return callToolResult(&mcp.TextContent{Text: "ok"}), nil
		},
	}

	ts := newTestToolset("test-server", "test-server", mock)
	// ts.callTimeout is left at its zero value.
	ts.markStartedForTesting()

	result, err := ts.callTool(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{Name: "test_tool", Arguments: `{}`},
	}, tools.NopRuntime{})

	require.NoError(t, err)
	assert.Equal(t, "ok", result.Output)
	assert.False(t, sawDeadline, "no call_timeout means the caller's context must be used unmodified")
}

func TestCallToolParentCancelWinsOverTimeout(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		mock := &mockMCPClient{
			callToolFn: func(ctx context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}

		ts := newTestToolset("test-server", "test-server", mock)
		ts.callTimeout = time.Hour // large enough that only the parent cancel can fire first
		ts.markStartedForTesting()

		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			<-started
			cancel()
		}()

		_, err := ts.callTool(ctx, tools.ToolCall{
			Function: tools.FunctionCall{Name: "test_tool", Arguments: `{}`},
		}, tools.NopRuntime{})

		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled, "expected context.Canceled, got: %v", err)
		assert.NotErrorIs(t, err, tools.ErrCallTimeout, "a parent cancel must not be misreported as a call_timeout")
	})
}

func TestCallToolTimeoutCoversReconnectRetry(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var callCount, initCount atomic.Int32
		mock := newReconnectableMock()
		mock.callToolFn = func(_ context.Context, _ *mcp.CallToolParams) (*mcp.CallToolResult, error) {
			callCount.Add(1)
			// Always fail with a connection error, forcing a reconnect attempt.
			return nil, fmt.Errorf("tools/call: %w", mcp.ErrSessionMissing)
		}
		slowInit := &slowReconnectClient{reconnectableMockClient: mock, reconnectDelay: 300 * time.Millisecond, initCount: &initCount}

		ts := newTestToolset("test-server", "test-server", slowInit)
		ts.callTimeout = 50 * time.Millisecond
		require.NoError(t, ts.Start(t.Context()))
		t.Cleanup(func() { _ = ts.Stop(context.WithoutCancel(t.Context())) })

		start := time.Now()
		_, err := ts.callTool(t.Context(), tools.ToolCall{
			Function: tools.FunctionCall{Name: "test_tool", Arguments: `{}`},
		}, tools.NopRuntime{})
		elapsed := time.Since(start)

		require.Error(t, err)
		require.ErrorIs(t, err, tools.ErrCallTimeout, "expected ErrCallTimeout, got: %v", err)
		assert.Less(t, elapsed, sessionMissingRetryTimeout,
			"the call_timeout must cover the reconnect-retry as one budget, not stack on top of the 35s retry wait")
		assert.GreaterOrEqual(t, callCount.Load(), int32(1))
	})
}

// slowReconnectClient blocks the second-and-later Initialize call for
// reconnectDelay, simulating a reconnect that outlasts a short call_timeout.
type slowReconnectClient struct {
	*reconnectableMockClient

	reconnectDelay time.Duration
	initCount      *atomic.Int32
}

func (m *slowReconnectClient) Initialize(ctx context.Context, req *mcp.InitializeRequest) (*mcp.InitializeResult, error) {
	if m.initCount.Add(1) > 1 {
		select {
		case <-time.After(m.reconnectDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return m.reconnectableMockClient.Initialize(ctx, req)
}

func TestNewToolsetCommandSetsCallTimeoutFromPolicy(t *testing.T) {
	t.Parallel()

	ts := NewToolsetCommand("test", "gopls", nil, nil, "", lifecycle.Policy{CallTimeout: 42 * time.Second})
	assert.Equal(t, 42*time.Second, ts.callTimeout)
}

func TestNewRemoteToolsetSetsCallTimeoutFromPolicy(t *testing.T) {
	t.Parallel()

	ts := NewRemoteToolset("test", "https://example.com", "streamable", nil, nil, lifecycle.Policy{CallTimeout: 7 * time.Second})
	assert.Equal(t, 7*time.Second, ts.callTimeout)
}

func TestNewToolsetCommandNoPolicyMeansNoCallTimeout(t *testing.T) {
	t.Parallel()

	ts := NewToolsetCommand("test", "gopls", nil, nil, "")
	assert.Equal(t, time.Duration(0), ts.callTimeout)
}

func TestRemoteToolsetDefaultsRestartAlways(t *testing.T) {
	t.Parallel()

	policy := remotePolicy(lifecycle.Policy{Restart: lifecycle.RestartOnFailure})

	assert.Equal(t, lifecycle.RestartAlways, policy.Restart)
}

func TestRemoteToolsetPreservesRestartNever(t *testing.T) {
	t.Parallel()

	policy := remotePolicy(lifecycle.Policy{Restart: lifecycle.RestartNever})

	assert.Equal(t, lifecycle.RestartNever, policy.Restart)
}

func TestRemotePolicyPreservesRestartAlways(t *testing.T) {
	t.Parallel()

	policy := remotePolicy(lifecycle.Policy{Restart: lifecycle.RestartAlways})

	assert.Equal(t, lifecycle.RestartAlways, policy.Restart)
}

func TestRemoteToolsetReconnectsAfterCleanClose(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		pingTool := &mcp.Tool{Name: "ping"}
		mock := &failingInitClient{
			toolsToList: []*mcp.Tool{pingTool},
			waitCh:      make(chan struct{}),
		}

		ts := newTestToolset("test-remote", "remote-server", mock)
		ts.supervisor = newSupervisor(ts, lifecycle.Policy{
			Restart: lifecycle.RestartAlways,
			Backoff: lifecycle.Backoff{
				Initial:    time.Millisecond,
				Max:        2 * time.Millisecond,
				Multiplier: 2,
			},
		})

		require.NoError(t, ts.Start(t.Context()))
		defer func() { _ = ts.Stop(t.Context()) }()
		require.True(t, ts.IsStarted())

		require.NoError(t, mock.Close(t.Context()))
		require.Eventually(t, func() bool {
			mock.mu.Lock()
			initCalls := mock.initCalls
			mock.mu.Unlock()
			return initCalls >= 2 && ts.IsStarted()
		}, 2*time.Second, 10*time.Millisecond, "remote toolset did not reconnect after clean close")

		toolList, err := ts.Tools(t.Context())
		require.NoError(t, err)
		require.Len(t, toolList, 1)
		assert.Equal(t, "test-remote_ping", toolList[0].Name)
	})
}

func TestMergeChildEnv_ToolsetEnvWins(t *testing.T) {
	t.Setenv("DOCKER_AGENT_ENV_PRECEDENCE_PROBE", "host")

	env := mergeChildEnv([]string{"DOCKER_AGENT_ENV_PRECEDENCE_PROBE=toolset"})

	var got []string
	for _, entry := range env {
		if strings.HasPrefix(entry, "DOCKER_AGENT_ENV_PRECEDENCE_PROBE=") {
			got = append(got, entry)
		}
	}
	// exec.Cmd keeps the last duplicate, so the configured value must be the last one.
	if len(got) != 2 || got[len(got)-1] != "DOCKER_AGENT_ENV_PRECEDENCE_PROBE=toolset" {
		t.Fatalf("configured env must come last for exec.Cmd to prefer it, got %v", got)
	}
}
