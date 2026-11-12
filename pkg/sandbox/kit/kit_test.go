package kit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/docker/portcullis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	latestcfg "github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/internal/portcullistest"
	"github.com/docker/docker-agent/pkg/promptfiles"
	"github.com/docker/docker-agent/pkg/skills"
)

// fakeGitHubToken is used only as input to the redactor, never as an
// actual credential.
func fakeGitHubToken() string {
	return portcullistest.FakeGitHubPAT("1234567890abcdefghijklmnopqrst")
}

// isolateEnv prevents a developer-exported DOCKER_AGENT_KIT_DIR from
// flipping the in-process resolvers (notably skills.Load) into
// kit-only mode mid-test, which would mask host paths the test
// expects to see.
func isolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv(skills.KitDirEnv, "")
}

func TestBuild_StagesSkillsAndRedacts(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()

	// Stage one local skill on the host with a secret embedded.
	skillDir := filepath.Join(hostHome, ".agents", "skills", "secret-keeper")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	token := fakeGitHubToken()
	skillBody := "---\nname: secret-keeper\ndescription: ships with a secret\n---\n\ntoken=" + token + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillBody), 0o644))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	// Run from an empty cwd so cwd-walking finds nothing extra.
	t.Chdir(t.TempDir())

	cacheDir := t.TempDir()
	res, err := Build(t.Context(), Options{
		AgentRef: "default",
		HostHome: hostHome,
		HostCwd:  t.TempDir(),
		CacheDir: cacheDir,
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	// The kit is rooted under cacheDir.
	rel, err := filepath.Rel(cacheDir, res.HostDir)
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(rel, ".."),
		"kit dir %s should be under cache dir %s", res.HostDir, cacheDir)

	// The skill made it into the kit.
	staged := filepath.Join(res.HostDir, skills.KitSkillsSubdir, "secret-keeper", "SKILL.md")
	data, err := os.ReadFile(staged)
	require.NoError(t, err)
	assert.NotContains(t, string(data), token, "host secret must not survive in kit")
	assert.Contains(t, string(data), portcullis.Marker, "redaction marker must be present")

	// The manifest records the skill and the redaction.
	require.Len(t, res.Manifest.Skills, 1)
	assert.Equal(t, skillDir, res.Manifest.Skills[0].Source)
	assert.Equal(t, filepath.Join(skills.KitSkillsSubdir, "secret-keeper"), res.Manifest.Skills[0].Target)
	require.Len(t, res.Manifest.Redactions, 1)
	assert.Equal(t, filepath.Join(skillDir, "SKILL.md"), res.Manifest.Redactions[0].Source,
		"redaction must record the host source path for caller-side debugging")
	assert.Equal(t, filepath.Join(skills.KitSkillsSubdir, "secret-keeper", "SKILL.md"), res.Manifest.Redactions[0].Target,
		"redaction Target must be kit-relative so it lines up with Entry.Target and never leaks the kit's absolute host path")

	// Manifest is also written to disk for human inspection.
	_, err = os.Stat(filepath.Join(res.HostDir, "manifest.json"))
	assert.NoError(t, err)
}

func TestBuild_RebuildsCleanDir(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(t.TempDir())

	// A local agent YAML gives us a stable, offline AgentRef. A bare
	// string like "stable-ref" would resolve as an OCI reference and
	// block on a real registry pull — seconds of network latency for a
	// test that only cares that the same ref maps to the same kit dir.
	workspace := t.TempDir()
	yamlPath := filepath.Join(workspace, "agent.yaml")
	require.NoError(t, os.WriteFile(yamlPath, []byte(`agents:
  root:
    model: openai/gpt-5
    description: tester
    instruction: hello
models:
  openai/gpt-5:
    provider: openai
    model: gpt-5
`), 0o600))

	cacheDir := t.TempDir()
	res1, err := Build(t.Context(), Options{
		AgentRef: yamlPath,
		HostHome: hostHome,
		HostCwd:  t.TempDir(),
		CacheDir: cacheDir,
	})
	require.NoError(t, err)

	// Drop a stale file inside the kit dir; it must be wiped on rebuild.
	stale := filepath.Join(res1.HostDir, "stale.txt")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))

	res2, err := Build(t.Context(), Options{
		AgentRef: yamlPath,
		HostHome: hostHome,
		HostCwd:  t.TempDir(),
		CacheDir: cacheDir,
	})
	require.NoError(t, err)
	assert.Equal(t, res1.HostDir, res2.HostDir, "stable AgentRef should yield stable kit dir")

	_, err = os.Stat(stale)
	assert.True(t, os.IsNotExist(err), "stale file must be removed when kit is rebuilt")
}

