package root

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/docker/docker-agent/pkg/app"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/programresume"
	"github.com/docker/docker-agent/pkg/session"
)

type (
	resumeTokenKey    struct{}
	resumeReporterKey struct{}
)

type resumeReporter struct {
	ctx       context.Context //nolint:containedctx // Reporter is scoped to this invocation.
	token     string
	command   []string
	last      string
	out       io.Writer
	lastCheck time.Time
	force     bool
}

func programResumeFromContext(ctx context.Context) *resumeReporter {
	r, _ := ctx.Value(resumeReporterKey{}).(*resumeReporter)
	return r
}

// Only replay commands whose full set of run-time options is known. Prompts,
// environment overrides and non-default flags are deliberately not replayed.
func (f *runExecFlags) withProgramResume(ctx context.Context, cmd *cobra.Command, args []string, useTUI bool) context.Context {
	token, _ := ctx.Value(resumeTokenKey{}).(string)
	if !programresume.ValidToken(token) || !useTUI || !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) || len(args) != 1 || f.sandbox {
		return ctx
	}
	if !resumeFlagsSupported(cmd) || resumeEnvironmentChangesTarget(ctx, f) {
		return ctx
	}
	var err error
	agent := args[0]
	if !filepath.IsAbs(agent) {
		agent, err = filepath.Abs(agent)
		if err != nil {
			return ctx
		}
	}
	info, err := os.Stat(agent)
	if err != nil || !info.Mode().IsRegular() {
		return ctx
	}
	executable, err := os.Executable()
	if err != nil || !filepath.IsAbs(executable) {
		return ctx
	}
	db, err := pathx.ExpandHomeDir(sessionDBPath(f.sessionDB))
	if err != nil {
		return ctx
	}
	db, err = filepath.Abs(db)
	if err != nil {
		return ctx
	}
	configDir, err := filepath.Abs(paths.GetConfigDir())
	if err != nil {
		return ctx
	}
	dataDir, err := filepath.Abs(paths.GetDataDir())
	if err != nil {
		return ctx
	}
	r := &resumeReporter{ctx: ctx, token: token, out: os.Stdout, command: []string{executable, "--config-dir", configDir, "--data-dir", dataDir, "run", agent, "--session-db", db, "--require-existing-session"}}
	return context.WithValue(ctx, resumeReporterKey{}, r)
}

func sameResumeDirectory(a, b string) bool {
	left, err := os.Stat(a)
	if err != nil || !left.IsDir() {
		return false
	}
	right, err := os.Stat(b)
	return err == nil && right.IsDir() && os.SameFile(left, right)
}

func resumeEnvironmentChangesTarget(ctx context.Context, f *runExecFlags) bool {
	env := f.runConfig.EnvProvider()
	for _, name := range []string{envModelsGateway, cagentEnvModelsGateway, envDefaultModel, cagentEnvDefaultModel} {
		if value, _ := env.Get(ctx, name); value != "" {
			return true
		}
	}
	return false
}

func resumeFlagsSupported(cmd *cobra.Command) bool {
	supported := true
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		if flag.Changed && flag.Name != "session" && flag.Name != "session-db" && flag.Name != "require-existing-session" {
			supported = false
		}
	})
	cmd.InheritedFlags().VisitAll(func(flag *pflag.Flag) {
		if flag.Changed && flag.Name != "config-dir" && flag.Name != "data-dir" && flag.Name != "session" && flag.Name != "session-db" && flag.Name != "require-existing-session" {
			supported = false
		}
	})
	return supported
}

func (r *resumeReporter) update(a *app.App, pending bool) {
	if a == nil || a.Session() == nil || a.SessionStore() == nil || pending {
		r.invalidate()
		return
	}
	sess := a.Session()
	if sess.ID == "" {
		r.invalidate()
		return
	}
	argv := append(slices.Clone(r.command), "--session", sess.ID)
	key := strings.Join(argv, "\x00") + "\x00" + sess.WorkingDir
	if key == r.last && !r.force && time.Since(r.lastCheck) < time.Second {
		return
	}
	r.lastCheck = time.Now()
	stored, err := a.SessionStore().GetSession(r.ctx, sess.ID)
	if err != nil || stored == nil || stored.ID != sess.ID || stored.WorkingDir != sess.WorkingDir || stored.GetSafetyPolicy() != sess.GetSafetyPolicy() {
		r.invalidate()
		return
	}
	cwd, err := session.ResolveWorkingDir(r.ctx, stored, nil)
	if err != nil {
		r.invalidate()
		return
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		r.invalidate()
		return
	}
	if key == r.last && !r.force {
		return
	}
	report := programresume.Report(r.token, os.Getpid(), time.Now(), argv, cwd)
	if report == "" {
		r.invalidate()
		return
	}
	if n, err := io.WriteString(r.out, report); err != nil || n != len(report) {
		r.invalidate()
		return
	}
	r.last = key
	r.force = false
}

func (r *resumeReporter) resumed() {
	r.force = true
}

func (r *resumeReporter) invalidate() {
	if r.last != "" {
		r.clear()
	}
}

func (r *resumeReporter) clear() {
	if r.last == "" {
		return
	}
	_, _ = io.WriteString(r.out, programresume.Clear(r.token, os.Getpid(), time.Now()))
	r.last = ""
}
