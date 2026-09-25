package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var cloudID = regexp.MustCompile(`^sbx_[a-zA-Z0-9_-]+$`)

// CreateCloud uses run's build target, which supports v3 source kits and
// multi-kit assembly. Detached run creates the sandbox without starting a TUI.
func (b *Backend) CreateCloud(ctx context.Context, opts Options, stderr io.Writer) (string, error) {
	// Never resume a sandbox by name: this random identity is only for
	// reconciling a create whose response is interrupted.
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generating cloud sandbox name: %w", err)
	}
	name := "docker-agent-" + hex.EncodeToString(nonce[:])
	args := []string{"--detached", "--new", "--name", name, "--ttl", opts.TTL.String(), "--on-timeout", "stop"}
	args = append(args, opts.kitFlags()...)
	if opts.Template != "" {
		args = append(args, "--template", opts.Template)
	} else {
		args = append(args, opts.agent())
	}
	cmd := exec.CommandContext(ctx, b.program, b.args("run", args...)...)
	b.applyEnv(cmd)
	cmd.Stderr = stderr
	gracefulCancel(cmd)
	out, err := cmd.Output()
	id := strings.TrimSpace(string(out))
	if !cloudID.MatchString(id) {
		id = ""
		if err == nil {
			err = fmt.Errorf("sandbox CLI returned an invalid cloud ID %q", strings.TrimSpace(string(out)))
		}
	}
	if err != nil {
		if id == "" {
			// sbx may have committed the create before losing its response.
			reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			id, _ = b.cloudIDForName(reconcileCtx, name)
		}
		if id == "" {
			fmt.Fprintf(stderr, "Cloud creation outcome is unknown for %s; check `sbx --cloud ls` before retrying. Its TTL stops compute if creation completed.\n", name)
		}
		return id, fmt.Errorf("creating cloud sandbox %s: %w", name, err)
	}
	return id, nil
}

// Stop preserves the remote filesystem while releasing cloud compute.
func (b *Backend) Stop(ctx context.Context, name string, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, b.program, b.args("stop", name)...)
	b.applyEnv(cmd)
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("stopping sandbox %s: %w", name, err)
	}
	return nil
}

func (b *Backend) cloudIDForName(ctx context.Context, name string) (string, error) {
	all, err := b.list(ctx)
	if err != nil {
		return "", err
	}
	var id string
	for _, entry := range all {
		if entry.Name != name && !strings.HasSuffix(entry.Name, "/"+name) {
			continue
		}
		if id != "" || !cloudID.MatchString(entry.ID) {
			return "", fmt.Errorf("ambiguous cloud sandbox identity for %s", name)
		}
		id = entry.ID
	}
	return id, nil
}

func gracefulCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	// sbx needs time to kill the remote process before we suspend the VM.
	cmd.WaitDelay = 10 * time.Second
}
