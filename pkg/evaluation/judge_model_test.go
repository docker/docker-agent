package evaluation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/providers"
)

// TestCreateJudgeModelKnownProvider reproduces issue #3219: creating the eval
// judge model failed with `unknown provider type "anthropic"` because the judge
// was built through the empty package-level default registry instead of the
// fully-populated provider registry.
func TestCreateJudgeModelKnownProvider(t *testing.T) {
	t.Parallel()

	runConfig := &config.RuntimeConfig{
		EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{
			"ANTHROPIC_API_KEY": "test-key",
			"OPENAI_API_KEY":    "test-key",
			"GOOGLE_API_KEY":    "test-key",
		}),
		// Mirror how the eval command wires the full provider set; the package
		// default registry is empty (see pkg/model/provider/providers).
		ProviderRegistry: providers.NewDefaultRegistry(),
	}

	for _, judgeModel := range []string{
		"anthropic/claude-opus-4-5-20251101", // the default judge model that triggered #3219
		"anthropic/claude-sonnet-4-0",
		"openai/gpt-5",
		"openai/gpt-5.6-terra",
		"google/gemini-2.5-flash",
	} {
		judge, err := createJudgeModel(t.Context(), judgeModel, runConfig)
		require.NoError(t, err, "judge model %q should resolve to a known provider", judgeModel)
		assert.NotNil(t, judge)
	}
}
