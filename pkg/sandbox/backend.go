package sandbox

import (
	"os"
	"os/exec"
)

// Backend describes how to invoke sandbox CLI commands.
// The two supported backends are "docker sandbox" and "sbx".
type Backend struct {
	// program is the executable name ("docker" or "sbx").
	program string
	// prefix is the sub-command prefix prepended to every command.
	// For "docker sandbox" this is ["sandbox"]; for "sbx" it is empty.
	prefix []string
	// extraEnv holds extra environment variables to set on every command.
	extraEnv []string
	// vmListKey is the JSON key returned by the "ls" command that holds
	// the list of sandboxes ("vms" for docker sandbox, "sandboxes" for sbx).
	vmListKey string
}

// NewBackend returns the appropriate backend.  When preferSbx is true
// and the "sbx" binary is on PATH, the sbx backend is used; otherwise
// it falls back to "docker sandbox".
func NewBackend(preferSbx bool) *Backend {
	if preferSbx {
		if _, err := exec.LookPath("sbx"); err == nil {
			return sbxBackend()
		}
	}
	return dockerSandboxBackend()
}

func dockerSandboxBackend() *Backend {
	return &Backend{
		program:   "docker",
		prefix:    []string{"sandbox"},
		vmListKey: "vms",
	}
}

func sbxBackend() *Backend {
	return &Backend{
		program:   "sbx",
		prefix:    nil,
		extraEnv:  []string{"DOCKER_CLI_PLUGIN_ORIGINAL_CLI_COMMAND="},
		vmListKey: "sandboxes",
	}
}

// command builds an exec.Cmd for the given sandbox sub-command and arguments.
// For example, command(ctx, "ls", "--json") produces either
// "docker sandbox ls --json" or "sbx ls --json".
func (b *Backend) args(subCmd string, extra ...string) []string {
	args := make([]string, 0, len(b.prefix)+1+len(extra))
	args = append(args, b.prefix...)
	args = append(args, subCmd)
	args = append(args, extra...)
	return args
}

// applyEnv augments the command's environment with any backend-specific
// variables.  It must be called on every exec.Cmd created for the backend.
func (b *Backend) applyEnv(cmd *exec.Cmd) {
	if len(b.extraEnv) > 0 {
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		cmd.Env = append(cmd.Env, b.extraEnv...)
	}
}
