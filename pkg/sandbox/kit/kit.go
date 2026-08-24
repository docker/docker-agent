// Package kit stages a docker-agent kit on the host before launching a
// sandbox.
//
// A kit is a self-contained directory that bundles every host resource
// an agent will need at runtime — local skills, AGENTS.md/CLAUDE.md
// prompt files, sub-agent YAMLs — laid out under a fixed schema:
//
//	<kit>/skills/<skill-name>/         # local skills, recursively
//	<kit>/prompt_files/<name>          # collected add_prompt_files inputs
//	<kit>/manifest.json                # debug + cache key
//
// The host stages the kit, redacts secrets via [portcullis.Redact] in
// every text file, and bind-mounts the kit read-only inside the sandbox.
// At runtime, the in-sandbox resolvers ([skills.Load],
// [promptfiles.Paths]) consult [skills.KitDirEnv] to read from the kit
// instead of the user's $HOME — which doesn't exist inside the sandbox.
//
// The kit solves four constraints inherent to the docker sandbox CLI:
//   - the user's $HOME inside the sandbox is unrelated to the host's;
//   - sandbox mounts target directories, not individual files;
//   - host files may contain secrets that must not leak;
//   - other host-only state (e.g. .agents/skills under a parent dir) is
//     unreachable from the sandbox.
package kit

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/docker/portcullis"
	"github.com/fatih/color"

	"github.com/docker/docker-agent/pkg/config"
	latestcfg "github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/promptfiles"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/toolinstall"
)

// Output styles for PrintSummary. fatih/color auto-disables when
// stdout is not a TTY, so unit tests and piped output stay plain.
var (
	styleSection  = color.New(color.Bold).SprintFunc()
	styleName     = color.New(color.Bold).SprintFunc()
	styleHostPath = color.New(color.FgCyan).SprintFunc()
	styleNote     = color.New(color.Faint).SprintFunc()
	styleRedacted = color.New(color.FgYellow).SprintFunc()
	styleCount    = color.New(color.Bold).SprintFunc()
)

// manifestFile is the on-disk name of the kit's table of contents.
const manifestFile = "manifest.json"

// Options describes a kit build.
type Options struct {
	// AgentRef is the user-facing reference to the agent (a YAML path,
	// an OCI ref, a URL or a builtin name). Used for cache keying and
	// for loading the team config so the kit knows which prompt files
	// and skills to ship.
	AgentRef string

	// EnvProvider is forwarded to [config.Resolve] so URL-sourced
	// agents can pick up GITHUB_TOKEN. May be nil.
	EnvProvider environment.Provider

	// HostCwd is the host's working directory. Prompt-file lookups
	// walk up from it the same way the in-sandbox runtime would.
	HostCwd string

	// HostHome overrides the host's $HOME for prompt-file lookups.
	// Empty means "use os.UserHomeDir".
	HostHome string

	// Workspace is the absolute host path that the sandbox mounts as
	// the agent's live working directory. Files under it are not
	// staged into the kit because the sandbox sees them through the
	// live mount; staging them would duplicate content and ship a
	// stale, redacted copy alongside the live one.
	Workspace string

	// CacheDir is the parent directory under which the kit will be
	// staged. Empty means "use [paths.GetCacheDir]/sandbox-kits".
	CacheDir string

	// Flavors are the config flavor patches to enable when loading the
	// agent config, so the kit stages prompt files and skills for the
	// flavored configuration the sandboxed run will actually use.
	Flavors []string
}

// Result is what [Build] returns.
type Result struct {
	// HostDir is the absolute host path of the staged kit. Mount it
	// read-only into the sandbox (the sandbox CLI exposes extras at
	// the same path as on the host) and forward
	// `-e DOCKER_AGENT_KIT_DIR=<HostDir>` so the in-sandbox resolvers
	// find it.
	HostDir string

	// Manifest describes what was staged. It contains absolute host
	// source paths and is meant for caller-side inspection only — the
	// on-disk copy under <HostDir>/manifest.json is sanitised so the
	// sandbox cannot learn the host filesystem layout.
	Manifest Manifest

	// NeedsToolInstall reports whether the agent has at least one
	// MCP / LSP toolset that the in-sandbox runtime might auto-install
	// (the toolset has a Command set and Version isn't "false"/"off").
	// Callers use this to decide whether to allowlist the package
	// hosts the auto-installer reaches — we keep that gated so we
	// don't open holes in the sandbox proxy when no agent could
	// possibly need them.
	NeedsToolInstall bool

	// ToolInstallHosts is the sorted, deduplicated set of hostnames
	// the in-sandbox auto-installer needs to reach in order to install
	// every auto-installable toolset declared by the agent. It is
	// populated only when NeedsToolInstall is true.
	//
	// Each toolset's package is looked up against the aqua registry
	// and contributes only the hosts its install path actually uses
	// (Go module proxy + toolchain bootstrap for go_install packages,
	// GitHub release hosts for github_release packages, plus the
	// shared registry-lookup hosts in both cases). When a lookup
	// fails, [toolinstall.FallbackHosts] is folded in instead so the
	// install can still succeed at the cost of opening every install
	// host — callers that want to fail closed should inspect
	// ToolInstallHostsResolutionErr.
	ToolInstallHosts []string

	// ToolInstallHostsResolutionErr lists the per-toolset registry
	// lookup errors encountered while computing ToolInstallHosts.
	// When non-empty, ToolInstallHosts conservatively contains the
	// fallback union of every install host so the run can still
	// proceed; callers can choose stricter behaviour (refuse the run,
	// surface the error to the user) by checking this slice.
	ToolInstallHostsResolutionErr []ToolHostError
}