func TestBuild_PromptFilesCollectedAndScopedOutsideWorkspace(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()
	workspace := t.TempDir()

	// AGENTS.md at $HOME — must be staged into the kit.
	homeAgents := filepath.Join(hostHome, "AGENTS.md")
	require.NoError(t, os.WriteFile(homeAgents, []byte("# host AGENTS.md\n"), 0o600))

	// AGENTS.md inside the workspace — must NOT be staged because the
	// live mount surfaces it inside the sandbox.
	workspaceAgents := filepath.Join(workspace, "AGENTS.md")
	require.NoError(t, os.WriteFile(workspaceAgents, []byte("# workspace AGENTS.md\n"), 0o600))

	// Build a tiny agent YAML that references AGENTS.md via add_prompt_files.
	agentYAML := []byte(`#!/usr/bin/env docker-agent
agents:
  root:
    model: openai/gpt-5
    description: tester
    instruction: hello
    add_prompt_files: ["AGENTS.md"]
models:
  openai/gpt-5:
    provider: openai
    model: gpt-5
`)
	yamlPath := filepath.Join(workspace, "agent.yaml")
	require.NoError(t, os.WriteFile(yamlPath, agentYAML, 0o600))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(workspace)

	cacheDir := t.TempDir()
	res, err := Build(t.Context(), Options{
		AgentRef:  yamlPath,
		HostHome:  hostHome,
		HostCwd:   workspace,
		Workspace: workspace,
		CacheDir:  cacheDir,
	})
	require.NoError(t, err)

	// Both candidates are recorded: the workspace one is reachable via
	// the live mount (Target empty) and the $HOME one is staged into
	// the kit. This mirrors the runtime promptfiles.Paths semantics
	// which would also return both inside the sandbox.
	require.Len(t, res.Manifest.PromptFiles, 2)

	var staged, mounted Entry
	for _, e := range res.Manifest.PromptFiles {
		if e.IsStaged() {
			staged = e
		} else {
			mounted = e
		}
	}
	assert.Equal(t, homeAgents, staged.Source, "the $HOME copy must be the staged one")
	assert.Equal(t, workspaceAgents, mounted.Source, "the workspace copy must be recorded as workspace-mounted")

	stagedPath := filepath.Join(res.HostDir, promptfiles.KitSubdir, "AGENTS.md")
	data, err := os.ReadFile(stagedPath)
	require.NoError(t, err)
	assert.Equal(t, "# host AGENTS.md\n", string(data))
}

func TestBuild_PromptFileInWorkspaceIsRecordedButNotStaged(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()
	workspace := t.TempDir()

	workspaceAgents := filepath.Join(workspace, "AGENTS.md")
	require.NoError(t, os.WriteFile(workspaceAgents, []byte("# workspace AGENTS.md\n"), 0o600))

	agentYAML := []byte(`agents:
  root:
    model: openai/gpt-5
    description: tester
    instruction: hello
    add_prompt_files: ["AGENTS.md"]
models:
  openai/gpt-5:
    provider: openai
    model: gpt-5
`)
	yamlPath := filepath.Join(workspace, "agent.yaml")
	require.NoError(t, os.WriteFile(yamlPath, agentYAML, 0o600))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(workspace)

	res, err := Build(t.Context(), Options{
		AgentRef:  yamlPath,
		HostHome:  hostHome,
		HostCwd:   workspace,
		Workspace: workspace,
		CacheDir:  t.TempDir(),
	})
	require.NoError(t, err)

	// The user's case: AGENTS.md exists only inside the workspace.
	// We must record it (so the printed kit summary lists it) but we
	// must not copy it into the kit — the live workspace mount makes
	// it visible to the agent already.
	require.Len(t, res.Manifest.PromptFiles, 1)
	entry := res.Manifest.PromptFiles[0]
	assert.Equal(t, workspaceAgents, entry.Source)
	assert.False(t, entry.IsStaged(), "workspace files must not be staged into the kit")

	assert.NoFileExists(t, filepath.Join(res.HostDir, promptfiles.KitSubdir, "AGENTS.md"),
		"workspace prompt file must not have a redacted copy in the kit")
}

