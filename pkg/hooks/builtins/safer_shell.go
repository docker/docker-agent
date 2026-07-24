package builtins

// safer_shell is a deprecated compatibility shim. The runtime now
// labels every tool call natively via pkg/safety and gates it through
// the (safety mode × label) table in pkg/runtime/toolexec, so this
// builtin no longer emits verdicts. It is kept registered for agent
// YAMLs that pin it explicitly under pre_tool_use: those entries keep
// working as pure labellers that attach classification metadata
// (safety_label, blast_radius, category, reason) to the call.

import (
	"context"
	"log/slog"
	"sync"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/safety"
)

// SaferShell is the registered name of the builtin.
//
// Deprecated: the runtime classifies shell commands natively; pinning
// this builtin only duplicates the metadata the runtime already
// attaches to confirmation events.
const SaferShell = "safer_shell"

// warnSaferShellOnce surfaces the behavior change to operators whose
// YAMLs still pin the builtin for enforcement: it used to emit ask/deny
// verdicts (preempting --yolo on destructive commands) and now only
// labels. Logged at first invocation, once per process.
var warnSaferShellOnce = sync.OnceFunc(func() {
	slog.Warn("The safer_shell hook is deprecated and no longer blocks or asks; " +
		"it only attaches classification metadata. Approval is decided by the " +
		"session's safety mode and permission rules — use a custom pre_tool_use " +
		"hook with an explicit decision if you relied on safer_shell to enforce.")
})

// saferShell is the [hooks.BuiltinFunc] registered under [SaferShell].
// Pure labeller: never blocks, never asks — it returns classification
// metadata with no permission decision so the approval pipeline is
// unaffected.
func saferShell(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	if in == nil || in.HookEventName != hooks.EventPreToolUse {
		return nil, nil
	}
	warnSaferShellOnce()
	if in.ToolName != safety.ShellToolName {
		return nil, nil
	}

	command, _ := safety.CommandArg(in.ToolInput)
	label := safety.ClassifyCommand(command)
	return &hooks.Output{
		HookSpecificOutput: &hooks.HookSpecificOutput{
			HookEventName: hooks.EventPreToolUse,
			Metadata:      label.Metadata(),
		},
	}, nil
}