// Manifest is the kit's table of contents.
type Manifest struct {
	AgentRef    string      `json:"agent_ref"`
	BuiltAt     time.Time   `json:"built_at"`
	Skills      []Entry     `json:"skills,omitempty"`
	PromptFiles []Entry     `json:"prompt_files,omitempty"`
	Redactions  []Redaction `json:"redactions,omitempty"`
}

// Entry records one staged file or directory.
type Entry struct {
	// Source is the original host path. Omitted from the on-disk
	// manifest so the sandbox cannot learn the host layout.
	Source string `json:"-"`
	// Target is the path relative to the kit root. Empty when the
	// file isn't staged into the kit because it's already reachable
	// inside the sandbox via the live workspace mount.
	Target string `json:"target,omitempty"`
}

// IsStaged reports whether the entry has a copy under the kit root.
// A non-staged entry is one the agent reaches via the live workspace
// mount instead, surfaced in the manifest and the printed summary so
// the user can see what the agent will read.
func (e Entry) IsStaged() bool {
	return e.Target != ""
}

// Redaction records that portcullis added at least one [portcullis.Marker]
// to the staged copy of a file.
type Redaction struct {
	// Source is the host path of the original file. Omitted from the
	// on-disk manifest for the same reason as [Entry.Source].
	Source string `json:"-"`
	// Target is the path relative to the kit root.
	Target string `json:"target"`
}

// Build stages the kit and returns its location.
//
// Each Build creates a temporary directory under CacheDir, populates it,
// then atomically replaces the final per-agent directory. Concurrent
// Builds for the same agent therefore never see a half-populated kit;
// the last one to finish wins. The kit directory itself is reused
// across runs (deterministic path keyed by AgentRef) so the sandbox VM
// can be reused as long as nothing else in the workspace mount set
// changed.
func Build(ctx context.Context, opts Options) (*Result, error) {
	hostHome := opts.HostHome
	if hostHome == "" {
		hostHome = paths.GetHomeDir()
	}

	cacheParent := opts.CacheDir
	if cacheParent == "" {
		cacheParent = filepath.Join(paths.GetCacheDir(), "sandbox-kits")
	}
	if err := os.MkdirAll(cacheParent, 0o750); err != nil {
		return nil, fmt.Errorf("preparing kit cache: %w", err)
	}

	finalDir := filepath.Join(cacheParent, hashKey(opts.AgentRef))

	// Stage to a temp sibling first so concurrent builds and
	// crashed runs cannot leave behind a half-populated final dir
	// that a later sandbox would mount.
	stagingDir, err := os.MkdirTemp(cacheParent, ".tmp-")
	if err != nil {
		return nil, fmt.Errorf("preparing kit staging dir: %w", err)
	}
	// On any error past this point, drop the staging dir.
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(stagingDir)
		}
	}()

	manifest := Manifest{
		AgentRef: opts.AgentRef,
		BuiltAt:  time.Now().UTC(),
	}

	// Load the team config so we know which prompt files / skills the
	// agent will request. A failure here is non-fatal: we ship an empty
	// kit (no prompt files, no skills) so the sandbox still boots, but
	// the agent will fall back to its in-sandbox defaults.
	cfg, err := loadConfig(ctx, opts)
	if err != nil {
		slog.DebugContext(ctx, "kit: agent config unavailable; shipping an empty kit", "err", err)
		cfg = &latestcfg.Config{}
	}

	skillsEntries, redactions, err := stageSkills(ctx, stagingDir, cfg)
	if err != nil {
		return nil, err
	}
	manifest.Skills = skillsEntries
	manifest.Redactions = append(manifest.Redactions, redactions...)

	promptEntries, redactions, err := stagePromptFiles(stagingDir, cfg, opts.HostCwd, hostHome, opts.Workspace)
	if err != nil {
		return nil, err
	}
	manifest.PromptFiles = promptEntries
	manifest.Redactions = append(manifest.Redactions, redactions...)

	if err := writeManifest(stagingDir, manifest); err != nil {
		return nil, err
	}

	if err := promote(stagingDir, finalDir); err != nil {
		return nil, fmt.Errorf("publishing kit: %w", err)
	}
	committed = true

	slog.DebugContext(ctx, "kit: built",
		"dir", finalDir,
		"skills", len(manifest.Skills),
		"prompt_files", len(manifest.PromptFiles),
		"redactions", len(manifest.Redactions))

	hosts, hostErrs := resolveToolInstallHosts(ctx, cfg)

	return &Result{
		HostDir:                       finalDir,
		Manifest:                      manifest,
		NeedsToolInstall:              len(hosts) > 0,
		ToolInstallHosts:              hosts,
		ToolInstallHostsResolutionErr: hostErrs,
	}, nil
}

