package openapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestOutputLimits(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		input string
		limit int
		want  string
	}{
		{"below limit", "abc", 4, "abc"},
		{"exact limit", "abcd", 4, "abcd"},
		{"disabled", strings.Repeat("a", maxOutputSize+1), 0, strings.Repeat("a", maxOutputSize+1)},
		{"custom limit", "abcde", 4, "abcd\n\n[Output truncated: exceeded 4 byte limit]"},
		{"UTF-8 boundary", "aéz", 2, "a\n\n[Output truncated: exceeded 2 byte limit]"},
		{"UTF-8 first rune", "éz", 1, "\n\n[Output truncated: exceeded 1 byte limit]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			output := limitOutput(test.input, test.limit)
			assert.Equal(t, test.want, output)
			assert.True(t, utf8.ValidString(output))
		})
	}
	assert.Equal(t, strings.Repeat("a", maxOutputSize)+"\n\n[Output truncated: exceeded 30,000 character limit]",
		limitOutput(strings.Repeat("a", maxOutputSize+1), maxOutputSize))
}

func TestConfiguredResponseLimits(t *testing.T) {
	t.Parallel()
	body := `{"padding":"` + strings.Repeat("x", maxOutputSize+100) + `","types":["electric"],"stats":[35,55,40,50,50,90]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.json" {
			_, _ = w.Write([]byte(petStoreSpec))
			return
		}
		if r.URL.Path == "/oversized" {
			_, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
			return
		}
		if r.URL.Query().Get("error") != "" {
			w.WriteHeader(http.StatusBadRequest)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	for _, test := range []struct {
		name  string
		limit *int
		want  string
	}{
		{"default", nil, limitOutput(body, maxOutputSize)},
		{"custom", new(100), limitOutput(body, 100)},
		{"disabled", new(0), body},
		{"larger", new(len(body)), body},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			toolset, err := CreateToolSet(t.Context(), latest.Toolset{
				Type: "openapi", URL: server.URL + "/openapi.json", AllowPrivateIPs: new(true),
				MaxOutputBytes: test.limit,
			}, &config.RuntimeConfig{})
			require.NoError(t, err)
			tools, err := toolset.Tools(t.Context())
			require.NoError(t, err)
			result := callTool(t, toolByName(t, tools, "listPets"), `{}`)
			assert.False(t, result.IsError)
			assert.Equal(t, test.want, result.Output)
			result = callTool(t, toolByName(t, tools, "listPets"), `{"error":"yes"}`)
			assert.True(t, result.IsError)
			assert.Equal(t, "HTTP 400: "+test.want, result.Output)
			if test.limit != nil && *test.limit == 0 {
				var parsed map[string]any
				require.NoError(t, json.Unmarshal([]byte(test.want), &parsed))
				assert.Contains(t, parsed, "types")
				assert.Contains(t, parsed, "stats")
			}
		})
	}
}

func TestDisabledOutputCutoffRetainsHTTPBodyCap(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
	}))
	t.Cleanup(server.Close)
	handler := &openAPIHandler{
		baseURL: server.URL, path: "/", method: http.MethodGet,
		maxOutputBytes: 0, allowPrivateIPs: true,
	}
	result, err := handler.callTool(t.Context(), nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(result.Output, "[WARNING: Response truncated at 1MB limit]\n"))
	assert.Len(t, result.Output, len("[WARNING: Response truncated at 1MB limit]\n")+(1<<20))
}

func TestHTTPBodyCapPreservesUTF8Boundary(t *testing.T) {
	t.Parallel()
	body := `{"padding":"` + strings.Repeat("x", (1<<20)-len(`{"padding":"`)-1) + "é" + `"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	for _, limit := range []int{0, 1 << 20} {
		handler := &openAPIHandler{
			baseURL: server.URL, path: "/", method: http.MethodGet,
			maxOutputBytes: limit, allowPrivateIPs: true,
		}
		result, err := handler.callTool(t.Context(), nil)
		require.NoError(t, err)
		assert.True(t, utf8.ValidString(result.Output))
		assert.Equal(t, "[WARNING: Response truncated at 1MB limit]\n"+body[:(1<<20)-1], result.Output)
	}
}
