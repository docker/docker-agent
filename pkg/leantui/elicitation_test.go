package leantui

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

type elicitationReply struct {
	id      string
	action  tools.ElicitationAction
	content map[string]any
}

type elicitationRuntime struct {
	cycleThinkingRuntime

	mu      sync.Mutex
	replies []elicitationReply
}

func (r *elicitationRuntime) ResumeElicitation(_ context.Context, action tools.ElicitationAction, content map[string]any, id ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replies = append(r.replies, elicitationReply{id: id[0], action: action, content: content})
	return nil
}

func (r *elicitationRuntime) got() []elicitationReply {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.replies)
}

func elicitationModel(t *testing.T) (*model, *elicitationRuntime) {
	t.Helper()
	rt := &elicitationRuntime{}
	m := bareModel(40)
	m.app = app.New(t.Context(), rt, session.New(session.WithID("root")))
	return m, rt
}

func formRequest(id, message string, schema any) *runtime.ElicitationRequestEvent {
	return runtime.ElicitationRequest(message, "form", schema, "", id, "", "root", nil, "root").(*runtime.ElicitationRequestEvent)
}

func frameText(m *model) string {
	lines, _, _ := m.buildLines()
	return ansi.Strip(strings.Join(lines, "\n"))
}

func TestElicitationFormRoundTrip(t *testing.T) {
	t.Parallel()
	m, rt := elicitationModel(t)
	schema := map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string", "title": "Name"}}}

	m.handleEvent(t.Context(), formRequest("e1", "Who are you?", schema))
	require.NotNil(t, m.screen.Elicitation)
	assert.Contains(t, frameText(m), "Who are you?")

	for _, r := range "Ada" {
		m.handleKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune{r}})
	}
	assert.True(t, m.screen.Editor.IsEmpty(), "typing must go to the form, not the editor")
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEnter})

	assert.Nil(t, m.screen.Elicitation)
	assert.Equal(t, []elicitationReply{{id: "e1", action: tools.ElicitationActionAccept, content: map[string]any{"name": "Ada"}}}, rt.got())
}

func TestElicitationsQueueInOrder(t *testing.T) {
	t.Parallel()
	m, rt := elicitationModel(t)

	m.handleEvent(t.Context(), formRequest("e1", "first", nil))
	m.handleEvent(t.Context(), formRequest("e2", "second", nil))
	assert.Contains(t, frameText(m), "first")

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	assert.Contains(t, frameText(m), "second")
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})

	assert.Nil(t, m.screen.Elicitation)
	assert.Equal(t, []elicitationReply{
		{id: "e1", action: tools.ElicitationActionCancel},
		{id: "e2", action: tools.ElicitationActionCancel},
	}, rt.got())
}

func TestElicitationClosedRetiresPrompt(t *testing.T) {
	t.Parallel()
	m, rt := elicitationModel(t)

	m.handleEvent(t.Context(), formRequest("e1", "first", nil))
	m.handleEvent(t.Context(), formRequest("e2", "second", nil))
	m.handleEvent(t.Context(), &runtime.ElicitationClosedEvent{ElicitationID: "e2"})
	assert.Contains(t, frameText(m), "first")

	m.handleEvent(t.Context(), &runtime.ElicitationClosedEvent{ElicitationID: "e1"})
	assert.Nil(t, m.screen.Elicitation)
	assert.Empty(t, m.elicitations)
	assert.Empty(t, rt.got())
}

func TestOAuthElicitationAuthorizes(t *testing.T) {
	t.Parallel()
	m, rt := elicitationModel(t)
	meta := map[string]any{"docker-agent/type": "oauth_flow", "docker-agent/server_url": "https://mcp.example.com"}
	event := runtime.ElicitationRequest("needs auth", "", nil, "", "o1", "", "root", meta, "root")

	m.handleEvent(t.Context(), event)
	assert.Contains(t, frameText(m), "OAuth authorization required")

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune{'y'}})
	assert.Equal(t, []elicitationReply{{id: "o1", action: tools.ElicitationActionAccept}}, rt.got())
	assert.Contains(t, frameText(m), "Opening browser to authorize https://mcp.example.com")

	m.handleEvent(t.Context(), runtime.Authorization(tools.ElicitationActionAccept, "root"))
	assert.Contains(t, frameText(m), "Authorization complete.")
}

func TestURLElicitationOpensBrowser(t *testing.T) {
	m, rt := elicitationModel(t)
	var opened string
	orig := openBrowser
	openBrowser = func(_ context.Context, url string) error { opened = url; return nil }
	t.Cleanup(func() { openBrowser = orig })

	m.handleEvent(t.Context(), runtime.ElicitationRequest("Sign in", "url", nil, "https://example.com/login", "u1", "", "root", nil, "root"))
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune{'o'}})
	assert.Equal(t, "https://example.com/login", opened)
	require.NotNil(t, m.screen.Elicitation)

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune{'y'}})
	assert.Equal(t, []elicitationReply{{id: "u1", action: tools.ElicitationActionAccept}}, rt.got())
}

func TestCtrlCCancelsElicitationAndInterruptsRun(t *testing.T) {
	t.Parallel()
	m, rt := elicitationModel(t)
	runCtx, cancel := context.WithCancel(t.Context())
	m.busy = true
	m.runCancel = cancel

	m.handleEvent(t.Context(), formRequest("e1", "?", nil))
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyCtrlC})

	assert.Equal(t, []elicitationReply{{id: "e1", action: tools.ElicitationActionCancel}}, rt.got())
	require.ErrorIs(t, runCtx.Err(), context.Canceled)
	assert.False(t, m.quitting)
}

func TestNewSessionDropsPendingPrompts(t *testing.T) {
	t.Parallel()
	m, rt := elicitationModel(t)

	m.handleEvent(t.Context(), formRequest("e1", "first", nil))
	m.handleEvent(t.Context(), formRequest("e2", "second", nil))
	m.handleSlash(t.Context(), "/new", busySubmitSteer)

	assert.Nil(t, m.screen.Elicitation)
	assert.Empty(t, m.elicitations)
	assert.Empty(t, rt.got(), "the app cancels a retired conversation's requests")
}
