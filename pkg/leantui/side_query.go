package leantui

import (
	"context"
	"strings"

	"github.com/docker/docker-agent/pkg/leantui/ui"
)

// sideQueryDone carries a finished /btw answer back to the main loop.
type sideQueryDone struct {
	generation uint64
	answer     string
	err        error
}

// handleSideQuestion runs /btw: an isolated, tool-free request that never
// touches the running conversation. The answer lands in the transcript when
// it completes.
func (m *model) handleSideQuestion(ctx context.Context, question string) {
	if question == "" {
		m.addNotice("", "Usage: /btw <question>", ui.StMuted())
		return
	}
	if m.app == nil || m.post == nil {
		m.addNotice("", "/btw is not available here", ui.StMuted())
		return
	}
	query, err := m.app.PrepareSideQuery(ctx)
	if err != nil {
		m.addNotice("✗ ", err.Error(), ui.StError())
		return
	}

	m.addUserEcho("/btw " + question)
	m.addNotice("", "Answering side question…", ui.StMuted())

	generation := m.eventGeneration.Load()
	post := m.post
	go func() {
		var answer strings.Builder
		err := query.Run(ctx, question, func(text string) { answer.WriteString(text) })
		post(sideQueryDone{generation: generation, answer: answer.String(), err: err})
	}()
}

func (m *model) handleSideQueryDone(e sideQueryDone) {
	// The session was replaced while the question was in flight.
	if e.generation != m.eventGeneration.Load() {
		return
	}
	if e.err != nil {
		m.addNotice("✗ ", "Side question failed: "+e.err.Error(), ui.StError())
		return
	}
	answer := e.answer
	m.screen.Transcript.AddBlock(func(w int) []string {
		lines := ui.RenderAssistantLines(answer, w)
		return append([]string{ui.StMuted().Render("btw")}, lines...)
	})
}
