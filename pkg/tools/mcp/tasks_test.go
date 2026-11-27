package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestCreateToolSetExperimentalTasks(t *testing.T) {
	for _, tc := range []struct {
		name         string
		experimental *userconfig.ExperimentalSettings
		want         bool
	}{
		{name: "unset", experimental: nil, want: false},
		{name: "disabled", experimental: &userconfig.ExperimentalSettings{}, want: false},
		{name: "enabled", experimental: &userconfig.ExperimentalSettings{MCPTasks: true}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths.SetConfigDir(t.TempDir())
			t.Cleanup(func() { paths.SetConfigDir("") })
			if tc.experimental != nil {
				require.NoError(t, (&userconfig.Config{
					Settings: &userconfig.Settings{Experimental: tc.experimental},
				}).Save())
			}

			runConfig := &config.RuntimeConfig{EnvProviderOverride: environment.NewMapEnvProvider(nil)}
			cfg := latest.Toolset{Type: "mcp", Remote: latest.Remote{URL: "https://example.com/mcp", TransportType: "streamable"}}
			toolset, err := CreateToolSet(t.Context(), cfg, runConfig)
			require.NoError(t, err)
			client := toolset.(*Toolset).mcpClient.(*remoteMCPClient)
			assert.Equal(t, tc.want, client.tasksEnabled())

			fromConfig := NewRemoteToolsetFromConfig(t.Context(), cfg, runConfig.EnvProvider())
			assert.Equal(t, tc.want, fromConfig.mcpClient.(*remoteMCPClient).tasksEnabled())
		})
	}
}

func TestGatewayToolsetExperimentalTasksSetter(t *testing.T) {
	ts, err := NewGatewayToolset(t.Context(), "demo", "demo-server", nil, nil, environment.NewMapEnvProvider(nil), "")
	require.NoError(t, err)

	var iface any = ts
	setter, ok := iface.(interface{ SetExperimentalTasks(enabled bool) })
	require.True(t, ok, "GatewayToolset must satisfy the SetExperimentalTasks interface")
	setter.SetExperimentalTasks(true)
	client := ts.mcpClient.(*stdioMCPClient)
	assert.True(t, client.tasksEnabled())
	client.setExperimentalTasks(false)
	assert.False(t, client.tasksEnabled())
}

