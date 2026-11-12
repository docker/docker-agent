package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/promptfiles"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func promptGuardRuntime(t *testing.T, dir string, judge hooks.BuiltinFunc) (*LocalRuntime, *handoffRecordingProvider) {
	t.Helper()
	reg := hooks.NewRegistry()
	require.NoError(t, reg.RegisterBuiltin("judge", judge))
	model := &handoffRecordingProvider{mockProvider: mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build()}}
	root := agent.New("root", "help", agent.WithModel(model), agent.WithAddPromptFiles([]string{"GUARDED.md"}), agent.WithHooks(&latest.HooksConfig{PromptFileGuard: []latest.HookDefinition{{Type: "builtin", Command: "judge", OnError: "ignore"}}}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithWorkingDir(dir), WithHooksRegistry(reg), WithModelStore(mockModelStore{}), WithSessionCompaction(false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	return rt, model
}

// These tests run sequentially because cache-stable prompts is a process-wide setting.
func promptGuardMode(t *testing.T, stable bool) {
	t.Helper()
	text := "settings:\n  cache_stable_prompts: false\n"
	if stable {
		text = "settings:\n  cache_stable_prompts: true\n"
	}
	require.NoError(t, os.WriteFile(userconfig.Path(), []byte(text), 0o600))
	t.Cleanup(func() { require.NoError(t, os.Remove(userconfig.Path())) })
}

func TestPromptFileGuardAdmission(t *testing.T) {
	for _, stable := range []bool{false, true} {
		mode := "legacy"
		if stable {
			mode = "stable"
		}
		for _, verdict := range []string{"allow", "deny", "error", "empty"} {
			t.Run(mode+"/"+verdict, func(t *testing.T) {
				promptGuardMode(t, stable)
				dir := t.TempDir()
				path := filepath.Join(dir, "GUARDED.md")
				require.NoError(t, os.WriteFile(path, []byte("ORIGINAL_INSTRUCTIONS"), 0o600))
				checks := 0
				rt, model := promptGuardRuntime(t, dir, func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
					checks++
					assert.Equal(t, "root", in.AgentName)
					assert.NotEmpty(t, in.SessionID)
					assert.Equal(t, "loaded", in.Source)
					assert.Equal(t, path, in.PromptFile.Path)
					assert.Contains(t, in.PromptFile.Content, "ORIGINAL_INSTRUCTIONS")
					// Consumption must not reopen an approved path.
					if err := os.WriteFile(path, []byte("UNAPPROVED_REPLACEMENT"), 0o600); err != nil {
						return nil, err
					}
					switch verdict {
					case "allow":
						return &hooks.Output{Continue: new(true)}, nil
					case "deny":
						return &hooks.Output{Decision: "block", Reason: "ORIGINAL_INSTRUCTIONS", SystemMessage: "ORIGINAL_INSTRUCTIONS"}, nil
					case "error":
						return nil, errors.New("ORIGINAL_INSTRUCTIONS")
					default:
						return nil, nil
					}
				})
				sess := session.New(session.WithUserMessage("hello"), session.WithToolsApproved(true))
				var eventText strings.Builder
				for ev := range rt.RunStream(t.Context(), sess) {
					data, err := json.Marshal(ev)
					require.NoError(t, err)
					eventText.Write(data)
				}
				assert.Equal(t, 1, checks)
				if verdict == "allow" {
					assert.Equal(t, 1, model.handoffCallCount())
					var content strings.Builder
					for _, msg := range model.lastMessages() {
						content.WriteString(msg.Content)
					}
					assert.Contains(t, content.String(), "ORIGINAL_INSTRUCTIONS")
					assert.NotContains(t, content.String(), "UNAPPROVED_REPLACEMENT")
					if stable {
						require.NotNil(t, sess.InstructionContextSnapshot())
					}
				} else {
					assert.Zero(t, model.handoffCallCount())
					assert.Nil(t, sess.InstructionContextSnapshot())
					assert.NotContains(t, eventText.String(), "ORIGINAL_INSTRUCTIONS")
					assert.Contains(t, eventText.String(), "prompt_file_guard stopped the turn")
					stored, err := rt.sessionStore.GetSession(t.Context(), sess.ID)
					require.NoError(t, err)
					assert.Nil(t, stored.InstructionContextSnapshot())
				}
			})
		}
	}
}

func promptSource(content string) session.InstructionSource {
	return session.InstructionSource{Key: "file", Group: promptfiles.InstructionGroup, Path: "/GUARDED.md", Label: "project rules", Content: content, RemovedContent: "old rules no longer apply", Available: true, CompleteGroup: true}
}

func TestPromptFileGuardRetainedHistory(t *testing.T) {
	for _, historical := range []string{"initial", "update", "current", "removal", "missing", "unavailable", "old session", "unrelated update"} {
		t.Run(historical, func(t *testing.T) {
			promptGuardMode(t, true)
			sess := session.New(session.WithUserMessage("hello"))
			first := promptSource("safe")
			if historical == "initial" || historical == "missing" || historical == "unavailable" || historical == "old session" {
				first.Content = "DANGEROUS"
			}
			if historical == "removal" {
				first.RemovedContent = "DANGEROUS"
			}
			if historical == "old session" {
				first.Path = ""
			}
			sess.PrepareInstructionContext([]session.InstructionSource{first})
			switch historical {
			case "update":
				sess.PrepareInstructionContext([]session.InstructionSource{promptSource("DANGEROUS")})
				sess.PrepareInstructionContext([]session.InstructionSource{promptSource("safe again")})
			case "current":
				// A compacted session will promote this value to Initial on preparation.
				sess.InstructionContext.Current["file"] = session.InstructionValue{Group: promptfiles.InstructionGroup, Content: "DANGEROUS"}
				sess.ApplyCompaction(0, 0, session.Item{Summary: "summary"})
			case "unrelated update":
				sess.PrepareInstructionContext([]session.InstructionSource{{Key: "other", Content: "DANGEROUS", Available: true}})
			}
			// Simulate restoring an older persisted session under a newly enabled guard.
			encoded, err := json.Marshal(sess)
			require.NoError(t, err)
			resumed := new(session.Session)
			require.NoError(t, json.Unmarshal(encoded, resumed))
			before := resumed.InstructionContextSnapshot()
			checks := 0
			rt, _ := promptGuardRuntime(t, t.TempDir(), func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
				checks++
				if strings.Contains(in.PromptFile.Content, "DANGEROUS") {
					return &hooks.Output{Decision: "block"}, nil
				}
				return &hooks.Output{Continue: new(true)}, nil
			})
			require.NoError(t, rt.sessionStore.AddSession(t.Context(), resumed))
			sources := []session.InstructionSource{promptSource("safe on disk")}
			if historical == "missing" || historical == "removal" {
				sources = []session.InstructionSource{{Group: promptfiles.InstructionGroup, Available: true, CompleteGroup: true, SetMarker: true}}
			}
			if historical == "unavailable" {
				sources = []session.InstructionSource{{Group: promptfiles.InstructionGroup, Available: false, SetMarker: true}}
			}
			messages, err := rt.messagesWithDynamicContext(t.Context(), resumed, rt.CurrentAgent(), sources, nil)
			require.ErrorIs(t, err, errPromptFileRejected)
			assert.Nil(t, messages)
			assert.Equal(t, before, resumed.InstructionContextSnapshot())
			if historical != "unavailable" {
				assert.Positive(t, checks)
			}
			stored, err := rt.sessionStore.GetSession(t.Context(), resumed.ID)
			require.NoError(t, err)
			assert.Equal(t, before, stored.InstructionContextSnapshot())
		})
	}
}

