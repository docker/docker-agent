package root

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestResumeReporterOnlyUsesStoredSession(t *testing.T) {
	var output bytes.Buffer
	r := &resumeReporter{ctx: t.Context(), token: "dummy-token-replace-me", command: []string{"/bin/docker-agent", "run", "/tmp/agent.yaml"}, out: &output}
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithWorkingDir("/tmp"))
	application := app.New(t.Context(), &recallRuntime{store: store}, sess)
	r.update(application, false)
	assert.Empty(t, output.String())
	require.NoError(t, store.AddSession(t.Context(), sess))
	r.update(application, false)
	assert.Contains(t, output.String(), "\x1b]7577;")
	first := output.String()
	r.update(application, false)
	assert.Equal(t, first, output.String())
	r.resumed()
	r.update(application, false)
	assert.Equal(t, 2, strings.Count(output.String(), "\x1b]7577;"))
	r.update(application, true)
	assert.Contains(t, output.String(), "clear=1")
	r.update(application, false)
	assert.Greater(t, len(output.String()), len(first))
	r.clear()
	assert.Empty(t, r.last)
}

func TestResumeReporterFollowsSessionAndPersistence(t *testing.T) {
	var output bytes.Buffer
	r := &resumeReporter{ctx: t.Context(), token: "dummy-token-replace-me", command: []string{"/bin/docker-agent"}, out: &output}
	store := session.NewInMemorySessionStore()
	first := session.New(session.WithWorkingDir(t.TempDir()))
	second := session.New(session.WithWorkingDir(t.TempDir()))
	require.NoError(t, store.AddSession(t.Context(), first))
	require.NoError(t, store.AddSession(t.Context(), second))
	a := app.New(t.Context(), &recallRuntime{store: store}, first)
	r.update(a, false)
	assert.Contains(t, r.last, first.ID)
	a = app.New(t.Context(), &recallRuntime{store: store}, second)
	r.update(a, false)
	assert.Contains(t, r.last, second.ID)
	require.NoError(t, store.DeleteSession(t.Context(), second.ID))
	r.lastCheck = time.Time{}
	r.update(a, false)
	assert.Empty(t, r.last)
	assert.Contains(t, output.String(), "clear=1")
}

func TestRequireExistingSessionFailsClosed(t *testing.T) {
	f := &runExecFlags{requireExistingSession: true}
	_, _, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "missing"}, session.NewInMemorySessionStore())
	require.ErrorIs(t, err, session.ErrNotFound)
}

func TestResumeTokenRemovedBeforeStartup(t *testing.T) {
	if os.Getenv("RESUME_CHILD_TEST") == "1" {
		err := Execute(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}, "version")
		if err != nil || os.Getenv("OSC7577_TOKEN") != "" {
			os.Exit(1)
		}
		c := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
		c.Env = os.Environ()
		for _, entry := range c.Env {
			if strings.HasPrefix(entry, "OSC7577_TOKEN=") {
				os.Exit(2)
			}
		}
		if err := c.Run(); err != nil {
			os.Exit(3)
		}
		return
	}
	c := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestResumeTokenRemovedBeforeStartup$")
	c.Env = append(os.Environ(), "RESUME_CHILD_TEST=1", "OSC7577_TOKEN=dummy-token-replace-me", "DOCKER_AGENT_AUTO_UPDATE=1")
	out, err := c.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestSameResumeDirectory(t *testing.T) {
	dir := t.TempDir()
	assert.True(t, sameResumeDirectory(dir, dir))
	assert.False(t, sameResumeDirectory(dir, t.TempDir()))
	assert.False(t, sameResumeDirectory(dir, filepath.Join(dir, "missing")))
	if goruntime.GOOS != "windows" {
		link := filepath.Join(t.TempDir(), "alias")
		require.NoError(t, os.Symlink(dir, link))
		assert.True(t, sameResumeDirectory(dir, link))
	}
}

func TestResumeEnvironmentChangesTarget(t *testing.T) {
	for _, name := range []string{envModelsGateway, cagentEnvModelsGateway, envDefaultModel, cagentEnvDefaultModel} {
		t.Run(name, func(t *testing.T) {
			f := &runExecFlags{}
			f.runConfig.EnvProviderOverride = environment.NewMapEnvProvider(map[string]string{name: "value"})
			assert.True(t, resumeEnvironmentChangesTarget(t.Context(), f))
		})
	}
	f := &runExecFlags{}
	f.runConfig.EnvProviderOverride = environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "credential"})
	assert.False(t, resumeEnvironmentChangesTarget(t.Context(), f))
}

func TestResumeReporterRejectsUnusableWorkspace(t *testing.T) {
	for _, dir := range []string{"", "relative", "/no/such/workspace"} {
		t.Run(dir, func(t *testing.T) {
			var output bytes.Buffer
			r := &resumeReporter{ctx: t.Context(), token: "dummy-token-replace-me", command: []string{"/bin/docker-agent"}, out: &output}
			store := session.NewInMemorySessionStore()
			sess := session.New(session.WithWorkingDir(dir))
			require.NoError(t, store.AddSession(t.Context(), sess))
			r.update(app.New(t.Context(), &recallRuntime{store: store}, sess), false)
			assert.Empty(t, output.String())
		})
	}
}

func TestResumeCommandFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"--session", "existing"}, true},
		{[]string{"--session", "existing", "--session-db", "/tmp/sessions", "--require-existing-session"}, true},
		{[]string{"--safety", "strict"}, false},
		{[]string{"--model", "openai/gpt-4o"}, false},
		{[]string{"--working-dir", "/tmp"}, false},
		{[]string{"--hook-pre-tool-use", "echo check"}, false},
		{[]string{"--env-from-file", "/tmp/env"}, false},
		{[]string{"--session-workingdir-root", "/tmp"}, false},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			root := NewRootCmd()
			cmd, _, err := root.Find([]string{"run"})
			require.NoError(t, err)
			require.NoError(t, cmd.ParseFlags(tc.args))
			assert.Equal(t, tc.ok, resumeFlagsSupported(cmd))
		})
	}
}
