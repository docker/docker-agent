package leantui

import (
	"context"
	"slices"

	"github.com/docker/docker-agent/pkg/browser"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

var openBrowser = browser.Open

// enqueueElicitation queues a server prompt; concurrent requests (e.g. from
// background jobs) are answered one at a time, oldest first.
func (m *model) enqueueElicitation(e *runtime.ElicitationRequestEvent) {
	m.elicitations = append(m.elicitations, e)
	if m.screen.Elicitation == nil {
		m.showNextElicitation()
	}
}

// retireElicitation drops a prompt the runtime resolved or cancelled itself.
func (m *model) retireElicitation(id string) {
	if len(m.elicitations) == 0 {
		return
	}
	head := m.elicitations[0]
	m.elicitations = slices.DeleteFunc(m.elicitations, func(e *runtime.ElicitationRequestEvent) bool {
		return e.ElicitationID == id
	})
	if head.ElicitationID == id {
		m.showNextElicitation()
	}
}

func (m *model) showNextElicitation() {
	m.screen.Elicitation = nil
	if len(m.elicitations) > 0 {
		m.screen.Elicitation = newElicitationPrompt(m.elicitations[0])
	}
}

func newElicitationPrompt(e *runtime.ElicitationRequestEvent) *ui.ElicitationModel {
	if e.Meta["docker-agent/type"] == "oauth_flow" {
		serverURL, _ := e.Meta["docker-agent/server_url"].(string)
		return ui.NewOAuthElicitation(serverURL)
	}
	if e.Mode == "url" {
		return ui.NewURLElicitation(e.Message, e.URL)
	}
	title, _ := e.Meta["cagent/title"].(string)
	return ui.NewFormElicitation(title, e.Message, e.Schema)
}

func (m *model) handleElicitationKey(ctx context.Context, k ui.Key) {
	if k.Typ == ui.KeyCtrlC {
		m.answerElicitation(ctx, tools.ElicitationActionCancel, nil)
		if m.busy {
			m.handleInterrupt()
		}
		return
	}
	prompt := m.screen.Elicitation
	result := prompt.HandleKey(k)
	if result.OpenURL {
		m.openURL(ctx, prompt.URL)
	}
	if !result.Done() {
		return
	}
	if prompt.Kind == ui.ElicitationOAuth && result.Action == tools.ElicitationActionAccept {
		m.addNotice("", "Opening browser to authorize "+prompt.URL+"…", ui.StMuted())
	}
	m.answerElicitation(ctx, result.Action, result.Content)
}

func (m *model) answerElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any) {
	if len(m.elicitations) == 0 {
		m.screen.Elicitation = nil
		return
	}
	e := m.elicitations[0]
	m.elicitations = m.elicitations[1:]
	m.statusSession.Resolve(e)
	m.showNextElicitation()
	if err := m.app.ResumeElicitation(ctx, action, content, e.ElicitationID); err != nil {
		m.addNotice("✗ ", "Failed to answer server request: "+err.Error(), ui.StError())
	}
}

func (m *model) openURL(ctx context.Context, url string) {
	if err := openBrowser(context.WithoutCancel(ctx), url); err != nil {
		m.addNotice("✗ ", "Failed to open browser: "+err.Error(), ui.StError())
	}
}

func (m *model) handleAuthorization(e *runtime.AuthorizationEvent) {
	if e.Confirmation == tools.ElicitationActionAccept {
		m.addNotice("✓ ", "Authorization complete.", ui.StSuccess())
	}
}
