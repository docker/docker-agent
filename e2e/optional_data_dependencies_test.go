package e2e

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOptionalCatalogAndConfigHistoryDependencies(t *testing.T) {
	t.Parallel()
	const prefix = "github.com/docker/docker-agent/pkg/"
	for _, target := range []string{"native", "wasm"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "./pkg/config", "./pkg/modelsdev", "./pkg/runtime", "./pkg/teamloader", "./pkg/embeddedchat")
			cmd.Dir = ".."
			if target == "wasm" {
				cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
			}
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
			for dep := range strings.FieldsSeq(string(out)) {
				assert.NotEqual(t, prefix+"modelsdev/embedded", dep)
				assert.NotEqual(t, prefix+"config/legacy", dep)
				if tail, ok := strings.CutPrefix(dep, prefix+"config/v"); ok {
					assert.Empty(t, tail, "historical schema leaked into the core: %s", dep)
				}
			}
		})
	}

	cli := listTransitiveDeps(t, "./cmd/root")
	assert.Contains(t, cli, prefix+"config/legacy", "CLI must retain historical YAML support")
	assert.Contains(t, cli, prefix+"modelsdev/embedded", "CLI must retain its offline catalog")
	for _, pkg := range []string{"./pkg/config/legacy", "./pkg/modelsdev/embedded"} {
		deps := listTransitiveDeps(t, pkg)
		other := prefix + "modelsdev/embedded"
		if strings.HasSuffix(pkg, "embedded") {
			other = prefix + "config/legacy"
		}
		assert.NotContains(t, deps, other, "the capabilities must be independent")
	}
}