// hashKey turns AgentRef into a short, filesystem-safe directory name.
//
// File-system refs are canonicalised (Abs + EvalSymlinks) so that
// "./agent.yaml" and "/abs/path/agent.yaml" share a kit when they
// resolve to the same file. Non-file refs (OCI, URL, builtin name) are
// hashed verbatim.
//
// The ref is type-tagged before hashing so that, for instance, an OCI
// ref named "default" and the empty/builtin "default" cannot collide.
//
// We truncate to 8 bytes (16 hex chars) of SHA-256 because the entire
// keyspace here is the agents the user runs locally — a handful at
// most — so 2^64 buckets is comically large for the use case while
// keeping kit directory names short and readable.
func hashKey(ref string) string {
	tag, key := classifyRef(ref)
	sum := sha256.Sum256([]byte(tag + "\x00" + key))
	return hex.EncodeToString(sum[:8])
}

// classifyRef returns a tag identifying the kind of ref and a
// canonicalised key for hashing. File refs that resolve on disk are
// returned as ("file", absolute-real-path); everything else falls
// through as ("ref", ref) — including the empty string, which becomes
// ("empty", "") so it cannot collide with a literal ref of "default".
func classifyRef(ref string) (tag, key string) {
	if ref == "" {
		return "empty", ""
	}
	abs, err := filepath.Abs(ref)
	if err != nil {
		return "ref", ref
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		if info, err := os.Stat(resolved); err == nil && !info.IsDir() {
			return "file", resolved
		}
	}
	return "ref", ref
}

// promote moves stagingDir into final atomically. The publish is
// safe under concurrent calls for the same final dir: at most one
// staging tree wins the rename, and the losers' staging dirs are
// dropped by the caller's deferred cleanup. From the caller's
// point of view, every concurrent Build sees a fully populated
// finalDir on return.
//
// When finalDir already exists at promote time, it is moved aside
// to a "<final>.old-<staging-base>-<attempt>" sibling first so the
// rename can succeed; the old content is removed in the background. A
// leftover .old-* sibling from a crashed run is harmless and will be
// reaped on the next successful build.
//
// Publishing is wrapped in a bounded retry loop to absorb two races:
//
//  1. Three-way rename race: between our failed rename and our
//     recovery Stat, a *different* concurrent build can retire
//     finalDir to publish its own kit, leaving finalDir transiently
//     absent. A single Stat would misread that as a hard failure;
//     retrying lets us simply take the now-empty slot.
//
//  2. Windows access-denied race: Windows refuses to rename a
//     directory while another goroutine holds an open handle to it
//     (ERROR_ACCESS_DENIED). A concurrent Build holding finalDir open
//     makes the retire rename fail; retrying waits for the handle to
//     be released. On POSIX, renames succeed regardless of open
//     handles, so this path is never taken there.
//
// Both windows are microseconds wide and real concurrency is a handful
// of builds, so the loop converges in one or two iterations.
func promote(stagingDir, finalDir string) error {
	const maxAttempts = 100
	// Retired siblings accumulate across attempts; clean them all up on
	// return (defer-in-loop would leak the closure until the function
	// exits anyway, so collect explicitly and remove once).
	var retiredDirs []string
	defer func() {
		for _, d := range retiredDirs {
			_ = os.RemoveAll(d)
		}
	}()

	var lastErr error
	for attempt := range maxAttempts {
		// Move any existing finalDir aside so the rename below can
		// succeed. The retired name is unique per attempt so a retry
		// can't collide with a sibling we moved aside earlier (which
		// is pending background removal).
		if _, err := os.Stat(finalDir); err == nil {
			retired := fmt.Sprintf("%s.old-%s-%d", finalDir, filepath.Base(stagingDir), attempt)
			if err := os.Rename(finalDir, retired); err != nil && !os.IsNotExist(err) {
				// On Windows another goroutine may hold finalDir open,
				// causing ERROR_ACCESS_DENIED. That's transient — retry
				// rather than failing the build.
				if isRetryableRenameErr(err) {
					lastErr = err
					runtime.Gosched() // yield to the goroutine holding the handle
					continue
				}
				return fmt.Errorf("retiring previous kit: %w", err)
			}
			retiredDirs = append(retiredDirs, retired)
		}

		err := os.Rename(stagingDir, finalDir)
		if err == nil {
			return nil
		}
		lastErr = err
		// A concurrent Build may have already promoted its own kit to
		// finalDir between our Stat above and our Rename here. If
		// finalDir is now a populated directory, we accept the other
		// run as the winner: the caller will see a complete kit, and
		// our staging tree will be cleaned up by Build's deferred
		// rollback.
		if _, statErr := os.Stat(finalDir); statErr == nil {
			return nil
		}
		// finalDir is transiently absent because another build retired
		// it mid-promote. stagingDir is still intact (the rename
		// failed), so loop and try to claim the empty slot.
	}
	return fmt.Errorf("renaming kit: %w", lastErr)
}

