package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

// Safety returns the author-declared default set via WithSafety, empty
// when unset.
func TestSafety(t *testing.T) {
	t.Parallel()
	assert.Equal(t, latest.SafetyMode(""), New("a", "").Safety())
	assert.Equal(t, latest.SafetyModeStrict, New("a", "", WithSafety(latest.SafetyModeStrict)).Safety())
}

type stubToolSet struct {
	startErr error
	tools    []tools.Tool
	listErr  error
}

// Verify interface compliance
var (
	_ tools.ToolSet   = (*stubToolSet)(nil)
	_ tools.Startable = (*stubToolSet)(nil)
)

func newStubToolSet(startErr error, toolsList []tools.Tool, listErr error) tools.ToolSet {
	return &stubToolSet{
		startErr: startErr,
		tools:    toolsList,
		listErr:  listErr,
	}
}

func (s *stubToolSet) Start(context.Context) error { return s.startErr }
func (s *stubToolSet) Stop(context.Context) error  { return nil }
func (s *stubToolSet) Tools(context.Context) ([]tools.Tool, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.tools, nil
}

// describedToolSet is a stubToolSet with a stable user-visible description,
// so tests can assert which toolset a warning attributes a tool to.
type describedToolSet struct {
	stubToolSet

	desc string
}

var _ tools.Describer = (*describedToolSet)(nil)

func (d *describedToolSet) Describe() string { return d.desc }

func newDescribedToolSet(desc string, toolsList []tools.Tool) *describedToolSet {
	return &describedToolSet{stubToolSet: stubToolSet{tools: toolsList}, desc: desc}
}

// flappyToolSet is a ToolSet+Startable that returns a scripted sequence of
// errors from Start(). nil in the sequence means success.
type flappyToolSet struct {
	errs    []error
	callIdx int
	stubs   []tools.Tool
}

var (
	_ tools.ToolSet   = (*flappyToolSet)(nil)
	_ tools.Startable = (*flappyToolSet)(nil)
)

func (f *flappyToolSet) Start(_ context.Context) error {
	if f.callIdx >= len(f.errs) {
		return nil
	}
	err := f.errs[f.callIdx]
	f.callIdx++
	return err
}

func (f *flappyToolSet) Stop(_ context.Context) error { return nil }

func (f *flappyToolSet) Tools(_ context.Context) ([]tools.Tool, error) {
	return f.stubs, nil
}

type reconnectingToolSet struct {
	started      bool
	startCalls   int
	restartCalls int
	listCalls    int
	stubs        []tools.Tool
}

var (
	_ tools.ToolSet   = (*reconnectingToolSet)(nil)
	_ tools.Startable = (*reconnectingToolSet)(nil)
)

func (r *reconnectingToolSet) Start(context.Context) error {
	r.startCalls++
	r.started = true
	return nil
}

func (r *reconnectingToolSet) Stop(context.Context) error {
	r.started = false
	return nil
}

func (r *reconnectingToolSet) Restart(context.Context) error {
	r.restartCalls++
	r.started = true
	return nil
}

func (r *reconnectingToolSet) IsStarted() bool { return r.started }

func (r *reconnectingToolSet) Tools(context.Context) ([]tools.Tool, error) {
	if !r.started {
		return nil, errors.New("toolset not started")
	}
	r.listCalls++
	if r.listCalls == 1 {
		r.started = false
	}
	return r.stubs, nil
}

func TestAgentTools(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		toolsets      []tools.ToolSet
		wantToolCount int
		wantWarnings  int
	}{
		{
			name:          "partial success",
			toolsets:      []tools.ToolSet{newStubToolSet(nil, []tools.Tool{{Name: "good", Parameters: map[string]any{}}}, nil), newStubToolSet(errors.New("boom"), nil, nil)},
			wantToolCount: 1,
			wantWarnings:  1,
		},
		{
			name:          "all fail on start",
			toolsets:      []tools.ToolSet{newStubToolSet(errors.New("fail1"), nil, nil), newStubToolSet(errors.New("fail2"), nil, nil)},
			wantToolCount: 0,
			wantWarnings:  2,
		},
		{
			name:          "list failure becomes warning",
			toolsets:      []tools.ToolSet{newStubToolSet(nil, nil, errors.New("list boom"))},
			wantToolCount: 0,
			wantWarnings:  1,
		},
		{
			name:          "no toolsets",
			toolsets:      nil,
			wantToolCount: 0,
			wantWarnings:  0,
		},
		{
			// #2251: two toolsets emitting the same names (e.g. the same
			// built-in type configured twice) must collapse to a unique set,
			// otherwise providers reject the request with "Tool names must
			// be unique." Each dropped duplicate surfaces one warning.
			name: "duplicate names across toolsets are deduped",
			toolsets: []tools.ToolSet{
				newStubToolSet(nil, []tools.Tool{{Name: "read_file", Parameters: map[string]any{}}, {Name: "write_file", Parameters: map[string]any{}}}, nil),
				newStubToolSet(nil, []tools.Tool{{Name: "read_file", Parameters: map[string]any{}}, {Name: "write_file", Parameters: map[string]any{}}}, nil),
			},
			wantToolCount: 2,
			wantWarnings:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New("root", "test", WithToolSets(tt.toolsets...))
			got, err := a.Tools(t.Context())

			require.NoError(t, err)
			require.Len(t, got, tt.wantToolCount)

			warnings := a.DrainWarnings()
			if tt.wantWarnings == 0 {
				require.Nil(t, warnings)
			} else {
				require.Len(t, warnings, tt.wantWarnings)
			}
		})
	}
}

