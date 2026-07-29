package gemini

import (
	"bytes"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestNewRequestShape_Minimal verifies that a request with no tools, no
// thinking override, and no structured output produces an all-zero-value
// shape (aside from the API surface), matching the actual gemini-2.5-flash-image
// image-generation request captured in production diagnostics.
func TestNewRequestShape_Minimal(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash-image"},
		},
		apiSurface: apiSurfaceGeminiAPI,
	}

	config := client.buildConfig()
	config.Tools = client.builtInTools()

	shape := newRequestShape(client, config, 0)

	assert.False(t, shape.ResponseModalitiesSet)
	assert.Empty(t, shape.ResponseModalities)
	assert.Equal(t, 0, shape.BuiltInToolCount)
	assert.Empty(t, shape.BuiltInToolKinds)
	assert.Equal(t, 0, shape.FunctionToolCount)
	assert.False(t, shape.HasToolConfig)
	assert.False(t, shape.StructuredOutputPresent)
	assert.False(t, shape.ThinkingConfigSet)
	assert.False(t, shape.NoThinkingRequested)
	assert.Equal(t, apiSurfaceGeminiAPI, shape.APISurface)
	// No authoritative output-capability source exists yet: diagnostics must
	// report "unknown", never guess from the model ID.
	assert.False(t, shape.OutputCapabilityKnown)
	assert.False(t, shape.OutputCapabilityEnabled)
}

// TestNewRequestShape_ToolsAndBuiltIns mirrors the tool-attachment logic in
// CreateChatCompletionStream to verify the shape captures built-in tool
// kinds/count, custom function count, ToolConfig mode, and the server-side
// tool invocation flag Gemini requires when mixing built-ins with functions.
func TestNewRequestShape_ToolsAndBuiltIns(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:     "google",
				Model:        "gemini-2.5-flash",
				ProviderOpts: map[string]any{"google_search": true, "google_maps": true},
			},
		},
		apiSurface: apiSurfaceVertexAI,
	}

	config := client.buildConfig()
	config.Tools = client.builtInTools()

	requestTools := []tools.Tool{
		{Name: "read_file", Description: "reads a file", Parameters: map[string]any{"type": "object"}},
		{Name: "write_file", Description: "writes a file", Parameters: map[string]any{"type": "object"}},
	}
	allTools, err := convertToolsToGemini(requestTools)
	require.NoError(t, err)
	config.Tools = append(config.Tools, allTools...)
	config.ToolConfig = &genai.ToolConfig{
		FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto},
	}
	if len(config.Tools) > len(allTools) {
		config.ToolConfig.IncludeServerSideToolInvocations = new(true)
	}

	shape := newRequestShape(client, config, len(requestTools))

	assert.Equal(t, 2, shape.BuiltInToolCount)
	assert.ElementsMatch(t, []string{"google_search", "google_maps"}, shape.BuiltInToolKinds)
	assert.Equal(t, 2, shape.FunctionToolCount)
	assert.True(t, shape.HasToolConfig)
	assert.Equal(t, string(genai.FunctionCallingConfigModeAuto), shape.FunctionCallingMode)
	assert.True(t, shape.ServerSideToolInvocation, "mixing built-in and function tools must set the server-side invocation flag")
	assert.Equal(t, apiSurfaceVertexAI, shape.APISurface)
}

// TestNewRequestShape_ResponseModalitiesNormalized verifies modality values
// are trimmed, upper-cased, and de-duplicated, and that "set" reflects
// presence independent of the normalized values.
func TestNewRequestShape_ResponseModalitiesNormalized(t *testing.T) {
	t.Parallel()

	client := &Client{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash-image"}}}
	config := client.buildConfig()
	config.ResponseModalities = []string{" text ", "IMAGE", "text", ""}

	shape := newRequestShape(client, config, 0)

	assert.True(t, shape.ResponseModalitiesSet)
	assert.Equal(t, []string{"TEXT", "IMAGE"}, shape.ResponseModalities)
}

// TestNewRequestShape_NoThinkingRequested verifies the title-generation path
// (options.WithNoThinking) is captured: a non-Gemini-3 model gets an
// explicit ThinkingConfig even though the model config itself set no
// thinking budget.
func TestNewRequestShape_NoThinkingRequested(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig:  latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash-image"},
			ModelOptions: options.Apply(options.WithNoThinking()),
		},
	}
	config := client.buildConfig()

	shape := newRequestShape(client, config, 0)

	assert.True(t, shape.ThinkingConfigSet)
	assert.True(t, shape.NoThinkingRequested)
}

// TestNewRequestShape_StructuredOutputPresent verifies structured-output
// presence is captured as a boolean only — never the schema contents.
func TestNewRequestShape_StructuredOutputPresent(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash"},
			ModelOptions: options.Apply(options.WithStructuredOutput(&latest.StructuredOutput{
				Schema: map[string]any{"type": "object", "properties": map[string]any{"secret_field_name": map[string]any{"type": "string"}}},
			})),
		},
	}
	config := client.buildConfig()

	shape := newRequestShape(client, config, 0)

	require.True(t, shape.StructuredOutputPresent)

	// The shape must never carry the schema itself.
	for _, attr := range shape.LogAttrs() {
		if s, ok := attr.(string); ok {
			assert.NotContains(t, s, "secret_field_name")
		}
	}
}