func TestBuild_PromptFileInWorkspaceParentIsStagedIntoKit(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()

	// User-realistic layout: the workspace is a project under a
	// parent directory that holds a shared AGENTS.md (e.g. a
	// monorepo / dotfiles arrangement). The cwd-walk on the host
	// finds the parent's AGENTS.md, but inside the sandbox the
	// parent directory exists only as a synthesised mount point
	// holding the workspace as its lone child — the host file at
	// that path is invisible. The kit must therefore stage a copy.
	parent := t.TempDir()
	parentAgents := filepath.Join(parent, "AGENTS.md")
	require.NoError(t, os.WriteFile(parentAgents, []byte("# parent AGENTS.md\n"), 0o600))

	workspace := filepath.Join(parent, "project")
	require.NoError(t, os.Mkdir(workspace, 0o755))

	agentYAML := []byte(`agents:
  root:
    model: openai/gpt-5
    description: tester
    instruction: hello
    add_prompt_files: ["AGENTS.md"]
models:
  openai/gpt-5:
    provider: openai
    model: gpt-5
`)
	yamlPath := filepath.Join(workspace, "agent.yaml")
	require.NoError(t, os.WriteFile(yamlPath, agentYAML, 0o600))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(workspace)

	res, err := Build(t.Context(), Options{
		AgentRef:  yamlPath,
		HostHome:  hostHome,
		HostCwd:   workspace,
		Workspace: workspace,
		CacheDir:  t.TempDir(),
	})
	require.NoError(t, err)

	require.Len(t, res.Manifest.PromptFiles, 1)
	entry := res.Manifest.PromptFiles[0]
	assert.Equal(t, parentAgents, entry.Source)
	assert.True(t, entry.IsStaged(),
		"a prompt file in the workspace's parent must be staged: the parent dir is not bind-mounted, only the workspace is")

	data, err := os.ReadFile(filepath.Join(res.HostDir, promptfiles.KitSubdir, "AGENTS.md"))
	require.NoError(t, err)
	assert.Equal(t, "# parent AGENTS.md\n", string(data))
}

func TestBuild_NoAgentRefLeavesPromptFilesEmpty(t *testing.T) {
	isolateEnv(t)
	// Without an AgentRef there is no team config to walk; the kit
	// still builds (so the host-only skills lookup runs) but no
	// prompt files are staged.
	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(t.TempDir())

	cacheDir := t.TempDir()
	res, err := Build(t.Context(), Options{
		AgentRef: "", // unresolved on purpose
		HostHome: hostHome,
		HostCwd:  t.TempDir(),
		CacheDir: cacheDir,
	})
	require.NoError(t, err)
	assert.Empty(t, res.Manifest.PromptFiles)
}

func TestIsUnder(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	inside := filepath.Join(base, "sub", "file")
	outside := t.TempDir()

	assert.True(t, isUnder(inside, base))
	assert.False(t, isUnder(outside, base))
	assert.False(t, isUnder(inside, ""), "empty base means no scoping")
}

func TestIsText(t *testing.T) {
	t.Parallel()

	assert.True(t, isText([]byte("hello world")))
	assert.True(t, isText([]byte{}))
	assert.True(t, isText([]byte("\xef\xbb\xbfhello"))) // UTF-8 BOM
	assert.False(t, isText([]byte{0x00, 0x01, 0x02}))   // NUL byte
	assert.False(t, isText([]byte{0xff, 0xfe, 0xfd}))   // invalid UTF-8
}