// TestAgentNoDuplicateListWarnings verifies that a toolset that starts
// successfully but keeps failing to list its tools (e.g. a remote MCP server
// stuck returning "toolset not started") surfaces only one warning per
// failure streak, not one on every turn.
func TestAgentNoDuplicateListWarnings(t *testing.T) {
	t.Parallel()

	stub := newStubToolSet(nil, nil, errors.New("toolset not started"))
	a := New("root", "test", WithToolSets(stub))

	// Turn 1: first list failure → warning.
	_, err := a.Tools(t.Context())
	require.NoError(t, err)
	warnings := a.DrainWarnings()
	require.Len(t, warnings, 1, "turn 1: exactly one warning on first list failure")
	assert.Contains(t, warnings[0], "list failed")

	// Turn 2: repeated list failure → no new warning.
	_, err = a.Tools(t.Context())
	require.NoError(t, err)
	assert.Empty(t, a.DrainWarnings(), "turn 2: no duplicate warning on repeated list failure")

	// Turn 3: still failing → still no new warning.
	_, err = a.Tools(t.Context())
	require.NoError(t, err)
	assert.Empty(t, a.DrainWarnings(), "turn 3: no duplicate warning on repeated list failure")
}

// TestAgentToolsDedupeKeepsFirstInOrder pins the deduplication contract from
// #2251 (and documented in the MCP toolset docs): when several toolsets emit
// the same tool name, Tools() keeps the occurrence from the first toolset in
// configuration order, preserves the first-seen order of distinct names, and
// never returns a duplicate name.
func TestAgentToolsDedupeKeepsFirstInOrder(t *testing.T) {
	t.Parallel()

	first := newStubToolSet(nil, []tools.Tool{
		{Name: "read_file", Description: "from first", Parameters: map[string]any{}},
		{Name: "fetch", Description: "from first", Parameters: map[string]any{}},
	}, nil)
	second := newStubToolSet(nil, []tools.Tool{
		{Name: "read_file", Description: "from second", Parameters: map[string]any{}},
		{Name: "write_file", Description: "from second", Parameters: map[string]any{}},
	}, nil)

	a := New("root", "test", WithToolSets(first, second))
	got, err := a.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 3)

	names := make([]string, len(got))
	for i, tool := range got {
		names[i] = tool.Name
	}
	assert.Equal(t, []string{"read_file", "fetch", "write_file"}, names,
		"distinct names in first-seen order, no duplicates")
	assert.Equal(t, "from first", got[0].Description,
		"the first occurrence of read_file must win")
}

// TestAgentToolsCollisionWarningIdentifiesOrigins verifies the duplicate-tool
// warning names both the toolset that won and the toolset whose tool was
// ignored, and points the user at the config-level remedies.
func TestAgentToolsCollisionWarningIdentifiesOrigins(t *testing.T) {
	t.Parallel()

	builtin := newDescribedToolSet("fetch built-in", []tools.Tool{{Name: "fetch", Parameters: map[string]any{}}})
	jira := newDescribedToolSet("mcp(ref=jira)", []tools.Tool{{Name: "fetch", Parameters: map[string]any{}}})

	a := New("root", "test", WithToolSets(builtin, jira))
	_, err := a.Tools(t.Context())
	require.NoError(t, err)

	warnings := a.DrainWarnings()
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], `duplicate tool "fetch"`)
	assert.Contains(t, warnings[0], "kept from fetch built-in")
	assert.Contains(t, warnings[0], "ignored from mcp(ref=jira)")
	assert.Contains(t, warnings[0], "'name:'", "warning must point at the config remedy")
}

