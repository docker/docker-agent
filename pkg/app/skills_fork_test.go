package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

// skillFakeRuntime extends mockRuntime with a real *skillstool.ToolSet so
// the App can detect fork-mode slash commands. RunSkillFork is recorded
// (no real sub-agent) since the contract under test is observable on the
// parent session alone.
type skillFakeRuntime struct {
	*mockRuntime

	skillset *skillstool.ToolSet

	mu       sync.Mutex
	calls    []skillstool.RunSkillArgs
	emitted  []runtime.Event
	stopCall atomic.Bool
	// skipStop, when true, omits the unconditional StreamStoppedEvent this
	// mock otherwise emits — used by the #4136 regression tests below to
	// exercise RunSkillFork's own channel-close fallback.
	skipStop bool
}

func (f *skillFakeRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet {
	return f.skillset
}

func (f *skillFakeRuntime) RunSkillFork(_ context.Context, sess *session.Session, args skillstool.RunSkillArgs, sink runtime.EventSink) (*tools.ToolCallResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	emitted := f.emitted
	skipStop := f.skipStop
	f.mu.Unlock()

	for _, ev := range emitted {
		sink.Emit(ev)
	}
	if !skipStop {
		// Emit StreamStoppedEvent so the App's drain loop terminates without
		// relying on its own #4136 fallback (see skipStop for the opposite).
		sink.Emit(runtime.StreamStopped(sess.ID, "", ""))
	}
	f.stopCall.Store(true)
	return tools.ResultSuccess("done"), nil
}

func (f *skillFakeRuntime) recordedCalls() []skillstool.RunSkillArgs {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.calls)
	if out == nil {
		out = []skillstool.RunSkillArgs{}
	}
	return out
}

// writeSkill creates a SKILL.md in a temp dir and returns a Local skill
// pointing at it so ReadSkillContent expands `!`shell“ placeholders.
func writeSkill(t *testing.T, name string, fork bool, body string) skills.Skill {
	t.Helper()

	dir := t.TempDir()
	skillFile := filepath.Join(dir, "SKILL.md")
	require.NoError(t, os.WriteFile(skillFile, []byte(body), 0o644))

	skill := skills.Skill{
		Name:        name,
		Description: "Test skill " + name,
		FilePath:    skillFile,
		BaseDir:     dir,
		Local:       true,
	}
	if fork {
		skill.Context = "fork"
	}
	return skill
}

// TestApp_SlashSkill_ForkContext_DispatchesToRunSkillFork verifies that a
// `/<skill>` slash command for a fork-mode skill is routed to
// Runtime.RunSkillFork and never inlines the skill body in the parent
// session as a regular user message.
func TestApp_SlashSkill_ForkContext_DispatchesToRunSkillFork(t *testing.T) {
	t.Parallel()

	const skillBody = "" +
		"# Commit\n" +
		"\n" +
		"Greeting: !`echo HELLO_FROM_SKILL`\n" +
		"\n" +
		"Please commit the staged changes.\n"

	skill := writeSkill(t, "commit", true /* fork */, skillBody)

	synctest.Test(t, func(t *testing.T) {
		st := skillstool.New([]skills.Skill{skill}, filepath.Dir(skill.FilePath))

		rt := &skillFakeRuntime{
			mockRuntime: &mockRuntime{},
			skillset:    st,
		}

		ctx := t.Context()
		// Pre-populate so the slash command is invoked mid-conversation.
		sess := session.New(session.WithUserMessage("hi there"))
		require.Equal(t, 1, sess.MessageCount())

		a := New(t.Context(), rt, sess)

		// Detection.
		skillName, task, ok := a.SkillCommandFork(ctx, "/commit please commit")
		require.True(t, ok, "fork-mode skill slash command must be detected")
		assert.Equal(t, "commit", skillName)
		assert.Equal(t, "please commit", task)

		// ResolveInput must NOT inline a fork-mode skill: that would cause
		// chat.processMessage to add it to the parent before fork dispatch runs.
		resolved := a.ResolveInput(ctx, "/commit please commit")
		assert.NotContains(t, resolved, "HELLO_FROM_SKILL")
		assert.NotContains(t, resolved, "<skill name=")
		assert.Equal(t, "/commit please commit", resolved, "raw input passes through; chat will take a different branch")

		// Dispatch.
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		a.RunSkillFork(runCtx, cancel, skillName, task, nil)

		synctest.Wait()
		require.True(t, rt.stopCall.Load(),
			"RunSkillFork goroutine should drain its event channel and finish")

		calls := rt.recordedCalls()
		require.Len(t, calls, 1)
		assert.Equal(t, "commit", calls[0].Name)
		assert.Equal(t, "please commit", calls[0].Task)

		// Parent session must be unchanged.
		require.Equal(t, 1, sess.MessageCount(), "parent session must not gain a user message from a fork-mode slash command")
		for _, item := range sess.GetAllMessages() {
			content := item.Message.Content
			assert.NotContains(t, content, "/commit")
			assert.NotContains(t, content, "HELLO_FROM_SKILL")
			assert.NotContains(t, content, "<skill name=")
		}

		assert.Same(t, sess, a.Session(), "App.Session() must still point at the parent")
	})
}