func loadConfig(ctx context.Context, opts Options) (*latestcfg.Config, error) {
	source, err := config.Resolve(opts.AgentRef, opts.EnvProvider)
	if err != nil {
		return nil, err
	}
	return config.Load(ctx, source, config.WithFlavors(opts.Flavors...))
}

// stageSkills copies every local skill the agent config enables into
// <kit>/skills/<skill-name>/, redacting text files in place.
//
// Only skills the agent will actually load are staged: if no agent in
// cfg enables local skills the kit ships nothing, and if every agent
// that enables local skills also restricts them via an `include`
// filter, only the union of those filters is staged. An agent that
// enables local skills without a filter is the wide case — every local
// skill is staged.
func stageSkills(ctx context.Context, kitDir string, cfg *latestcfg.Config) ([]Entry, []Redaction, error) {
	target := filepath.Join(kitDir, skills.KitSkillsSubdir)
	if err := os.MkdirAll(target, 0o750); err != nil {
		return nil, nil, fmt.Errorf("creating kit skills dir: %w", err)
	}

	include, ok := localSkillFilter(cfg)
	if !ok {
		return nil, nil, nil
	}

	var (
		entries    []Entry
		redactions []Redaction
	)
	for _, skill := range skills.Load(ctx, []string{"local"}) {
		if skill.BaseDir == "" {
			continue
		}
		if include != nil && !include[skill.Name] {
			continue
		}
		dst := filepath.Join(target, sanitise(skill.Name))
		reds, err := copyTree(kitDir, skill.BaseDir, dst)
		if err != nil {
			return nil, nil, fmt.Errorf("staging skill %q: %w", skill.Name, err)
		}
		entries = append(entries, Entry{
			Source: skill.BaseDir,
			Target: filepath.Join(skills.KitSkillsSubdir, sanitise(skill.Name)),
		})
		redactions = append(redactions, reds...)
	}
	return entries, redactions, nil
}

// localSkillFilter inspects every agent in cfg and reports the local
// skill subset the kit should stage.
//
// Returns:
//   - (nil, false) when no agent enables local skills — the kit ships
//     nothing.
//   - (nil, true)  when at least one agent enables local skills without
//     an `include` filter — every local skill is staged.
//   - (set, true)  when every agent that enables local skills also
//     restricts them — only skills whose name is in the union of
//     `include` lists are staged.
func localSkillFilter(cfg *latestcfg.Config) (map[string]bool, bool) {
	if cfg == nil {
		return nil, false
	}
	include := make(map[string]bool)
	anyLocal := false
	for _, agent := range cfg.Agents {
		if !agent.Skills.HasLocal() {
			continue
		}
		anyLocal = true
		if len(agent.Skills.Include) == 0 {
			return nil, true
		}
		for _, name := range agent.Skills.Include {
			include[name] = true
		}
	}
	if !anyLocal {
		return nil, false
	}
	return include, true
}

