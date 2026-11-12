package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/dgageot/rubocop-go/prog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBoundedCommandPipeLifetime(t *testing.T) {
	// These fixtures use Unix process groups, regardless of the test host.
	t.Setenv("GOOS", "linux")
	t.Setenv("GOARCH", "amd64")
	const groupCommand = `cmd := ex.CommandContext(ctx, "sh", "-c", script); cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid:true}; stdout, _ := cmd.StdoutPipe(); `
	const command = `cmd := ex.CommandContext(ctx, "/bin/sh", "-c", script); stdout, _ := cmd.StdoutPipe(); `
	const grouped = groupCommand + `_ = cmd.Start(); `
	const setup = command + `_ = cmd.Start(); `
	tests := []struct {
		name, body string
		want       int
	}{
		{"literal shell", setup + `_, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"parenthesized pipe", `cmd := ex.CommandContext(ctx, "sh", "-c", script); stdout, _ := (cmd.StdoutPipe)(); _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"nested block pipe", `{ ` + setup + `_, _ = io.ReadAll(stdout); _ = cmd.Wait() }`, 1},
		{"no command pipe", `_, _ = io.ReadAll(io.LimitReader(nil, 0))`, 0},
		{"copy", setup + `_, _ = io.Copy(io.Discard, stdout); _ = cmd.Wait()`, 1},
		{"limited reader", setup + `_, _ = io.ReadAll(io.LimitReader(stdout, 1024)); _ = cmd.Wait()`, 1},
		{"zero limited reader cannot block", setup + `_, _ = io.ReadAll(io.LimitReader(stdout, 0)); _ = cmd.Wait()`, 0},
		{"nonblocking limit then drain", setup + `_, _ = io.ReadAll(io.LimitReader(stdout, 0)); _, _ = io.Copy(io.Discard, stdout); _ = cmd.Wait()`, 1},
		{"limited then drain reports once", setup + `_, _ = io.ReadAll(io.LimitReader(stdout, 1024)); _, _ = io.Copy(io.Discard, stdout); _ = cmd.Wait()`, 1},
		{"limited reader alias", setup + `limited := io.LimitReader(stdout, 1024); _, _ = io.ReadAll(limited); _ = cmd.Wait()`, 1},
		{"pipe alias", setup + `out := stdout; _, _ = io.ReadAll(out); _ = cmd.Wait()`, 1},
		{"command alias", `cmd := ex.CommandContext(ctx, "bash", "-c", script); alias := cmd; stdout, _ := alias.StdoutPipe(); _ = alias.Start(); _, _ = io.ReadAll(stdout); _ = alias.Wait()`, 1},
		{"stderr", `cmd := ex.CommandContext(ctx, "zsh", "-c", script); stderr, _ := cmd.StderrPipe(); _ = cmd.Start(); _, _ = io.Copy(io.Discard, stderr); _ = cmd.Wait()`, 1},
		{"both pipes", setup + `stderr, _ := cmd.StderrPipe(); _, _ = io.ReadAll(stdout); _, _ = io.ReadAll(stderr); _ = cmd.Wait()`, 2},
		{"var declarations", `var cmd = ex.CommandContext(ctx, "dash", "-c", script); var stdout, _ = cmd.StdoutPipe(); _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"detected shell", `shell, args := sp.DetectShell(); cmd := ex.CommandContext(ctx, shell, append(args, script)...); stdout, _ := cmd.StdoutPipe(); if err := cmd.Start(); err != nil { return }; _, _ = io.ReadAll(stdout); if err := cmd.Wait(); err != nil { return }`, 1},
		{"detected shell alias", `shell, args := sp.DetectShell(); alias := shell; cmd := ex.CommandContext(ctx, alias, append(args, script)...); stdout, _ := cmd.StdoutPipe(); _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"wait delay does not bound external read", setup + `cmd.WaitDelay = time.Second; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"deferred close is too late", setup + `defer stdout.Close(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"deferred cancellation is too late", setup + `_, cancel := context.WithCancel(ctx); defer cancel(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"cancel kills only shell", setup + `cmd.Cancel = func() error { return cmd.Process.Kill() }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"signal zero does not clean up", setup + `cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, 0) }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"positive pid kills only shell", setup + `cmd.Cancel = func() error { return syscall.Kill(cmd.Process.Pid, syscall.SIGKILL) }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"unrelated pipe close", setup + `extra, _ := cmd.StderrPipe(); context.AfterFunc(ctx, func(){ _ = extra.Close() }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"unrelated context close", setup + `context.AfterFunc(other, func(){ _ = stdout.Close() }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"unrelated close method", setup + `context.AfterFunc(ctx, func(){ _ = fake.Close() }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"unrelated done method", setup + `go func(){ <-fake.Done(); _ = stdout.Close() }(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"cleanup installed after read", setup + `_, _ = io.ReadAll(stdout); context.AfterFunc(ctx, func(){ _ = stdout.Close() }); _ = cmd.Wait()`, 1},
		{"uninvoked close closure", setup + `_ = func(){ _ = stdout.Close() }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"uninvoked closure inside callback", setup + `context.AfterFunc(ctx, func(){ _ = func(){ _ = stdout.Close() } }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"unrelated wait", `cmd := ex.CommandContext(ctx, "sh", "-c", script); stdout, _ := cmd.StdoutPipe(); _ = cmd.Start(); different := ex.CommandContext(ctx, "echo"); _ = different.Wait(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"shadowed command", setup + `{ cmd := &fakeCommand{}; _ = cmd.Wait() }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"inside function literal", `_ = func(){ ` + setup + `_, _ = io.ReadAll(stdout); _ = cmd.Wait() }`, 1},
		{"non shell executable", `cmd := ex.CommandContext(ctx, "cat", "-c", script); stdout, _ := cmd.StdoutPipe(); _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"non context command", `cmd := ex.Command("sh", "-c", script); stdout, _ := cmd.StdoutPipe(); _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"shell script without c flag", `cmd := ex.CommandContext(ctx, "sh", script); stdout, _ := cmd.StdoutPipe(); _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"c flag is a script argument", `cmd := ex.CommandContext(ctx, "sh", script, "-c"); stdout, _ := cmd.StdoutPipe(); _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"unknown executable", `cmd := ex.CommandContext(ctx, script, "-c", script); stdout, _ := cmd.StdoutPipe(); _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"shadowed DetectShell", `sp := fakeShell{}; shell, args := sp.DetectShell(); cmd := ex.CommandContext(ctx, shell, append(args, script)...); stdout, _ := cmd.StdoutPipe(); _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"shadowed exec", `ex := fakeExec{}; cmd := ex.CommandContext(ctx, "sh", "-c", script); stdout, _ := cmd.StdoutPipe(); _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"shadowed io", setup + `io := fakeIO{}; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"async read", setup + `go func(){ _, _ = io.ReadAll(stdout) }(); _ = cmd.Wait()`, 0},
		{"async copy", setup + `go io.Copy(io.Discard, stdout); _ = cmd.Wait()`, 0},
		{"deferred read", setup + `defer io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"not started", `cmd := ex.CommandContext(ctx, "sh", "-c", script); stdout, _ := cmd.StdoutPipe(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"no wait", setup + `_, _ = io.ReadAll(stdout)`, 0},
		{"wait before read", setup + `_ = cmd.Wait(); _, _ = io.ReadAll(stdout)`, 0},
		{"afterfunc pipe close", setup + `stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); defer stop(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"afterfunc pipe alias close", setup + `out := stdout; context.AfterFunc(ctx, func(){ _ = out.Close() }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"afterfunc context alias", setup + `alias := ctx; context.AfterFunc(alias, func(){ _ = stdout.Close() }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"goroutine cancellation close", setup + `go func(){ <-ctx.Done(); _ = stdout.Close() }(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"select cancellation close can exit early", setup + `go func(){ select { case <-ctx.Done(): _ = stdout.Close(); case <-done: } }(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"cancel callback closes pipe", setup + `cmd.Cancel = func() error { return stdout.Close() }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"cancel callback method", setup + `cmd.Cancel = stdout.Close; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"cancel callback deferred close", setup + `cmd.Cancel = func() error { defer stdout.Close(); return nil }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"afterfunc deferred close", setup + `context.AfterFunc(ctx, func(){ defer stdout.Close() }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"afterfunc conditional close is not guaranteed", setup + `context.AfterFunc(ctx, func(){ if len(script) > 0 { _ = stdout.Close() } }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"missing process group setup", setup + `cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"afterfunc missing process group setup", setup + `context.AfterFunc(ctx, func(){ _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"own process group", grouped + `cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"own group afterfunc", grouped + `context.AfterFunc(ctx, func(){ _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"setup after start is too late", setup + `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid:true}; cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"foreign process group", `cmd := ex.CommandContext(ctx, "sh", "-c", script); cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid:true, Pgid:42}; stdout, _ := cmd.StdoutPipe(); _ = cmd.Start(); cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"own-group-sigterm-ignored", groupCommand + `cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }; _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"group-sigstop", groupCommand + `cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGSTOP) }; _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"overwritten-cancel", command + `cmd.Cancel = stdout.Close; cmd.Cancel = func() error { return cmd.Process.Kill() }; _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"disabled-afterfunc", setup + `stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); stop(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"conditional-wait", setup + `waited := enabled && cmd.Wait() == nil; _ = waited; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"conditional-afterfunc", setup + `installed := enabled && context.AfterFunc(ctx, func(){ _ = stdout.Close() }) != nil; _ = installed; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"own group afterfunc sigterm", grouped + `context.AfterFunc(ctx, func(){ _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"group setup overwritten", `cmd := ex.CommandContext(ctx, "sh", "-c", script); cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid:true}; cmd.SysProcAttr.Setpgid = false; stdout, _ := cmd.StdoutPipe(); cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }; _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"group setup aliased", `cmd := ex.CommandContext(ctx, "sh", "-c", script); cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid:true}; attrs := cmd.SysProcAttr; attrs.Setpgid = false; stdout, _ := cmd.StdoutPipe(); cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }; _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"repaired cancel", setup + `cmd.Cancel = func() error { return cmd.Process.Kill() }; cmd.Cancel = stdout.Close; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"nil cancel", setup + `cmd.Cancel = stdout.Close; cmd.Cancel = nil; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"cancel overwritten through alias", setup + `alias := cmd; alias.Cancel = stdout.Close; cmd.Cancel = nil; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"cancel overwritten after read", setup + `cmd.Cancel = stdout.Close; _, _ = io.ReadAll(stdout); cmd.Cancel = nil; _ = cmd.Wait()`, 0},
		{"independent afterfunc survives cancel overwrite", setup + `context.AfterFunc(ctx, func(){ _ = stdout.Close() }); cmd.Cancel = stdout.Close; cmd.Cancel = nil; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"independent cancel survives stopped afterfunc", setup + `cmd.Cancel = stdout.Close; stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); stop(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"independent afterfunc survives stopped afterfunc", setup + `context.AfterFunc(ctx, func(){ _ = stdout.Close() }); stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); stop(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"all afterfuncs stopped", setup + `first := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); second := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); first(); second(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"afterfunc stop alias", setup + `stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); alias := stop; alias(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"afterfunc reinstalled", setup + `stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); stop(); context.AfterFunc(ctx, func(){ _ = stdout.Close() }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"afterfunc stopped after read", setup + `stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); _, _ = io.ReadAll(stdout); stop(); _ = cmd.Wait()`, 0},
		{"afterfunc deferred stop alias", setup + `stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); alias := stop; defer alias(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"afterfunc mutable stop", setup + `stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); stop = func() bool { return false }; _ = stop; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"afterfunc mutable stop alias", setup + `stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); alias := stop; alias(); alias = func() bool { return false }; _ = alias; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"afterfunc escaped stop", setup + `stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); invoke(stop); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"afterfunc asynchronous stop", setup + `stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); go stop(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"afterfunc early return", setup + `context.AfterFunc(ctx, func(){ if enabled { return }; _ = stdout.Close() }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"afterfunc conditional expression", setup + `context.AfterFunc(ctx, func(){ _ = enabled && stdout.Close() == nil }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"cancel early return", setup + `cmd.Cancel = func() error { if enabled { return nil }; return stdout.Close() }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"afterfunc stale pipe alias", setup + `out := stdout; context.AfterFunc(ctx, func(){ _ = out.Close() }); out, _ = os.Open(script); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"conditional wait or", setup + `waited := enabled || cmd.Wait() == nil; _ = waited; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"conditional afterfunc or", setup + `installed := enabled || context.AfterFunc(ctx, func(){ _ = stdout.Close() }) != nil; _ = installed; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"conditional stop", setup + `stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); _ = enabled && stop(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"known true wait", setup + `_ = true && cmd.Wait() == nil; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"known false or wait", setup + `_ = false || cmd.Wait() == nil; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"known true afterfunc", setup + `_ = true && context.AfterFunc(ctx, func(){ _ = stdout.Close() }) != nil; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"known false and wait", setup + `_ = false && cmd.Wait() == nil; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"known true or afterfunc", setup + `_ = true || context.AfterFunc(ctx, func(){ _ = stdout.Close() }) != nil; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"short circuited stop", setup + `stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); _ = false && stop(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"conditional readall", setup + `_ = enabled && string(mustRead(io.ReadAll(stdout))) != ""; _ = cmd.Wait()`, 1},
		{"unreachable readall", setup + `_ = false && string(mustRead(io.ReadAll(stdout))) != ""; _ = cmd.Wait()`, 0},
		{"else if conditional wait", setup + `if enabled {} else if cmd.Wait() == nil {}; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"loop post conditional wait", setup + `for ; enabled; cmd.Wait() { break }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"conditional cancel overwrite", setup + `cmd.Cancel = stdout.Close; if enabled { cmd.Cancel = nil }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"conditional stop statement", setup + `stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); if enabled { stop() }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"stop var declaration alias", setup + `var stop = context.AfterFunc(ctx, func(){ _ = stdout.Close() }); var alias = stop; alias(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"stop var deferred alias", setup + `var stop = context.AfterFunc(ctx, func(){ _ = stdout.Close() }); var alias = stop; defer alias(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"cancel only protects latest pipe", setup + `stderr, _ := cmd.StderrPipe(); cmd.Cancel = stdout.Close; cmd.Cancel = stderr.Close; _, _ = io.ReadAll(stdout); _, _ = io.ReadAll(stderr); _ = cmd.Wait()`, 1},
		{"afterfunc closes both pipes", setup + `stderr, _ := cmd.StderrPipe(); context.AfterFunc(ctx, func(){ _ = stdout.Close(); _ = stderr.Close() }); _, _ = io.ReadAll(stdout); _, _ = io.ReadAll(stderr); _ = cmd.Wait()`, 0},
		{"single case cancellation select", setup + `go func(){ select { case <-ctx.Done(): _ = stdout.Close() } }(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"callback known unconditional close", setup + `context.AfterFunc(ctx, func(){ _ = true && stdout.Close() == nil }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"unknown group signal", grouped + `cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.Signal(len(script))) }; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"new session process group", `cmd := ex.CommandContext(ctx, "sh", "-c", script); cmd.SysProcAttr = &syscall.SysProcAttr{Setsid:true}; stdout, _ := cmd.StdoutPipe(); cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }; _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"group kill callback before start", groupCommand + `cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }; _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"independent goroutine survives stopped afterfunc", setup + `go func(){ <-ctx.Done(); _ = stdout.Close() }(); stop := context.AfterFunc(ctx, func(){ _ = stdout.Close() }); stop(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"inline afterfunc stopped", setup + `(context.AfterFunc(ctx, func(){ _ = stdout.Close() }))(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"cancel field pointer overwrite", setup + `cmd.Cancel = stdout.Close; callback := &cmd.Cancel; *callback = nil; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"nested callback early return", setup + `context.AfterFunc(ctx, func(){ { if enabled { return } }; _ = stdout.Close() }); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"cancel escaped before callback installation", setup + `callback := &cmd.Cancel; cmd.Cancel = stdout.Close; *callback = nil; _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"group slot escaped before setup", `cmd := ex.CommandContext(ctx, "sh", "-c", script); attrs := &cmd.SysProcAttr; cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid:true}; *attrs = nil; stdout, _ := cmd.StdoutPipe(); cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }; _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 1},
		{"reassigned command", setup + `cmd = ex.CommandContext(ctx, "cat"); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"reassigned pipe", setup + `stdout, _ = os.Open(script); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
		{"reassigned shell", `shell, args := sp.DetectShell(); shell = "cat"; cmd := ex.CommandContext(ctx, shell, append(args, script)...); stdout, _ := cmd.StdoutPipe(); _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait()`, 0},
	}
	files := coptest.ProgramFiles{
		"go.mod":                     "module github.com/docker/docker-agent\n\ngo 1.27\n",
		"pkg/shellpath/shellpath.go": `package shellpath; func DetectShell() (string, []string) { return "sh", []string{"-c"} }`,
	}
	for i, tc := range tests {
		files[fmt.Sprintf("case%d/sample.go", i)] = `package p
import (
    "context"
    "io"
    "os"
    ex "os/exec"
    "syscall"
    "time"
    sp "github.com/docker/docker-agent/pkg/shellpath"
)
var _ = os.Open
var _ = time.Second
var _ = syscall.SIGKILL
var _ = sp.DetectShell
var _ = ex.CommandContext
var _ = io.Discard

func invoke(stop func() bool) { stop() }
func mustRead(b []byte, _ error) []byte { return b }

type fakeCommand struct{}
func (*fakeCommand) StdoutPipe() (io.ReadCloser, error) { return nil, nil }
func (*fakeCommand) Start() error { return nil }
func (*fakeCommand) Wait() error { return nil }
type fakeExec struct{}
func (fakeExec) CommandContext(context.Context, string, ...string) *fakeCommand { return nil }
type fakeIO struct{}
func (fakeIO) ReadAll(io.Reader) ([]byte, error) { return nil, nil }
type fakeShell struct{}
func (fakeShell) DetectShell() (string, []string) { return "sh", nil }
type fakeContext struct{}
func (fakeContext) Close() error { return nil }
func (fakeContext) Done() <-chan struct{} { return nil }
func run(ctx, other context.Context, script string, done <-chan struct{}, fake fakeContext, enabled bool) { ` + tc.body + ` }
`
	}
	dir := t.TempDir()
	for name, source := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
	}
	program, err := prog.LoadDir(dir, "./...")
	require.NoError(t, err)
	for _, pkg := range program.Packages {
		require.Empty(t, pkg.Errors, pkg.PkgPath)
	}
	require.False(t, program.HasErrors(), "fixtures must type-check")
	pass := &prog.Pass{Cop: BoundedCommandPipeLifetime, Program: program}
	BoundedCommandPipeLifetime.Check(pass)
	offenses := pass.Offenses()
	got := make(map[string]int)
	for _, offense := range offenses {
		assert.Equal(t, "Lint/BoundedCommandPipeLifetime", offense.CopName)
		assert.Contains(t, offense.Message, "descendants keep the pipe open")
		got[filepath.Base(filepath.Dir(offense.Pos.Filename))]++
	}
	for i, tc := range tests {
		assert.Equal(t, tc.want, got[fmt.Sprintf("case%d", i)], tc.name)
	}
}

func TestBoundedCommandPipeLifetimeScope(t *testing.T) {
	t.Parallel()
	const src = `package p
import ("context"; "io"; "os/exec")
func f(ctx context.Context) {
    cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 10 &")
    stdout, _ := cmd.StdoutPipe()
    _ = cmd.Start()
    _, _ = io.ReadAll(stdout)
    _ = cmd.Wait()
}
`
	offenses := coptest.RunProgram(t, BoundedCommandPipeLifetime, coptest.ProgramFiles{
		"pkg/config/v0/sample.go":     src,
		"pkg/config/v15/sample.go":    src,
		"pkg/config/latest/sample.go": src,
		"generated/sample.go":         "// Code generated by fixture; DO NOT EDIT.\n" + src,
		"tests/sample.go":             "package p\n",
		"tests/sample_test.go":        src,
	})
	require.Len(t, offenses, 1)
	assert.True(t, strings.HasSuffix(filepath.ToSlash(offenses[0].Pos.Filename), "/pkg/config/latest/sample.go"))
}

func TestBoundedCommandPipeLifetimeSkillsRunCommand(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("../pkg/skills/expand.go")
	require.NoError(t, err)
	shellpath, err := os.ReadFile("../pkg/shellpath/shellpath.go")
	require.NoError(t, err)
	offenses := coptest.RunProgram(t, BoundedCommandPipeLifetime, coptest.ProgramFiles{
		"go.mod":                     "module github.com/docker/docker-agent\n\ngo 1.27\n",
		"pkg/skills/expand.go":       string(src),
		"pkg/shellpath/shellpath.go": string(shellpath),
	})
	assert.Empty(t, offenses)
}

func TestBoundedCommandPipeLifetimeSuppressions(t *testing.T) {
	testCorrectnessCopSuppressions(t, BoundedCommandPipeLifetime, `package sample
import ("context"; "io"; "os/exec")
func f(ctx context.Context) { cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 2 & wait"); stdout, _ := cmd.StdoutPipe(); _ = cmd.Start(); _, _ = io.ReadAll(stdout); _ = cmd.Wait() } // offense
`)
}
