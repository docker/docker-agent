package latest

import (
	"errors"
	"fmt"
	"time"
)

// LifecycleConfig configures how the agent supervises a long-running
// toolset process (MCP server, remote MCP, LSP server).
//
// All fields are optional. The simplest usage is to pick a Profile that
// matches your taste:
//
//   - "resilient" (default): auto-restart on failure with exponential
//     backoff, optional toolset (a missing MCP doesn't block the agent).
//     This matches the historical docker-agent behaviour.
//   - "strict": fail fast — Required=true, no auto-restart. Use this in
//     CI/headless runs where you want the agent to refuse to start if a
//     dependency is unavailable.
//   - "best-effort": single attempt, no restart, optional. Use for
//     experimental MCPs whose flakiness you don't want to amplify with
//     restart loops.
//
// Explicit fields override the profile's defaults, so a user can write:
//
//	lifecycle:
//	  profile: resilient
//	  max_restarts: 10
//
// to get resilient behaviour with a higher restart budget.
//
// YAML example with all knobs:
//
//	lifecycle:
//	  profile: resilient        # strict | resilient | best-effort
//	  required: false           # block agent startup if not Ready in startup_timeout
//	  startup_timeout: 30s      # max wait for initial Connect+initialize
//	  call_timeout: 60s         # default per-call timeout (informational)
//	  restart: on_failure       # never | on_failure | always
//	  max_restarts: 5           # consecutive attempts; 0 = profile default; -1 = unlimited
//	  backoff:
//	    initial: 1s
//	    max: 32s
//	    multiplier: 2.0
//	    jitter: 0.2             # 0..1, 0 disables (default)
type LifecycleConfig struct {
	// Profile is a shorthand that picks defaults for all the other fields.
	// Empty means "resilient". Explicit fields override the profile.
	Profile string `json:"profile,omitempty" yaml:"profile,omitempty"`

	// Required, when set, indicates the toolset is critical to the agent.
	//
	// NOTE: this field is currently informational — the runtime does NOT
	// yet block agent startup on it. The wiring lives behind a planned
	// eager-startup commit; until then, callers can read the effective
	// value via IsRequired() but the supervisor itself does not act on it.
	//
	// nil pointer means "use the profile default" (true under "strict",
	// false otherwise).
	Required *bool `json:"required,omitempty" yaml:"required,omitempty"`

	// StartupTimeout caps the duration of the initial Connect call.
	//
	// NOTE: this field is currently informational — the runtime does NOT
	// yet enforce it. The supervisor's Start uses the caller's context
	// for cancellation today; honouring StartupTimeout requires the same
	// eager-startup wiring as Required.
	//
	// Zero means "no timeout".
	StartupTimeout Duration `json:"startup_timeout,omitzero" yaml:"startup_timeout,omitempty"`

	// CallTimeout is informational; it documents the user's expectation
	// for individual tool calls. The runtime currently uses the caller's
	// context for cancellation.
	CallTimeout Duration `json:"call_timeout,omitzero" yaml:"call_timeout,omitempty"`

	// Restart controls how the supervisor reacts to an unexpected
	// disconnect: "never", "on_failure" (default), or "always".
	Restart string `json:"restart,omitempty" yaml:"restart,omitempty"`

	// MaxRestarts is the maximum number of consecutive restart attempts
	// after a disconnect. 0 = use profile default (5). -1 = unlimited.
	MaxRestarts int `json:"max_restarts,omitempty" yaml:"max_restarts,omitempty"`

	// Backoff controls the wait between restart attempts.
	Backoff *BackoffConfig `json:"backoff,omitempty" yaml:"backoff,omitempty"`
}

// BackoffConfig controls the exponential backoff used between restart
// attempts. Zero fields fall back to profile defaults (Initial=1s,
// Max=32s, Multiplier=2.0, Jitter=0).
type BackoffConfig struct {
	Initial    Duration `json:"initial,omitzero" yaml:"initial,omitempty"`
	Max        Duration `json:"max,omitzero" yaml:"max,omitempty"`
	Multiplier float64  `json:"multiplier,omitempty" yaml:"multiplier,omitempty"`
	// Jitter is a 0..1 fraction of the computed delay applied as a
	// uniform random offset. 0 disables jitter (the default).
	Jitter float64 `json:"jitter,omitempty" yaml:"jitter,omitempty"`
}

// Lifecycle profile names.
const (
	LifecycleProfileResilient  = "resilient"
	LifecycleProfileStrict     = "strict"
	LifecycleProfileBestEffort = "best-effort"
)

// validate checks that LifecycleConfig values are within accepted ranges.
// Empty fields are accepted and resolved to profile defaults at use time.
func (l *LifecycleConfig) validate() error {
	if l == nil {
		return nil
	}
	switch l.Profile {
	case "", LifecycleProfileResilient, LifecycleProfileStrict, LifecycleProfileBestEffort:
	default:
		return fmt.Errorf("lifecycle.profile %q is not supported (want one of: %q, %q, %q)",
			l.Profile, LifecycleProfileResilient, LifecycleProfileStrict, LifecycleProfileBestEffort)
	}
	switch l.Restart {
	case "", "never", "on_failure", "always":
	default:
		return fmt.Errorf("lifecycle.restart %q is not supported (want one of: never, on_failure, always)", l.Restart)
	}
	if l.MaxRestarts < -1 {
		return fmt.Errorf("lifecycle.max_restarts %d must be >= -1 (use -1 for unlimited)", l.MaxRestarts)
	}
	if l.Backoff != nil {
		if l.Backoff.Initial.Duration < 0 {
			return errors.New("lifecycle.backoff.initial must be non-negative")
		}
		if l.Backoff.Max.Duration < 0 {
			return errors.New("lifecycle.backoff.max must be non-negative")
		}
		if l.Backoff.Multiplier < 0 {
			return errors.New("lifecycle.backoff.multiplier must be non-negative")
		}
		if l.Backoff.Jitter < 0 || l.Backoff.Jitter > 1 {
			return errors.New("lifecycle.backoff.jitter must be between 0 and 1")
		}
	}
	if l.StartupTimeout.Duration < 0 {
		return errors.New("lifecycle.startup_timeout must be non-negative")
	}
	if l.CallTimeout.Duration < 0 {
		return errors.New("lifecycle.call_timeout must be non-negative")
	}
	return nil
}

// IsRequired returns the effective Required flag for the given profile +
// explicit override. nil pointer means "use profile default".
func (l *LifecycleConfig) IsRequired() bool {
	if l == nil {
		return profileRequired("")
	}
	if l.Required != nil {
		return *l.Required
	}
	return profileRequired(l.Profile)
}

// EffectiveStartupTimeout returns StartupTimeout, falling back to a
// profile default when zero. Zero in the result means "no timeout".
func (l *LifecycleConfig) EffectiveStartupTimeout() time.Duration {
	if l == nil {
		return profileStartupTimeout("")
	}
	if l.StartupTimeout.Duration > 0 {
		return l.StartupTimeout.Duration
	}
	return profileStartupTimeout(l.Profile)
}

// profileRequired returns the Required default for the given profile.
func profileRequired(profile string) bool {
	return profile == LifecycleProfileStrict
}

// profileStartupTimeout returns the StartupTimeout default for the given
// profile. The "strict" profile uses 30s; others use 0 (no timeout).
func profileStartupTimeout(profile string) time.Duration {
	if profile == LifecycleProfileStrict {
		return 30 * time.Second
	}
	return 0
}
