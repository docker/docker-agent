// Package sandbox provides Docker sandbox lifecycle management including
// creation, detection, argument building, and environment forwarding.
package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/environment"
)

// CheckAvailable checks the selected CLI without requiring a local daemon.
func (b *Backend) CheckAvailable(ctx context.Context) error {
	if _, err := exec.LookPath(b.program); err != nil {
		return fmt.Errorf("--sandbox requires Docker Sandboxes (%s): %w\nInstall from https://docs.docker.com/ai/sandboxes/", b.program, err)
	}
	cmd := exec.CommandContext(ctx, b.program, b.args("version")...)
	b.applyEnv(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		mode := ""
		if b.cloud {
			mode = " with cloud support"
		}
		return fmt.Errorf("--sandbox requires a working Docker Sandboxes CLI%s: %w\n%s", mode, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Existing holds the name and workspaces of an existing Docker sandbox.
type Existing struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Workspaces []string `json:"workspaces"`
}

// HasWorkspace reports whether the sandbox has dir mounted as a workspace.
func (s *Existing) HasWorkspace(dir string) bool {
	return slices.ContainsFunc(s.Workspaces, func(ws string) bool {
		// Workspaces may have a ":ro" suffix.
		return strings.TrimSuffix(ws, ":ro") == dir
	})
}

// ForWorkspace returns the existing sandbox whose primary workspace
// matches wd, or nil if none exists. When several sandboxes share the
// same primary workspace (e.g. "foo" and "foo-1" left behind by a
// previous run that couldn't rm cleanly), the first one returned by
// the backend is picked.
func (b *Backend) ForWorkspace(ctx context.Context, wd string) *Existing {
	all := b.allForWorkspace(ctx, wd)
	if len(all) == 0 {
		return nil
	}
	return &all[0]
}

// allForWorkspace returns every existing sandbox whose primary
// workspace matches wd, in backend-listing order.
func (b *Backend) allForWorkspace(ctx context.Context, wd string) []Existing {
	cmd := exec.CommandContext(ctx, b.program, b.args("ls", "--json")...)
	b.applyEnv(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var raw struct {
		Sandboxes []Existing `json:"sandboxes"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil
	}

	var matches []Existing
	for _, entry := range raw.Sandboxes {
		if len(entry.Workspaces) > 0 && entry.Workspaces[0] == wd {
			matches = append(matches, entry)
		}
	}
	return matches
}

// Ensure reuses only the sandbox named for this launch configuration. Other
// sandboxes, including older docker-agent sandboxes, are never removed.
func (b *Backend) Ensure(ctx context.Context, wd string, extras []string, configDir, loginKit string, opts Options) (string, error) {
	wd, err := CanonicalPath(wd)
	if err != nil {
		return "", fmt.Errorf("resolving workspace path: %w", err)
	}
	configDir, err = CanonicalPath(configDir)
	if err != nil {
		return "", fmt.Errorf("resolving config dir: %w", err)
	}
	if loginKit != "" {
		extras = append(extras, loginKit)
	}
	extras = append(extras, configDir)
	extras, err = cleanExtras(extras, wd)
	if err != nil {
		return "", err
	}
	name, err := launchName(wd, extras, loginKit, opts)
	if err != nil {
		return "", err
	}
	all, err := b.list(ctx)
	if err != nil {
		return "", err
	}
	for _, candidate := range all {
		if candidate.Name != name {
			continue
		}
		wanted := []string{wd}
		for _, extra := range extras {
			wanted = append(wanted, extra+":ro")
		}
		if !sameWorkspaces(candidate.Workspaces, wanted) {
			return "", fmt.Errorf("sandbox %s already exists with different mounts; refusing to replace it", name)
		}
		return name, nil
	}

	createExtra := []string{"--name", name}
	createExtra = append(createExtra, opts.kitFlags()...)
	if opts.Template != "" {
		createExtra = append(createExtra, "-t", opts.Template)
	}
	if loginKit != "" {
		createExtra = append(createExtra, "--kit="+csvValue(loginKit))
	}
	createExtra = append(createExtra, opts.agent(), wd)
	for _, e := range extras {
		createExtra = append(createExtra, e+":ro")
	}
	createCmd := exec.CommandContext(ctx, b.program, b.args("create", createExtra...)...)
	b.applyEnv(createCmd)
	createCmd.Stdin = os.Stdin
	// Keep machine-readable agent output separate from provisioning messages.
	createCmd.Stdout = os.Stderr
	createCmd.Stderr = os.Stderr
	if err := createCmd.Run(); err != nil {
		return "", fmt.Errorf("sandbox create failed: %w", err)
	}
	return name, nil
}

func (b *Backend) list(ctx context.Context) ([]Existing, error) {
	cmd := exec.CommandContext(ctx, b.program, b.args("ls", "--json")...)
	b.applyEnv(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing sandboxes: %w", err)
	}
	var raw struct {
		Sandboxes []Existing `json:"sandboxes"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("decoding sandbox list: %w", err)
	}
	return raw.Sandboxes, nil
}

// cleanExtras drops empty entries, normalises every other entry with
// [CanonicalPath], removes duplicates, and filters out anything that
// resolves to wd — wd is already mounted read-write, so a second
// mount of the same path would shadow it.
//
// wd is expected to already be canonical (caller passes Ensure's
// resolved wd).
func cleanExtras(extras []string, wd string) ([]string, error) {
	dedup := map[string]bool{wd: true}
	cleaned := make([]string, 0, len(extras))
	for _, e := range extras {
		if e == "" {
			continue
		}
		abs, err := CanonicalPath(e)
		if err != nil {
			return nil, fmt.Errorf("resolving extra workspace %q: %w", e, err)
		}
		if dedup[abs] {
			continue
		}
		dedup[abs] = true
		cleaned = append(cleaned, abs)
	}
	return cleaned, nil
}

func sameWorkspaces(actual, wanted []string) bool {
	if len(actual) != len(wanted) || len(actual) == 0 || actual[0] != wanted[0] {
		return false
	}
	actual = slices.Clone(actual[1:])
	wanted = slices.Clone(wanted[1:])
	slices.Sort(actual)
	slices.Sort(wanted)
	return slices.Equal(actual, wanted)
}

// BuildExecCmd assembles the sandbox exec command.
func (b *Backend) BuildExecCmd(ctx context.Context, name, wd string, tty bool, cagentArgs, envFlags, envVars []string) *exec.Cmd {
	execExtra := []string{"-i"}
	if tty {
		execExtra = append(execExtra, "-t")
	}
	if wd != "" {
		execExtra = append(execExtra, "-w", wd)
	}
	execExtra = append(execExtra, envFlags...)

	// Improve the rendering of the TUI. C.UTF-8 is the only UTF-8 locale
	// shipped by the template image; a locale it lacks (en_US.UTF-8) makes
	// vim fall back to latin1 and mangle non-ASCII input.
	execExtra = append(execExtra,
		"-e", "TERM=xterm-256color",
		"-e", "COLORTERM=truecolor",
		"-e", "LANG=C.UTF-8",
		name, "docker-agent", "run",
	)
	execExtra = append(execExtra, cagentArgs...)

	args := b.args("exec", execExtra...)

	cmd := exec.CommandContext(ctx, b.program, args...)
	if b.cloud {
		gracefulCancel(cmd)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), envVars...)
	b.applyEnv(cmd)

	return cmd
}

// proxyManagedEnvVars lists env vars EnvForAgent never forwards to
// the sandbox. Docker Desktop proxies the API keys automatically;
// DOCKER_TOKEN is handled by the caller — it is either the login
// kit's proxy-managed sentinel (see [LoginKit]) or an explicit
// one-shot forward for localhost gateways.
var proxyManagedEnvVars = []string{
	"OPENAI_API_KEY",
	"ANTHROPIC_API_KEY",
	"GOOGLE_API_KEY",
	"MISTRAL_API_KEY",
	"OPENROUTER_API_KEY",
	"XAI_API_KEY",
	"NEBIUS_API_KEY",
	environment.DockerDesktopTokenEnv,
}

// EnvForAgent loads the agent config and gathers the environment
// variables it requires. It returns:
//   - flags: `-e KEY` args for docker sandbox exec (name only, no value)
//   - envVars: `KEY=VALUE` entries to set on the exec process environment
//
// Variables that Docker Desktop already proxies are skipped.
func EnvForAgent(ctx context.Context, agentRef string, env environment.Provider, flavors []string, defaults ...*config.RuntimeConfig) (flags, envVars []string) {
	if agentRef == "" {
		return nil, nil
	}

	source, err := sources.Resolve(agentRef, env)
	if err != nil {
		slog.DebugContext(ctx, "Failed to resolve agent for sandbox", "error", err)
		return nil, nil
	}
	return EnvForSource(ctx, source, env, flavors, defaults...)
}

// EnvForSource collects credentials from the already resolved agent selection.
func EnvForSource(ctx context.Context, source config.Source, env environment.Provider, flavors []string, defaults ...*config.RuntimeConfig) (flags, envVars []string) {
	names, err := gatherSourceEnvVars(ctx, source, env, flavors, defaults...)
	if err != nil {
		slog.DebugContext(ctx, "Failed to gather agent env vars for sandbox", "error", err)
		return nil, nil
	}

	for _, name := range names {
		if slices.Contains(proxyManagedEnvVars, name) {
			continue
		}
		val, ok := env.Get(ctx, name)
		if !ok || val == "" {
			continue
		}
		flags = append(flags, "-e", name)
		envVars = append(envVars, name+"="+val)
	}

	return flags, envVars
}

func gatherSourceEnvVars(ctx context.Context, source config.Source, env environment.Provider, flavors []string, defaults ...*config.RuntimeConfig) ([]string, error) {
	cfg, err := config.Load(ctx, source, config.WithFlavors(flavors...))
	if err != nil {
		return nil, fmt.Errorf("loading agent config: %w", err)
	}

	for _, rc := range defaults {
		if rc == nil {
			continue
		}
		config.MergeGlobalProviders(cfg, rc.Providers)
		config.MergeAgentHooks(cfg, config.MergeHooks(rc.GlobalHooks, rc.CLIHooks()))
	}
	if err := cfg.ValidateEvaluators(); err != nil {
		return nil, err
	}
	for name, def := range cfg.Evaluators {
		if _, err := def.Resolve(cfg.Providers); err != nil {
			return nil, fmt.Errorf("evaluator %q: %w", name, err)
		}
	}

	var names []string
	names = append(names, config.GatherEnvVarsForModels(ctx, cfg, env)...)
	names = append(names, config.GatherEnvVarsForEvaluators(cfg)...)

	toolNames, err := config.GatherEnvVarsForTools(ctx, cfg)
	if err != nil {
		slog.DebugContext(ctx, "Failed to gather tool env vars", "error", err)
	}
	names = append(names, toolNames...)

	return names, nil
}
