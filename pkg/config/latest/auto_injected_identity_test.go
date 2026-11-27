package latest

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"
)

// TestAutoInjectedIdentityIsNotConfigurable pins that the internal dedup
// identity of runtime-synthesized hooks cannot be set from YAML: the config
// parser runs in strict mode and the field is excluded from the schema, so no
// user-authored hook entry can reproduce an automatic hook's identity.
func TestAutoInjectedIdentityIsNotConfigurable(t *testing.T) {
	t.Parallel()

	var hook HookDefinition
	err := yaml.UnmarshalWithOptions([]byte("type: builtin\ncommand: redact_secrets\nauto_injected: true\n"), &hook, yaml.Strict())
	require.Error(t, err)
	require.Contains(t, err.Error(), "auto_injected")

	// A plain unmarshal (used by tests and legacy paths) must not set it
	// either: the marker only ever comes from the runtime.
	require.NoError(t, yaml.Unmarshal([]byte("type: builtin\ncommand: redact_secrets\nauto_injected: true\n"), &hook))
	require.False(t, hook.AutoInjected)
}