func TestSanitise(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "abc", sanitise("abc"))
	assert.Equal(t, "a_b", sanitise("a/b"))
	assert.Equal(t, "a_b", sanitise("a..b"))
}

func TestBuild_DropsSymlinksEscapingSkillRoot(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()

	// A sensitive host file the user did NOT intend to ship.
	secretsDir := t.TempDir()
	secretFile := filepath.Join(secretsDir, "credentials")
	require.NoError(t, os.WriteFile(secretFile, []byte("super-secret\n"), 0o600))

	// A skill whose tree contains a symlink pointing at the secret.
	// Without the escape check this content would land in the kit
	// (and thus inside the sandbox).
	skillDir := filepath.Join(hostHome, ".agents", "skills", "sneaky")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: sneaky\ndescription: tries to exfiltrate\n---\n"), 0o644))
	require.NoError(t, os.Symlink(secretFile, filepath.Join(skillDir, "creds")))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(t.TempDir())

	res, err := Build(t.Context(), Options{
		AgentRef: "default",
		HostHome: hostHome,
		HostCwd:  t.TempDir(),
		CacheDir: t.TempDir(),
	})
	require.NoError(t, err)

	// SKILL.md is staged.
	stagedSkill := filepath.Join(res.HostDir, skills.KitSkillsSubdir, "sneaky", "SKILL.md")
	_, err = os.Stat(stagedSkill)
	require.NoError(t, err)

	// The symlinked secret is NOT.
	stagedCreds := filepath.Join(res.HostDir, skills.KitSkillsSubdir, "sneaky", "creds")
	_, err = os.Stat(stagedCreds)
	assert.True(t, os.IsNotExist(err), "escape symlink must not be followed into the kit")
}

func TestBuild_AllowsSymlinksWithinSkillRoot(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()

	skillDir := filepath.Join(hostHome, ".agents", "skills", "linked")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: linked\ndescription: legit\n---\n"), 0o644))
	helper := filepath.Join(skillDir, "helper.txt")
	require.NoError(t, os.WriteFile(helper, []byte("helper\n"), 0o644))
	require.NoError(t, os.Symlink(helper, filepath.Join(skillDir, "alias.txt")))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(t.TempDir())

	res, err := Build(t.Context(), Options{
		AgentRef: "default",
		HostHome: hostHome,
		HostCwd:  t.TempDir(),
		CacheDir: t.TempDir(),
	})
	require.NoError(t, err)

	staged := filepath.Join(res.HostDir, skills.KitSkillsSubdir, "linked", "alias.txt")
	data, err := os.ReadFile(staged)
	require.NoError(t, err)
	assert.Equal(t, "helper\n", string(data),
		"symlink whose target lives inside the skill root must be inlined")
}

func TestBuild_PreservesExecutableBit(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()

	skillDir := filepath.Join(hostHome, ".agents", "skills", "with-script")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: with-script\ndescription: includes a helper script\n---\n"), 0o644))
	script := filepath.Join(skillDir, "run.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o700))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(t.TempDir())

	res, err := Build(t.Context(), Options{
		AgentRef: "default",
		HostHome: hostHome,
		HostCwd:  t.TempDir(),
		CacheDir: t.TempDir(),
	})
	require.NoError(t, err)

	staged := filepath.Join(res.HostDir, skills.KitSkillsSubdir, "with-script", "run.sh")
	info, err := os.Stat(staged)
	require.NoError(t, err)
	if runtime.GOOS == "windows" {
		assert.FileExists(t, staged)
		return
	}
	assert.NotZero(t, info.Mode().Perm()&0o100,
		"staged script %s must keep its executable bit (got %v)", staged, info.Mode())
}

func TestBuild_OnDiskManifestOmitsHostPaths(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()

	skillDir := filepath.Join(hostHome, ".agents", "skills", "plain")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: plain\ndescription: plain\n---\n"), 0o644))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(t.TempDir())

	res, err := Build(t.Context(), Options{
		AgentRef: "default",
		HostHome: hostHome,
		HostCwd:  t.TempDir(),
		CacheDir: t.TempDir(),
	})
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(res.HostDir, manifestFile))
	require.NoError(t, err)

	// The on-disk manifest is bind-mounted into the sandbox; it must
	// not reveal the host filesystem layout. The in-memory Manifest
	// keeps the source paths for caller-side debugging.
	assert.NotContains(t, string(raw), hostHome,
		"on-disk manifest must not leak host paths")

	var onDisk Manifest
	require.NoError(t, json.Unmarshal(raw, &onDisk))
	for _, e := range onDisk.Skills {
		assert.Empty(t, e.Source, "manifest entries must not include host source paths")
	}

	require.NotEmpty(t, res.Manifest.Skills)
	assert.NotEmpty(t, res.Manifest.Skills[0].Source,
		"in-memory manifest still carries the host source for callers")
}

