package webhook

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/js"
)

type recordingTemplateExpander struct{ calls []string }

func (e *recordingTemplateExpander) Expand(ctx context.Context, text string, values map[string]string) string {
	e.calls = append(e.calls, "url")
	return "https://example.com/webhook"
}

func (e *recordingTemplateExpander) ExpandMap(ctx context.Context, values map[string]string) map[string]string {
	e.calls = append(e.calls, "headers")
	return map[string]string{"Authorization": "resolved"}
}

func TestTemplateExpanderAtEachAttempt(t *testing.T) {
	t.Parallel()
	expander := &recordingTemplateExpander{}
	tool := New(latest.WebhookToolConfig{URL: "template"}, expander, time.Second)
	client := &fakeDoer{}
	tool.client = client
	assert.Empty(t, expander.calls)
	for range 2 {
		verdict, _, _ := tool.attempt(t.Context(), SendArgs{Message: "hello"})
		assert.Equal(t, delivered, verdict)
	}
	assert.Equal(t, []string{"url", "headers", "url", "headers"}, expander.calls)
	require.Len(t, client.reqs, 2)
	assert.Equal(t, "resolved", client.reqs[0].Header.Get("Authorization"))
}

func TestNilTemplateExpander(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		expander templateExpander
	}{
		{"nil", nil}, {"typed nil", (*js.Expander)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tool := New(latest.WebhookToolConfig{URL: "https://example.com"}, tc.expander, time.Second)
			tool.client = &fakeDoer{}
			verdict, _, _ := tool.attempt(t.Context(), SendArgs{Message: "hello"})
			assert.Equal(t, delivered, verdict)
			assert.Panics(t, func() { tool.expander.ExpandMap(t.Context(), map[string]string{}) })
			assert.Panics(t, func() { tool.expander.Expand(t.Context(), "${env.VALUE}", nil) })
		})
	}
}
