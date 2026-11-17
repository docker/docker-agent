package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/team"
)

type compactionOutcomeReport struct {
	warnings          []string
	warningSessionIDs []string
	errors            []string
	outcome           string
}

func summarizeAndReport(t *testing.T, sink *collectSink) compactionOutcomeReport {
	t.Helper()
	var report compactionOutcomeReport
	for _, ev := range sink.events {
		switch e := ev.(type) {
		case *WarningEvent:
			report.warnings = append(report.warnings, e.Message)
			report.warningSessionIDs = append(report.warningSessionIDs, e.SessionID)
		case *ErrorEvent:
			report.errors = append(report.errors, e.Error)
		case *SessionCompactionEvent:
			if e.Status == "completed" {
				report.outcome = e.Outcome
			}
		}
	}
	return report
}

// An unknown summary-model window is a configuration gap: it must surface as a
// session warning, not a transcript error, and the outcome must stay failed.
func TestCompactionUnknownContextWindowWarnsWithoutTranscriptError(t *testing.T) {
	t.Parallel()

	primary := &providerOptsProvider{id: "unknown/primary-model"}
	root := agent.New("root", "test", agent.WithModel(primary))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := twoMessageSession()
	sink := &collectSink{}
	rt.Summarize(t.Context(), sess, "", sink)
	report := summarizeAndReport(t, sink)

	assert.Empty(t, report.errors)
	require.Len(t, report.warnings, 1)
	assert.Contains(t, report.warnings[0], `model "unknown/primary-model" has no known context window`)
	assert.Contains(t, report.warnings[0], "context_window")
	assert.Contains(t, report.warnings[0], "compaction_model")
	assert.Equal(t, sess.ID, report.warningSessionIDs[0], "the warning must be routed to the session being compacted")
	assert.Equal(t, CompactionOutcomeFailed, report.outcome)
	assert.Len(t, sess.Messages, 2, "failed compaction must leave the conversation unchanged")
}

// With a dedicated compaction model, the warning names that model rather than
// the primary, which may have a known window.
func TestCompactionUnknownContextWindowNamesDedicatedCompactionModel(t *testing.T) {
	t.Parallel()

	primary := &providerOptsProvider{id: "openai/gpt-5", contextWindow: windowPtr(200_000)}
	summarizer := &providerOptsProvider{id: "unknown/summarizer"}
	root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(summarizer))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := twoMessageSession()
	sink := &collectSink{}
	rt.Summarize(t.Context(), sess, "", sink)
	report := summarizeAndReport(t, sink)

	assert.Empty(t, report.errors)
	require.Len(t, report.warnings, 1)
	assert.Contains(t, report.warnings[0], `compaction model "unknown/summarizer" has no known context window`)
	assert.NotContains(t, report.warnings[0], "openai/gpt-5")
	assert.Contains(t, report.warnings[0], "context_window")
	assert.Equal(t, CompactionOutcomeFailed, report.outcome)
	assert.Len(t, sess.Messages, 2)
}