// stagePromptFiles walks every agent in cfg, records every
// add_prompt_files entry the agent will read, and stages the ones
// that aren't already reachable inside the sandbox via the live
// workspace mount.
//
// Files under workspace are NOT copied (the live mount surfaces them
// directly), but they are still recorded in the returned [Entry]
// slice with Target == "" so the printed kit summary reflects every
// file the agent will see — not just the ones the host had to ship.
//
// When a single name (e.g. "AGENTS.md") resolves to both a workspace
// match and a host-home match we record both: the workspace one
// surfaces through the live mount and the host-home one is staged
// into the kit, exactly mirroring the runtime [promptfiles.Paths]
// behaviour that returns up to two paths.
func stagePromptFiles(kitDir string, cfg *latestcfg.Config, hostCwd, hostHome, workspace string) ([]Entry, []Redaction, error) {
	target := filepath.Join(kitDir, promptfiles.KitSubdir)
	if err := os.MkdirAll(target, 0o750); err != nil {
		return nil, nil, fmt.Errorf("creating kit prompt-files dir: %w", err)
	}

	var (
		entries    []Entry
		redactions []Redaction
		seen       = make(map[string]bool)
		stagedKey  = make(map[string]bool) // dedupe by kit-relative target
	)
	for _, agent := range cfg.Agents {
		for _, name := range agent.AddPromptFiles {
			if seen[name] {
				continue
			}
			seen[name] = true

			for _, src := range promptfiles.Paths(hostCwd, hostHome, "", name) {
				if isUnder(src, workspace) {
					// The live mount will surface it inside the sandbox —
					// no copy needed, but record the entry so the user can
					// see in the kit summary that AGENTS.md (etc.) is
					// covered.
					entries = append(entries, Entry{Source: src, Target: ""})
					continue
				}

				rel := filepath.Join(promptfiles.KitSubdir, name)
				if stagedKey[rel] {
					// Two non-workspace matches for the same name (e.g.
					// the rare case of nested $HOMEs in tests). Keep the
					// first — [promptfiles.Paths] orders them with the
					// closest match first.
					continue
				}
				dst := filepath.Join(kitDir, rel)
				red, err := copyFile(kitDir, src, dst)
				if err != nil {
					return nil, nil, fmt.Errorf("staging prompt file %q: %w", src, err)
				}
				entries = append(entries, Entry{Source: src, Target: rel})
				if red != nil {
					redactions = append(redactions, *red)
				}
				stagedKey[rel] = true
			}
		}
	}
	return entries, redactions, nil
}

// isUnder reports whether path is contained within base. Both paths
// are made absolute and have their symlinks resolved, so a symlink
// from outside-workspace into the workspace (or vice versa) cannot
// trick the check. When EvalSymlinks fails (e.g. dangling links) the
// best-effort absolute paths are used instead, which is still strict
// enough to defeat the textual "../" escape.
func isUnder(path, base string) bool {
	if base == "" {
		return false
	}
	resolvedBase := resolveAbs(base)
	resolvedPath := resolveAbs(path)
	if resolvedBase == "" || resolvedPath == "" {
		return false
	}
	rel, err := filepath.Rel(resolvedBase, resolvedPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveAbs returns p resolved to an absolute, symlink-free path. If
// p itself doesn't exist, the deepest existing ancestor is resolved
// and the remaining (non-existent) tail is appended to it. This
// matters on systems whose temp dir is itself a symlink (e.g. macOS
// /var → /private/var): an existing base resolves to /private/var
// while a not-yet-created child of it would otherwise resolve to
// /var/..., causing isUnder to wrongly report that they're unrelated.
func resolveAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	// Walk up until we find an existing ancestor we can resolve.
	tail := ""
	current := abs
	for {
		parent := filepath.Dir(current)
		tail = filepath.Join(filepath.Base(current), tail)
		if parent == current {
			return abs // hit the root without finding anything resolvable
		}
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(resolved, tail)
		}
		current = parent
	}
}

// sanitise replaces filesystem-unfriendly characters in a skill name so
// it becomes a usable directory entry under <kit>/skills/. We only
// replace separators; the rest is allowed because skills loaded from
// disk already had filesystem-friendly names.
func sanitise(name string) string {
	r := strings.NewReplacer(string(filepath.Separator), "_", "..", "_", "/", "_", "\\", "_")
	return r.Replace(name)
}

