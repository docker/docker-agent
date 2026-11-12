package runtime

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/datetime"
	"github.com/docker/docker-agent/pkg/tools/codemode"
)

func TestRunStream_RepeatedDatetimeReads(t *testing.T) {
	t.Parallel()

	for _, codeMode := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			args   string
			failed bool
			denied bool
		}{
			{name: "valid", args: `{"format":"2006-01-02T15:04:05.999999999Z07:00","timezone":"UTC"}`},
			{name: "missing format", args: `{}`, failed: true},
			{name: "invalid timezone", args: `{"format":"2006-01-02","timezone":"Not/A_Zone"}`, failed: true},
			{name: "malformed arguments", args: `{"format":`, failed: true},
			{name: "denied", args: `{"format":"2006-01-02"}`, failed: true, denied: true},
		} {
			t.Run(fmt.Sprintf("%s/code_mode=%t", tc.name, codeMode), func(t *testing.T) {
				t.Parallel()

				var streams []chat.MessageStream
				for i := range 6 {
					id := fmt.Sprintf("clock_%d", i)
					streams = append(streams, newStreamBuilder().
						AddToolCallName(id, datetime.ToolNameGetDatetime).
						AddToolCallArguments(id, tc.args).
						AddToolCallStopWithUsage(2, 2).Build())
				}
				streams = append(streams, newStreamBuilder().AddContent("checked six times").AddStopWithUsage(2, 2).Build())
				prov := &queueProvider{id: "test/mock-model", streams: streams}
				ts := tools.ToolSet(datetime.New())
				if codeMode {
					ts = codemode.Wrap(ts)
				}
				root := agent.New("root", "clock", agent.WithModel(prov), agent.WithToolSets(ts))
				rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, rt.Close()) })
				sess := session.New(session.WithUserMessage("check the clock"), session.WithToolsApproved(true))
				sess.Title = "Unit Test"
				if tc.denied {
					sess.Permissions = &session.PermissionsConfig{Deny: []string{datetime.ToolNameGetDatetime}}
				}

				var failures []*ErrorEvent
				responses := 0
				for event := range rt.RunStream(t.Context(), sess) {
					if e, ok := event.(*ErrorEvent); ok {
						failures = append(failures, e)
					}
					if e, ok := event.(*ToolCallResponseEvent); ok {
						responses++
						require.NotNil(t, e.Result)
						assert.Equal(t, tc.failed, e.Result.IsError)
						if !tc.failed {
							_, err := time.Parse(time.RFC3339Nano, e.Result.Output)
							require.NoError(t, err)
						}
					}
				}
				if tc.failed {
					require.Len(t, failures, 1)
					assert.Equal(t, ErrorCodeLoopDetected, failures[0].Code)
					assert.Equal(t, 5, responses)
					assert.Len(t, prov.streams, 2)
				} else {
					assert.Empty(t, failures)
					assert.Equal(t, 6, responses)
					assert.Equal(t, "checked six times", sess.GetLastAssistantMessageContent())
					assert.Empty(t, prov.streams)
				}
			})
		}
	}
}