// TestAgentToolsCollisionWarningOncePerStreak verifies once-per-streak
// semantics for duplicate-tool warnings: a persistent collision warns only on
// the first turn, a collision that disappears resets the streak, and a new
// collision (e.g. introduced by an MCP ToolListChanged notification) is
// reported as fresh.
func TestAgentToolsCollisionWarningOncePerStreak(t *testing.T) {
	t.Parallel()

	static := newDescribedToolSet("static", []tools.Tool{{Name: "fetch", Parameters: map[string]any{}}})
	dynamic := newDescribedToolSet("dynamic", []tools.Tool{{Name: "fetch", Parameters: map[string]any{}}})
	a := New("root", "test", WithToolSets(static, dynamic))

	// Turn 1: collision on "fetch" → one warning.
	_, err := a.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, a.DrainWarnings(), 1, "turn 1: one warning on first collision")

	// Turn 2: same collision persists → no new warning.
	_, err = a.Tools(t.Context())
	require.NoError(t, err)
	assert.Empty(t, a.DrainWarnings(), "turn 2: persistent collision must not re-warn")

	// Turn 3: the dynamic toolset changes its list (ToolListChanged) and now
	// also collides on "search" → exactly one warning, for the new collision.
	dynamic.tools = []tools.Tool{
		{Name: "fetch", Parameters: map[string]any{}},
		{Name: "search", Parameters: map[string]any{}},
	}
	static.tools = []tools.Tool{
		{Name: "fetch", Parameters: map[string]any{}},
		{Name: "search", Parameters: map[string]any{}},
	}
	_, err = a.Tools(t.Context())
	require.NoError(t, err)
	warnings := a.DrainWarnings()
	require.Len(t, warnings, 1, "turn 3: only the new collision warns")
	assert.Contains(t, warnings[0], `duplicate tool "search"`)

	// Turn 4: all collisions resolved → streak resets, no warning.
	dynamic.tools = nil
	static.tools = []tools.Tool{{Name: "fetch", Parameters: map[string]any{}}, {Name: "search", Parameters: map[string]any{}}}
	_, err = a.Tools(t.Context())
	require.NoError(t, err)
	assert.Empty(t, a.DrainWarnings(), "turn 4: no collision, no warning")

	// Turn 5: the same collision reappears → reported as a fresh streak.
	dynamic.tools = []tools.Tool{{Name: "fetch", Parameters: map[string]any{}}}
	_, err = a.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, a.DrainWarnings(), 1, "turn 5: reappearing collision starts a new streak")
}

// mockProvider implements provider.Provider for testing
type mockProvider struct {
	id modelsdev.ID
}

func (m *mockProvider) ID() modelsdev.ID { return m.id }
func (m *mockProvider) CreateChatCompletionStream(_ context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	return nil, nil
}
func (m *mockProvider) BaseConfig() base.Config { return base.Config{} }

func TestModelOverride(t *testing.T) {
	t.Parallel()

	defaultModel := &mockProvider{id: modelsdev.NewID("openai", "gpt-4o")}
	overrideModel := &mockProvider{id: modelsdev.NewID("anthropic", "claude-sonnet-4-0")}

	a := New("root", "test", WithModel(defaultModel))

	// Initially should return the default model
	assert.Equal(t, "openai/gpt-4o", a.Model(t.Context()).ID().String())
	assert.False(t, a.HasModelOverride())

	// Set an override
	a.SetModelOverride(overrideModel)
	assert.True(t, a.HasModelOverride())
	assert.Equal(t, "anthropic/claude-sonnet-4-0", a.Model(t.Context()).ID().String())

	// ConfiguredModels still reflects the originally configured models
	configuredModels := a.ConfiguredModels()
	require.Len(t, configuredModels, 1)
	assert.Equal(t, "openai/gpt-4o", configuredModels[0].ID().String())

	// Clear the override
	a.SetModelOverride(nil)
	assert.False(t, a.HasModelOverride())
	assert.Equal(t, "openai/gpt-4o", a.Model(t.Context()).ID().String())
}

func TestSetModelOverride_ReturnsSnapshotOfStoredValue(t *testing.T) {
	// SetModelOverride must return a snapshot of the value it just stored,
	// not what a subsequent SnapshotModelOverride() would load. This is the
	// guarantee that closes the race window for scoped overrides: if a
	// concurrent caller stores a different override after our store but
	// before we capture our snapshot, our snapshot must still refer to
	// what we stored, so the deferred CAS-restore will fail (concurrent
	// change wins) instead of incorrectly succeeding.
	t.Parallel()

	defaultModel := &mockProvider{id: modelsdev.NewID("default", "x")}
	oursModel := &mockProvider{id: modelsdev.NewID("ours", "x")}
	othersModel := &mockProvider{id: modelsdev.NewID("others", "x")}

	a := New("root", "test", WithModel(defaultModel))

	// Capture the snapshot returned by SetModelOverride.
	prev := a.SnapshotModelOverride()
	oursSnap := a.SetModelOverride(oursModel)

	// Simulate a concurrent caller storing a different override _after_ we
	// stored ours but _before_ a hypothetical post-store SnapshotModelOverride.
	a.SetModelOverride(othersModel)
	require.Equal(t, "others/x", a.Model(t.Context()).ID().String())

	// The deferred restore must be a no-op because oursSnap holds the
	// pointer we stored, not the current pointer.
	a.RestoreModelOverride(prev, oursSnap)
	assert.Equal(t, "others/x", a.Model(t.Context()).ID().String(),
		"concurrent override must be preserved; the snapshot returned by SetModelOverride captures the stored pointer")
}

