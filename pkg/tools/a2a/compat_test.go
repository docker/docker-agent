package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	goa2asrv "github.com/a2aproject/a2a-go/a2asrv"
	a2av1 "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	a2asrvv1 "github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestToolSetDiscoversCardUnderBaseURLPath(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	card := testV1Card(a2av1.NewAgentInterface(server.URL+"/subagent/invoke", a2av1.TransportProtocolJSONRPC))
	mux.Handle("/subagent"+a2asrvv1.WellKnownAgentCardPath, a2asrvv1.NewStaticAgentCardHandler(card))
	mux.Handle("/subagent/invoke", a2asrvv1.NewJSONRPCHandler(a2asrvv1.NewHandler(testV1Executor{})))

	assertToolSetCalls(t, NewToolset("test", server.URL+"/subagent", nil, WithAllowPrivateIPs(true)))
}

func TestToolSetRejectsNullInterfaces(t *testing.T) {
	t.Parallel()

	for name, interfaces := range map[string]string{
		"first": `[null]`,
		"later": `[{"url":"http://127.0.0.1:1/invoke","protocolBinding":"JSONRPC","protocolVersion":"1.0"},null]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			url := serveAgent(t, func(string) string {
				return fmt.Sprintf(`{"name":"bad-card","supportedInterfaces":%s}`, interfaces)
			})

			err := NewToolset("test", url, nil, WithAllowPrivateIPs(true)).Start(t.Context())
			if err == nil {
				t.Fatal("Start() expected error")
			}
		})
	}
}

func TestToolSetSendsHeadersToSelectedEndpoint(t *testing.T) {
	t.Parallel()

	for name, first := range map[string]func(serverURL string) *a2av1.AgentInterface{
		"older version first": func(serverURL string) *a2av1.AgentInterface {
			return &a2av1.AgentInterface{URL: serverURL + "/invoke", ProtocolBinding: a2av1.TransportProtocolJSONRPC, ProtocolVersion: a2av0.Version}
		},
		"unsupported binding first": func(serverURL string) *a2av1.AgentInterface {
			return a2av1.NewAgentInterface(serverURL+"/invoke", a2av1.TransportProtocolHTTPJSON)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			selected := httptest.NewServer(requireCanaryAuth(a2asrvv1.NewJSONRPCHandler(a2asrvv1.NewHandler(testV1Executor{}))))
			t.Cleanup(selected.Close)

			mux := http.NewServeMux()
			other := httptest.NewServer(mux)
			t.Cleanup(other.Close)

			card := testV1Card(first(other.URL), a2av1.NewAgentInterface(selected.URL, a2av1.TransportProtocolJSONRPC))
			mux.Handle(a2asrvv1.WellKnownAgentCardPath, a2asrvv1.NewStaticAgentCardHandler(card))

			headers := map[string]string{"Authorization": "Bearer canary"}
			assertToolSetCalls(t, NewToolset("test", other.URL, headers, WithExpander(identityExpander{}), WithAllowPrivateIPs(true)))
		})
	}
}

type identityExpander struct{}

func (identityExpander) ExpandMap(_ context.Context, values map[string]string) map[string]string {
	return values
}

func requireCanaryAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer canary" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestToolSetKeepsLegacyBindingsSharingTheURL(t *testing.T) {
	t.Parallel()

	url := serveAgent(t, func(serverURL string) string {
		return legacyCardJSON(serverURL+"/invoke", map[string]any{
			"preferredTransport":   "HTTP+JSON",
			"additionalInterfaces": []map[string]string{{"url": serverURL + "/invoke", "transport": "JSONRPC"}},
		})
	})

	assertToolSetCalls(t, NewToolset("test", url, nil, WithAllowPrivateIPs(true)))
}

// A 0.3 additional interface missing its url or transport is skipped, as it
// was before the 1.0 migration; only the interfaces the client dials are validated.
func TestToolSetSkipsIncompleteLegacyInterfaces(t *testing.T) {
	t.Parallel()

	url := serveAgent(t, func(serverURL string) string {
		return legacyCardJSON(serverURL+"/invoke", map[string]any{
			"additionalInterfaces": []map[string]string{
				{"url": "", "transport": "JSONRPC"},
				{"url": serverURL + "/other", "transport": ""},
			},
		})
	})

	assertToolSetCalls(t, NewToolset("test", url, nil, WithAllowPrivateIPs(true)))
}

func TestToolSetAcceptsLegacyOAuthFlows(t *testing.T) {
	t.Parallel()

	for name, flows := range map[string]map[string]any{
		"several": {
			"authorizationCode": map[string]any{"authorizationUrl": "https://auth.example.com/authorize", "tokenUrl": "https://auth.example.com/token", "scopes": map[string]string{}},
			"clientCredentials": map[string]any{"tokenUrl": "https://auth.example.com/token", "scopes": map[string]string{}},
		},
		"none": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			url := serveAgent(t, func(serverURL string) string {
				return legacyCardJSON(serverURL+"/invoke", map[string]any{
					"securitySchemes": map[string]any{"oauth": map[string]any{"type": "oauth2", "flows": flows}},
					"security":        []map[string][]string{{"oauth": {}}},
				})
			})

			assertToolSetCalls(t, NewToolset("test", url, nil, WithAllowPrivateIPs(true)))
		})
	}
}

// legacyCardJSON returns an A2A 0.3 card for rpcURL with fields overridden.
func legacyCardJSON(rpcURL string, fields map[string]any) string {
	card := map[string]any{
		"name":               "test",
		"description":        "test",
		"version":            "1.0.0",
		"protocolVersion":    "0.3.0",
		"url":                rpcURL,
		"preferredTransport": "JSONRPC",
		"capabilities":       map[string]any{"streaming": true},
		"defaultInputModes":  []string{"text/plain"},
		"defaultOutputModes": []string{"text/plain"},
		"skills":             []map[string]any{{"id": "test", "name": "test", "description": "test", "tags": []string{"test"}}},
	}
	maps.Copy(card, fields)
	b, _ := json.Marshal(card)
	return string(b)
}

// serveAgent serves cardJSON(serverURL) at the well-known path and an A2A 0.3 JSON-RPC agent at /invoke.
func serveAgent(t *testing.T, cardJSON func(serverURL string) string) string {
	t.Helper()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mux.Handle("/invoke", goa2asrv.NewJSONRPCHandler(goa2asrv.NewHandler(testA2AHandler{})))
	mux.HandleFunc(a2asrvv1.WellKnownAgentCardPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, cardJSON(server.URL))
	})
	return server.URL
}

func testV1Card(interfaces ...*a2av1.AgentInterface) *a2av1.AgentCard {
	return &a2av1.AgentCard{
		Name:                "test",
		Description:         "test",
		Version:             "1.0.0",
		SupportedInterfaces: interfaces,
		Capabilities:        a2av1.AgentCapabilities{Streaming: true},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Skills:              []a2av1.AgentSkill{{ID: "test", Name: "test", Description: "test", Tags: []string{"test"}}},
	}
}

func assertToolSetCalls(t *testing.T, toolSet *Toolset) {
	t.Helper()

	if err := toolSet.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	toolList, err := toolSet.Tools(t.Context())
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	if len(toolList) != 1 {
		t.Fatalf("Tools() returned %d tools, want 1", len(toolList))
	}

	result, err := toolList[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"message":"hello"}`}}, tools.NopRuntime{})
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if result == nil || result.Output != "ok" {
		t.Fatalf("Handler() result = %+v, want output %q", result, "ok")
	}
}
