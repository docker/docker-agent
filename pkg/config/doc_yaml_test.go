package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xeipuuv/gojsonschema"
)

// yamlFence matches fenced YAML code blocks in markdown (```yaml or ```yml).
// Group 1: the line immediately before the opening fence.
// Group 2: the YAML body between the fences.
var yamlFence = regexp.MustCompile("(?m)(^[^\n]*)\n```ya?ml\n(?s:(.*?))```")

// topLevelConfigKeys are the keys that appear at the top level of a full
// agent configuration file (matching the JSON Schema root properties).
var topLevelConfigKeys = map[string]bool{
	"version":     true,
	"agents":      true,
	"models":      true,
	"providers":   true,
	"rag":         true,
	"metadata":    true,
	"permissions": true,
}

// TestDocYAMLSnippetsAreValid extracts every ```yaml code block from docs/
// and validates that:
//  1. Every snippet is valid YAML (parses without error).
//  2. Snippets that look like full agent configs are validated against the
//     JSON Schema (agent-schema.json).
//
// Add a <!-- yaml-lint:skip --> HTML comment on the line immediately before
// the opening fence to skip a specific block.
func TestDocYAMLSnippetsAreValid(t *testing.T) {
	t.Parallel()

	schemaData, err := os.ReadFile(schemaFile)
	require.NoError(t, err)
	schema, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(schemaData))
	require.NoError(t, err)

	snippetCount := 0
	err = filepath.WalkDir("../../docs", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for _, m := range yamlFence.FindAllSubmatchIndex(content, -1) {
			prevLine := string(content[m[2]:m[3]])
			if strings.Contains(prevLine, "<!-- yaml-lint:skip -->") {
				continue
			}

			body := string(content[m[4]:m[5]])
			line := 1 + strings.Count(string(content[:m[4]]), "\n")
			name := fmt.Sprintf("%s:%d", path, line)
			snippetCount++

			t.Run(name, func(t *testing.T) {
				t.Parallel()

				var raw any
				require.NoError(t, yaml.Unmarshal([]byte(body), &raw), "invalid YAML syntax")

				if looksLikeFullConfig(raw) {
					result, err := schema.Validate(gojsonschema.NewRawLoader(raw))
					require.NoError(t, err)
					assert.True(t, result.Valid(), "schema errors: %v", result.Errors())
				}
			})
		}

		return nil
	})
	require.NoError(t, err)
	require.NotZero(t, snippetCount, "expected to find YAML snippets in docs/")
}

// looksLikeFullConfig returns true when the parsed YAML value is a map whose
// keys are all recognized top-level config keys. This avoids schema-validating
// partial snippets that would trivially fail required-field checks.
func looksLikeFullConfig(v any) bool {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return false
	}
	for k := range m {
		if !topLevelConfigKeys[k] {
			return false
		}
	}
	return true
}