func TestBuild_ConcurrentRunsForSameAgentAreSafe(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()
	skillDir := filepath.Join(hostHome, ".agents", "skills", "shared")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: shared\ndescription: shared skill\n---\n"), 0o644))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(t.TempDir())

	cacheDir := t.TempDir()
	optsTemplate := Options{
		AgentRef: "default", // builtin enables skills, so the kit stages them
		HostHome: hostHome,
		HostCwd:  t.TempDir(),
		CacheDir: cacheDir,
	}

	const N = 6
	var wg sync.WaitGroup
	dirs := make([]string, N)
	errs := make([]error, N)
	for i := range N {
		wg.Go(func() {
			res, err := Build(t.Context(), optsTemplate)
			errs[i] = err
			if res != nil {
				dirs[i] = res.HostDir
			}
		})
	}
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "concurrent build %d", i)
	}
	// Every winner ends up with the same final dir.
	for i := 1; i < N; i++ {
		assert.Equal(t, dirs[0], dirs[i])
	}
	// And the final dir is fully populated.
	_, err := os.Stat(filepath.Join(dirs[0], skills.KitSkillsSubdir, "shared", "SKILL.md"))
	assert.NoError(t, err)
}

func TestHashKey_FileRefsCanonicalised(t *testing.T) {
	dir := t.TempDir()
	agent := filepath.Join(dir, "agent.yaml")
	require.NoError(t, os.WriteFile(agent, []byte("x"), 0o600))

	t.Chdir(dir)

	kAbs := hashKey(agent)
	kRel := hashKey("./agent.yaml")
	kBare := hashKey("agent.yaml")
	assert.Equal(t, kAbs, kRel,
		"./agent.yaml and the absolute path must share a kit")
	assert.Equal(t, kAbs, kBare,
		"agent.yaml and the absolute path must share a kit")
}

func TestHashKey_EmptyDoesNotCollideWithDefault(t *testing.T) {
	t.Parallel()

	// A literal ref of "default" used to share a hash with the empty
	// fallback because the latter was rewritten to "default" before
	// hashing. They now sit in different namespaces.
	assert.NotEqual(t, hashKey(""), hashKey("default"))
}

