package agent

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/scheduler"
	"github.com/docker/docker-agent/pkg/tools/codemode"
)

type schedulerRecallRuntime struct {
	tools.NopRuntime

	recalls chan string
}

func (r *schedulerRecallRuntime) Supports(c tools.Capability) bool {
	return c == tools.CapabilityRecall
}

func (r *schedulerRecallRuntime) Recall(ctx context.Context, prompt string) error {
	select {
	case r.recalls <- prompt:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestAgentOwnsSchedulerRun(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"ordinary", "additional", "code_mode"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				raw := scheduler.New()
				ts := tools.WithName(raw, "scheduler")
				a := New("root", "")
				switch mode {
				case "ordinary":
					WithToolSets(ts)(a)
				case "additional":
					WithAdditionalToolSets(ts)(a)
				case "code_mode":
					WithToolSets(codemode.Wrap(ts))(a)
				}
				t.Cleanup(func() { require.NoError(t, a.StopToolSets(t.Context())) })
				ctx, cancel := context.WithCancel(t.Context())
				for _, outcome := range a.StartToolSets(ctx, time.Second) {
					require.Equal(t, tools.StartReady, (<-outcome).Kind)
				}
				cancel()
				all, err := raw.Tools(t.Context())
				require.NoError(t, err)
				rt := &schedulerRecallRuntime{recalls: make(chan string, 2)}
				result, err := all[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{
					Name: scheduler.ToolNameCreateSchedule, Arguments: `{"prompt":"check build","when":"every:1m"}`,
				}}, rt)
				require.NoError(t, err)
				require.False(t, result.IsError)
				synctest.Sleep(time.Minute)
				select {
				case prompt := <-rt.recalls:
					assert.Contains(t, prompt, "check build")
				default:
					t.Fatal("scheduler did not survive startup cancellation")
				}
				require.NoError(t, a.StopToolSets(ctx))
				synctest.Sleep(time.Minute)
				assert.Empty(t, rt.recalls)
			})
		})
	}
}
