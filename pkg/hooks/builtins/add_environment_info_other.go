//go:build !js

package builtins

import (
	"fmt"

	"github.com/docker/docker-agent/pkg/shellpath"
)

// environmentInfo builds the <env> block. Long-form dialect rules live in
// shellSyntaxHint (tool description) so this stays terse.
func environmentInfo(workingDir string) string {
	gitRepo := "No"
	if isGitRepo(workingDir) {
		gitRepo = "Yes"
	}
	shellPath, _ := shellpath.DetectShell() // second value is argsPrefix, unused here
	return fmt.Sprintf(`Here is useful information about the environment you are running in:
	<env>
	Working directory: %s
	Is directory a git repo: %s
	Operating System: %s
	CPU Architecture: %s
	Shell: %s (%s)
	</env>`, workingDir, gitRepo, displayOS(), displayArch(), shellpath.ShellBaseName(shellPath), shellPath)
}