func TestPrintSummary(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()

	// Two skills, one of which contains a secret and a helper script,
	// plus a $HOME-rooted prompt file with a secret. This exercises
	// every branch of PrintSummary: header, multi-file skill listing,
	// per-file redaction marker, prompt-file section, summary line
	// with secret count, and ~ collapsing of host paths.
	withSecret := filepath.Join(hostHome, ".agents", "skills", "with-secret")
	require.NoError(t, os.MkdirAll(withSecret, 0o755))
	token := fakeGitHubToken()
	require.NoError(t, os.WriteFile(filepath.Join(withSecret, "SKILL.md"),
		[]byte("---\nname: with-secret\ndescription: leaks\n---\n\ntoken="+token+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(withSecret, "helper.sh"),
		[]byte("#!/bin/sh\necho hi\n"), 0o755))

	plain := filepath.Join(hostHome, ".agents", "skills", "plain")
	require.NoError(t, os.MkdirAll(plain, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(plain, "SKILL.md"),
		[]byte("---\nname: plain\ndescription: plain\n---\n"), 0o644))

	agentsMD := filepath.Join(hostHome, "AGENTS.md")
	require.NoError(t, os.WriteFile(agentsMD, []byte("token="+token+"\n"), 0o600))

	workspace := t.TempDir()
	agentYAML := []byte(`#!/usr/bin/env docker-agent
agents:
  root:
    model: openai/gpt-5
    description: tester
    instruction: hello
    skills: true
    add_prompt_files: ["AGENTS.md"]
models:
  openai/gpt-5:
    provider: openai
    model: gpt-5
`)
	yamlPath := filepath.Join(workspace, "agent.yaml")
	require.NoError(t, os.WriteFile(yamlPath, agentYAML, 0o600))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(workspace)

	res, err := Build(t.Context(), Options{
		AgentRef:  yamlPath,
		HostHome:  hostHome,
		HostCwd:   workspace,
		Workspace: workspace,
		CacheDir:  t.TempDir(),
	})
	require.NoError(t, err)

	var buf strings.Builder
	res.PrintSummary(&buf)
	out := buf.String()

	// Header + skills section.
	assert.Contains(t, out, "Preparing docker-agent kit at "+res.HostDir)
	assert.Contains(t, out, "skills:")
	assert.Contains(t, out, "plain (from "+filepath.Join("~", ".agents", "skills", "plain")+")",
		"$HOME prefix should collapse to ~ in printed paths")
	assert.Contains(t, out, "with-secret (from "+filepath.Join("~", ".agents", "skills", "with-secret")+")")

	// Every staged file appears, and only the redacted one carries the marker.
	assert.Contains(t, out, "SKILL.md (redacted)", "redacted skill file must be tagged")
	assert.Contains(t, out, "helper.sh")
	assert.NotContains(t, out, "helper.sh (redacted)",
		"non-text / non-redacted files must not carry the marker")

	// Prompt files section.
	assert.Contains(t, out, "prompt files:")
	assert.Contains(t, out, "AGENTS.md (from "+filepath.Join("~", "AGENTS.md")+", redacted)")

	// Summary line.
	assert.Contains(t, out, "summary: 2 skills, 1 prompt file, 2 secrets redacted")

	// And no host secret leaks into the printed output.
	assert.NotContains(t, out, token)
}

func TestPrintSummary_WorkspacePromptFile(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()
	workspace := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(workspace, "AGENTS.md"),
		[]byte("# project AGENTS.md\n"), 0o600))

	agentYAML := []byte(`agents:
  root:
    model: openai/gpt-5
    description: tester
    instruction: hello
    add_prompt_files: ["AGENTS.md"]
models:
  openai/gpt-5:
    provider: openai
    model: gpt-5
`)
	yamlPath := filepath.Join(workspace, "agent.yaml")
	require.NoError(t, os.WriteFile(yamlPath, agentYAML, 0o600))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(workspace)

	res, err := Build(t.Context(), Options{
		AgentRef:  yamlPath,
		HostHome:  hostHome,
		HostCwd:   workspace,
		Workspace: workspace,
		CacheDir:  t.TempDir(),
	})
	require.NoError(t, err)

	var buf strings.Builder
	res.PrintSummary(&buf)
	out := buf.String()

	// The user must see that AGENTS.md is part of the agent's input,
	// even when the host did not need to stage it (the workspace mount
	// surfaces it directly inside the sandbox).
	assert.Contains(t, out, "prompt files:")
	assert.Contains(t, out, "AGENTS.md (from "+filepath.Join(workspace, "AGENTS.md")+", workspace mount)")
	assert.NotContains(t, out, "redacted",
		"workspace prompt files are not redacted because they aren't copied into the kit")
}

func TestPrintSummary_Empty(t *testing.T) {
	t.Parallel()

	// A kit that ships nothing must print nothing — the caller is
	// expected to handle the "no kit needed" case.
	res := &Result{HostDir: "/tmp/empty"}

	var buf strings.Builder
	res.PrintSummary(&buf)
	assert.Empty(t, buf.String())
}

func TestPrintSummary_NilReceiver(t *testing.T) {
	t.Parallel()

	// Defensive: callers may invoke PrintSummary on a nil result if the
	// kit build failed; it must not panic.
	var res *Result
	var buf strings.Builder
	assert.NotPanics(t, func() { res.PrintSummary(&buf) })
	assert.Empty(t, buf.String())
}