func TestPromptFileGuardRefreshAndPolicyChanges(t *testing.T) {
	promptGuardMode(t, true)
	dir := t.TempDir()
	path := filepath.Join(dir, "GUARDED.md")
	require.NoError(t, os.WriteFile(path, []byte("first"), 0o600))
	checks := 0
	deny := false
	rt, _ := promptGuardRuntime(t, dir, func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		checks++
		assert.Equal(t, path, in.PromptFile.Path)
		if deny {
			return &hooks.Output{Decision: "block"}, nil
		}
		return &hooks.Output{Continue: new(true)}, nil
	})
	sess := session.New(session.WithUserMessage("hello"))
	require.NoError(t, rt.sessionStore.AddSession(t.Context(), sess))
	assemble := func() ([]chat.Message, error) {
		observed := rt.executeTurnStartHooks(t.Context(), sess, rt.CurrentAgent(), nil)
		return rt.messagesWithDynamicContext(t.Context(), sess, rt.CurrentAgent(), observed.sources, observed.legacyMessages())
	}
	_, err := assemble()
	require.NoError(t, err)
	assert.Equal(t, 1, checks)
	require.NoError(t, os.WriteFile(path, []byte("second"), 0o600))
	_, err = assemble()
	require.NoError(t, err)
	assert.Greater(t, checks, 1)
	snapshot := sess.InstructionContextSnapshot()
	require.Len(t, snapshot.Updates, 1)
	assert.Contains(t, snapshot.Updates[0].Content, "second")
	// Do not reuse approvals after the policy changes, even if the bytes do not.
	deny = true
	messages, err := assemble()
	require.ErrorIs(t, err, errPromptFileRejected)
	assert.Nil(t, messages)
	assert.Equal(t, snapshot, sess.InstructionContextSnapshot())
}

