package openapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/js"
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
			expander := New("https://example.com/spec", nil, WithExpander(tc.expander)).expander
			assert.Nil(t, expander.ExpandMap(t.Context(), nil))
			assert.Panics(t, func() { expander.ExpandMap(t.Context(), map[string]string{}) })
		})
	}
}

func TestHeaderExpanderAtCallSite(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/spec" {
			assert.Equal(t, "template", r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(petStoreSpec))
			return
		}
		assert.Equal(t, "resolved", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("content"))
	}))
	t.Cleanup(server.Close)
	expander := &recordingHeaderExpander{}
	tool := New(server.URL+"/spec", map[string]string{"Authorization": "template"}, WithExpander(expander), WithAllowPrivateIPs(true))
	list, err := tool.Tools(t.Context())
	require.NoError(t, err)
	assert.Zero(t, expander.calls)
	for range 2 {
		result := callTool(t, toolByName(t, list, "listPets"), `{}`)
		assert.False(t, result.IsError)
	}
	assert.Equal(t, 2, expander.calls)
}