// TestApp_SlashSkill_InlineContext_StillInlines covers the
// backwards-compatible path: skills without `context: fork` keep being
// inlined into the parent transcript via the <skill> envelope.
func TestApp_SlashSkill_InlineContext_StillInlines(t *testing.T) {
	t.Parallel()

	skill := writeSkill(t, "review", false /* not fork */, "# Review\nPlease review.\n")
	st := skillstool.New([]skills.Skill{skill}, filepath.Dir(skill.FilePath))

	rt := &skillFakeRuntime{
		mockRuntime: &mockRuntime{},
		skillset:    st,
	}

	ctx := t.Context()
	a := New(t.Context(), rt, session.New())

	_, _, ok := a.SkillCommandFork(ctx, "/review the diff")
	assert.False(t, ok)

	resolved := a.ResolveInput(ctx, "/review the diff")
	assert.Contains(t, resolved, "<skill name=\"review\">")
	assert.Contains(t, resolved, "Please review.")
	assert.Contains(t, resolved, "the diff")
}

// TestApp_SlashSkill_NonFork_E2E exercises the full chat dispatch path for
// a non-fork skill: SkillCommandFork returns false, App.Run inlines the
// resolved body as a user message in the parent session, and
// Runtime.RunSkillFork is never called. Pairs with the fork-mode test to
// pin both halves of the dispatcher contract.
func TestApp_SlashSkill_NonFork_E2E(t *testing.T) {
	t.Parallel()

	const skillBody = "" +
		"---\n" +
		"name: review\n" +
		"description: Review staged changes\n" +
		"---\n" +
		"\n" +
		"# Review\n" +
		"\n" +
		"Greeting: !`echo HELLO_FROM_SKILL`\n" +
		"\n" +
		"Please review the staged changes.\n"

	skill := writeSkill(t, "review", false /* not fork */, skillBody)

	synctest.Test(t, func(t *testing.T) {
		st := skillstool.New([]skills.Skill{skill}, filepath.Dir(skill.FilePath))

		rt := &skillFakeRuntime{
			mockRuntime: &mockRuntime{},
			skillset:    st,
		}

		ctx := t.Context()
		// Pre-populate so the slash command is invoked mid-conversation.
		sess := session.New(session.WithUserMessage("hi there"))
		require.Equal(t, 1, sess.MessageCount())

		a := New(t.Context(), rt, sess)

		// Same dispatch as chat.processMessage.
		input := "/review carefully"
		_, _, ok := a.SkillCommandFork(ctx, input)
		require.False(t, ok)

		resolved := a.ResolveInput(ctx, input)
		require.Contains(t, resolved, `<skill name="review">`)
		require.Contains(t, resolved, "User's request: carefully")
		// A slash command is resolved synchronously inside the UI loop, so an
		// embedded command cannot be approved and is reported as skipped.
		require.Contains(t, resolved, "[error executing `echo HELLO_FROM_SKILL`")
		require.Contains(t, resolved, "Please review the staged changes.")

		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		a.Run(runCtx, cancel, resolved, nil)

		synctest.Wait()
		require.Equal(t, 2, sess.MessageCount(), "App.Run must append the resolved skill content")

		messages := sess.GetAllMessages()
		require.Len(t, messages, 2)
		last := messages[len(messages)-1]
		assert.Equal(t, chat.MessageRoleUser, last.Message.Role)
		assert.Contains(t, last.Message.Content, `<skill name="review">`)
		assert.Contains(t, last.Message.Content, "Please review the staged changes.")
		assert.Contains(t, last.Message.Content, "[error executing `echo HELLO_FROM_SKILL`")
		assert.Contains(t, last.Message.Content, "User's request: carefully")

		assert.Empty(t, rt.recordedCalls(), "Runtime.RunSkillFork must not be called for non-fork skills")
		assert.False(t, rt.stopCall.Load())
	})
}

