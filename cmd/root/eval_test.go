package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/evaluation"
)

func TestEvalJudgeModelFlagDefaultsToGPT56Terra(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()

	flag := cmd.Flags().Lookup("judge-model")
	require.NotNil(t, flag, "eval must expose --judge-model")
	assert.Equal(t, "openai/gpt-5.6-terra", flag.DefValue)

	value, err := cmd.Flags().GetString("judge-model")
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-5.6-terra", value)
}

func TestEvalContainerRuntimeFlagDefaultsToDocker(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()

	flag := cmd.Flags().Lookup("container-runtime")
	require.NotNil(t, flag, "eval must expose --container-runtime")
	assert.Equal(t, "docker", flag.DefValue)
}

func TestEvalContainerRuntimeFlagAcceptsCustomExecutable(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()
	require.NoError(t, cmd.Flags().Parse([]string{"--container-runtime", "podman"}))

	value, err := cmd.Flags().GetString("container-runtime")
	require.NoError(t, err)
	assert.Equal(t, "podman", value)
}

func TestEvalAgentImageFlagDefaultsToEmpty(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()

	flag := cmd.Flags().Lookup("agent-image")
	require.NotNil(t, flag, "eval must expose --agent-image")
	assert.Empty(t, flag.DefValue, "unset --agent-image must defer to the version-derived default")
}

func TestEvalAgentImageFlagAcceptsOverride(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()
	require.NoError(t, cmd.Flags().Parse([]string{"--agent-image", "docker/docker-agent:1.2.3"}))

	value, err := cmd.Flags().GetString("agent-image")
	require.NoError(t, err)
	assert.Equal(t, "docker/docker-agent:1.2.3", value)
}

func TestEvalAgentImageFlagAcceptsNoneToSkipInjection(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()
	require.NoError(t, cmd.Flags().Parse([]string{"--agent-image", "none"}))

	value, err := cmd.Flags().GetString("agent-image")
	require.NoError(t, err)
	assert.Equal(t, evaluation.NoAgentImage, value)
}

func TestEvalJudgeTypeFlags(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name                 string
		args                 []string
		model, kind, wantErr string
	}{
		{"defaults", nil, defaultJudgeModel, evaluation.JudgeTypeLLM, ""},
		{"evaluator default", []string{"--judge-type", "evaluator"}, defaultEvaluatorJudgeModel, evaluation.JudgeTypeEvaluator, ""},
		{"evaluator explicit model", []string{"--judge-type", "evaluator", "--judge-model", "typesafe/jev-1.13.0"}, "typesafe/jev-1.13.0", evaluation.JudgeTypeEvaluator, ""},
		{"named evaluator", []string{"--judge-model", "relevance", "--judge-type", "evaluator"}, "relevance", evaluation.JudgeTypeEvaluator, ""},
		{"explicit empty", []string{"--judge-type", "evaluator", "--judge-model="}, "", evaluation.JudgeTypeEvaluator, ""},
		{"LLM custom", []string{"--judge-model", "anthropic/claude-sonnet-4-0"}, "anthropic/claude-sonnet-4-0", evaluation.JudgeTypeLLM, ""},
		{"invalid type", []string{"--judge-type", "unknown"}, "", "", "invalid --judge-type"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := newEvalCmd()
			require.NoError(t, cmd.Flags().Parse(tt.args))
			model, err := cmd.Flags().GetString("judge-model")
			require.NoError(t, err)
			kind, err := cmd.Flags().GetString("judge-type")
			require.NoError(t, err)
			flags := evalFlags{Config: evaluation.Config{JudgeModel: model, JudgeType: kind}}
			err = flags.resolveJudgeFlags(cmd)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.model, flags.JudgeModel)
			assert.Equal(t, tt.kind, flags.JudgeType)
		})
	}
}
