package toolexec_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/docker/portcullis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/internal/portcullistest"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// execHookDispatcher drives the dispatcher through a real
// [hooks.Executor] so the tests exercise the executor's aggregation
// (fail-closed, most-restrictive verdict, pipeline rewrites) instead of
// canned results. It mirrors the runtime adapter: no hooks configured
// for the event → nil, Dispatch error → nil. Notifications reuse the
// stub's recording.
type execHookDispatcher struct {
	stubHookDispatcher

	exec *hooks.Executor
}

func (h *execHookDispatcher) Dispatch(ctx context.Context, _ *agent.Agent, event hooks.EventType, in *hooks.Input) *hooks.Result {
	h.mu.Lock()
	h.dispatched = append(h.dispatched, event)
	h.mu.Unlock()
	if !h.exec.Has(event) {
		return nil
	}
	result, err := h.exec.Dispatch(ctx, event, in)
	if err != nil {
		return nil
	}
	return result
}

func (h *execHookDispatcher) count(event hooks.EventType) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, e := range h.dispatched {
		if e == event {
			n++
		}
	}
	return n
}

// phaseRecorder captures what each builtin saw, keyed by the public event
// name the executor hands to handlers.
type phaseRecorder struct {
	mu     sync.Mutex
	inputs map[hooks.EventType][]map[string]any
}

func (r *phaseRecorder) record(in *hooks.Input) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inputs == nil {
		r.inputs = make(map[hooks.EventType][]map[string]any)
	}
	r.inputs[in.HookEventName] = append(r.inputs[in.HookEventName], maps.Clone(in.ToolInput))
}

func (r *phaseRecorder) seen(event hooks.EventType) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inputs[event]
}

func (r *phaseRecorder) count(event hooks.EventType) int {
	return len(r.seen(event))
}

func (r *phaseRecorder) cmds(event hooks.EventType) []string {
	var out []string
	for _, in := range r.seen(event) {
		cmd, _ := in["cmd"].(string)
		out = append(out, cmd)
	}
	return out
}

// Builtin names registered by newPhaseHooks. Each records its input.
const (
	// rewrite <key> <value>: patches one argument.
	bRewrite = "rewrite"
	// verdict <decision> <reason> [k=v ...]: a permission_decision plus metadata.
	bVerdict = "verdict"
	// block <reason>: generic decision="block".
	bBlock = "block"
	// crash: handler error.
	bCrash = "crash"
	// verdict_when <substring> <decision> <reason>: verdict iff cmd contains substring.
	bVerdictWhen = "verdict_when"
	// permit: permission_request allow.
	bPermit = "permit"
)

func hook(name string, args ...string) hooks.Hook {
	return hooks.Hook{Type: hooks.HookTypeBuiltin, Command: name, Args: args, Timeout: 5}
}

func matchAll(hs ...hooks.Hook) []hooks.MatcherConfig {
	return []hooks.MatcherConfig{{Matcher: "*", Hooks: hs}}
}

func preemptYolo(hs ...hooks.Hook) []hooks.MatcherConfig {
	yes := true
	return []hooks.MatcherConfig{{Matcher: "*", Hooks: hs, PreemptYolo: &yes}}
}

