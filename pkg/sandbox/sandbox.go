// Package sandbox provides Docker sandbox lifecycle management including
// creation, detection, argument building, and environment forwarding.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/environment"
)

// CheckAvailable returns a user-friendly error when Docker is not
// installed or the sandbox feature is not supported.
func (b *Backend) CheckAvailable(ctx context.Context) error {
	if _, err := exec.LookPath(b.program); err != nil {
		return fmt.Errorf("--sandbox requires Docker Desktop: %w\n\nInstall Docker Desktop from https://docs.docker.com/get-docker/", err)
	}

	cmd := exec.CommandContext(ctx, b.program, b.args("version")...)
	b.applyEnv(cmd)
	if err := cmd.Run(); err != nil {
		return errors.New("--sandbox requires Docker Desktop with sandbox support\n\n" +
			"Make sure Docker Desktop is running and up to date.\n" +
			"For more information, see https://docs.docker.com/ai/sandboxes/")
	}

	return nil
}

// Existing holds the name and workspaces of an existing Docker sandbox.
type Existing struct {
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

// Ensure makes sure a sandbox exists for the given workspace,
// creating or recreating it as needed. extras is a list of additional
// host directories to mount read-only (kit dir, agent yaml dir, ...).
// Each entry is made absolute and cleaned; duplicates and entries that
// resolve to wd are filtered out. When template is non-empty it is
// passed to `docker sandbox create -t`. loginKit, when non-empty, is a
// directory containing an sbx mixin kit (see [LoginKit]) passed to
// `create --kit` and also mounted read-only so that sandboxes created
// without it (or for a different gateway host) are not reused.
// Returns the sandbox name.
func (b *Backend) Ensure(ctx context.Context, wd string, extras []string, template, configDir, loginKit string) (string, error) {
	wd, err := absClean(wd)
	if err != nil {
		return "", fmt.Errorf("resolving workspace path: %w", err)
	}
	absConfigDir, err := absClean(configDir)
	if err != nil {
		return "", fmt.Errorf("resolving config dir: %w", err)
	}
	configDir = absConfigDir

	if loginKit != "" {
		extras = append(extras, loginKit)
	}
	extras, err = cleanExtras(extras, wd)
	if err != nil {
		return "", err
	}

	// Find every sandbox already attached to this workspace. After
	// previous failed runs there may be more than one (the original
	// foo, plus foo-1, foo-2 left behind by name-conflict suffixing
	// when an earlier rm couldn't finish). We pick the first one that
	// already has the mounts we need — and no login kit we no longer
	// want, which would keep authenticating gateway requests with the
	// user's Docker login; the rest are stale and get removed before
	// we create a fresh sandbox.
	matches := b.allForWorkspace(ctx, wd)

	for _, candidate := range matches {
		if hasAllWorkspaces(&candidate, extras) && candidate.HasWorkspace(configDir) && !staleLoginKit(&candidate, loginKit) {
			slog.DebugContext(ctx, "Reusing existing sandbox", "name", candidate.Name)
			return candidate.Name, nil
		}
	}

	// Nothing reusable. Remove every stale sandbox bound to this
	// workspace before creating a fresh one — if we leave any of them
	// behind, "docker sandbox create" / "sbx create" will detect the
	// name as taken and silently suffix the new sandbox with -1, -2,
	// ... which then accumulate forever and confuse subsequent reuse
	// lookups.
	for _, stale := range matches {
		slog.DebugContext(ctx, "Removing stale sandbox before recreate", "name", stale.Name)
		if rmOut, rmErr := b.rm(ctx, stale.Name); rmErr != nil {
			slog.WarnContext(ctx, "Failed to remove stale sandbox; the new one may end up with a name suffix",
				"name", stale.Name, "error", rmErr, "output", strings.TrimSpace(string(rmOut)))
		}
	}

	createExtra := []string{}
	if template != "" {
		createExtra = append(createExtra, "-t", template)
	}
	if loginKit != "" {
		createExtra = append(createExtra, "--kit="+loginKit)
	}
	createExtra = append(createExtra, "docker-agent", wd)
	for _, e := range extras {
		createExtra = append(createExtra, e+":ro")
	}
	// Mount config directory read-only so the sandbox can
	// access user config.
	createExtra = append(createExtra, configDir+":ro")

	createArgs := b.args("create", createExtra...)
	slog.DebugContext(ctx, "Creating sandbox", "args", createArgs)

	createCmd := exec.CommandContext(ctx, b.program, createArgs...)
	b.applyEnv(createCmd)
	createCmd.Stdin = os.Stdin
	createCmd.Stdout = os.Stdout
	createCmd.Stderr = os.Stderr

	if err := createCmd.Run(); err != nil {
		return "", fmt.Errorf("sandbox create failed: %w", err)
	}

	// Read back the sandbox name that was just created.
	created := b.ForWorkspace(ctx, wd)
	if created == nil {
		return "", errors.New("sandbox was created but could not be found")
	}

	return created.Name, nil
}

// absClean normalises a path so that two textually different but
// equivalent paths (e.g. "./foo" and "foo", "a//b" and "a/b") collapse
// before reaching the workspace dedup / reuse comparisons. Returns an
// error if filepath.Abs fails.
func absClean(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// cleanExtras drops empty entries, normalises every other entry with
// [absClean], removes duplicates, and filters out anything that
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
		abs, err := absClean(e)
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

// hasAllWorkspaces reports whether every entry of extras is mounted
// in s. Empty extras returns true.
func hasAllWorkspaces(s *Existing, extras []string) bool {
	for _, e := range extras {
		if !s.HasWorkspace(e) {
			return false
		}
	}
	return true
}

// BuildExecCmd assembles the sandbox exec command.
func (b *Backend) BuildExecCmd(ctx context.Context, name, wd string, cagentArgs, envFlags, envVars []string) *exec.Cmd {
	execExtra := []string{"-it", "-w", wd}
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
func EnvForAgent(ctx context.Context, agentRef string, env environment.Provider, flavors []string) (flags, envVars []string) {
	if agentRef == "" {
		return nil, nil
	}

	names, err := gatherAgentEnvVars(ctx, agentRef, env, flavors)
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

// gatherAgentEnvVars resolves the agent config and returns the list of
// environment variable names required by its models and tools.
func gatherAgentEnvVars(ctx context.Context, agentRef string, env environment.Provider, flavors []string) ([]string, error) {
	source, err := sources.Resolve(agentRef, env)
	if err != nil {
		return nil, fmt.Errorf("resolving agent: %w", err)
	}

	cfg, err := config.Load(ctx, source, config.WithFlavors(flavors...))
	if err != nil {
		return nil, fmt.Errorf("loading agent config: %w", err)
	}

	var names []string
	names = append(names, config.GatherEnvVarsForModels(ctx, cfg, env)...)

	toolNames, err := config.GatherEnvVarsForTools(ctx, cfg)
	if err != nil {
		slog.DebugContext(ctx, "Failed to gather tool env vars", "error", err)
	}
	names = append(names, toolNames...)

	return names, nil
}
