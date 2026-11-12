package openurl

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/js"
)

type recordingURLExpander struct{ calls int }

func (e *recordingURLExpander) Expand(ctx context.Context, text string, values map[string]string) string {
	e.calls++
	return "https://example.com/resolved"
}

func TestURLExpanderAtCallSite(t *testing.T) {
	t.Parallel()
	expander := &recordingURLExpander{}
	var opened string
	tool := New("https://example.com/${env.VALUE}", WithExpander(expander), WithOpener(func(_ context.Context, url string) error { opened = url; return nil }))
	_, err := tool.Tools(t.Context())
	require.NoError(t, err)
	assert.Zero(t, expander.calls)
	for range 2 {
		_, err = tool.callTool(t.Context(), struct{}{})
		require.NoError(t, err)
	}
	assert.Equal(t, 2, expander.calls)
	assert.Equal(t, "https://example.com/resolved", opened)
}

func TestNilURLExpanderSkipsExpansion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		expander urlExpander
	}{
		{"nil", nil}, {"typed nil", (*js.Expander)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var opened string
			tool := New("https://example.com/${env.VALUE}", WithExpander(tc.expander), WithOpener(func(_ context.Context, url string) error { opened = url; return nil }))
			_, err := tool.callTool(t.Context(), struct{}{})
			require.NoError(t, err)
			assert.Nil(t, tool.expander)
			assert.Equal(t, "https://example.com/${env.VALUE}", opened)
		})
	}
}
