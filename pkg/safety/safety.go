// Package safety is the runtime's canonical tool-call safety taxonomy.
// It attaches a [Label] to every tool call before any approval decision
// is made; the (safety mode × label) table in pkg/runtime/toolexec is
// what actually gates the call.
//
// The package is a pure classifier: no I/O beyond the embedded pattern
// taxonomy, no verdicts, no side effects. Classification failures
// degrade to [ClassUnknown] — a labeller must never block a call.
package safety

import "strings"

// Class is the three-value safety taxonomy every tool call maps onto.
type Class string

const (
	// ClassSafe: positively recognised as read-only / side-effect free
	// (a safe-list shell command or a ReadOnlyHint annotation).
	ClassSafe Class = "safe"
	// ClassDestructive: positively recognised as destructive (a
	// destructive-list shell command or a DestructiveHint annotation).
	ClassDestructive Class = "destructive"
	// ClassUnknown: everything the classifier has no positive
	// knowledge about.
	ClassUnknown Class = "unknown"
)

// Origin records where a [Label] came from. The legacy default safety
// mode distinguishes annotation-derived safety (trusted, auto-approved
// historically) from classifier-derived safety.
type Origin string

const (
	// OriginAnnotation: derived from the tool's MCP annotations
	// (ReadOnlyHint / DestructiveHint).
	OriginAnnotation Origin = "annotation"
	// OriginClassifier: derived from the shell-command pattern
	// taxonomy.
	OriginClassifier Origin = "classifier"
)

// Metadata keys carried on tool-call confirmation events. Renderers
// key badges and explanations off these; hook authors can match the
// same names.
const (
	MetaSafetyLabel = "safety_label"
	MetaBlastRadius = "blast_radius"
	MetaCategory    = "category"
	MetaReason      = "reason"
)

// Label is the safety sticker attached to a tool call.
type Label struct {
	Class  Class
	Origin Origin

	// BlastRadius refines Class for display: safe | low | medium |
	// high | unknown. Empty when the classifier had nothing to say.
	BlastRadius string
	// Category is the taxonomy category of the matched pattern
	// (e.g. "fs-delete", "git-read"). Empty when no pattern matched.
	Category string
	// Reason is a human-readable explanation of the classification.
	Reason string
}

// Metadata renders the label as confirmation-event metadata. Empty
// fields are omitted so events stay lean, and the reason is only
// included for positive classifications (safe / destructive) — the
// unknown class carries boilerplate that must not clobber a more
// specific reason contributed by a hook.
func (l Label) Metadata() map[string]string {
	meta := map[string]string{MetaSafetyLabel: string(l.Class)}
	if l.BlastRadius != "" {
		meta[MetaBlastRadius] = l.BlastRadius
	}
	if l.Category != "" {
		meta[MetaCategory] = l.Category
	}
	if l.Reason != "" && l.Class != ClassUnknown {
		meta[MetaReason] = l.Reason
	}
	return meta
}

// Canonical names of the builtin tools that execute the shell command
// carried in their cmd argument. Duplicated here as string literals so
// pkg/safety does not depend on pkg/tools/builtin; the names are part
// of the user-facing wire protocol and a rename is caught by tests in
// both packages.
const (
	ShellToolName         = "shell"
	BackgroundJobToolName = "run_background_job"
)

// IsCommandTool reports whether toolName runs the shell command in its
// cmd argument, and so must be classified by command text rather than
// by annotation hints.
func IsCommandTool(toolName string) bool {
	return toolName == ShellToolName || toolName == BackgroundJobToolName
}

// LabelToolCall labels a tool call for the safety-mode table. Command
// tools (see [IsCommandTool]) are classified by command text; every
// other tool is labelled from its MCP annotation hints.
func LabelToolCall(toolName string, args map[string]any, readOnlyHint, destructiveHint bool) Label {
	if IsCommandTool(toolName) {
		cmd, _ := CommandArg(args)
		return ClassifyCommand(cmd)
	}
	return LabelForHints(readOnlyHint, destructiveHint)
}

// LabelForHints maps MCP annotation hints onto a label. DestructiveHint
// wins over ReadOnlyHint; neither yields [ClassUnknown].
func LabelForHints(readOnlyHint, destructiveHint bool) Label {
	switch {
	case destructiveHint:
		return Label{Class: ClassDestructive, Origin: OriginAnnotation, BlastRadius: "high"}
	case readOnlyHint:
		return Label{Class: ClassSafe, Origin: OriginAnnotation, BlastRadius: "safe"}
	default:
		return Label{Class: ClassUnknown, Origin: OriginAnnotation}
	}
}

// CommandArg extracts the shell command string from tool-call
// arguments with the same precedence as the shell and background-job
// handlers: a non-blank canonical "cmd" wins, otherwise the "command"
// alias some models emit. Classifying anything else would label a
// command the handler does not run.
func CommandArg(input map[string]any) (string, bool) {
	cmd, hasCmd := input["cmd"].(string)
	if hasCmd && strings.TrimSpace(cmd) != "" {
		return cmd, true
	}
	if v, ok := input["command"].(string); ok {
		return v, true
	}
	return cmd, hasCmd
}
