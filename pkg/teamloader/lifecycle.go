package teamloader

import (
	"log/slog"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// lifecyclePolicyFromConfig converts a latest.LifecycleConfig into a
// lifecycle.Policy. nil cfg returns the resilient default policy.
//
// Resolution order: profile defaults first, then explicit field overrides.
// The Logger field is always populated with a component-tagged slog so
// supervisor messages identify which toolset produced them.
func lifecyclePolicyFromConfig(name string, cfg *latest.LifecycleConfig) lifecycle.Policy {
	policy := profilePolicy(profileName(cfg))
	policy.Logger = slog.With("component", "supervisor", "toolset", name)

	if cfg == nil {
		return policy
	}
	if cfg.Restart != "" {
		policy.Restart = parseRestart(cfg.Restart)
	}
	if cfg.MaxRestarts != 0 {
		// 0 keeps the profile default; -1 means unlimited (in both this
		// config and the supervisor).
		policy.MaxAttempts = cfg.MaxRestarts
	}
	if b := cfg.Backoff; b != nil {
		if b.Initial.Duration > 0 {
			policy.Backoff.Initial = b.Initial.Duration
		}
		if b.Max.Duration > 0 {
			policy.Backoff.Max = b.Max.Duration
		}
		if b.Multiplier > 0 {
			policy.Backoff.Multiplier = b.Multiplier
		}
		if b.Jitter > 0 {
			policy.Backoff.Jitter = b.Jitter
		}
	}
	return policy
}

// profileName returns the effective profile name, defaulting to
// "resilient" when cfg is nil or its Profile field is empty.
func profileName(cfg *latest.LifecycleConfig) string {
	if cfg == nil || cfg.Profile == "" {
		return latest.LifecycleProfileResilient
	}
	return cfg.Profile
}

// profilePolicy returns the lifecycle.Policy defaults for a profile name.
// Strict and best-effort produce the same supervisor policy (no restart);
// they differ in the Required flag which is documented but not yet
// enforced by the runtime.
func profilePolicy(profile string) lifecycle.Policy {
	switch profile {
	case latest.LifecycleProfileStrict, latest.LifecycleProfileBestEffort:
		// MaxAttempts is moot when Restart=Never; -1 keeps it explicit.
		return lifecycle.Policy{Restart: lifecycle.RestartNever, MaxAttempts: -1}
	default: // resilient (default + fallback for unknown names; the
		// validator already rejects unknown names).
		return lifecycle.Policy{Restart: lifecycle.RestartOnFailure, MaxAttempts: 5}
	}
}

// parseRestart converts a YAML restart string into the supervisor enum.
// Unknown values fall back to RestartOnFailure (the validator rejects
// them upstream).
func parseRestart(s string) lifecycle.Restart {
	switch s {
	case "never":
		return lifecycle.RestartNever
	case "always":
		return lifecycle.RestartAlways
	default:
		return lifecycle.RestartOnFailure
	}
}
