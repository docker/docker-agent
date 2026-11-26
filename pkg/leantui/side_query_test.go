package leantui

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/session"
)

func transcriptText(m *model) string {
	return strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
}

func TestBtwWithoutQuestionShowsUsage(t *testing.T) {
	m := bareModel(24)

	assert.True(t, m.handleSlash(t.Context(), "/btw", busySubmitSteer))
	assert.Contains(t, transcriptText(m), "Usage: /btw <question>")
}

func TestBtwUnavailableRuntimeReportsError(t *testing.T) {
	m := bareModel(24)
	m.app = app.New(t.Context(), &cycleThinkingRuntime{}, session.New())
	m.post = func(any) { t.Fatal("no background work expected") }

	assert.True(t, m.handleSlash(t.Context(), "/btw what is this?", busySubmitSteer))
	assert.Contains(t, transcriptText(m), "/btw is not available for this runtime")
}

func TestSideQueryDoneRendersAnswer(t *testing.T) {
	m := bareModel(24)

	m.handleEvent(t.Context(), sideQueryDone{generation: m.eventGeneration.Load(), answer: "the **answer**"})

	text := transcriptText(m)
	assert.Contains(t, text, "btw")
	assert.Contains(t, text, "answer")
}

func TestSideQueryDoneReportsFailure(t *testing.T) {
	m := bareModel(24)

	m.handleEvent(t.Context(), sideQueryDone{generation: m.eventGeneration.Load(), err: errors.New("boom")})

	assert.Contains(t, transcriptText(m), "Side question failed: boom")
}

func TestSideQueryDoneDroppedAfterSessionChange(t *testing.T) {
	m := bareModel(24)
	generation := m.eventGeneration.Load()
	m.eventGeneration.Add(1)

	m.handleEvent(t.Context(), sideQueryDone{generation: generation, answer: "stale"})

	assert.Zero(t, m.screen.Transcript.BlockCount())
}
