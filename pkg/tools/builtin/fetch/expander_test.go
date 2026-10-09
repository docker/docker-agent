package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/tools"
)

type recordingHeaderExpander struct{ calls int }

func (e *recordingHeaderExpander) ExpandMap(ctx context.Context, values map[string]string) map[string]string {
	e.calls++
	return map[string]string{"Authorization": "resolved"}
}

func TestNilExpander(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		expander headerExpander
	}{
		{"nil", nil}, {"typed nil", (*js.Expander)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expander := New(WithExpander(tc.expander)).handler.expander
			assert.Nil(t, expander.ExpandMap(t.Context(), nil))
			assert.Panics(t, func() { expander.ExpandMap(t.Context(), map[string]string{}) })
		})
	}
}

func TestHeaderExpanderAtCallSite(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "resolved", r.Header.Get("Authorization"))
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("content"))
	}))
	t.Cleanup(server.Close)
	expander := &recordingHeaderExpander{}
	tool := New(WithExpander(expander), WithHeaders(map[string]string{"Authorization": "template"}), WithAllowPrivateIPs(true))
	list, err := tool.Tools(t.Context())
	require.NoError(t, err)
	assert.Zero(t, expander.calls)
	for range 2 {
		result, err := list[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"urls":["` + server.URL + `"],"format":"text"}`}}, tools.NopRuntime{})
		require.NoError(t, err)
		assert.False(t, result.IsError)
	}
	assert.Equal(t, 2, expander.calls)
}
