package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
)

func TestHooksExecutorsIsolateWorkspaceConfiguration(t *testing.T) {
	t.Parallel()

	for _, concurrent := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent=%t", concurrent), func(t *testing.T) {
			t.Parallel()

			source := config.NewBytesSource("hooks.yaml", []byte(`version: "16"
agents:
  root:
    model: openai/test
    skills: false
    add_date: true
    add_prompt_files: [PROMPT.md]
    redact_secrets: true
    hooks:
      turn_start:
        - type: builtin
          command: add_context
          args: [configured]
          env: {HOOK_ENV: original}
      pre_tool_use:
        - matcher: "*"
          preempt_yolo: true
          hooks:
            - type: builtin
              command: add_context
              args: [matched]
`))
			tm, err := teamloader.Load(t.Context(), source, &config.RuntimeConfig{
				Config:              config.Config{WorkingDir: t.TempDir()},
				EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"}),
			}, teamloader.WithProviderRegistry(provider.NewRegistry(map[string]provider.Factory{
				"openai": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
					return &mockProvider{id: "test/mock-model"}, nil
				},
			})))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, tm.Dispose(context.WithoutCancel(t.Context()))) })
			a, err := tm.DefaultAgent()
			require.NoError(t, err)
			original := a.Hooks()
			before, err := json.Marshal(original)
			require.NoError(t, err)

			build := func(t *testing.T, tm *team.Team, dir string) {
				t.Helper()
				var effective *hooks.Config
				injector := hooksConfigInjector(func(cfg *hooks.Config) {
					effective = cfg
					cfg.TurnStart[0].Args[0] = dir
					cfg.TurnStart[0].Env["HOOK_ENV"] = dir
					cfg.TurnStart[2].Args[0] = dir
					cfg.PreToolUse[0].Hooks[0].Args[0] = dir
					*cfg.PreToolUse[0].PreemptYolo = false
					cfg.SessionStart = append(cfg.SessionStart, hooks.Hook{
						Type: hooks.HookTypeBuiltin, Command: builtins.AddContext, Args: []string{dir},
					})
				})
				r, err := NewLocalRuntime(t.Context(), tm, WithWorkingDir(dir), WithModelStore(mockModelStore{}), WithAutoInjector(injector))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, r.Close()) })

				require.NotNil(t, effective)
				assert.NotSame(t, original, effective)
				assert.Len(t, effective.TurnStart, 3, "user hook, add_date, and add_prompt_files")
				assert.Len(t, effective.SessionEnd, 1, "large-result cleanup")
				assert.Len(t, effective.ToolInputTransform, 1, "redaction")
				assert.Len(t, effective.BeforeLLMCall, 1, "redaction")
				assert.Len(t, effective.ToolResponseTransform, 2, "redaction and large-result limiter")
				assert.Len(t, effective.SessionStart, 1, "one injection per runtime")
				assert.Equal(t, dir, effective.TurnStart[0].Args[0])
				assert.Equal(t, dir, effective.TurnStart[0].Env["HOOK_ENV"])
				assert.Equal(t, dir, effective.TurnStart[2].Args[0])
				assert.Equal(t, dir, effective.PreToolUse[0].Hooks[0].Args[0])

				a, err := tm.DefaultAgent()
				require.NoError(t, err)
				result, err := r.hooksExec(a).Dispatch(t.Context(), hooks.EventSessionStart, &hooks.Input{Cwd: dir})
				require.NoError(t, err)
				assert.Equal(t, dir, result.AdditionalContext, "other runtimes' injections must not leak")
			}

			build(t, tm, t.TempDir())
			for i := range 4 {
				t.Run(fmt.Sprintf("workspace-%d", i), func(t *testing.T) {
					if concurrent {
						t.Parallel()
					}
					dir := t.TempDir()
					workspace, err := tm.ForWorkingDir(t.Context(), dir)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, workspace.Dispose(context.WithoutCancel(t.Context()))) })
					build(t, workspace, dir)
				})
			}
			// Cleanup runs after the parallel workspace subtests have finished.
			t.Cleanup(func() {
				after, err := json.Marshal(original)
				require.NoError(t, err)
				assert.JSONEq(t, string(before), string(after), "runtime setup must not mutate the shared source")
				assert.Equal(t, []string{"PROMPT.md"}, a.AddPromptFiles(), "injected arguments must not alias the agent's prompt files")
			})
		})
	}
}

type hooksConfigInjector func(*hooks.Config)

func (f hooksConfigInjector) AutoInject(cfg *hooks.Config) { f(cfg) }