// stageSkillsTestSetup creates two local skills ("alpha" and "beta")
// under hostHome and returns the agent YAML path the test will load.
// The agent's `skills:` value is configurable so each scenario can
// exercise a different filter.
func stageSkillsTestSetup(t *testing.T, skillsYAML string) (hostHome, workspace, yamlPath string) {
	t.Helper()
	isolateEnv(t)
	hostHome = t.TempDir()
	workspace = t.TempDir()

	for _, name := range []string{"alpha", "beta"} {
		dir := filepath.Join(hostHome, ".agents", "skills", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"),
			[]byte("---\nname: "+name+"\ndescription: "+name+"\n---\n"), 0o644))
	}

	agentYAML := []byte(`agents:
  root:
    model: openai/gpt-5
    description: tester
    instruction: hello
    skills: ` + skillsYAML + `
models:
  openai/gpt-5:
    provider: openai
    model: gpt-5
`)
	yamlPath = filepath.Join(workspace, "agent.yaml")
	require.NoError(t, os.WriteFile(yamlPath, agentYAML, 0o600))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(workspace)
	return hostHome, workspace, yamlPath
}

func stagedSkillNames(res *Result) []string {
	names := make([]string, 0, len(res.Manifest.Skills))
	for _, e := range res.Manifest.Skills {
		names = append(names, filepath.Base(e.Target))
	}
	sort.Strings(names)
	return names
}

func TestBuild_SkillsDisabledShipsNothing(t *testing.T) {
	hostHome, workspace, yamlPath := stageSkillsTestSetup(t, "false")

	res, err := Build(t.Context(), Options{
		AgentRef:  yamlPath,
		HostHome:  hostHome,
		HostCwd:   workspace,
		Workspace: workspace,
		CacheDir:  t.TempDir(),
	})
	require.NoError(t, err)
	assert.Empty(t, res.Manifest.Skills,
		"agent with skills: false must not stage any local skill into the kit")
}

func TestBuild_SkillsTrueShipsAll(t *testing.T) {
	hostHome, workspace, yamlPath := stageSkillsTestSetup(t, "true")

	res, err := Build(t.Context(), Options{
		AgentRef:  yamlPath,
		HostHome:  hostHome,
		HostCwd:   workspace,
		Workspace: workspace,
		CacheDir:  t.TempDir(),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "beta"}, stagedSkillNames(res))
}

func TestBuild_SkillsIncludeFilters(t *testing.T) {
	hostHome, workspace, yamlPath := stageSkillsTestSetup(t, `["alpha"]`)

	res, err := Build(t.Context(), Options{
		AgentRef:  yamlPath,
		HostHome:  hostHome,
		HostCwd:   workspace,
		Workspace: workspace,
		CacheDir:  t.TempDir(),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha"}, stagedSkillNames(res),
		"only the named skill must be staged when skills is filtered")
}

func TestBuild_SkillsIncludeUnionAcrossAgents(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()
	workspace := t.TempDir()

	for _, name := range []string{"alpha", "beta", "gamma"} {
		dir := filepath.Join(hostHome, ".agents", "skills", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"),
			[]byte("---\nname: "+name+"\ndescription: x\n---\n"), 0o644))
	}

	// Two agents with disjoint filters and a third that disables
	// skills entirely. The kit must stage the union of the first two
	// (alpha + beta) and ignore the third.
	agentYAML := []byte(`agents:
  one:
    model: openai/gpt-5
    description: one
    instruction: hi
    skills: ["alpha"]
  two:
    model: openai/gpt-5
    description: two
    instruction: hi
    skills: ["beta"]
  three:
    model: openai/gpt-5
    description: three
    instruction: hi
    skills: false
models:
  openai/gpt-5:
    provider: openai
    model: gpt-5
`)
	yamlPath := filepath.Join(workspace, "agent.yaml")
	require.NoError(t, os.WriteFile(yamlPath, agentYAML, 0o600))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(workspace)

	res, err := Build(t.Context(), Options{
		AgentRef:  yamlPath,
		HostHome:  hostHome,
		HostCwd:   workspace,
		Workspace: workspace,
		CacheDir:  t.TempDir(),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "beta"}, stagedSkillNames(res))
}