func TestSetModelOverride_ClearReturnsZeroSnapshot(t *testing.T) {
	t.Parallel()

	a := New("root", "test", WithModel(&mockProvider{id: modelsdev.NewID("default", "x")}))

	// Calling SetModelOverride with no providers (or nil) clears the override.
	// The returned snapshot should round-trip cleanly through RestoreModelOverride.
	cleared := a.SetModelOverride()
	assert.False(t, a.HasModelOverride())

	// Now set an override and restore using `cleared` as `prev`.
	oursSnap := a.SetModelOverride(&mockProvider{id: modelsdev.NewID("ours", "x")})
	require.True(t, a.HasModelOverride())

	a.RestoreModelOverride(cleared, oursSnap)
	assert.False(t, a.HasModelOverride(), "restoring a cleared snapshot must clear the override")
}

func TestSnapshotAndRestoreModelOverride(t *testing.T) {
	t.Parallel()

	defaultModel := &mockProvider{id: modelsdev.NewID("openai", "gpt-4o")}
	skillModel := &mockProvider{id: modelsdev.NewID("openai", "gpt-4o-mini")}
	userModel := &mockProvider{id: modelsdev.NewID("anthropic", "claude-sonnet-4-0")}

	t.Run("restores when no concurrent change", func(t *testing.T) {
		t.Parallel()
		a := New("root", "test", WithModel(defaultModel))

		prev := a.SnapshotModelOverride()
		a.SetModelOverride(skillModel)
		ours := a.SnapshotModelOverride()
		assert.Equal(t, "openai/gpt-4o-mini", a.Model(t.Context()).ID().String())

		a.RestoreModelOverride(prev, ours)
		assert.False(t, a.HasModelOverride())
		assert.Equal(t, "openai/gpt-4o", a.Model(t.Context()).ID().String())
	})

	t.Run("restores back to a pre-existing override", func(t *testing.T) {
		t.Parallel()
		a := New("root", "test", WithModel(defaultModel))
		a.SetModelOverride(userModel)

		prev := a.SnapshotModelOverride()
		a.SetModelOverride(skillModel)
		ours := a.SnapshotModelOverride()
		assert.Equal(t, "openai/gpt-4o-mini", a.Model(t.Context()).ID().String())

		a.RestoreModelOverride(prev, ours)
		assert.Equal(t, "anthropic/claude-sonnet-4-0", a.Model(t.Context()).ID().String())
	})

	t.Run("keeps a concurrent change instead of restoring", func(t *testing.T) {
		// This is the TUI-while-skill-runs scenario: another caller
		// changes the override between SnapshotModelOverride and
		// RestoreModelOverride. The deferred restore must NOT clobber
		// that change.
		t.Parallel()
		a := New("root", "test", WithModel(defaultModel))

		prev := a.SnapshotModelOverride()
		a.SetModelOverride(skillModel)
		ours := a.SnapshotModelOverride()

		// Simulate concurrent TUI model switch.
		a.SetModelOverride(userModel)

		a.RestoreModelOverride(prev, ours)
		require.True(t, a.HasModelOverride(), "user's model choice must be preserved")
		assert.Equal(t, "anthropic/claude-sonnet-4-0", a.Model(t.Context()).ID().String())
	})

	t.Run("keeps a concurrent clear instead of restoring", func(t *testing.T) {
		// Same as above but the concurrent caller clears the override
		// (e.g. user revert via TUI). The restore must respect that.
		t.Parallel()
		a := New("root", "test", WithModel(defaultModel))
		a.SetModelOverride(userModel)

		prev := a.SnapshotModelOverride()
		a.SetModelOverride(skillModel)
		ours := a.SnapshotModelOverride()

		// Simulate concurrent TUI revert.
		a.SetModelOverride()

		a.RestoreModelOverride(prev, ours)
		assert.False(t, a.HasModelOverride(), "user's revert must be preserved")
	})
}