// TestApp_RunSkillFork_SynthesizesStreamStoppedWhenMissing is part of the
// #4136 regression coverage: RunSkillFork's drain loop mirrors
// forwardRunStreamEvents and has the identical gap. If the runtime's
// events channel closes without a StreamStoppedEvent (dropped under
// back-pressure, see LocalRuntime.finalizeEventChannel's non-blocking
// emit), the App must still synthesize one so the TUI's "Working…"
// indicator clears.
func TestApp_RunSkillFork_SynthesizesStreamStoppedWhenMissing(t *testing.T) {
	t.Parallel()

	skill := writeSkill(t, "commit", true /* fork */, "# Commit\nPlease commit.\n")

	synctest.Test(t, func(t *testing.T) {
		st := skillstool.New([]skills.Skill{skill}, filepath.Dir(skill.FilePath))

		rt := &skillFakeRuntime{
			mockRuntime: &mockRuntime{},
			skillset:    st,
			skipStop:    true,
		}

		sess := session.New()
		a := New(t.Context(), rt, sess)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		a.RunSkillFork(ctx, cancel, "commit", "please commit", nil)

		synctest.Wait()
		require.True(t, rt.stopCall.Load(),
			"RunSkillFork goroutine should finish even without an explicit stop event")

		collected := collectUntilQuiet(t, a.events)
		stops := streamStoppedEvents(collected)
		require.Len(t, stops, 1, "exactly one StreamStoppedEvent must be synthesized")
		assert.Equal(t, "normal", stops[0].Reason)
		assert.Equal(t, sess.ID, stops[0].SessionID,
			"with no sub-session id ever observed, the fallback must use the parent session's own id")
		assert.Equal(t, "mock", stops[0].AgentName, "must fall back to Runtime.CurrentAgentName since no event carried one")
	})
}

// TestApp_RunSkillFork_DoesNotDuplicateRealStreamStopped pins the flip
// side: a fork run that does emit its own StreamStoppedEvent (the common
// case) must not also get a synthesized duplicate.
func TestApp_RunSkillFork_DoesNotDuplicateRealStreamStopped(t *testing.T) {
	t.Parallel()

	skill := writeSkill(t, "commit", true /* fork */, "# Commit\nPlease commit.\n")

	synctest.Test(t, func(t *testing.T) {
		st := skillstool.New([]skills.Skill{skill}, filepath.Dir(skill.FilePath))

		rt := &skillFakeRuntime{
			mockRuntime: &mockRuntime{},
			skillset:    st,
		}

		a := New(t.Context(), rt, session.New())

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		a.RunSkillFork(ctx, cancel, "commit", "please commit", nil)

		synctest.Wait()
		require.True(t, rt.stopCall.Load(),
			"RunSkillFork goroutine should finish")

		collected := collectUntilQuiet(t, a.events)
		stops := streamStoppedEvents(collected)
		require.Len(t, stops, 1, "the real StreamStoppedEvent must not be duplicated")
	})
}

func (f *skillFakeRuntime) ReadSkillContent(ctx context.Context, sess *session.Session, name string) (string, error) {
	st := f.CurrentAgentSkillsToolset()
	if st == nil {
		return "", nil
	}
	return st.ReadSkillContent(ctx, name, tools.NopRuntime{})
}

type rejectingSkillRuntime struct {
	*skillFakeRuntime
}

func (r *rejectingSkillRuntime) ReadSkillContent(context.Context, *session.Session, string) (string, error) {
	return "", errors.New("skill content rejected by policy")
}

func TestSlashSkillUsesRuntimeGuard(t *testing.T) {
	t.Parallel()
	st := skillstool.New([]skills.Skill{{Name: "unsafe", InlineContent: "untrusted instructions"}}, "")
	rt := &rejectingSkillRuntime{skillFakeRuntime: &skillFakeRuntime{mockRuntime: &mockRuntime{}, skillset: st}}
	a := New(t.Context(), rt, session.New())
	content, err := a.ResolveSkillCommand(t.Context(), "/unsafe")
	require.ErrorContains(t, err, "rejected by policy")
	assert.Empty(t, content)
	resolved := a.ResolveInput(t.Context(), "/unsafe")
	assert.Contains(t, resolved, "rejected by policy")
	assert.NotContains(t, resolved, "untrusted instructions")
}
