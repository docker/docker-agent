package acp

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestACPCapabilityMatrix(t *testing.T) {
	t.Parallel()
	const expected = `{"auth":{"logout":{}},"loadSession":true,"sessionCapabilities":{"additionalDirectories":{},"close":{},"delete":{},"list":{},"resume":{}},"promptCapabilities":{"audio":true,"embeddedContext":true,"image":true},"mcpCapabilities":{"http":true,"sse":true}}`
	for fsBits := range 4 {
		for _, terminal := range []bool{false, true} {
			for elicitBits := range 4 {
				t.Run(fmt.Sprintf("fs=%d/terminal=%t/elicitation=%d", fsBits, terminal, elicitBits), func(t *testing.T) {
					t.Parallel()
					source := config.NewBytesSource("matrix", []byte("agents:\n  root:\n    model: openai/gpt-4o\n    toolsets:\n      - type: filesystem\n      - type: shell\n      - type: environment\n"))
					a := NewAgent(source, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}, EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"}), ModelsDevStoreOverride: modelsdev.NewDatabaseStore(embedded.Snapshot())}, session.NewInMemorySessionStore())
					defer func() { require.NoError(t, a.Stop(t.Context())) }()
					caps := acpsdk.ClientCapabilities{Fs: acpsdk.FileSystemCapabilities{ReadTextFile: fsBits&1 != 0, WriteTextFile: fsBits&2 != 0}, Terminal: terminal}
					if elicitBits != 0 {
						caps.Elicitation = &acpsdk.ElicitationCapabilities{}
						if elicitBits&1 != 0 {
							caps.Elicitation.Form = &acpsdk.ElicitationFormCapabilities{}
						}
						if elicitBits&2 != 0 {
							caps.Elicitation.Url = &acpsdk.ElicitationUrlCapabilities{}
						}
					}
					peer := newElicitationPeer(t, a, nil, func(map[string]any) any { return map[string]any{"action": "decline"} })
					response, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{ProtocolVersion: 1, ClientCapabilities: caps})
					require.NoError(t, err)
					wire, err := json.Marshal(response.AgentCapabilities)
					require.NoError(t, err)
					assert.JSONEq(t, expected, string(wire))
					require.Len(t, response.AuthMethods, 1)
					assert.Equal(t, hostCredentialsMethod, response.AuthMethods[0].Agent.Id)
					created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
					require.NoError(t, err)
					s := a.sessions[string(created.SessionId)]
					available, err := s.rt.CurrentAgentTools(t.Context())
					require.NoError(t, err)
					byName := map[string]tools.Tool{}
					for _, tool := range available {
						byName[tool.Name] = tool
					}
					for name, want := range map[string]bool{"read_file": fsBits&1 != 0, "read_multiple_files": fsBits&1 != 0, "write_file": fsBits&2 != 0, "edit_file": fsBits == 3} {
						_, found := byName[name]
						assert.Equal(t, want, found, name)
					}
					require.Contains(t, byName, "shell")
					assert.Equal(t, terminal, strings.Contains(byName["shell"].Description, "ACP client's terminal"))
					require.Contains(t, byName, "get_environment_info")
					assert.Equal(t, terminal, s.terminals != nil)
					handler := a.elicitationHandler(s.id)
					assert.Equal(t, elicitBits != 0, handler != nil)
					assert.Empty(t, peer.snapshot(), "discovery must not call client methods")
					if handler != nil {
						for _, mode := range []string{"form", "url"} {
							req := formRequest()
							req.Mode = mode
							if mode == "url" {
								req.URL = "https://example.invalid/authorize"
								req.ElicitationID = "id"
							}
							_, err := handler(t.Context(), req)
							require.NoError(t, err)
						}
					}
					wantCalls := 0
					if elicitBits&1 != 0 {
						wantCalls++
					}
					if elicitBits&2 != 0 {
						wantCalls++
					}
					assert.Len(t, peer.snapshot(), wantCalls)
					for _, line := range peer.output.lines() {
						assert.NotContains(t, line, `"method":"fs/`)
						assert.NotContains(t, line, `"method":"terminal/`)
					}
				})
			}
		}
	}
}

func TestACPCapabilityWireDefaultsAndUnsupportedRequests(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{}`, `{"elicitation":null}`, `{"elicitation":{}}`} {
		var caps acpsdk.ClientCapabilities
		require.NoError(t, json.Unmarshal([]byte(raw), &caps))
		assert.False(t, caps.Terminal)
		assert.False(t, caps.Fs.ReadTextFile)
		assert.False(t, caps.Fs.WriteTextFile)
		if caps.Elicitation != nil {
			assert.Nil(t, caps.Elicitation.Form)
			assert.Nil(t, caps.Elicitation.Url)
		}
	}
	a := clientMCPAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	input, send := io.Pipe()
	receive, output := io.Pipe()
	conn := a.NewConnection(output, input)
	t.Cleanup(func() { _ = send.Close(); _ = receive.Close(); <-conn.Done() })
	encoder, decoder := json.NewEncoder(send), json.NewDecoder(receive)
	for i, method := range []string{"session/fork", "providers/list", "unknown/method"} {
		params := map[string]any{}
		if method == "session/fork" {
			params = map[string]any{"sessionId": "missing", "cwd": t.TempDir(), "mcpServers": []any{}}
		}
		require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": i, "method": method, "params": params}))
		var response struct {
			Error *acpsdk.RequestError `json:"error"`
		}
		require.NoError(t, decoder.Decode(&response))
		require.NotNil(t, response.Error)
		assert.Equal(t, -32601, response.Error.Code)
	}
}
