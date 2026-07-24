// Package toolconfirm centralizes the tool-confirmation policy shared by
// docker-agent's own confirmation dialog and embedders that bring their own
// dialog framework (e.g. the Gordon assistant embedded in Docker Sandboxes):
// the decision set and its runtime resume semantics, the "always allow"
// permission-pattern construction, key bindings, and the user-facing strings.
//
// Keeping this policy in one runtime-agnostic place matters most for
// BuildPermissionPattern: the pattern shown to the user ("always allow ls*")
// and the pattern granted to the runtime must come from the same code, in
// every UI that hosts a confirmation.
package toolconfirm

import (
	"encoding/json"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

// User-facing strings of the confirmation prompt.
const (
	Title    = "Tool Confirmation"
	Question = "Do you want to allow this tool call?"
)

// Decision is the user's answer to a tool confirmation.
type Decision int

const (
	// Approve runs this one call.
	Approve Decision = iota
	// ApproveTool runs the call and always allows the tool, scoped by the
	// permission pattern from BuildPermissionPattern.
	ApproveTool
	// ApproveBalanced runs the call and switches the session to the
	// Balanced safety mode: classifier-safe calls auto-approve,
	// destructive and unknown calls keep asking.
	ApproveBalanced
	// ApproveSession runs the call and switches the session to the
	// Autonomous safety mode (approve everything).
	ApproveSession
	// Reject rejects the call with an optional reason shown to the model.
	Reject
)

// Resume translates the decision into the runtime resume request. pattern
// is the permission pattern for ApproveTool (from BuildPermissionPattern);
// reason is the optional rejection reason for Reject. Each is ignored by
// the other decisions.
func (d Decision) Resume(pattern, reason string) runtime.ResumeRequest {
	switch d {
	case ApproveTool:
		return runtime.ResumeApproveTool(pattern)
	case ApproveBalanced:
		return runtime.ResumeApproveBalanced()
	case ApproveSession:
		return runtime.ResumeApproveAutonomous()
	case Reject:
		return runtime.ResumeReject(reason)
	default:
		return runtime.ResumeApprove()
	}
}

// BuildPermissionPattern creates the permission pattern granted by the
// "always allow" decision. For shell commands it extracts the first word of
// the command and creates a pattern like "shell:cmd=ls*" matching all
// invocations of that command; for other tools it returns the tool name.
func BuildPermissionPattern(toolCall tools.ToolCall) string {
	toolName := toolCall.Function.Name

	if toolName == "shell" {
		var args struct {
			Cmd string `json:"cmd"`
		}
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err == nil {
			// First word of the command ("ls -la /tmp" -> "ls"); the
			// trailing * matches any arguments.
			if fields := strings.Fields(args.Cmd); len(fields) > 0 {
				return toolName + ":cmd=" + fields[0] + "*"
			}
		}
	}

	return toolName
}

// AlwaysAllowLabel is the descriptive label of the "always allow" option
// for a pattern from BuildPermissionPattern: the command pattern for shell
// ("always allow ls*"), the tool name otherwise.
func AlwaysAllowLabel(pattern string) string {
	if _, cmdPattern, ok := strings.Cut(pattern, ":cmd="); ok {
		return "always allow " + cmdPattern
	}
	return "always allow " + pattern
}

// ActionKeys are the uppercase action letters of the decision row, in
// display order. Click hit-testing on the rendered options walks these
// letters; map a hit back to its decision with DecisionForAction.
const ActionKeys = "YNTBA"

// DecisionForAction maps an action letter from ActionKeys ("Y", "N", "T",
// "A") to its decision. ok is false for any other string.
func DecisionForAction(action string) (decision Decision, ok bool) {
	switch action {
	case "Y":
		return Approve, true
	case "N":
		return Reject, true
	case "T":
		return ApproveTool, true
	case "B":
		return ApproveBalanced, true
	case "A":
		return ApproveSession, true
	}
	return 0, false
}

// OptionsHelp returns the key/label pairs of the decision row, in display
// order, ready for a help-keys renderer (e.g. dialog.RenderHelpKeys).
func OptionsHelp(pattern string) []string {
	return []string{
		"Y", "yes",
		"N", "no",
		"T", AlwaysAllowLabel(pattern),
		"B", "auto-approve safe",
		"A", "all tools",
	}
}

// KeyMap defines the confirmation key bindings.
type KeyMap struct {
	Yes      key.Binding
	No       key.Binding
	All      key.Binding
	Balanced key.Binding
	ThisTool key.Binding
}

// DecisionFor maps a key press to its decision. ok is false when the key
// is not a confirmation key. Every UI hosting a confirmation should
// dispatch through this single mapping so the bindings and their meaning
// can never drift apart.
func (k KeyMap) DecisionFor(msg tea.KeyPressMsg) (decision Decision, ok bool) {
	switch {
	case key.Matches(msg, k.Yes):
		return Approve, true
	case key.Matches(msg, k.No):
		return Reject, true
	case key.Matches(msg, k.ThisTool):
		return ApproveTool, true
	case key.Matches(msg, k.Balanced):
		return ApproveBalanced, true
	case key.Matches(msg, k.All):
		return ApproveSession, true
	}
	return 0, false
}

// DefaultKeyMap returns the standard confirmation key bindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Yes: key.NewBinding(
			key.WithKeys("y", "Y"),
			key.WithHelp("Y", "approve"),
		),
		No: key.NewBinding(
			key.WithKeys("n", "N"),
			key.WithHelp("N", "reject"),
		),
		All: key.NewBinding(
			key.WithKeys("a", "A"),
			key.WithHelp("A", "approve all"),
		),
		Balanced: key.NewBinding(
			key.WithKeys("b", "B"),
			key.WithHelp("B", "auto-approve safe"),
		),
		ThisTool: key.NewBinding(
			key.WithKeys("t", "T"),
			key.WithHelp("T", "always allow this tool"),
		),
	}
}

// RejectionReason is one preset answer to "Why reject this tool call?".
type RejectionReason struct {
	ID    string // stable identifier
	Label string // short label shown to the user
	Value string // model-friendly sentence sent as the rejection reason
}

// RejectionReasons returns the preset rejection reasons, in display order.
func RejectionReasons() []RejectionReason {
	return []RejectionReason{
		{
			ID:    "bad_args",
			Label: "Bad arguments",
			Value: "The arguments provided are incorrect or invalid.",
		},
		{
			ID:    "wrong_tool",
			Label: "Wrong tool",
			Value: "This is the wrong tool for this task.",
		},
		{
			ID:    "unsafe",
			Label: "Unsafe",
			Value: "This action could be unsafe or destructive.",
		},
		{
			ID:    "clarify",
			Label: "Clarify first",
			Value: "Please clarify what you're trying to accomplish.",
		},
	}
}