func newPhaseHooks(t *testing.T, cfg *hooks.Config) (*execHookDispatcher, *phaseRecorder) {
	t.Helper()
	rec := &phaseRecorder{}
	reg := hooks.NewRegistry()
	must := func(err error) { require.NoError(t, err) }
	must(reg.RegisterBuiltin(bRewrite, func(_ context.Context, in *hooks.Input, args []string) (*hooks.Output, error) {
		rec.record(in)
		return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{
			UpdatedInput: map[string]any{args[0]: args[1]},
		}}, nil
	}))
	must(reg.RegisterBuiltin(bVerdict, func(_ context.Context, in *hooks.Input, args []string) (*hooks.Output, error) {
		rec.record(in)
		var meta map[string]string
		for _, kv := range args[2:] {
			k, v, _ := strings.Cut(kv, "=")
			if meta == nil {
				meta = map[string]string{}
			}
			meta[k] = v
		}
		return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{
			PermissionDecision:       hooks.Decision(args[0]),
			PermissionDecisionReason: args[1],
			Metadata:                 meta,
		}}, nil
	}))
	must(reg.RegisterBuiltin(bBlock, func(_ context.Context, in *hooks.Input, args []string) (*hooks.Output, error) {
		rec.record(in)
		return &hooks.Output{Decision: hooks.DecisionBlockValue, Reason: args[0]}, nil
	}))
	must(reg.RegisterBuiltin(bCrash, func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		rec.record(in)
		return nil, errors.New("boom")
	}))
	must(reg.RegisterBuiltin(bVerdictWhen, func(_ context.Context, in *hooks.Input, args []string) (*hooks.Output, error) {
		rec.record(in)
		if cmd, _ := in.ToolInput["cmd"].(string); !strings.Contains(cmd, args[0]) {
			return nil, nil
		}
		return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{
			PermissionDecision:       hooks.Decision(args[1]),
			PermissionDecisionReason: args[2],
		}}, nil
	}))
	must(reg.RegisterBuiltin(bPermit, func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		rec.record(in)
		return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{
			PermissionDecision: hooks.DecisionAllow,
		}}, nil
	}))
	return &execHookDispatcher{exec: hooks.NewExecutorWithRegistry(cfg, t.TempDir(), nil, reg)}, rec
}

// shellTool records the arguments the handler actually received.
func shellTool(gotArgs *[]string) tools.Tool {
	var mu sync.Mutex
	return tools.Tool{
		Name:     safety.ShellToolName,
		Category: "shell",
		Handler: func(_ context.Context, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			mu.Lock()
			defer mu.Unlock()
			*gotArgs = append(*gotArgs, tc.Function.Arguments)
			return tools.ResultSuccess("ok"), nil
		},
	}
}

func neverRun() tools.Tool {
	return tools.Tool{
		Name:     safety.ShellToolName,
		Category: "shell",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			panic("tool must not run")
		},
	}
}

func shellCall(id, cmd string) tools.ToolCall {
	return tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: safety.ShellToolName, Arguments: `{"cmd":"` + cmd + `"}`}}
}

func teamRules(allow, deny []string) func(*session.Session) []toolexec.NamedChecker {
	return staticCheckers(toolexec.NamedChecker{
		Checker: permissions.NewCheckerFromRules(allow, nil, deny),
		Source:  "permissions configuration",
		Tier:    toolexec.TierTeam,
	})
}

func newDispatcher(hd toolexec.HookDispatcher, resume <-chan toolexec.ResumeRequest) *toolexec.Dispatcher {
	a := newAgent()
	return &toolexec.Dispatcher{
		AgentFor: func(*session.Session) *agent.Agent { return a },
		Hooks:    hd,
		Resume:   resume,
	}
}

func lastApproval(t *testing.T, hd *execHookDispatcher) approvalRecord {
	t.Helper()
	hd.mu.Lock()
	defer hd.mu.Unlock()
	require.NotEmpty(t, hd.approvals)
	return hd.approvals[len(hd.approvals)-1]
}

func requireDenied(t *testing.T, em *captureEmitter, hd *execHookDispatcher, source string, fragments ...string) {
	t.Helper()
	assert.Empty(t, em.confirmations, "denied calls must not prompt")
	require.Len(t, em.responses, 1)
	assert.True(t, em.responses[0].IsError)
	for _, f := range fragments {
		assert.Contains(t, em.responses[0].Output, f)
	}
	assert.Equal(t, approvalRecord{Decision: toolexec.ApprovalDecisionDeny, Source: source}, lastApproval(t, hd))
}

// --- tool_input_transform ordering -------------------------------------

