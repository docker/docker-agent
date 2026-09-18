package e2e

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDependencies(t *testing.T) {
	t.Parallel()
	t.Run("TUI musn't know about teams", func(t *testing.T) {
		imports := listImports(t, "../pkg/tui")

		assert.True(t, imports["github.com/docker/docker-agent/pkg/runtime"])
		assert.False(t, imports["github.com/docker/docker-agent/pkg/team"])
	})

	// Embedders that build teams in code (agent.New + team.New + the runtime,
	// see docs/guides/go-sdk) must not link docker-agent's heavy optional dependencies:
	// the goja JS engine, the OpenAI SDK, the SQLite driver, full go-git or
	// expr. Each entry below was cut deliberately; if this test fails you
	// probably added an import that drags one of them back in — keep it
	// behind a leaf package or a registration hook instead.
	t.Run("code-built team surface stays lean", func(t *testing.T) {
		t.Parallel()

		deps := listTransitiveDeps(t,
			"./pkg/runtime",
			"./pkg/agent",
			"./pkg/team",
			"./pkg/session",
			"./pkg/tools",
			"./pkg/model/provider/anthropic",
		)

		// Prefixes, so subpackage-only leaks (e.g. openai-go/v3/option) are
		// caught too.
		forbiddenPrefixes := []string{
			"github.com/dop251/goja",
			"github.com/expr-lang/expr",
			"github.com/openai/openai-go",
			"github.com/go-git/go-git",
			"github.com/rumpl/harness",
			"modernc.org/sqlite",
			"github.com/docker/docker-agent/pkg/js",
			"github.com/docker/docker-agent/pkg/codingharness",
			"github.com/docker/docker-agent/pkg/tools/mcp",
			"github.com/docker/docker-agent/pkg/toolinstall",
		}
		// Deliberate exceptions: fsx's gitignore matching uses go-git's
		// lightweight format package (and the helpers it drags in), not the
		// full object/transport stack; the runtime's remote OAuth login uses
		// the MCP-toolset-free oauthflow leaf.
		allowed := map[string]bool{
			"github.com/go-git/go-git/v5/plumbing/format/gitignore":  true,
			"github.com/go-git/go-git/v5/plumbing/format/config":     true,
			"github.com/go-git/go-git/v5/internal/path_util":         true,
			"github.com/go-git/go-git/v5/utils/ioutil":               true,
			"github.com/docker/docker-agent/pkg/tools/mcp/oauthflow": true,
		}

		for dep := range deps {
			if allowed[dep] {
				continue
			}
			for _, prefix := range forbiddenPrefixes {
				if dep == prefix || strings.HasPrefix(dep, prefix+"/") {
					assert.Fail(t, "heavy dependency leaked back into the embedder surface", "%s (matched %s)", dep, prefix)
				}
			}
		}
	})

	// Embedders that load YAML through teamloader with hand-picked registries
	// (teamloader.WithToolsetRegistry / WithProviderRegistry / WithStrict)
	// must not link every toolset, provider and agent source. The default
	// registries live in pkg/teamloader/defaults, pkg/teamloader/toolsets and
	// pkg/model/provider/providers; the OCI/HCL source stacks live in
	// pkg/config/ocisource, pkg/config/hcl and pkg/config/sources; optional
	// loader features (JS expansion, code mode, TOON, deferred tools, LSP
	// merging, DMR probing) are opt-in options. None of them may be reachable
	// from the loader itself: pkg/teamloader must add no module over
	// pkg/runtime.
	t.Run("YAML-loading surface with custom registries stays lean", func(t *testing.T) {
		t.Parallel()

		deps := listTransitiveDeps(t,
			"./pkg/config",
			"./pkg/teamloader",
			"./pkg/embeddedchat",
			"./pkg/tools/builtin/api/client",
			"./examples/golibrary/leanapi",
			"./pkg/tools/builtin/think",
			"./pkg/model/provider/anthropic",
		)

		forbiddenPrefixes := []string{
			// Opt-in loader features.
			"github.com/dop251/goja",
			"github.com/docker/docker-agent/pkg/js",
			"github.com/docker/docker-agent/pkg/tools/codemode",
			"github.com/docker/docker-agent/pkg/tools/toon",
			"github.com/alpkeskin/gotoon",
			"github.com/docker/docker-agent/pkg/tools/builtin/deferred",
			"github.com/junegunn/fzf",
			"github.com/docker/docker-agent/pkg/tools/builtin/lsp",
			"github.com/docker/docker-agent/pkg/toolinstall",
			"github.com/expr-lang/expr",
			"github.com/docker/docker-agent/pkg/model/provider/dmr",
			"github.com/openai/openai-go",
			"github.com/docker/docker-agent/pkg/codingharness",
			"github.com/rumpl/harness",
			// Other providers and their SDKs.
			"github.com/docker/docker-agent/pkg/model/provider/providers",
			"github.com/docker/docker-agent/pkg/model/provider/bedrock",
			"github.com/docker/docker-agent/pkg/model/provider/gemini",
			"github.com/docker/docker-agent/pkg/model/provider/vertexai",
			"github.com/aws/aws-sdk-go-v2",
			"google.golang.org/genai",
			// The full toolset registry and the heavier toolsets.
			"github.com/docker/docker-agent/pkg/teamloader/defaults",
			"github.com/docker/docker-agent/pkg/teamloader/toolsets",
			"github.com/docker/docker-agent/pkg/tools/builtin/rag",
			"github.com/docker/docker-agent/pkg/tools/builtin/memory",
			"github.com/docker/docker-agent/pkg/tools/builtin/shell",
			"github.com/docker/docker-agent/pkg/tools/builtin/filesystem",
			"modernc.org/sqlite",
			// Agent sources the embedder did not ask for.
			"github.com/docker/docker-agent/pkg/config/sources",
			"github.com/docker/docker-agent/pkg/config/ocisource",
			"github.com/docker/docker-agent/pkg/config/hcl",
			"github.com/docker/docker-agent/pkg/remote",
			"github.com/docker/docker-agent/pkg/content",
			"github.com/hashicorp/hcl",
			"github.com/zclconf/go-cty",
			"github.com/google/go-containerregistry",
		}
		// Reference classification (config.IsOCIReference) only needs the
		// registry-free name parser; DMR model discovery is a stdlib-only leaf.
		allowed := map[string]bool{
			"github.com/google/go-containerregistry/pkg/name":                 true,
			"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels": true,
		}

		for dep := range deps {
			if allowed[dep] {
				continue
			}
			for _, prefix := range forbiddenPrefixes {
				if dep == prefix || strings.HasPrefix(dep, prefix+"/") {
					assert.Fail(t, "dependency leaked into the YAML-loading embedder surface", "%s (matched %s)", dep, prefix)
				}
			}
		}
	})
}