func TestModel_LogsSelection(t *testing.T) {
	// Not parallel: it swaps the process-global default logger via slog.SetDefault.
	var buf concurrent.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	model1 := &mockProvider{id: modelsdev.NewID("anthropic", "claude-sonnet-4-0")}
	model2 := &mockProvider{id: modelsdev.NewID("openai", "gpt-4o")}

	a := New("scanner", "test", WithModel(model1), WithModel(model2))

	// Verify basic selection logging
	selected := a.Model(t.Context())
	logOutput := buf.String()

	assert.Contains(t, logOutput, "Model selected")
	assert.Contains(t, logOutput, "agent=scanner")
	assert.Contains(t, logOutput, selected.ID().String())
	assert.Contains(t, logOutput, "pool_size=2")

	// Verify override scenario logs correct pool_size
	buf.Reset()
	override := &mockProvider{id: modelsdev.NewID("google", "gemini-2.0-flash")}
	a.SetModelOverride(override)

	selected = a.Model(t.Context())
	logOutput = buf.String()

	assert.Equal(t, "google/gemini-2.0-flash", selected.ID().String())
	assert.Contains(t, logOutput, "google/gemini-2.0-flash")
	assert.Contains(t, logOutput, "pool_size=1")
}

func TestModelOverride_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	defaultModel := &mockProvider{id: modelsdev.NewID("default", "x")}
	overrideModel := &mockProvider{id: modelsdev.NewID("override", "x")}

	a := New("root", "test", WithModel(defaultModel))

	// Run concurrent reads and writes
	done := make(chan bool)

	// Writer goroutine
	go func() {
		for range 100 {
			a.SetModelOverride(overrideModel)
			a.SetModelOverride(nil)
		}
		done <- true
	}()

	// Reader goroutine
	go func() {
		for range 100 {
			_ = a.Model(t.Context())
			_ = a.HasModelOverride()
		}
		done <- true
	}()

	<-done
	<-done
	// If we got here without a race condition panic, the test passes
}

// TestAgentReProbeRecoveryDoesNotEmitNotice verifies the full retry
// lifecycle: turn 1 fails → warning emitted; turn 2 succeeds → tools
// available, NO follow-up notification. Recoveries are intentionally
// silent: "X is now available" right after the user completes an OAuth
// dance reads as a spurious warning, not a useful signal.
func TestAgentReProbeRecoveryDoesNotEmitNotice(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("server unavailable")
	stub := &flappyToolSet{
		errs:  []error{errBoom, nil},
		stubs: []tools.Tool{{Name: "mcp_ping", Parameters: map[string]any{}}},
	}
	a := New("root", "test", WithToolSets(stub))

	// Turn 1: start fails → 1 warning, 0 tools.
	got, err := a.Tools(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got, "turn 1: no tools while toolset is unavailable")
	warnings := a.DrainWarnings()
	require.Len(t, warnings, 1, "turn 1: exactly one warning expected")
	assert.Contains(t, warnings[0], "start failed")

	// Turn 2: start succeeds → tools available, NO recovery warning.
	got, err = a.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, got, 1, "turn 2: tool should be available after recovery")
	assert.Empty(t, a.DrainWarnings(), "turn 2: recovery must not emit any user-visible warning")
}

// TestAgentNoDuplicateStartWarnings verifies that repeated failures generate
// only one warning (on the first failure), not one per retry.
// slowToolSet blocks in Start until release is closed, recording completion.
type slowToolSet struct {
	stubToolSet

	release chan struct{}
	done    atomic.Bool
}

func (s *slowToolSet) Start(context.Context) error {
	<-s.release
	s.done.Store(true)
	return nil
}

// peerDependentToolSet implements tools.PeerDependent and records whether
// its peer had finished starting when its own Start ran.
type peerDependentToolSet struct {
	stubToolSet

	peer        *slowToolSet
	peerStarted atomic.Bool
}

var _ tools.PeerDependent = (*peerDependentToolSet)(nil)

func (p *peerDependentToolSet) StartsAfterPeers() {}

func (p *peerDependentToolSet) Start(context.Context) error {
	p.peerStarted.Store(p.peer.done.Load())
	return nil
}

// A peer-dependent toolset (e.g. the deferred aggregator) must only start
// once every other toolset has settled, even though starts run concurrently
// — its Start reads from its peers.
func TestEnsureToolSetsAreStartedPeerDependentStartsLast(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slow := &slowToolSet{release: make(chan struct{})}
		dep := &peerDependentToolSet{peer: slow}
		// The dependent is listed first on purpose: order in config must not matter.
		a := New("root", "test", WithToolSets(dep, slow))

		// In the bubble the sleep only fires once every other goroutine is
		// blocked, so a buggy concurrent dep.Start deterministically runs
		// (and records slow.done == false) before the release.
		go func() {
			time.Sleep(50 * time.Millisecond) //nolint:forbidigo // fake time inside the synctest bubble
			close(slow.release)
		}()

		_, err := a.Tools(t.Context())
		require.NoError(t, err)
		assert.True(t, dep.peerStarted.Load(), "peer-dependent toolset started before its peers settled")
	})
}

