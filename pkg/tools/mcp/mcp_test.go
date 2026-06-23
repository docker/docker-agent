package mcp

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

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

func (m *mockMCPClient) SetSamplingHandler(tools.SamplingHandler) {}

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
	m.waitCh = make(chan struct{}) // fresh channel for each session
	m.mu.Unlock()
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
	// Close the wait channel to unblock Wait().
	select {
	case <-m.waitCh:
	default:
		close(m.waitCh)
	}
	m.mu.Unlock()
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
			})
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
			})

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
			})

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
	t.Cleanup(func() { _ = ts.Stop(t.Context()) })

	result, err := ts.callTool(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Name:      "test_tool",
			Arguments: `{"key": "value"}`,
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "recovered", result.Output)
	assert.Equal(t, int32(2), callCount.Load(), "expected exactly 2 CallTool invocations (1 failed + 1 retry)")
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

	_ = ts.Stop(t.Context())
}