// Check the target-specific closure without cross-compiling the native test
// dependencies, which intentionally exercise providers unavailable in WASM.
func TestWasmProviderDependencies(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps",
		"./pkg/model/provider", "./pkg/teamloader", "./pkg/runtime",
		"./pkg/model/provider/anthropic")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	forbidden := []string{
		"github.com/docker/docker-agent/pkg/model/provider/openai",
		"github.com/docker/docker-agent/pkg/model/provider/gemini",
		"github.com/docker/docker-agent/pkg/model/provider/providers",
		"github.com/openai/openai-go",
		"google.golang.org/genai",
		"google.golang.org/grpc",
		"cloud.google.com/go",
	}
	for dep := range strings.FieldsSeq(string(out)) {
		for _, prefix := range forbidden {
			assert.False(t, dep == prefix || strings.HasPrefix(dep, prefix+"/"), "unrequested provider dependency: %s", dep)
		}
	}
}

// listTransitiveDeps returns the full (non-test) dependency closure of the
// given packages, as reported by `go list -deps`.
func listTransitiveDeps(t *testing.T, pkgs ...string) map[string]bool {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "go", append([]string{"list", "-deps"}, pkgs...)...)
	cmd.Dir = ".."
	out, err := cmd.Output()
	require.NoError(t, err)

	deps := map[string]bool{}
	for dep := range strings.FieldsSeq(string(out)) {
		deps[dep] = true
	}
	return deps
}

func listImports(t *testing.T, pkg string) map[string]bool {
	t.Helper()

	imports := map[string]bool{}

	fileSet := token.NewFileSet()
	err := filepath.WalkDir(pkg, func(path string, d os.DirEntry, err error) error {
		if err != nil || !strings.HasSuffix(path, ".go") || d.IsDir() {
			return err
		}

		ast, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		require.NoError(t, err)

		for _, i := range ast.Imports {
			imports[strings.Trim(i.Path.Value, `"`)] = true
		}

		return nil
	})
	require.NoError(t, err)

	return imports
}