func TestAgentNoDuplicateStartWarnings(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("server unavailable")
	stub := &flappyToolSet{
		errs:  []error{errBoom, errBoom, errBoom},
		stubs: []tools.Tool{{Name: "mcp_ping", Parameters: map[string]any{}}},
	}
	a := New("root", "test", WithToolSets(stub))

	// Turn 1: first failure → warning.
	_, err := a.Tools(t.Context())
	require.NoError(t, err)
	warnings := a.DrainWarnings()
	require.Len(t, warnings, 1, "turn 1: exactly one warning on first failure")

	// Turn 2: repeated failure → no new warning.
	_, err = a.Tools(t.Context())
	require.NoError(t, err)
	assert.Empty(t, a.DrainWarnings(), "turn 2: no duplicate warning on repeated failure")

	// Turn 3: still failing → still no new warning.
	_, err = a.Tools(t.Context())
	require.NoError(t, err)
	assert.Empty(t, a.DrainWarnings(), "turn 3: no duplicate warning on repeated failure")
}

// TestAgentWarningsConcurrentAccess exercises the warnings queue from
// multiple goroutines to catch regressions in locking. Run with -race to
// actually detect a regression.
func TestAgentWarningsConcurrentAccess(t *testing.T) {
	t.Parallel()

	a := New("root", "test")

	const writers = 8
	const drainers = 4
	const perWriter = 200

	var writersWg, drainersWg sync.WaitGroup
	writersWg.Add(writers)
	drainersWg.Add(drainers)

	for range writers {
		go func() {
			defer writersWg.Done()
			for range perWriter {
				a.AddToolWarning("boom")
			}
		}()
	}

	stop := make(chan struct{})
	for range drainers {
		go func() {
			defer drainersWg.Done()
			for {
				select {
				case <-stop:
					// One final drain so we can assert a total count.
					_ = a.DrainWarnings()
					return
				default:
					_ = a.DrainWarnings()
				}
			}
		}()
	}

	// Keep drainers racing until every writer has finished, then stop them.
	writersWg.Wait()
	close(stop)
	drainersWg.Wait()

	// A successful run means no data race and no panic; we don't assert a
	// specific number of warnings drained because drainers run concurrently
	// with writers.
}

func TestAgentToolsRecoversWhenUnderlyingToolsetDies(t *testing.T) {
	t.Parallel()

	stub := &reconnectingToolSet{
		stubs: []tools.Tool{{Name: "mcp_ping", Parameters: map[string]any{}}},
	}
	a := New("root", "test", WithToolSets(stub))

	got, err := a.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, got, 1, "turn 1: tool should be available after initial start")
	assert.Empty(t, a.DrainWarnings())
	assert.Equal(t, 1, stub.startCalls)
	assert.Equal(t, 0, stub.restartCalls)
	assert.False(t, stub.IsStarted(), "test fixture simulates an underlying session death after turn 1")

	got, err = a.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, got, 1, "turn 2: tool should be available after Startable-triggered recovery")
	assert.Empty(t, a.DrainWarnings(), "silent recovery must not emit a warning")
	assert.Equal(t, 1, stub.startCalls)
	assert.Equal(t, 1, stub.restartCalls)
}

// hungStartToolSet blocks in Start until release is closed; entered is
// closed on the first call so tests can line up with the in-flight attempt.
type hungStartToolSet struct {
	stubToolSet

	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
	stops   atomic.Int32
}

func (h *hungStartToolSet) Start(context.Context) error {
	if h.calls.Add(1) == 1 {
		close(h.entered)
	}
	<-h.release
	return nil
}

func (h *hungStartToolSet) Stop(context.Context) error {
	h.stops.Add(1)
	return nil
}

func toolNames(ts []tools.Tool) []string {
	names := make([]string, len(ts))
	for i, tool := range ts {
		names[i] = tool.Name
	}
	return names
}