func TestBuild_SkillsUnfilteredAgentWidens(t *testing.T) {
	isolateEnv(t)
	hostHome := t.TempDir()
	workspace := t.TempDir()

	for _, name := range []string{"alpha", "beta"} {
		dir := filepath.Join(hostHome, ".agents", "skills", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"),
			[]byte("---\nname: "+name+"\ndescription: x\n---\n"), 0o644))
	}

	// One agent restricts skills to "alpha"; another has no filter.
	// The unfiltered agent widens the kit to every local skill.
	agentYAML := []byte(`agents:
  one:
    model: openai/gpt-5
    description: one
    instruction: hi
    skills: ["alpha"]
  two:
    model: openai/gpt-5
    description: two
    instruction: hi
    skills: true
models:
  openai/gpt-5:
    provider: openai
    model: gpt-5
`)
	yamlPath := filepath.Join(workspace, "agent.yaml")
	require.NoError(t, os.WriteFile(yamlPath, agentYAML, 0o600))

	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	t.Chdir(workspace)

	res, err := Build(t.Context(), Options{
		AgentRef:  yamlPath,
		HostHome:  hostHome,
		HostCwd:   workspace,
		Workspace: workspace,
		CacheDir:  t.TempDir(),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "beta"}, stagedSkillNames(res))
}

func TestCollectAutoInstallable(t *testing.T) {
	t.Parallel()

	mcpInstallable := latestcfg.Toolset{Type: "mcp", Command: "fzf"}
	lspInstallable := latestcfg.Toolset{Type: "lsp", Command: "gopls"}
	disabledByVersion := latestcfg.Toolset{Type: "mcp", Command: "fzf", Version: "off"}
	noCommand := latestcfg.Toolset{Type: "mcp"}
	wrongType := latestcfg.Toolset{Type: "shell", Command: "ls"}

	cases := []struct {
		name string
		cfg  *latestcfg.Config
		want []latestcfg.Toolset
	}{
		{"empty cfg", &latestcfg.Config{}, nil},
		{
			name: "agent has installable lsp",
			cfg: &latestcfg.Config{
				Agents: latestcfg.Agents{{Name: "root", Toolsets: []latestcfg.Toolset{lspInstallable}}},
			},
			want: []latestcfg.Toolset{lspInstallable},
		},
		{
			name: "top-level mcps entry",
			cfg: &latestcfg.Config{
				MCPs: map[string]latestcfg.MCPToolset{
					"x": {Toolset: mcpInstallable},
				},
			},
			want: []latestcfg.Toolset{mcpInstallable},
		},
		{
			name: "auto-install disabled per toolset",
			cfg: &latestcfg.Config{
				Agents: latestcfg.Agents{{Name: "root", Toolsets: []latestcfg.Toolset{disabledByVersion}}},
			},
		},
		{
			name: "no command means nothing to look up",
			cfg: &latestcfg.Config{
				Agents: latestcfg.Agents{{Name: "root", Toolsets: []latestcfg.Toolset{noCommand}}},
			},
		},
		{
			name: "shell toolsets do not auto-install",
			cfg: &latestcfg.Config{
				Agents: latestcfg.Agents{{Name: "root", Toolsets: []latestcfg.Toolset{wrongType}}},
			},
		},
		{
			name: "case-insensitive disable",
			cfg: &latestcfg.Config{
				MCPs: map[string]latestcfg.MCPToolset{
					"x": {Toolset: latestcfg.Toolset{Type: "mcp", Command: "fzf", Version: "FALSE"}},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, collectAutoInstallable(tc.cfg))
		})
	}
}

func TestResolveToolInstallHosts_NilConfig(t *testing.T) {
	t.Parallel()

	hosts, errs := resolveToolInstallHosts(t.Context(), nil)
	assert.Nil(t, hosts)
	assert.Nil(t, errs)
}