// TestRequestShape_LogAttrsNeverLeaksToolSchemas is the core safety
// regression for this diagnostic: it builds a request with a function tool
// carrying a marker description and parameter schema, then verifies that
// nothing serialized by LogAttrs (or logged through CreateChatCompletionStream)
// contains that marker. This is exactly the kind of leak the previous
// per-tool debug logging in CreateChatCompletionStream produced.
func TestRequestShape_LogAttrsNeverLeaksToolSchemas(t *testing.T) {
	t.Parallel()

	const marker = "SECRET_TOOL_DESCRIPTION_MARKER"

	client := &Client{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash"}}}
	config := client.buildConfig()

	requestTools := []tools.Tool{
		{Name: "danger_tool", Description: marker, Parameters: map[string]any{"type": "object", "properties": map[string]any{marker: map[string]any{"type": "string"}}}},
	}
	allTools, err := convertToolsToGemini(requestTools)
	require.NoError(t, err)
	config.Tools = allTools

	shape := newRequestShape(client, config, len(requestTools))

	for _, attr := range flattenAttrs(shape.LogAttrs()) {
		assert.NotContains(t, attr, marker, "RequestShape must never carry tool descriptions or schemas")
	}
}

// flattenAttrs renders slog-style key/value pairs to strings for substring
// assertions in tests.
func flattenAttrs(attrs []any) []string {
	out := make([]string, 0, len(attrs))
	for _, a := range attrs {
		switch v := a.(type) {
		case string:
			out = append(out, v)
		case []string:
			out = append(out, v...)
		}
	}
	return out
}

// TestCreateChatCompletionStream_DebugLogNeverLeaksToolSchemas is an
// end-to-end regression at the CreateChatCompletionStream boundary: it
// swaps the default slog logger for a buffer-backed one, issues a real
// request (against a local httptest server) with a marker-carrying tool,
// and asserts the marker never reaches the debug log — the leak the old
// per-tool "Function" debug log would have produced. Not parallel: it
// swaps process-global slog state.
func TestCreateChatCompletionStream_DebugLogNeverLeaksToolSchemas(t *testing.T) {
	const marker = "SECRET_TOOL_DESCRIPTION_MARKER_E2E"

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeGeminiSSEResponse(w)
	}))
	defer server.Close()

	cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash", BaseURL: server.URL}
	env := environment.NewMapEnvProvider(map[string]string{"GOOGLE_API_KEY": "test-key"})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hello"},
	}, []tools.Tool{
		{Name: marker, Description: marker, Parameters: map[string]any{"type": "object", "properties": map[string]any{marker: map[string]any{"type": "string"}}}},
	})
	require.NoError(t, err)
	defer stream.Close()

	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	assert.NotContains(t, buf.String(), marker, "debug logs must never contain a tool name, description, or schema value")
}

// splitDiffNeedles are case-insensitive substrings that would appear in Go
// source if Split Diff View state ever leaked into a name, description, or
// argument sent to a model provider.
var splitDiffNeedles = []string{"splitdiff", "split-diff", "split_diff"}

// splitDiffTraceRoots are the source directories a Split Diff reference
// would have to pass through to ever reach a Gemini request: this package
// (where the request is finally serialized) and the two layers upstream of
// it that assemble what every provider call receives — pkg/tools (where the
// []tools.Tool list handed to CreateChatCompletionStream is built) and
// pkg/runtime (where messages and tools are gathered before any provider
// call). Dynamic MCP/external tool registrations are out of reach of a
// static scan; the local built-in tool definitions and the request-building
// path itself are not.
var splitDiffTraceRoots = []string{".", "../../../tools", "../../../runtime"}

// TestSplitDiffView_NeverReferencedInToolOrRequestBuildingSource is a static
// source-scan classification for the owner's screenshot, which showed
// "Split Diff View" active in the sidebar alongside an observed Gemini 400.
// It is a source scan, not a runtime trace: it proves that no non-test .go
// file under [splitDiffTraceRoots] mentions Split Diff View in any form,
// tracing the actual boundary a reference would have to cross — tool
// registration (pkg/tools) and request assembly (pkg/runtime) — rather than
// just this leaf package. It cannot see into MCP/other dynamically
// registered tools, which no static scan can enumerate.
//
// That, combined with reading pkg/tui/service/sessionstate.go (SplitDiffView
// is a plain bool getter/setter backed by session/userconfig persistence,
// wired only into TUI rendering — pkg/tui/components/sidebar/sidebar.go,
// pkg/tui/components/tool/editfile) and confirming pkg/tools and
// pkg/runtime contain no reference to it either, is the evidence for
// classifying Split Diff View as local-only TUI/session state with no path
// into any provider request, Gemini included.
//
// If a future change ever wires it into a tool/config sent to a provider,
// this test fails wherever that reference lands, and the classification
// must move from "local-only" to "serialized-tool".
func TestSplitDiffView_NeverReferencedInToolOrRequestBuildingSource(t *testing.T) {
	t.Parallel()

	for _, root := range splitDiffTraceRoots {
		for _, path := range goSourceFiles(t, root) {
			assertNoSplitDiffReference(t, path)
		}
	}
}

// goSourceFiles returns every non-test .go file under root, recursively.
func goSourceFiles(t *testing.T, root string) []string {
	t.Helper()

	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	require.NoError(t, err)
	return files
}

func assertNoSplitDiffReference(t *testing.T, path string) {
	t.Helper()

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	lower := strings.ToLower(string(content))
	for _, needle := range splitDiffNeedles {
		assert.NotContains(t, lower, needle,
			"%s must never reference split-diff: it is local-only TUI state, never a tool/request field", path)
	}
}