// TestAgentToolsSkipsStartAlreadyInFlight reproduces #4001: the runtime's
// startup probe initiates Start, times out and abandons the goroutine, which
// keeps holding the toolset's single-flight lock. The next turn must not
// block behind that lock: Tools() returns promptly with the static tools and
// the toolsets that are ready, emits no warning, and does not pile a second
// underlying Start onto the wedged toolset.
func TestAgentToolsSkipsStartAlreadyInFlight(t *testing.T) {
	t.Parallel()

	hung := &hungStartToolSet{
		stubToolSet: stubToolSet{tools: []tools.Tool{{Name: "hung_tool", Parameters: map[string]any{}}}},
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	ready := newStubToolSet(nil, []tools.Tool{{Name: "ready_tool", Parameters: map[string]any{}}}, nil)
	a := New("root", "test",
		WithToolSets(hung, ready),
		WithTools(tools.Tool{Name: "static_tool", Parameters: map[string]any{}}))

	// Always unblock the wedged Start so no goroutine outlives the test.
	releaseHung := sync.OnceFunc(func() { close(hung.release) })
	t.Cleanup(releaseHung)

	// Simulate the startup probe: Start is initiated and hangs, holding the
	// single-flight lock past the probe's deadline.
	probeDone := make(chan error, 1)
	go func() { probeDone <- a.toolsets[0].Start(t.Context()) }()
	<-hung.entered

	type toolsResult struct {
		tools []tools.Tool
		err   error
	}
	turnDone := make(chan toolsResult, 1)
	go func() {
		got, err := a.Tools(t.Context())
		turnDone <- toolsResult{tools: got, err: err}
	}()

	select {
	case res := <-turnDone:
		require.NoError(t, res.err)
		assert.ElementsMatch(t, []string{"ready_tool", "static_tool"}, toolNames(res.tools))
	case <-time.After(5 * time.Second):
		t.Fatal("Agent.Tools blocked behind an in-flight toolset Start")
	}

	assert.Empty(t, a.DrainWarnings(), "a start already in flight must not surface a warning")
	assert.EqualValues(t, 1, hung.calls.Load(), "the turn must not run a second underlying Start")

	// Once the wedged start completes, the next turn picks the toolset up
	// without another underlying Start.
	releaseHung()
	require.NoError(t, <-probeDone)

	got, err := a.Tools(t.Context())
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"hung_tool", "ready_tool", "static_tool"}, toolNames(got))
	assert.EqualValues(t, 1, hung.calls.Load())
}

// TestAgentToolsBoundsTurnInitiatedStart verifies the other half of #4001:
// when the turn itself initiates a Start and the toolset wedges (ignoring
// cancellation), the turn is bounded by tools.DefaultStartTimeout instead of
// hanging. The abandoned attempt stays silent and finishes in the
// background, so the next turn sees the toolset started.
func TestAgentToolsBoundsTurnInitiatedStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hung := &hungStartToolSet{
			stubToolSet: stubToolSet{tools: []tools.Tool{{Name: "hung_tool", Parameters: map[string]any{}}}},
			entered:     make(chan struct{}),
			release:     make(chan struct{}),
		}
		a := New("root", "test",
			WithToolSets(hung),
			WithTools(tools.Tool{Name: "static_tool", Parameters: map[string]any{}}))

		begin := time.Now()
		got, err := a.Tools(t.Context())
		require.NoError(t, err)
		assert.Equal(t, tools.DefaultStartTimeout, time.Since(begin), "the turn must give up exactly at the bound (fake time)")
		assert.Equal(t, []string{"static_tool"}, toolNames(got))
		assert.Empty(t, a.DrainWarnings(), "a start abandoned on timeout must be skipped silently")

		// Let the abandoned attempt finish; it completes the start in the
		// background under the single-flight lock.
		close(hung.release)
		synctest.Wait()

		got, err = a.Tools(t.Context())
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"hung_tool", "static_tool"}, toolNames(got))
		assert.EqualValues(t, 1, hung.calls.Load(), "the abandoned attempt is the only underlying Start")
	})
}

// TestAgentStopToolSetsWaitsForInFlightStart pins the shutdown half of the
// lifecycle contract: StopToolSets must not skip a toolset whose Start is
// still in flight. It waits for the attempt to settle, so a start abandoned
// by the turn path that eventually succeeds is still stopped instead of
// leaking.
func TestAgentStopToolSetsWaitsForInFlightStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hung := &hungStartToolSet{
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		a := New("root", "test", WithToolSets(hung))

		// Always unblock the wedged Start so no goroutine outlives the test.
		releaseHung := sync.OnceFunc(func() { close(hung.release) })
		t.Cleanup(releaseHung)

		startDone := make(chan error, 1)
		go func() { startDone <- a.toolsets[0].Start(t.Context()) }()
		<-hung.entered

		stopDone := make(chan error, 1)
		go func() { stopDone <- a.StopToolSets(t.Context()) }()

		// StopToolSets must wait for the in-flight Start to settle, not decide
		// early that the toolset is unstarted. Once every goroutine is durably
		// blocked, the stop must still be parked behind the wedged Start.
		synctest.Wait()
		select {
		case err := <-stopDone:
			t.Fatalf("StopToolSets returned (err=%v) while Start was still in flight", err)
		default:
		}

		releaseHung()
		require.NoError(t, <-startDone)
		require.NoError(t, <-stopDone)
		assert.EqualValues(t, 1, hung.stops.Load(), "the toolset that finished starting must be stopped, not leaked")
	})
}