// The transform's rewrite must be what every later stage sees: guard,
// classifier, rules, legacy hook and the tool itself.
func TestToolPhases_TransformPrecedesEveryStage(t *testing.T) {
	t.Parallel()

	t.Run("autonomous: guard and tool see rewritten args", func(t *testing.T) {
		t.Parallel()
		hd, rec := newPhaseHooks(t, &hooks.Config{
			ToolInputTransform: matchAll(hook(bRewrite, "cmd", "ls")),
			ToolGuard:          matchAll(hook(bVerdict, "allow", "fine")),
			PreToolUse:         matchAll(hook(bVerdict, "ask", "legacy")),
		})
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
		var got []string
		em := &captureEmitter{}

		newDispatcher(hd, nil).Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "rm -rf /")}, []tools.Tool{shellTool(&got)}, em)

		assert.Equal(t, []string{`{"cmd":"ls"}`}, got)
		assert.Equal(t, []string{"rm -rf /"}, rec.cmds(hooks.EventToolInputTransform))
		assert.Equal(t, []string{"ls"}, rec.cmds(hooks.EventToolGuard))
		assert.Zero(t, rec.count(hooks.EventPreToolUse), "default lane stays skipped under autonomous")
		assert.Empty(t, em.confirmations)
		assert.Equal(t, approvalRecord{Decision: toolexec.ApprovalDecisionAllow, Source: toolexec.ApprovalSourceYolo}, lastApproval(t, hd))
	})

	t.Run("balanced: classifier labels the rewritten command", func(t *testing.T) {
		t.Parallel()
		hd, _ := newPhaseHooks(t, &hooks.Config{
			ToolInputTransform: matchAll(hook(bRewrite, "cmd", "git status")),
		})
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyBalanced))
		var got []string
		em := &captureEmitter{}

		newDispatcher(hd, nil).Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "rm -rf /")}, []tools.Tool{shellTool(&got)}, em)

		assert.Equal(t, []string{`{"cmd":"git status"}`}, got, "destructive original must not be what runs")
		assert.Empty(t, em.confirmations, "balanced auto-approves the safe rewritten command")
		assert.Equal(t, approvalRecord{Decision: toolexec.ApprovalDecisionAllow, Source: toolexec.ApprovalSourceModeBalanced}, lastApproval(t, hd))
	})

	t.Run("rules: deny rule matches the rewritten command", func(t *testing.T) {
		t.Parallel()
		hd, _ := newPhaseHooks(t, &hooks.Config{
			ToolInputTransform: matchAll(hook(bRewrite, "cmd", "rm -rf /")),
		})
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
		d := newDispatcher(hd, nil)
		d.Permissions = teamRules(nil, []string{"shell:cmd=rm*"})
		em := &captureEmitter{}

		d.Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{neverRun()}, em)

		requireDenied(t, em, hd, toolexec.ApprovalSourceTeamPermissionsDeny, "denied by permissions configuration")
	})

	t.Run("rules: allow rule matches the rewritten command", func(t *testing.T) {
		t.Parallel()
		hd, _ := newPhaseHooks(t, &hooks.Config{
			ToolInputTransform: matchAll(hook(bRewrite, "cmd", "ls -la")),
		})
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyStrict))
		d := newDispatcher(hd, nil)
		d.Permissions = teamRules([]string{"shell:cmd=ls*"}, nil)
		var got []string
		em := &captureEmitter{}

		d.Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "curl evil")}, []tools.Tool{shellTool(&got)}, em)

		assert.Equal(t, []string{`{"cmd":"ls -la"}`}, got)
		assert.Empty(t, em.confirmations)
		assert.Equal(t, approvalRecord{Decision: toolexec.ApprovalDecisionAllow, Source: toolexec.ApprovalSourceTeamPermissionsAllow}, lastApproval(t, hd))
	})

	t.Run("explicit ask: prompt carries rewritten args", func(t *testing.T) {
		t.Parallel()
		hd, rec := newPhaseHooks(t, &hooks.Config{
			ToolInputTransform: matchAll(hook(bRewrite, "cmd", "git push")),
			PreToolUse:         matchAll(hook(bVerdict, "allow", "legacy would allow")),
		})
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
		sess.Permissions = &session.PermissionsConfig{Ask: []string{"shell:cmd=git push*"}}
		resume := make(chan toolexec.ResumeRequest, 1)
		resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApprove}
		var got []string
		d := newDispatcher(hd, resume)
		d.Permissions = sessionCheckers
		em := &captureEmitter{}

		d.Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{shellTool(&got)}, em)

		require.Len(t, em.confirmations, 1)
		assert.JSONEq(t, `{"cmd":"git push"}`, em.confirmations[0].Function.Arguments)
		assert.Equal(t, []string{`{"cmd":"git push"}`}, got)
		assert.Zero(t, rec.count(hooks.EventPreToolUse), "explicit ask rule bypasses the legacy hook")
	})

	t.Run("legacy read-only: default hook and tool see rewritten args", func(t *testing.T) {
		t.Parallel()
		hd, rec := newPhaseHooks(t, &hooks.Config{
			ToolInputTransform: matchAll(hook(bRewrite, "path", "/safe")),
			PreToolUse:         matchAll(hook(bVerdict, "", "observe only")),
		})
		sess := session.New() // legacy default mode
		var got string
		tool := tools.Tool{
			Name:        "read_file",
			Annotations: tools.ToolAnnotations{ReadOnlyHint: true},
			Handler: func(_ context.Context, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
				got = tc.Function.Arguments
				return tools.ResultSuccess("ok"), nil
			},
		}
		em := &captureEmitter{}

		newDispatcher(hd, nil).Process(t.Context(), sess, []tools.ToolCall{{
			ID: "x", Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"/etc/shadow"}`},
		}}, []tools.Tool{tool}, em)

		assert.JSONEq(t, `{"path":"/safe"}`, got)
		assert.Empty(t, em.confirmations)
		require.Len(t, rec.seen(hooks.EventPreToolUse), 1)
		assert.Equal(t, "/safe", rec.seen(hooks.EventPreToolUse)[0]["path"])
		assert.Equal(t, approvalRecord{Decision: toolexec.ApprovalDecisionAllow, Source: toolexec.ApprovalSourceReadOnlyHint}, lastApproval(t, hd))
	})
}

func TestToolPhases_TransformReachesGate(t *testing.T) {
	t.Parallel()
	hd, rec := newPhaseHooks(t, &hooks.Config{
		ToolInputTransform: matchAll(hook(bRewrite, "cmd", "make build")),
		ToolGuard:          matchAll(hook(bVerdict, "allow", "fine")),
	})
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	em := &captureEmitter{}
	g := newGate(sess, em, nil)
	g.Hooks = hd

	var executed tools.ConfirmedRun
	out, err := g.ConfirmAndRun(t.Context(), echoCommand, func(_ context.Context, run tools.ConfirmedRun) (string, error) {
		executed = run
		return "built", nil
	})

	require.NoError(t, err)
	assert.Equal(t, "built", out)
	assert.Equal(t, "make build", executed.Args["cmd"], "gate must execute the transformed command")
	assert.Equal(t, echoCommand.Metadata, executed.Metadata)
	assert.Equal(t, []string{"make build"}, rec.cmds(hooks.EventToolGuard))
}

func TestToolPhases_TransformBlockDenies(t *testing.T) {
	t.Parallel()

	t.Run("explicit block", func(t *testing.T) {
		t.Parallel()
		hd, rec := newPhaseHooks(t, &hooks.Config{
			ToolInputTransform: matchAll(hook(bBlock, "policy says no")),
			ToolGuard:          matchAll(hook(bVerdict, "allow", "fine")),
		})
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
		em := &captureEmitter{}

		newDispatcher(hd, nil).Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{neverRun()}, em)

		requireDenied(t, em, hd, toolexec.ApprovalSourceToolInputTransformDeny, "tool_input_transform hook", "policy says no")
		require.Len(t, em.hookBlocks, 1)
		assert.Zero(t, rec.count(hooks.EventToolGuard), "a blocked transform never reaches the guard")
	})

	t.Run("crash follows on_error", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			onError string
			runs    bool
		}{
			{name: "default warn proceeds", runs: true},
			{name: "block denies", onError: "block"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				crash := hook(bCrash)
				crash.OnError = tc.onError
				hd, _ := newPhaseHooks(t, &hooks.Config{ToolInputTransform: matchAll(crash)})
				sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
				var got []string
				em := &captureEmitter{}

				newDispatcher(hd, nil).Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{shellTool(&got)}, em)

				if tc.runs {
					assert.Equal(t, []string{`{"cmd":"ls"}`}, got, "warn policy keeps the original input")
					return
				}
				assert.Empty(t, got)
				requireDenied(t, em, hd, toolexec.ApprovalSourceToolInputTransformDeny, "tool_input_transform hook failed to execute")
			})
		}
	})
}

// --- tool_guard ----------------------------------------------------------

func TestToolPhases_GuardDenyPreemptsYolo(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		guard hooks.Hook
		want  []string
	}{
		{name: "deny verdict", guard: hook(bVerdict, "deny", "destructive"), want: []string{"tool_guard hook", "destructive"}},
		{name: "generic block", guard: hook(bBlock, "nope"), want: []string{"tool_guard hook", "nope"}},
		{name: "crash fails closed", guard: hook(bCrash), want: []string{"tool_guard hook failed to execute", "boom"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hd, rec := newPhaseHooks(t, &hooks.Config{
				ToolGuard:  matchAll(tc.guard),
				PreToolUse: matchAll(hook(bVerdict, "allow", "legacy allow")),
			})
			sess := session.New()
			sess.ToolsApproved = true
			d := newDispatcher(hd, nil)
			d.Permissions = teamRules([]string{"shell"}, nil)
			em := &captureEmitter{}

			d.Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{neverRun()}, em)

			requireDenied(t, em, hd, toolexec.ApprovalSourceToolGuardDeny, tc.want...)
			require.Len(t, em.hookBlocks, 1)
			assert.Zero(t, rec.count(hooks.EventPreToolUse))
			assert.Zero(t, hd.count(hooks.EventPermissionRequest))
		})
	}
}

// A guard Ask is strict: neither an "always allow" session grant nor a
// permission_request allow may silence it. The user is prompted once and
// the guard is dispatched once per call, including for calls queued
// behind another confirmation.
func TestToolPhases_GuardAskForcesPromptDespiteGrants(t *testing.T) {
	t.Parallel()
	hd, rec := newPhaseHooks(t, &hooks.Config{
		ToolGuard:         matchAll(hook(bVerdict, "ask", "needs a human", "blast_radius=high", "category=deploy")),
		PermissionRequest: matchAll(hook(bPermit)),
	})
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	sess.Permissions = &session.PermissionsConfig{Allow: []string{"shell:cmd=mkdir*"}}
	resume := make(chan toolexec.ResumeRequest, 2)
	d := newDispatcher(hd, resume)
	d.Permissions = sessionCheckers
	var got []string
	em := &captureEmitter{}

	// The first answer grants "always allow"; the second call, queued
	// behind the confirmation mutex, must still prompt.
	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApproveTool, ToolName: "shell"}
	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApprove}

	d.Process(t.Context(), sess, []tools.ToolCall{
		shellCall("a", "mkdir one"),
		shellCall("b", "mkdir two"),
	}, []tools.Tool{shellTool(&got)}, em)

	assert.Len(t, em.confirmations, 2, "guard Ask prompts each call even with session allow grants")
	assert.Len(t, got, 2, "both calls run once approved")
	assert.Equal(t, 2, rec.count(hooks.EventToolGuard), "one guard dispatch per call, none after the prompt wait")
	assert.Zero(t, rec.count(hooks.EventPermissionRequest), "permission_request must not fire behind a guard Ask")
	for _, meta := range em.confirmationMeta {
		assert.Equal(t, "high", meta["blast_radius"], "guard metadata reaches the prompt")
		assert.Equal(t, "deploy", meta["category"])
	}
	hd.mu.Lock()
	defer hd.mu.Unlock()
	for _, ap := range hd.approvals {
		assert.Equal(t, toolexec.ApprovalDecisionAllow, ap.Decision)
		assert.Contains(t, []string{toolexec.ApprovalSourceUserApprovedTool, toolexec.ApprovalSourceUserApproved}, ap.Source)
	}
}

func TestToolPhases_GuardAskOverridesSafeClassification(t *testing.T) {
	t.Parallel()
	hd, _ := newPhaseHooks(t, &hooks.Config{
		ToolGuard: matchAll(hook(bVerdict, "ask", "double check")),
	})
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyBalanced))
	resume := make(chan toolexec.ResumeRequest, 1)
	resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeReject, Reason: "no thanks"}
	em := &captureEmitter{}

	newDispatcher(hd, resume).Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "git status")}, []tools.Tool{neverRun()}, em)

	require.Len(t, em.confirmations, 1, "balanced would auto-allow git status; the guard Ask wins")
	require.Len(t, em.responses, 1)
	assert.Contains(t, em.responses[0].Output, "no thanks")
}

func TestToolPhases_GuardAllowIsAdvisory(t *testing.T) {
	t.Parallel()

	t.Run("cannot bypass a deny rule", func(t *testing.T) {
		t.Parallel()
		hd, _ := newPhaseHooks(t, &hooks.Config{
			ToolGuard: matchAll(hook(bVerdict, "allow", "looks fine")),
		})
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
		d := newDispatcher(hd, nil)
		d.Permissions = teamRules(nil, []string{"shell:cmd=rm*"})
		em := &captureEmitter{}

		d.Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "rm -rf /")}, []tools.Tool{neverRun()}, em)

		requireDenied(t, em, hd, toolexec.ApprovalSourceTeamPermissionsDeny, "denied by permissions configuration")
	})

	t.Run("cannot skip the strict-mode prompt", func(t *testing.T) {
		t.Parallel()
		hd, rec := newPhaseHooks(t, &hooks.Config{
			ToolGuard:  matchAll(hook(bVerdict, "allow", "looks fine")),
			PreToolUse: matchAll(hook(bVerdict, "", "observe")),
		})
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyStrict))
		resume := make(chan toolexec.ResumeRequest, 1)
		resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApprove}
		var got []string
		em := &captureEmitter{}

		newDispatcher(hd, resume).Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "git status")}, []tools.Tool{shellTool(&got)}, em)

		require.Len(t, em.confirmations, 1, "guard allow must not auto-approve")
		assert.Len(t, got, 1)
		assert.Equal(t, 1, rec.count(hooks.EventPreToolUse), "default lane still runs when the mode asks")
	})
}

func TestToolPhases_GuardAskCannotOverrideExplicitDeny(t *testing.T) {
	t.Parallel()
	hd, _ := newPhaseHooks(t, &hooks.Config{
		ToolGuard: matchAll(hook(bVerdict, "ask", "let the user decide")),
	})
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	sess.Permissions = &session.PermissionsConfig{Deny: []string{"shell:cmd=rm*"}}
	d := newDispatcher(hd, nil)
	d.Permissions = sessionCheckers
	em := &captureEmitter{}

	d.Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "rm -rf /")}, []tools.Tool{neverRun()}, em)

	requireDenied(t, em, hd, toolexec.ApprovalSourceSessionPermissionsDeny, "denied by session permissions")
}

// --- legacy pre_tool_use lanes ------------------------------------------

// A preempt-yolo hook that fails (Allowed=false without a Deny verdict)
// now blocks instead of being read as "no opinion".
func TestToolPhases_PreemptYoloFailureBlocks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		h    hooks.Hook
		want string
	}{
		{name: "crash", h: hook(bCrash), want: "pre_tool_use hook failed to execute"},
		{name: "generic block", h: hook(bBlock, "stop right there"), want: "stop right there"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hd, _ := newPhaseHooks(t, &hooks.Config{PreToolUse: preemptYolo(tc.h)})
			sess := session.New()
			sess.ToolsApproved = true
			em := &captureEmitter{}

			newDispatcher(hd, nil).Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{neverRun()}, em)

			requireDenied(t, em, hd, toolexec.ApprovalSourcePreToolUseHookDeny, "pre_tool_use hook", tc.want)
		})
	}
}

// After a legacy pre_tool_use rewrite the guard, preempt lane and rules
// are re-evaluated exactly once on the new arguments; the transformer and
// the default lane are not rerun.
func TestToolPhases_LegacyRewriteRechecksGuardsAndRules(t *testing.T) {
	t.Parallel()

	t.Run("deny rule catches the rewritten command", func(t *testing.T) {
		t.Parallel()
		hd, rec := newPhaseHooks(t, &hooks.Config{
			ToolInputTransform: matchAll(hook(bRewrite, "cwd", "/work")),
			ToolGuard:          matchAll(hook(bVerdict, "allow", "fine")),
			PreToolUse:         matchAll(hook(bRewrite, "cmd", "rm -rf /")),
		})
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyStrict))
		d := newDispatcher(hd, nil)
		d.Permissions = teamRules(nil, []string{"shell:cmd=rm*"})
		em := &captureEmitter{}

		d.Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{neverRun()}, em)

		requireDenied(t, em, hd, toolexec.ApprovalSourceTeamPermissionsDeny, "denied by permissions configuration")
		assert.Equal(t, 1, rec.count(hooks.EventToolInputTransform), "transformer runs once")
		assert.Equal(t, 1, rec.count(hooks.EventPreToolUse), "default lane runs once")
		assert.Equal(t, []string{"ls", "rm -rf /"}, rec.cmds(hooks.EventToolGuard), "guard rechecked on the rewrite")
		assert.Equal(t, 2, hd.count(hooks.EventPreToolUsePreYolo), "preempt lane consulted again after the rewrite")
	})

	t.Run("guard denies the rewritten command", func(t *testing.T) {
		t.Parallel()
		hd, rec := newPhaseHooks(t, &hooks.Config{
			ToolGuard:  matchAll(hook(bVerdictWhen, "rm", "deny", "no deletes")),
			PreToolUse: matchAll(hook(bRewrite, "cmd", "rm -rf /")),
		})
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyStrict))
		em := &captureEmitter{}

		newDispatcher(hd, nil).Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{neverRun()}, em)

		requireDenied(t, em, hd, toolexec.ApprovalSourceToolGuardDeny, "tool_guard hook", "no deletes")
		assert.Equal(t, []string{"ls", "rm -rf /"}, rec.cmds(hooks.EventToolGuard))
		assert.Equal(t, 1, rec.count(hooks.EventPreToolUse))
	})

	t.Run("guard asks about the rewritten command", func(t *testing.T) {
		t.Parallel()
		hd, rec := newPhaseHooks(t, &hooks.Config{
			ToolGuard:  matchAll(hook(bVerdictWhen, "-la", "ask", "review")),
			PreToolUse: matchAll(hook(bRewrite, "cmd", "ls -la")),
		})
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyStrict))
		resume := make(chan toolexec.ResumeRequest, 1)
		resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApprove}
		var got []string
		em := &captureEmitter{}

		newDispatcher(hd, resume).Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{shellTool(&got)}, em)

		require.Len(t, em.confirmations, 1)
		assert.JSONEq(t, `{"cmd":"ls -la"}`, em.confirmations[0].Function.Arguments)
		assert.Equal(t, []string{`{"cmd":"ls -la"}`}, got)
		assert.Equal(t, 2, rec.count(hooks.EventToolGuard), "once before and once after the rewrite, never after the prompt")
		assert.Equal(t, 1, rec.count(hooks.EventPreToolUse))
	})

	t.Run("no-op rewrite does not rerun the guard", func(t *testing.T) {
		t.Parallel()
		hd, rec := newPhaseHooks(t, &hooks.Config{
			ToolGuard:  matchAll(hook(bVerdict, "allow", "fine")),
			PreToolUse: matchAll(hook(bRewrite, "cmd", "ls")),
		})
		sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyStrict))
		resume := make(chan toolexec.ResumeRequest, 1)
		resume <- toolexec.ResumeRequest{Type: toolexec.ResumeTypeApprove}
		var got []string
		em := &captureEmitter{}

		newDispatcher(hd, resume).Process(t.Context(), sess, []tools.ToolCall{shellCall("x", "ls")}, []tools.Tool{shellTool(&got)}, em)

		assert.Len(t, got, 1)
		assert.Equal(t, 1, rec.count(hooks.EventToolGuard))
		assert.Equal(t, 1, hd.count(hooks.EventPreToolUsePreYolo))
	})
}

func TestToolPhases_DefaultLaneSkippedOnAutoApproval(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		sess func() *session.Session
		perm func(*session.Session) []toolexec.NamedChecker
	}{
		{name: "autonomous", sess: func() *session.Session { return session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous)) }},
		{name: "balanced safe", sess: func() *session.Session { return session.New(session.WithSafetyPolicy(session.SafetyPolicyBalanced)) }},
		{name: "allow rule", sess: func() *session.Session { return session.New(session.WithSafetyPolicy(session.SafetyPolicyStrict)) }, perm: teamRules([]string{"shell"}, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hd, rec := newPhaseHooks(t, &hooks.Config{
				ToolInputTransform: matchAll(hook(bRewrite, "cwd", "/work")),
				ToolGuard:          matchAll(hook(bVerdict, "allow", "fine")),
				PreToolUse:         matchAll(hook(bVerdict, "ask", "would prompt")),
			})
			d := newDispatcher(hd, nil)
			d.Permissions = tc.perm
			var got []string
			em := &captureEmitter{}

			d.Process(t.Context(), tc.sess(), []tools.ToolCall{shellCall("x", "git status")}, []tools.Tool{shellTool(&got)}, em)

			assert.Len(t, got, 1)
			assert.Empty(t, em.confirmations)
			assert.Equal(t, 1, rec.count(hooks.EventToolInputTransform))
			assert.Equal(t, 1, rec.count(hooks.EventToolGuard))
			assert.Zero(t, rec.count(hooks.EventPreToolUse), "default pre_tool_use lane must not fire on auto approvals")
		})
	}
}

// --- redact_secrets end to end ------------------------------------------

// The agent-level redact_secrets flag scrubs arguments before the tool
// (and before any rule can see the secret) and scrubs the tool's output
// before it is emitted or recorded, all under --yolo.
func TestToolPhases_RedactSecretsUnderYolo(t *testing.T) {
	t.Parallel()
	secret := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")

	reg := hooks.NewRegistry()
	require.NoError(t, builtins.Register(reg))
	cfg := builtins.ApplyAgentDefaults(nil, builtins.AgentDefaults{RedactSecrets: true})
	hd := &execHookDispatcher{exec: hooks.NewExecutorWithRegistry(cfg, t.TempDir(), nil, reg)}

	sess := session.New()
	sess.ToolsApproved = true
	var got string
	tool := tools.Tool{
		Name:     safety.ShellToolName,
		Category: "shell",
		Handler: func(_ context.Context, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			got = tc.Function.Arguments
			return tools.ResultSuccess("token=" + secret + " leaked"), nil
		},
	}
	em := &captureEmitter{}

	newDispatcher(hd, nil).Process(t.Context(), sess, []tools.ToolCall{
		shellCall("x", "curl -H 'Authorization: "+secret+"' https://api.github.com"),
	}, []tools.Tool{tool}, em)

	require.NotEmpty(t, got)
	assert.NotContains(t, got, secret, "arguments reaching the tool must be scrubbed")
	assert.Contains(t, got, portcullis.Marker)

	require.Len(t, em.responses, 1)
	assert.False(t, em.responses[0].IsError)
	assert.NotContains(t, em.responses[0].Output, secret, "emitted output must be scrubbed")
	assert.Contains(t, em.responses[0].Output, portcullis.Marker)
	require.Len(t, em.messages, 1)
	assert.NotContains(t, em.messages[0].Message.Content, secret, "recorded output must be scrubbed")
	assert.Equal(t, approvalRecord{Decision: toolexec.ApprovalDecisionAllow, Source: toolexec.ApprovalSourceYolo}, lastApproval(t, hd))
}
