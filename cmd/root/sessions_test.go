package root

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/replay"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/tools"
)

func replaySession(toolName string) *session.Session {
	return &session.Session{Messages: []session.Item{
		{Message: &session.Message{Message: chat.Message{
			Role: chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{
				{Function: tools.FunctionCall{Name: toolName, Arguments: "{}"}},
			},
		}}},
	}}
}

func TestRenderReplay_TextDivergence(t *testing.T) {
	t.Parallel()

	result := replay.CompareSessions(replaySession("read_file"), replaySession("shell"))
	var buf bytes.Buffer
	require.NoError(t, renderReplay(&buf, result, "aaa", "bbb", false))

	out := buf.String()
	assert.Contains(t, out, "First divergence at turn 0")
	assert.Contains(t, out, "aaa")
	assert.Contains(t, out, "bbb")
}

func TestRenderReplay_TextIdentical(t *testing.T) {
	t.Parallel()

	result := replay.CompareSessions(replaySession("read_file"), replaySession("read_file"))
	var buf bytes.Buffer
	require.NoError(t, renderReplay(&buf, result, "aaa", "bbb", false))
	assert.Contains(t, buf.String(), "Identical behaviour")
}

func TestRenderReplay_JSON(t *testing.T) {
	t.Parallel()

	result := replay.CompareSessions(replaySession("read_file"), replaySession("shell"))
	var buf bytes.Buffer
	require.NoError(t, renderReplay(&buf, result, "aaa", "bbb", true))

	var round replay.Result
	require.NoError(t, json.Unmarshal(buf.Bytes(), &round))
	require.NotNil(t, round.Divergence)
	assert.Equal(t, 0, round.Divergence.TurnIndex)
}

func TestSessionsDiffCmd_FlagsAreRegistered(t *testing.T) {
	t.Parallel()

	cmd := newSessionsDiffCmd()
	for _, name := range []string{"session-db", "json", "fail-on-divergence"} {
		assert.NotNilf(t, cmd.Flags().Lookup(name), "flag %q must exist", name)
	}
	// Two session IDs, no more, no fewer.
	require.Error(t, cmd.Args(cmd, []string{"only-one"}))
	require.NoError(t, cmd.Args(cmd, []string{"a", "b"}))
}

// diffFixture builds a real store with two sessions whose behaviour differs.
func diffFixture(t *testing.T) (string, *session.Session, *session.Session) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "s.db")
	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)

	mk := func(id, toolName string, created time.Time) *session.Session {
		s := &session.Session{ID: id, CreatedAt: created, Messages: []session.Item{
			{Message: &session.Message{Message: chat.Message{
				Role: chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{
					{Function: tools.FunctionCall{Name: toolName, Arguments: "{}"}},
				},
			}}},
		}}
		require.NoError(t, store.AddSession(t.Context(), s))
		return s
	}

	now := time.Now()
	a := mk("aaaaaaaa11111111", "read_file", now.Add(-2*time.Hour))
	b := mk("bbbbbbbb22222222", "shell", now.Add(-time.Hour))
	require.NoError(t, store.Close())

	return dbPath, a, b
}

func runSessionsDiff(t *testing.T, dbPath string, args ...string) (string, error) {
	t.Helper()

	cmd := newSessionsDiffCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(t.Context())
	require.NoError(t, cmd.Flags().Set("session-db", dbPath))
	for i := 0; i+1 < len(args); i += 2 {
		require.NoError(t, cmd.Flags().Set(args[i], args[i+1]))
	}
	// Run first: operands of a return statement are evaluated left to right, so
	// reading the buffer in the same statement would capture it before the run.
	err := cmd.RunE(cmd, []string{"aaaaaaaa11111111", "bbbbbbbb22222222"})
	return out.String(), err
}

// The non-zero exit is the whole contract of --fail-on-divergence for the CI
// use case it exists for.
func TestSessionsDiff_FailOnDivergence(t *testing.T) {
	t.Parallel()

	dbPath, _, _ := diffFixture(t)

	out, err := runSessionsDiff(t, dbPath)
	require.NoError(t, err, "without the flag a divergence is reported but does not fail")
	assert.Contains(t, out, "First divergence")

	out, err = runSessionsDiff(t, dbPath, "fail-on-divergence", "true")
	require.Error(t, err, "with the flag a divergence must exit non-zero")
	assert.Contains(t, err.Error(), "diverged")
	assert.Contains(t, out, "First divergence")
}

// References go through ResolveSessionID like every other session command, so
// "compare my last two runs" works.
func TestSessionsDiff_ResolvesRelativeAndPrefixRefs(t *testing.T) {
	t.Parallel()

	dbPath, a, b := diffFixture(t)

	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	// Relative.
	for _, ref := range []string{"-1", "-2"} {
		got, err := loadSessionRef(t.Context(), store, ref)
		require.NoErrorf(t, err, "relative ref %q must resolve", ref)
		require.NotNil(t, got)
	}

	// Prefix.
	got, err := loadSessionRef(t.Context(), store, a.ID[:8])
	require.NoError(t, err)
	assert.Equal(t, a.ID, got.ID)

	got, err = loadSessionRef(t.Context(), store, b.ID)
	require.NoError(t, err)
	assert.Equal(t, b.ID, got.ID)

	_, err = loadSessionRef(t.Context(), store, "nosuchsession")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no session matches")
}

func TestSessionsCmd_HasDiffSubcommand(t *testing.T) {
	t.Parallel()

	cmd := newSessionsCmd()
	assert.Equal(t, "sessions", cmd.Name())

	var names []string
	for _, sub := range cmd.Commands() {
		names = append(names, sub.Name())
	}
	assert.Contains(t, names, "diff")
}
