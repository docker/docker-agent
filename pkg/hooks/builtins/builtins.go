// Package builtins contains the stock in-process hook implementations
// shipped with docker-agent.
//
// Available builtins:
//
//   - add_date              (turn_start)      — today's date
//   - add_environment_info  (session_start)   — cwd, git, OS, arch
//   - add_prompt_files      (turn_start)      — contents of prompt files
//   - add_git_status        (turn_start)      — `git status --short --branch`
//   - add_git_diff          (turn_start)      — `git diff --stat` (or full)
//   - add_directory_listing (session_start)   — top-level entries of cwd
//   - add_user_info         (session_start)   — current OS user and host
//   - add_recent_commits    (session_start)   — `git log --oneline -n N`
//   - max_iterations        (before_llm_call) — hard stop after N model calls
//   - unload                (on_agent_switch) — release the previous
//     agent's local-engine resources via HTTP unload (DMR today)
//   - snapshot              (session_start,
//     turn_start, turn_end,
//     pre_tool_use, post_tool_use,
//     session_end) — shadow-git snapshots. Installed via
//     [RegisterSnapshot] (separate entry point) so the embedder receives
//     a [SnapshotController] to drive /undo, /snapshots, /reset.
//   - redact_secrets        (pre_tool_use,
//     before_llm_call,
//     tool_response_transform) — scrub secrets
//     from tool args, outgoing chat content, and
//     tool output. Same builtin, dispatches on
//     event so a single name covers all three
//     legs of the feature.
//   - http_post              (any event)       — POST args[1] to args[0]
//
// Reference any of them from a hook YAML entry as
// `{type: builtin, command: "<name>"}`. The runtime additionally
// auto-injects add_date / add_environment_info / add_prompt_files /
// redact_secrets from the matching agent flags via [ApplyAgentDefaults].
// snapshot auto-injection lives on the controller returned by
// [RegisterSnapshot] and is plumbed into the runtime as an
// [AutoInjector], not as another bool on [AgentDefaults].
//
// turn_start builtins recompute every turn (date, git state).
// session_start builtins run once per session for context that's
// stable for its duration. snapshot is stateful: it keeps per-session
// turn/tool snapshot hashes and undo checkpoints in memory while the
// shadow git objects live under the data directory. Undo checkpoints
// intentionally survive the RunStream session_end cleanup so /undo
// can run after the response stops.
//
// LLM-as-a-judge hooks are NOT shipped here: write `type: model` with
// `schema: pre_tool_use_decision` instead — see
// pkg/hooks/shape_pre_tool_use_decision.go and examples/llm_judge.yaml.
package builtins

import (
	"errors"

	"github.com/docker/docker-agent/pkg/hooks"
)

// Register installs the stock builtin hooks on r.
//
// Note: the snapshot builtin is NOT installed by Register. It ships
// its own entry point ([RegisterSnapshot]) so the embedder receives a
// [SnapshotController] for driving /undo, /snapshots, /reset.
func Register(r *hooks.Registry) error {
	return errors.Join(
		r.RegisterBuiltin(AddDate, addDate),
		r.RegisterBuiltin(AddEnvironmentInfo, addEnvironmentInfo),
		r.RegisterBuiltin(AddPromptFiles, addPromptFiles),
		r.RegisterBuiltin(AddGitStatus, addGitStatus),
		r.RegisterBuiltin(AddGitDiff, addGitDiff),
		r.RegisterBuiltin(AddDirectoryListing, addDirectoryListing),
		r.RegisterBuiltin(AddUserInfo, addUserInfo),
		r.RegisterBuiltin(AddRecentCommits, addRecentCommits),
		r.RegisterBuiltin(MaxIterations, maxIterations),
		r.RegisterBuiltin(RedactSecrets, redactSecrets),
		r.RegisterBuiltin(HTTPPost, httpPost),
		r.RegisterBuiltin(Unload, unload),
	)
}

// AgentDefaults captures defaults that map onto stock builtin hook entries.
// Pass each AgentConfig.AddXxx flag as-is.
type AgentDefaults struct {
	AddDate            bool
	AddEnvironmentInfo bool
	AddPromptFiles     []string
	// RedactSecrets auto-injects the redact_secrets builtin under
	// pre_tool_use, before_llm_call, and tool_response_transform — the
	// three legs of the feature. Equivalent to writing those three
	// hook entries by hand; the dedup in [hooks.Executor.hooksFor]
	// makes the auto-injection idempotent against an explicit YAML
	// entry that already names the same builtin.
	RedactSecrets bool
}

// AutoInjector adds default hooks to an agent's hook configuration.
// The runtime invokes AutoInject for every registered injector when
// it builds per-agent executors, so a builtin that wants to be
// auto-wired only needs to ship its own AutoInjector and let the
// embedder plumb it in via runtime.WithAutoInjector.
//
// The snapshot controller returned by [RegisterSnapshot] satisfies
// this interface and is the canonical use case today; future builtins
// can opt in the same way without growing the central
// [ApplyAgentDefaults] table.
type AutoInjector interface {
	AutoInject(cfg *hooks.Config)
}

// ApplyAgentDefaults appends the stock builtin hook entries implied by
// d to cfg. A nil cfg is treated as empty. Returns nil iff no hook
// (user-configured or auto-injected) is present.
//
// Snapshot auto-injection is handled separately via [SnapshotController]
// (an [AutoInjector]) so it can be configured by the embedder rather
// than by another bool on AgentDefaults.
func ApplyAgentDefaults(cfg *hooks.Config, d AgentDefaults) *hooks.Config {
	if cfg == nil {
		cfg = &hooks.Config{}
	}
	if d.AddDate {
		cfg.TurnStart = append(cfg.TurnStart, builtinHook(AddDate))
	}
	if len(d.AddPromptFiles) > 0 {
		cfg.TurnStart = append(cfg.TurnStart, builtinHook(AddPromptFiles, d.AddPromptFiles...))
	}
	if d.AddEnvironmentInfo {
		cfg.SessionStart = append(cfg.SessionStart, builtinHook(AddEnvironmentInfo))
	}
	if d.RedactSecrets {
		// Wire all three legs at once. The same builtin handles each
		// event — it dispatches on input.HookEventName — so a single
		// `command: redact_secrets` entry would already work, but we
		// inject explicit entries here so the resulting effective
		// config is self-describing (a user inspecting it sees that
		// args, messages, and tool output are all covered, without
		// having to read the dispatch table).
		cfg.PreToolUse = append(cfg.PreToolUse, hooks.MatcherConfig{
			Matcher: "*",
			Hooks:   []hooks.Hook{builtinHook(RedactSecrets)},
		})
		cfg.BeforeLLMCall = append(cfg.BeforeLLMCall, builtinHook(RedactSecrets))
		cfg.ToolResponseTransform = append(cfg.ToolResponseTransform, hooks.MatcherConfig{
			Matcher: "*",
			Hooks:   []hooks.Hook{builtinHook(RedactSecrets)},
		})
	}
	if cfg.IsEmpty() {
		return nil
	}
	return cfg
}

// builtinHook returns a hook entry that dispatches to the named builtin.
func builtinHook(name string, args ...string) hooks.Hook {
	return hooks.Hook{Type: hooks.HookTypeBuiltin, Command: name, Args: args}
}