// TestAgentStopToolSetsHonorsContextWhileStartInFlight pins the other
// shutdown half of the contract: when an in-flight Start ignores
// cancellation and never settles, StopToolSets gives up with ctx.Err() at
// the deadline instead of blocking shutdown forever, without running the
// underlying Stop behind the wedged attempt. Once that attempt eventually
// settles, the abandoned stop request still reaps the toolset.
func TestAgentStopToolSetsHonorsContextWhileStartInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hung := &hungStartToolSet{
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		a := New("root", "test", WithToolSets(hung))

		// Always unblock the wedged Start so no goroutine outlives the test.
		releaseHung := sync.OnceFunc(func() { close(hung.release) })
		t.Cleanup(releaseHung)

		startDone := make(chan error, 1)
		go func() { startDone <- a.toolsets[0].Start(t.Context()) }()
		<-hung.entered

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		begin := time.Now()
		err := a.StopToolSets(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, time.Second, time.Since(begin), "StopToolSets must give up exactly at the deadline (fake time)")
		assert.EqualValues(t, 0, hung.stops.Load(), "the underlying Stop must not run while Start is still wedged")

		// The wedged Start eventually settles; the stop request abandoned at
		// the deadline must reap the fresh start so it doesn't leak.
		releaseHung()
		require.NoError(t, <-startDone)
		assert.EqualValues(t, 1, hung.stops.Load(), "a start that settles after the abandoned shutdown must still be stopped")
	})
}

// stopRecordingToolSet starts immediately and records Stop calls; stopErr,
// when set, is returned from every Stop.
type stopRecordingToolSet struct {
	stubToolSet

	stopErr error
	stops   atomic.Int32
}

func (s *stopRecordingToolSet) Stop(context.Context) error {
	s.stops.Add(1)
	return s.stopErr
}

// TestAgentStopToolSetsContinuesPastStopFailure pins the aggregation
// contract of StopToolSets: a toolset whose Stop fails must not abandon
// stopping the later toolsets, and every failure surfaces in the joined
// error.
func TestAgentStopToolSetsContinuesPastStopFailure(t *testing.T) {
	t.Parallel()

	errFirst := errors.New("first boom")
	errSecond := errors.New("second boom")
	failingFirst := &stopRecordingToolSet{stopErr: errFirst}
	failingSecond := &stopRecordingToolSet{stopErr: errSecond}
	healthy := &stopRecordingToolSet{}
	a := New("root", "test", WithToolSets(failingFirst, failingSecond, healthy))
	for _, ts := range a.toolsets {
		require.NoError(t, ts.Start(t.Context()))
	}

	err := a.StopToolSets(t.Context())
	require.ErrorIs(t, err, errFirst)
	require.ErrorIs(t, err, errSecond)
	assert.EqualValues(t, 1, failingFirst.stops.Load())
	assert.EqualValues(t, 1, failingSecond.stops.Load(), "an earlier failed stop must not abandon the next toolset")
	assert.EqualValues(t, 1, healthy.stops.Load(), "a failed stop must not abandon the later toolsets")
}

// TestAgentStopToolSetsStopsLaterToolsetsPastDeadline pins the other half of
// the aggregation contract: a toolset wedged past the shutdown deadline
// surfaces ctx.Err() but must not abandon the later toolsets — a responsive
// started toolset is still stopped even though the deadline has already
// expired by the time its StopIfStarted runs, via the uncontended fast path
// in the lifecycle lock's LockContext.
func TestAgentStopToolSetsStopsLaterToolsetsPastDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedged := &hungStartToolSet{
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		responsive := &hungStartToolSet{
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		close(responsive.release) // starts and stops without blocking
		a := New("root", "test", WithToolSets(wedged, responsive))

		// Always unblock the wedged Start so no goroutine outlives the test.
		releaseWedged := sync.OnceFunc(func() { close(wedged.release) })
		t.Cleanup(releaseWedged)

		startDone := make(chan error, 1)
		go func() { startDone <- a.toolsets[0].Start(t.Context()) }()
		<-wedged.entered
		require.NoError(t, a.toolsets[1].Start(t.Context()))

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := a.StopToolSets(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.EqualValues(t, 0, wedged.stops.Load(), "the underlying Stop must not run while Start is wedged")
		assert.EqualValues(t, 1, responsive.stops.Load(), "a wedged earlier toolset must not abandon stopping the later ones")

		// The wedged Start settles; the stop request abandoned at the
		// deadline still reaps it.
		releaseWedged()
		require.NoError(t, <-startDone)
		assert.EqualValues(t, 1, wedged.stops.Load(), "a start that settles after the abandoned shutdown must still be stopped")
	})
}
