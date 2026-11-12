//go:build js

package builtins

import "fmt"

// environmentInfo builds the <env> block for the browser, where there is no
// shell to detect and no filesystem to look for a git repo in; the host's
// process env must not leak into the model context either.
func environmentInfo(workingDir string) string {
	return fmt.Sprintf(`Here is useful information about the environment you are running in:
	<env>
	Working directory: %s
	Operating System: %s
	CPU Architecture: %s
	</env>`, workingDir, displayOS(), displayArch())
}