func TestPromptFileGuardLegacyDoesNotReplayStoredHistory(t *testing.T) {
	promptGuardMode(t, false)
	rt, _ := promptGuardRuntime(t, t.TempDir(), func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		assert.Equal(t, "loaded", in.Source)
		return &hooks.Output{Continue: new(true)}, nil
	})
	sess := session.New()
	sess.PrepareInstructionContext([]session.InstructionSource{promptSource("old")})
	require.NoError(t, rt.sessionStore.AddSession(t.Context(), sess))
	messages, err := rt.messagesWithDynamicContext(t.Context(), sess, rt.CurrentAgent(), []session.InstructionSource{promptSource("new")}, []chat.Message{{Role: chat.MessageRoleSystem, Content: "new"}})
	require.NoError(t, err)
	assert.Nil(t, sess.InstructionContextSnapshot())
	for _, msg := range messages {
		assert.NotContains(t, msg.Content, "old")
	}
}

func TestPromptFileGuardCancellationAndUnavailable(t *testing.T) {
	t.Parallel()
	rt, _ := promptGuardRuntime(t, t.TempDir(), func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
		return &hooks.Output{Continue: new(true)}, nil
	})
	sess := session.New()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := rt.checkPromptFiles(ctx, sess, rt.CurrentAgent(), []session.InstructionSource{promptSource("data")}, false)
	require.ErrorIs(t, err, context.Canceled)
	unavailable := []session.InstructionSource{{Group: promptfiles.InstructionGroup, Available: false, SetMarker: true}}
	require.ErrorIs(t, rt.checkPromptFiles(t.Context(), sess, rt.CurrentAgent(), unavailable, false), errPromptFileRejected)
	// An unconfigured guard must not change legacy availability behavior.
	unguarded := agent.New("other", "")
	require.NoError(t, rt.checkPromptFiles(t.Context(), sess, unguarded, unavailable, true))
}

func TestPromptFileGuardNativeCompaction(t *testing.T) {
	for _, allow := range []bool{false, true} {
		name := "deny"
		if allow {
			name = "allow"
		}
		t.Run(name, func(t *testing.T) {
			promptGuardMode(t, false)
			comp := &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, result: nativeResult()}
			root := agent.New("root", "", agent.WithModel(comp), agent.WithHooks(&latest.HooksConfig{PromptFileGuard: []latest.HookDefinition{{Type: "builtin", Command: "judge"}}}))
			reg := hooks.NewRegistry()
			sess := nativeTestSession()
			sess.PrepareInstructionContext([]session.InstructionSource{promptSource("checked")})
			require.NoError(t, reg.RegisterBuiltin("judge", func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
				assert.Equal(t, "stored", in.Source)
				assert.Contains(t, in.PromptFile.Content, "checked")
				if !allow {
					return &hooks.Output{Decision: "block"}, nil
				}
				// Native assembly must use the snapshot being checked, not this replacement.
				sess.PrepareInstructionContext([]session.InstructionSource{promptSource("unapproved")})
				return &hooks.Output{Continue: new(true)}, nil
			}))
			rt := newNativeRuntime(t, root, WithHooksRegistry(reg))
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			result, err := rt.compactNatively(t.Context(), sess, root, comp, "", NewChannelSink(make(chan Event, 128)))
			if !allow {
				require.ErrorIs(t, err, errPromptFileRejected)
				assert.Nil(t, result)
				assert.Zero(t, comp.callCount())
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.Equal(t, 1, comp.callCount())
				for _, msg := range comp.messages {
					assert.NotContains(t, msg.Content, "unapproved")
				}
			}
		})
	}
}

func TestPromptFileGuardLoaderTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rt, model := promptGuardRuntime(t, t.TempDir(), func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
			return &hooks.Output{Continue: new(true)}, nil
		})
		require.NoError(t, rt.hooksRegistry.RegisterBuiltin(builtins.AddPromptFiles, func(ctx context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}))
		sess := session.New(session.WithUserMessage("hello"))
		var stopped bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if e, ok := ev.(*ErrorEvent); ok && strings.Contains(e.Error, "prompt_file_guard stopped") {
				stopped = true
			}
		}
		assert.True(t, stopped)
		assert.Zero(t, model.handoffCallCount())
		assert.Nil(t, sess.InstructionContextSnapshot())
	})
}