func TestTasksStreamableHTTP(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/event-stream"} {
		t.Run(contentType, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()

			var mu sync.Mutex
			polls := map[string]int{}
			updatedInputs := map[string]string{}
			cancelledTasks := map[string]bool{}
			seenHeaders := map[string]string{}
			var tasksCapAdvertised atomic.Bool
			var elicitCalls atomic.Int32
			cancelTaskPolled := make(chan struct{})
			var cancelTaskPolledOnce sync.Once

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(req.ID) == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}

				var metaWrap struct {
					Meta map[string]json.RawMessage `json:"_meta"`
				}
				if json.Unmarshal(req.Params, &metaWrap) == nil {
					var caps gomcp.ClientCapabilities
					if json.Unmarshal(metaWrap.Meta["io.modelcontextprotocol/clientCapabilities"], &caps) == nil {
						if _, ok := caps.Extensions[tasksExtension]; ok {
							tasksCapAdvertised.Store(true)
						}
					}
				}

				writeResp := func(result any) {
					payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
					if contentType == "text/event-stream" && req.Method == "tools/call" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
						_ = http.NewResponseController(w).Flush()
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(payload)
				}

				now := time.Now().UTC().Format(time.RFC3339Nano)
				switch req.Method {
				case "server/discover":
					writeResp(map[string]any{
						"resultType":        "complete",
						"supportedVersions": []string{"2026-07-28"},
						"capabilities": map[string]any{
							"tools":      map[string]any{},
							"extensions": map[string]any{tasksExtension: map[string]any{}},
						},
					})
				case "tools/list":
					writeResp(map[string]any{
						"resultType": "complete",
						"tools": []any{
							map[string]any{"name": "sync_tool", "inputSchema": map[string]any{"type": "object"}},
							map[string]any{"name": "async_elicit", "inputSchema": map[string]any{"type": "object"}},
							map[string]any{"name": "async_tool_error", "inputSchema": map[string]any{"type": "object"}},
							map[string]any{"name": "async_fail", "inputSchema": map[string]any{"type": "object"}},
							map[string]any{"name": "async_cancel", "inputSchema": map[string]any{"type": "object"}},
						},
					})
				case "tools/call":
					var p struct {
						Name string `json:"name"`
					}
					_ = json.Unmarshal(req.Params, &p)
					switch p.Name {
					case "sync_tool":
						writeResp(map[string]any{
							"resultType": "complete",
							"content":    []any{map[string]any{"type": "text", "text": "sync-ok"}},
						})
					case "async_elicit":
						writeResp(map[string]any{
							"resultType":     "task",
							"taskId":         "task-elicit",
							"status":         "working",
							"createdAt":      now,
							"lastUpdatedAt":  now,
							"ttlMs":          60000,
							"pollIntervalMs": 5,
						})
					case "async_tool_error":
						writeResp(map[string]any{
							"resultType":     "task",
							"taskId":         "task-tool-err",
							"status":         "working",
							"createdAt":      now,
							"lastUpdatedAt":  now,
							"ttlMs":          60000,
							"pollIntervalMs": 5,
						})
					case "async_fail":
						writeResp(map[string]any{
							"resultType":     "task",
							"taskId":         "task-fail",
							"status":         "working",
							"createdAt":      now,
							"lastUpdatedAt":  now,
							"ttlMs":          60000,
							"pollIntervalMs": 5,
						})
					case "async_cancel":
						writeResp(map[string]any{
							"resultType":     "task",
							"taskId":         "task-cancel",
							"status":         "working",
							"createdAt":      now,
							"lastUpdatedAt":  now,
							"ttlMs":          60000,
							"pollIntervalMs": 20,
						})
					}
				case "tasks/get":
					var p struct {
						TaskID string `json:"taskId"`
					}
					_ = json.Unmarshal(req.Params, &p)
					mu.Lock()
					seenHeaders["tasks/get:"+p.TaskID] = r.Header.Get(mcpNameHeader)
					polls[p.TaskID]++
					n := polls[p.TaskID]
					nameVal := updatedInputs[p.TaskID]
					mu.Unlock()

					switch p.TaskID {
					case "task-elicit":
						switch n {
						case 1:
							writeResp(map[string]any{
								"resultType":     "complete",
								"taskId":         p.TaskID,
								"status":         "working",
								"statusMessage":  "preparing",
								"createdAt":      now,
								"lastUpdatedAt":  now,
								"ttlMs":          60000,
								"pollIntervalMs": 5,
							})
						case 2, 3:
							// Return the same inputRequests key on poll 2 AND poll 3 to
							// verify client-side deduplication across eventual consistency.
							writeResp(map[string]any{
								"resultType":     "complete",
								"taskId":         p.TaskID,
								"status":         "input_required",
								"createdAt":      now,
								"lastUpdatedAt":  now,
								"ttlMs":          60000,
								"pollIntervalMs": 5,
								"inputRequests": map[string]any{
									"ask_name": map[string]any{
										"method": "elicitation/create",
										"params": map[string]any{
											"mode":    "form",
											"message": "Enter your name",
											"requestedSchema": map[string]any{
												"type":       "object",
												"properties": map[string]any{"name": map[string]any{"type": "string"}},
											},
										},
									},
								},
							})
						default:
							writeResp(map[string]any{
								"resultType":    "complete",
								"taskId":        p.TaskID,
								"status":        "completed",
								"createdAt":     now,
								"lastUpdatedAt": now,
								"ttlMs":         60000,
								"result": map[string]any{
									"content": []any{map[string]any{"type": "text", "text": "Hello, " + nameVal + "!"}},
								},
							})
						}
					case "task-tool-err":
						writeResp(map[string]any{
							"resultType":    "complete",
							"taskId":        p.TaskID,
							"status":        "completed",
							"createdAt":     now,
							"lastUpdatedAt": now,
							"ttlMs":         60000,
							"result": map[string]any{
								"content": []any{map[string]any{"type": "text", "text": "tool-level error"}},
								"isError": true,
							},
						})
					case "task-fail":
						writeResp(map[string]any{
							"resultType":    "complete",
							"taskId":        p.TaskID,
							"status":        "failed",
							"statusMessage": "rate limit exceeded",
							"createdAt":     now,
							"lastUpdatedAt": now,
							"ttlMs":         60000,
							"error": map[string]any{
								"code":    -32603,
								"message": "API rate limit exceeded",
							},
						})
					case "task-cancel":
						cancelTaskPolledOnce.Do(func() { close(cancelTaskPolled) })
						writeResp(map[string]any{
							"resultType":     "complete",
							"taskId":         p.TaskID,
							"status":         "working",
							"createdAt":      now,
							"lastUpdatedAt":  now,
							"ttlMs":          60000,
							"pollIntervalMs": 5000,
						})
					}
				case "tasks/update":
					var p struct {
						TaskID         string                    `json:"taskId"`
						InputResponses map[string]map[string]any `json:"inputResponses"`
					}
					_ = json.Unmarshal(req.Params, &p)
					mu.Lock()
					seenHeaders["tasks/update:"+p.TaskID] = r.Header.Get(mcpNameHeader)
					if resp, ok := p.InputResponses["ask_name"]; ok {
						if content, ok := resp["content"].(map[string]any); ok {
							if v, ok := content["name"].(string); ok {
								updatedInputs[p.TaskID] = v
							}
						}
					}
					mu.Unlock()
					writeResp(map[string]any{"resultType": "complete"})
				case "tasks/cancel":
					var p struct {
						TaskID string `json:"taskId"`
					}
					_ = json.Unmarshal(req.Params, &p)
					mu.Lock()
					seenHeaders["tasks/cancel:"+p.TaskID] = r.Header.Get(mcpNameHeader)
					cancelledTasks[p.TaskID] = true
					mu.Unlock()
					writeResp(map[string]any{"resultType": "complete"})
				default:
					writeResp(map[string]any{"resultType": "complete"})
				}
			}))
			defer srv.Close()

			ts := NewRemoteToolsetWithAllowPrivateIPs("demo", srv.URL, "streamable", nil, nil, true, lifecycle.Policy{Restart: lifecycle.RestartNever})
			ts.SetExperimentalTasks(true)
			ts.SetElicitationHandler(func(_ context.Context, req *gomcp.ElicitParams) (tools.ElicitationResult, error) {
				elicitCalls.Add(1)
				assert.Equal(t, "Enter your name", req.Message)
				return tools.ElicitationResult{
					Action:  tools.ElicitationActionAccept,
					Content: map[string]any{"name": "Ada"},
				}, nil
			})
			require.NoError(t, ts.Start(ctx))
			defer func() { _ = ts.Stop(t.Context()) }()

			toolList, err := ts.Tools(ctx)
			require.NoError(t, err)
			byName := map[string]tools.Tool{}
			for _, tl := range toolList {
				byName[tl.Name] = tl
			}

			// 1. Synchronous tool call still works when tasks are enabled.
			syncRes, err := byName["demo_sync_tool"].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Name: "demo_sync_tool", Arguments: "{}"}}, nil)
			require.NoError(t, err)
			assert.Equal(t, "sync-ok", syncRes.Output)
			assert.True(t, tasksCapAdvertised.Load(), "client must advertise io.modelcontextprotocol/tasks")

			// 2. Async task with working -> input_required (deduplicated) -> tasks/update -> completed.
			elicitRes, err := byName["demo_async_elicit"].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Name: "demo_async_elicit", Arguments: "{}"}}, nil)
			require.NoError(t, err)
			assert.Equal(t, "Hello, Ada!", elicitRes.Output)
			assert.Equal(t, int32(1), elicitCalls.Load(), "duplicate inputRequests key must only prompt once")

			// 3. Completed task carrying a tool-level error (isError: true).
			toolErrRes, err := byName["demo_async_tool_error"].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Name: "demo_async_tool_error", Arguments: "{}"}}, nil)
			require.NoError(t, err)
			assert.Equal(t, "tool-level error", toolErrRes.Output)

			// 4. Failed task carrying a JSON-RPC error.
			_, err = byName["demo_async_fail"].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Name: "demo_async_fail", Arguments: "{}"}}, nil)
			require.ErrorContains(t, err, "API rate limit exceeded")

			// 5. Caller cancellation sends tasks/cancel and returns context.Canceled.
			callCtx, callCancel := context.WithCancel(ctx)
			go func() {
				<-cancelTaskPolled
				callCancel()
			}()
			_, err = byName["demo_async_cancel"].Handler(callCtx, tools.ToolCall{Function: tools.FunctionCall{Name: "demo_async_cancel", Arguments: "{}"}}, nil)
			require.ErrorIs(t, err, context.Canceled)

			mu.Lock()
			defer mu.Unlock()
			assert.True(t, cancelledTasks["task-cancel"], "client must send tasks/cancel on context cancellation")
			assert.Equal(t, "task-elicit", seenHeaders["tasks/get:task-elicit"], "Mcp-Name header must equal taskId on tasks/get")
			assert.Equal(t, "task-elicit", seenHeaders["tasks/update:task-elicit"], "Mcp-Name header must equal taskId on tasks/update")
			assert.Equal(t, "task-cancel", seenHeaders["tasks/cancel:task-cancel"], "Mcp-Name header must equal taskId on tasks/cancel")
		})
	}
}