// copyTree copies the directory rooted at src to dst recursively,
// applying [portcullis.Redact] to every text file. Symlinks inside
// the tree are followed only when their resolved target stays within
// src (resolved); links pointing outside src are silently skipped to
// prevent a hostile or careless skill author from exfiltrating
// arbitrary host files (e.g. a symlink to ~/.aws/credentials) into
// the kit. Directory symlinks are also skipped, which matches the
// recursive skill loader's behaviour and avoids cycles.
//
// kitRoot is the kit's staging directory; it is forwarded to
// [copyFile] so [Redaction.Target] entries are recorded relative to
// it (matching [Entry.Target]).
func copyTree(kitRoot, src, dst string) ([]Redaction, error) {
	root := resolveAbs(src)
	if root == "" {
		return nil, fmt.Errorf("resolving source: %s", src)
	}

	var redactions []Redaction
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		out := filepath.Join(dst, rel)

		switch {
		case d.IsDir():
			return os.MkdirAll(out, 0o750)
		case d.Type()&fs.ModeSymlink != 0:
			realTarget, terr := filepath.EvalSymlinks(path)
			if terr != nil {
				return nil
			}
			if !isUnder(realTarget, root) {
				slog.Warn("kit: skipping symlink that escapes skill root",
					"link", path, "target", realTarget, "root", root)
				return nil
			}
			info, sterr := os.Stat(realTarget)
			if sterr != nil || info.IsDir() {
				return nil
			}
			red, copyErr := copyFile(kitRoot, realTarget, out)
			if copyErr != nil {
				return copyErr
			}
			if red != nil {
				redactions = append(redactions, *red)
			}
			return nil
		default:
			red, copyErr := copyFile(kitRoot, path, out)
			if copyErr != nil {
				return copyErr
			}
			if red != nil {
				redactions = append(redactions, *red)
			}
			return nil
		}
	})
	if err != nil {
		return nil, err
	}
	return redactions, nil
}

// copyFile copies a regular file from src to dst, redacting via
// [portcullis.Redact] when src is detected as text. Returns a non-nil
// [Redaction] when at least one secret was scrubbed. The redaction's
// Target is recorded relative to kitRoot so it stays consistent with
// [Entry.Target] (and so the on-disk manifest never leaks the kit's
// absolute host path).
//
// The destination inherits the source's permission bits (e.g. so an
// executable helper script next to a SKILL.md keeps its +x), masked
// to user-only since the kit is bind-mounted read-only into the
// sandbox anyway and there's no reason to expose it to other users
// on the host.
func copyFile(kitRoot, src, dst string) (*Redaction, error) {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return nil, err
	}

	out := data
	var redaction *Redaction
	if isText(data) {
		original := string(data)
		scrubbed := portcullis.Redact(original)
		if scrubbed != original {
			out = []byte(scrubbed)
			rel, relErr := filepath.Rel(kitRoot, dst)
			if relErr != nil {
				rel = dst // best effort; should never happen for staged files
			}
			redaction = &Redaction{Source: src, Target: rel}
		}
	}

	mode := srcInfo.Mode().Perm() & 0o700
	if mode == 0 {
		mode = 0o600
	}
	if err := os.WriteFile(dst, out, mode); err != nil {
		return nil, err
	}
	return redaction, nil
}

// isText reports whether b looks like a text file. The heuristic is
// deliberately strict: NUL byte → binary, invalid UTF-8 → binary. This
// errs on the side of not feeding [portcullis.Redact] non-text input
// (which would either corrupt binaries or produce nonsense output).
func isText(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	// Strip a UTF-8 BOM if present so it doesn't trip up the UTF-8 check.
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	if slices.Contains(b, 0) {
		return false
	}
	return utf8.Valid(b)
}

// writeManifest serialises the manifest as pretty-printed JSON. Source
// host paths are stripped (Entry.Source / Redaction.Source carry
// json:"-") so the sandbox-visible manifest cannot be used to map the
// host filesystem.
func writeManifest(dir string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, manifestFile), data, 0o600)
}

