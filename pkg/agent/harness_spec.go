package agent

import "time"

// HarnessSpec describes the external harness backing a harness-backed agent.
// Built from HarnessConfig at team-load time.
type HarnessSpec struct {
	Type             string
	Command          string // resolved binary path
	Args             []string
	Env              map[string]string
	WorkingDir       string
	Timeout          time.Duration
	Config           map[string]any
	PermissionPolicy *PermissionPolicy
}

// PermissionMode controls how the harness handles tool permissions.
type PermissionMode string

const (
	PermissionModeAsk       PermissionMode = "ask"
	PermissionModeAutoAllow PermissionMode = "auto_allow"
	PermissionModeDenyAll   PermissionMode = "deny_all"
)

// PermissionPolicy configures permission handling for a harness-backed agent.
type PermissionPolicy struct {
	Mode PermissionMode
}