func TestPromptFileGuardParentCancellation(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	rt, model := promptGuardRuntime(t, t.TempDir(), func(ctx context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	require.NoError(t, os.WriteFile(filepath.Join(rt.workingDir, "GUARDED.md"), []byte("data"), 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sess := session.New(session.WithUserMessage("hello"))
	stream := rt.RunStream(ctx, sess)
	<-entered
	cancel()
	for ev := range stream {
		assert.IsNotType(t, &ErrorEvent{}, ev)
		if stop, ok := ev.(*StreamStoppedEvent); ok {
			assert.Equal(t, turnEndReasonCanceled, stop.Reason)
		}
	}
	assert.Zero(t, model.handoffCallCount())
	assert.Nil(t, sess.InstructionContextSnapshot())
}

func TestPromptFileGuardCompactionDuringCheck(t *testing.T) {
	promptGuardMode(t, true)
	sess := session.New(session.WithUserMessage("hello"))
	sess.PrepareInstructionContext([]session.InstructionSource{promptSource("first")})
	sess.PrepareInstructionContext([]session.InstructionSource{promptSource("current")})
	var checked []string
	compacted := false
	rt, _ := promptGuardRuntime(t, t.TempDir(), func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		checked = append(checked, in.PromptFile.Content)
		if !compacted {
			compacted = true
			sess.ApplyCompaction(0, 0, session.Item{Summary: "summary"})
		}
		return &hooks.Output{Continue: new(true)}, nil
	})
	require.NoError(t, rt.sessionStore.AddSession(t.Context(), sess))
	messages, err := rt.messagesWithDynamicContext(t.Context(), sess, rt.CurrentAgent(), []session.InstructionSource{promptSource("next")}, nil)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(checked, "\n"), "current")
	assert.Equal(t, "current", sess.InstructionContextSnapshot().Initial["file"].Content)
	assert.Equal(t, "next", sess.InstructionContextSnapshot().Current["file"].Content)
	var sent strings.Builder
	for _, msg := range messages {
		sent.WriteString(msg.Content)
	}
	assert.Contains(t, sent.String(), "current")
	assert.Contains(t, sent.String(), "next")
}

func TestPromptFileGuardUsesResolvedAgentPolicy(t *testing.T) {
	t.Parallel()
	rt, _ := promptGuardRuntime(t, t.TempDir(), func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
		return &hooks.Output{Decision: "block"}, nil
	})
	owner := rt.CurrentAgent()
	// The admission caller owns the agent snapshot, not the shared current pointer.
	rt.agents.Set("missing")
	err := rt.checkPromptFiles(t.Context(), session.New(session.WithAgentName("root")), owner, []session.InstructionSource{promptSource("data")}, true)
	require.ErrorIs(t, err, errPromptFileRejected)
}

func TestPromptFileGuardDiscoveryFailure(t *testing.T) {
	for _, stable := range []bool{false, true} {
		for _, guarded := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				t.Run(fmt.Sprintf("stable=%t/guarded=%t/explicit=%t", stable, guarded, explicit), func(t *testing.T) {
					promptGuardMode(t, stable)
					parent := t.TempDir()
					require.NoError(t, os.WriteFile(filepath.Join(parent, "GUARDED.md"), []byte("ancestor instructions"), 0o600))
					work := filepath.Join(parent, "project")
					require.NoError(t, os.Mkdir(work, 0o700))
					broken := filepath.Join(work, "GUARDED.md")
					if err := os.Symlink(broken, broken); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
					model := &handoffRecordingProvider{mockProvider: mockProvider{id: "test/model", stream: newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build()}}
					cfg := &latest.HooksConfig{}
					reg := hooks.NewRegistry()
					if guarded {
						cfg.PromptFileGuard = []latest.HookDefinition{{Type: "builtin", Command: "judge"}}
						require.NoError(t, reg.RegisterBuiltin("judge", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
							return &hooks.Output{Continue: new(true)}, nil
						}))
					}
					opts := []agent.Opt{agent.WithModel(model), agent.WithHooks(cfg)}
					if explicit {
						cfg.SessionStart = []latest.HookDefinition{{Type: "builtin", Command: builtins.AddPromptFiles, Args: []string{"GUARDED.md"}}}
					} else {
						opts = append(opts, agent.WithAddPromptFiles([]string{"GUARDED.md"}))
					}
					root := agent.New("root", "", opts...)
					rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithWorkingDir(work), WithHooksRegistry(reg), WithModelStore(mockModelStore{}), WithSessionCompaction(false))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, rt.Close()) })
					sess := session.New(session.WithUserMessage("hello"))
					for range rt.RunStream(t.Context(), sess) {
					}
					if guarded {
						assert.Zero(t, model.handoffCallCount())
						assert.Nil(t, sess.InstructionContextSnapshot())
					} else {
						assert.Equal(t, 1, model.handoffCallCount())
						data, err := json.Marshal(model.lastMessages())
						require.NoError(t, err)
						assert.Contains(t, string(data), "ancestor instructions")
						if stable {
							assert.NotNil(t, sess.InstructionContextSnapshot())
						}
					}
				})
			}
		}
	}
}