// PrintSummary writes a human-readable description of what was staged
// to w. The output groups files by skill (one block per skill, listing
// every file inside), then lists prompt files. Files whose host
// content was scrubbed are tagged "(redacted)". A trailing summary
// line counts skills, prompt files, and redactions.
//
// PrintSummary is silent when the kit shipped nothing — the caller is
// expected to print its own "no kit needed" hint in that case.
func (r *Result) PrintSummary(w io.Writer) {
	if r == nil {
		return
	}

	redacted := make(map[string]bool, len(r.Manifest.Redactions))
	for _, red := range r.Manifest.Redactions {
		redacted[red.Target] = true
	}

	skillFiles := r.skillFilesGrouped()
	promptEntries := append([]Entry(nil), r.Manifest.PromptFiles...)
	slices.SortFunc(promptEntries, func(a, b Entry) int { return cmp.Compare(a.Target, b.Target) })

	if len(skillFiles) == 0 && len(promptEntries) == 0 {
		return
	}

	fmt.Fprintf(w, "Preparing docker-agent kit at %s\n", styleHostPath(pathx.ShortenHome(r.HostDir)))

	if len(skillFiles) > 0 {
		fmt.Fprintf(w, "  %s\n", styleSection("skills:"))
		for _, group := range skillFiles {
			fmt.Fprintf(w, "    %s\n", displaySkillHeader(group.entry))
			for _, file := range group.files {
				rel := strings.TrimPrefix(file, group.entry.Target+string(filepath.Separator))
				if redacted[file] {
					fmt.Fprintf(w, "      %s %s\n", rel, styleRedacted("(redacted)"))
				} else {
					fmt.Fprintf(w, "      %s\n", rel)
				}
			}
		}
	}

	if len(promptEntries) > 0 {
		fmt.Fprintf(w, "  %s\n", styleSection("prompt files:"))
		for _, e := range promptEntries {
			name := promptFileName(e)
			notes := []string{"from " + pathx.ShortenHome(e.Source)}
			isRedacted := false
			if !e.IsStaged() {
				notes = append(notes, "workspace mount")
			} else if redacted[e.Target] {
				notes = append(notes, "redacted")
				isRedacted = true
			}
			joined := strings.Join(notes, ", ")
			paren := fmt.Sprintf("(%s)", joined)
			if isRedacted {
				paren = styleRedacted(paren)
			} else {
				paren = styleNote(paren)
			}
			fmt.Fprintf(w, "    %s %s\n", styleName(name), paren)
		}
	}

	fmt.Fprintf(w, "  %s %s\n", styleSection("summary:"), summaryCounts(len(skillFiles), len(promptEntries), len(r.Manifest.Redactions)))
}

// promptFileName returns the user-visible name for a prompt-file
// entry: the kit-relative basename for staged entries, or the host
// basename for entries reachable via the live workspace mount.
func promptFileName(e Entry) string {
	if e.IsStaged() {
		return filepath.Base(e.Target)
	}
	return filepath.Base(e.Source)
}

// skillGroup pairs a skill manifest entry with the kit-relative paths
// of every file staged under it (sorted).
type skillGroup struct {
	entry Entry
	files []string
}

// skillFilesGrouped walks the staged skills directory and returns one
// group per skill manifest entry, listing every file under the
// skill's target path. The walk happens after staging is complete, so
// it sees exactly what the sandbox will see.
func (r *Result) skillFilesGrouped() []skillGroup {
	entries := append([]Entry(nil), r.Manifest.Skills...)
	slices.SortFunc(entries, func(a, b Entry) int { return cmp.Compare(a.Target, b.Target) })

	groups := make([]skillGroup, 0, len(entries))
	for _, e := range entries {
		absRoot := filepath.Join(r.HostDir, e.Target)
		var files []string
		_ = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(r.HostDir, path)
			if relErr != nil {
				return nil
			}
			files = append(files, rel)
			return nil
		})
		sort.Strings(files)
		groups = append(groups, skillGroup{entry: e, files: files})
	}
	return groups
}

// displaySkillHeader renders the "name (from /host/path)" line shown
// at the top of each skill block.
func displaySkillHeader(e Entry) string {
	name := filepath.Base(e.Target)
	if e.Source == "" {
		return styleName(name)
	}
	return fmt.Sprintf("%s %s", styleName(name), styleNote(fmt.Sprintf("(from %s)", pathx.ShortenHome(e.Source))))
}