func TestTasksStdioTransportAndEdgeCases(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	clientTransport, serverTransport := gomcp.NewInMemoryTransports()

	// Start a lightweight raw JSON-RPC server over the in-memory server connection.
	serverConn, err := serverTransport.Connect(ctx)
	require.NoError(t, err)
	defer func() { _ = serverConn.Close() }()

	go func() {
		for {
			msg, err := serverConn.Read(ctx)
			if err != nil {
				return
			}
			req, ok := msg.(*jsonrpc.Request)
			if !ok || !req.IsCall() {
				continue
			}
			reply := func(result any) {
				raw, _ := json.Marshal(result)
				_ = serverConn.Write(ctx, &jsonrpc.Response{ID: req.ID, Result: raw})
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			switch req.Method {
			case "server/discover":
				reply(map[string]any{
					"resultType":        "complete",
					"supportedVersions": []string{"2026-07-28"},
					"serverInfo":        map[string]any{"name": "test-stdio-tasks", "version": "1.0.0"},
					"capabilities": map[string]any{
						"tools":      map[string]any{},
						"extensions": map[string]any{tasksExtension: map[string]any{}},
					},
				})
			case "initialize":
				reply(map[string]any{
					"protocolVersion": "2026-07-28",
					"serverInfo":      map[string]any{"name": "test-stdio-tasks", "version": "1.0.0"},
					"capabilities": map[string]any{
						"tools":      map[string]any{},
						"extensions": map[string]any{tasksExtension: map[string]any{}},
					},
				})
			case "tools/call":
				var p struct {
					Name string `json:"name"`
				}
				_ = json.Unmarshal(req.Params, &p)
				switch p.Name {
				case "ok":
					reply(map[string]any{
						"resultType":     "task",
						"taskId":         "stdio-task-1",
						"status":         "working",
						"createdAt":      now,
						"lastUpdatedAt":  now,
						"ttlMs":          60000,
						"pollIntervalMs": 5,
					})
				case "expired":
					reply(map[string]any{
						"resultType":     "task",
						"taskId":         "stdio-task-expired",
						"status":         "working",
						"createdAt":      time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano),
						"lastUpdatedAt":  now,
						"ttlMs":          100,
						"pollIntervalMs": 5,
					})
				case "expired_poll":
					reply(map[string]any{
						"resultType":     "task",
						"taskId":         "stdio-task-hang-poll",
						"status":         "working",
						"createdAt":      now,
						"lastUpdatedAt":  now,
						"ttlMs":          80,
						"pollIntervalMs": 10,
					})
				case "cancelled":
					reply(map[string]any{
						"resultType":    "task",
						"taskId":        "stdio-task-cancelled",
						"status":        "cancelled",
						"statusMessage": "stopped by server",
						"createdAt":     now,
						"lastUpdatedAt": now,
						"ttlMs":         60000,
					})
				}
			case "tasks/get":
				var p struct {
					TaskID string `json:"taskId"`
				}
				_ = json.Unmarshal(req.Params, &p)
				switch p.TaskID {
				case "stdio-task-cancelled":
					reply(map[string]any{
						"resultType":    "complete",
						"taskId":        p.TaskID,
						"status":        "cancelled",
						"statusMessage": "stopped by server",
						"createdAt":     now,
						"lastUpdatedAt": now,
						"ttlMs":         60000,
						"error": map[string]any{
							"code":    -32000,
							"message": "upstream job aborted",
						},
					})
				case "stdio-task-hang-poll":
					// Intentionally do not reply so TTL must bound tasks/get.
				default:
					reply(map[string]any{
						"resultType":    "complete",
						"taskId":        p.TaskID,
						"status":        "completed",
						"createdAt":     now,
						"lastUpdatedAt": now,
						"ttlMs":         60000,
						"result": map[string]any{
							"content": []any{map[string]any{"type": "text", "text": "stdio-done"}},
						},
					})
				}
			case "tools/list":
				// Invalid: returning CreateTaskResult for an unsupported method.
				reply(map[string]any{
					"resultType":    "task",
					"taskId":        "illegal",
					"status":        "working",
					"createdAt":     now,
					"lastUpdatedAt": now,
					"ttlMs":         60000,
				})
			}
		}
	}()

	opts := &gomcp.ClientOptions{Capabilities: &gomcp.ClientCapabilities{}}
	opts.Capabilities.AddExtension(tasksExtension, nil)
	client := gomcp.NewClient(&gomcp.Implementation{Name: "test", Version: "1.0.0"}, opts)
	tasks, err := newTaskClient(client)
	require.NoError(t, err)

	session, err := client.Connect(ctx, &taskTransport{Transport: clientTransport, tasks: tasks}, nil)
	require.NoError(t, err)
	defer func() { _ = session.Close() }()

	sc := &sessionClient{
		session:           session,
		tasks:             tasks,
		experimentalTasks: true,
	}

	// 1. Happy path over stdio transport.
	res, err := sc.CallTool(ctx, &gomcp.CallToolParams{Name: "ok"})
	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	assert.Equal(t, "stdio-done", res.Content[0].(*gomcp.TextContent).Text)

	// 2. TTL expiration backstop (already expired and hanging tasks/get).
	_, err = sc.CallTool(ctx, &gomcp.CallToolParams{Name: "expired"})
	require.ErrorContains(t, err, "expired after TTL")
	_, err = sc.CallTool(ctx, &gomcp.CallToolParams{Name: "expired_poll"})
	require.ErrorContains(t, err, "expired after TTL")
	assert.True(t, isTaskExecutionError(err))

	// 3. Server-cancelled task fetches detailed error via tasks/get.
	_, err = sc.CallTool(ctx, &gomcp.CallToolParams{Name: "cancelled"})
	require.ErrorContains(t, err, "upstream job aborted")

	// 4. CreateTaskResult on unsupported method (tools/list) must be rejected.
	_, err = session.ListTools(ctx, &gomcp.ListToolsParams{})
	require.ErrorContains(t, err, "unexpected CreateTaskResult")
}

func TestTasksDisabledByDefaultDoesNotAdvertise(t *testing.T) {
	var advertised atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Meta map[string]json.RawMessage `json:"_meta"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var caps gomcp.ClientCapabilities
		_ = json.Unmarshal(req.Params.Meta["io.modelcontextprotocol/clientCapabilities"], &caps)
		if _, ok := caps.Extensions[tasksExtension]; ok {
			advertised.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"resultType":        "complete",
				"supportedVersions": []string{"2026-07-28"},
				"capabilities": map[string]any{
					"extensions": map[string]any{tasksExtension: map[string]any{}},
				},
			},
		})
	}))
	defer srv.Close()

	client := newRemoteClient(srv.URL, "streamable", nil, nil, nil, true, nil)
	_, err := client.Initialize(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = client.Close(t.Context()) }()
	assert.False(t, advertised.Load(), "io.modelcontextprotocol/tasks must not be advertised when disabled")
}