// summaryCounts formats the trailing line of PrintSummary.
func summaryCounts(skillCount, promptCount, redactionCount int) string {
	parts := []string{plural(skillCount, "skill")}
	parts = append(parts, plural(promptCount, "prompt file"))
	if redactionCount > 0 {
		parts = append(parts, styleRedacted(plural(redactionCount, "secret")+" redacted"))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, what string) string {
	if n == 1 {
		return styleCount("1") + " " + what
	}
	return fmt.Sprintf("%s %ss", styleCount(strconv.Itoa(n)), what)
}

// needsAutoInstall reports whether cfg has at least one toolset that
// the in-sandbox runtime would push through [toolinstall.EnsureCommand].
// Today only MCP and LSP toolsets do; shell toolsets run a user-
// supplied binary directly without auto-install.
//
// A toolset is a candidate when:
//   - its type is "mcp" or "lsp" (top-level cfg.MCPs entries are
//     also implicitly mcp);
//   - it has a Command (no command means the in-sandbox runtime has
//     nothing to look up on PATH);
//   - its Version is not "false" / "off" (the per-toolset opt-out
//     [toolinstall.EnsureCommand] honours).
//
// We deliberately don't try to predict whether the command will
// actually be missing inside the sandbox image. We only know that on
// the host. The sandbox image may already ship the binary, in which
// case auto-install never fires and the allowlist we open is unused —
// but unused allow rules are harmless, while a blocked auto-install
// is an opaque "403 Blocked by network policy" that's hard to
// diagnose. Erring on the side of allow keeps the failure mode loud
// instead of mysterious.
func needsAutoInstall(cfg *latestcfg.Config) bool {
	if cfg == nil {
		return false
	}
	return len(collectAutoInstallable(cfg)) > 0
}

// isAutoInstallable returns true if ts is the kind of toolset
// [toolinstall.EnsureCommand] would touch.
func isAutoInstallable(ts latestcfg.Toolset) bool {
	if ts.Command == "" {
		return false
	}
	switch ts.Type {
	case "mcp", "lsp":
	default:
		return false
	}
	switch strings.ToLower(strings.TrimSpace(ts.Version)) {
	case "false", "off":
		return false
	}
	return true
}

// ToolHostError records a single toolset whose package could not be
// resolved against the aqua registry while computing the sandbox
// allowlist. Callers can use it to surface a precise diagnostic to
// the user ("could not resolve gopls; falling back to the union of
// every install host") instead of silently degrading the network
// policy.
type ToolHostError struct {
	// Command is the toolset's Command field — the same string the
	// in-sandbox runtime would auto-install.
	Command string
	// Version is the toolset's Version field. Empty means "latest /
	// resolve by command".
	Version string
	// Err is the underlying registry / lookup error.
	Err error
}

func (e ToolHostError) Error() string {
	if e.Version == "" {
		return fmt.Sprintf("resolving install hosts for %q: %v", e.Command, e.Err)
	}
	return fmt.Sprintf("resolving install hosts for %q@%q: %v", e.Command, e.Version, e.Err)
}

func (e ToolHostError) Unwrap() error { return e.Err }

// resolveToolInstallHosts walks every auto-installable toolset in cfg
// and returns the merged set of hosts the in-sandbox auto-installer
// must reach to install them, plus any per-toolset resolution errors.
//
// On any resolution error, the conservative fallback union (every
// install host known to the toolinstall package) is folded in so
// that the run still succeeds — trading minimisation for
// availability. Callers that want strict failure behaviour can
// inspect the returned error slice and refuse to launch.
//
// Returns (nil, nil) when cfg has no auto-installable toolset —
// callers use len(hosts)==0 to mean "don't open any holes".
func resolveToolInstallHosts(ctx context.Context, cfg *latestcfg.Config) ([]string, []ToolHostError) {
	if cfg == nil {
		return nil, nil
	}

	toolsets := collectAutoInstallable(cfg)
	if len(toolsets) == 0 {
		return nil, nil
	}

	seen := make(map[string]bool)
	var hosts []string
	var errs []ToolHostError

	for _, ts := range toolsets {
		resolved, err := toolinstall.ResolveHosts(ctx, ts.Command, ts.Version)
		if err != nil {
			errs = append(errs, ToolHostError{Command: ts.Command, Version: ts.Version, Err: err})
			continue
		}
		for _, h := range resolved {
			if !seen[h] {
				seen[h] = true
				hosts = append(hosts, h)
			}
		}
	}

	if len(errs) > 0 {
		// Fail-open: fold in the fallback union so the run can still
		// install. Callers that prefer fail-closed inspect errs.
		for _, h := range toolinstall.FallbackHosts() {
			if !seen[h] {
				seen[h] = true
				hosts = append(hosts, h)
			}
		}
	}

	sort.Strings(hosts)
	return hosts, errs
}

// collectAutoInstallable returns every toolset in cfg whose Command
// the in-sandbox runtime would push through
// [toolinstall.EnsureCommand]. Order is deterministic (top-level
// MCPs by map-key, then per-agent toolsets in declaration order)
// so the resolution loop's slog output is reproducible.
func collectAutoInstallable(cfg *latestcfg.Config) []latestcfg.Toolset {
	var out []latestcfg.Toolset

	names := make([]string, 0, len(cfg.MCPs))
	for name := range cfg.MCPs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ts := cfg.MCPs[name].Toolset
		if isAutoInstallable(ts) {
			out = append(out, ts)
		}
	}

	for _, agent := range cfg.Agents {
		for _, ts := range agent.Toolsets {
			if isAutoInstallable(ts) {
				out = append(out, ts)
			}
		}
	}

	return out
}
