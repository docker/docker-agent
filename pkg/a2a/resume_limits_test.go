package a2a

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dagent "github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func newA2AResumeStore(t *testing.T, backend string) *recordingStore {
	t.Helper()
	store := &recordingStore{}
	if backend == "sqlite" {
		var err error
		store.Store, err = sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
		require.NoError(t, err)
	} else {
		store.Store = session.NewInMemorySessionStore()
	}
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func TestRunDockerAgent_ResumeUsesSelectedAgentLimits(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name   string
				limits [4]int
			}{
				{name: "enabled", limits: [4]int{7, 3, 1100, 900}},
				{name: "zero", limits: [4]int{0, 0, 0, 0}},
				{name: "disabled", limits: [4]int{-1, 0, -1, -1}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					store := newA2AResumeStore(t, backend)
					_, root := newMockTeam("resumed answer")
					selected := dagent.New("analyst", "Selected agent", dagent.WithModel(root.Model(t.Context())),
						dagent.WithMaxIterations(tc.limits[0]),
						dagent.WithMaxConsecutiveToolCalls(tc.limits[1]),
						dagent.WithMaxOldToolCallTokens(tc.limits[2]),
						dagent.WithMaxToolResultTokens(tc.limits[3]),
					)
					tm := team.New(team.WithAgents(root, selected))
					workingDir := t.TempDir()
					existing := session.New(
						session.WithID("resume-limits"),
						session.WithOrigin("a2a"),
						session.WithTitle("Existing conversation"),
						session.WithUserMessage("original question"),
						session.WithWorkingDir(workingDir),
						session.WithSafetyPolicy(session.SafetyPolicyStrict),
						session.WithMaxIterations(2),
						session.WithMaxConsecutiveToolCalls(9),
						session.WithMaxOldToolCallTokens(2200),
						session.WithMaxToolResultTokens(1800),
					)
					existing.AddMessage(&session.Message{AgentName: "analyst", Message: chat.Message{
						Role: chat.MessageRoleAssistant, Content: "original answer",
					}})
					require.NoError(t, store.AddSession(t.Context(), existing))

					events := collectRunEvents(newFakeInvocationContext(t.Context(), existing.ID, "follow-up question"),
						tm, selected, store, session.SafetyPolicyBalanced)
					require.Len(t, events, 2)
					for _, event := range events {
						require.NoError(t, event.err)
						assert.Equal(t, "analyst", event.event.Author)
					}
					assert.True(t, events[1].event.TurnComplete)
					assert.Equal(t, "resumed answer", eventText(t, events[1].event))

					updated := store.updatedSessions()
					require.Len(t, updated, 1)
					resumed := updated[0]
					gotLimits := [4]int{
						resumed.MaxIterations, resumed.MaxConsecutiveToolCalls,
						resumed.MaxOldToolCallTokens, resumed.MaxToolResultTokens,
					}
					assert.Equal(t, tc.limits, gotLimits)
					assert.Equal(t, existing.ID, resumed.ID)
					assert.Equal(t, "a2a", resumed.Origin)
					assert.Equal(t, "Existing conversation", resumed.Title)
					assert.Equal(t, workingDir, resumed.WorkingDir, "resume must retain the conversation workspace")
					assert.Equal(t, session.SafetyPolicyStrict, resumed.GetSafetyPolicy())
					assert.False(t, resumed.IsToolsApproved())
					assert.True(t, resumed.NonInteractive)
					assert.Nil(t, resumed.Termination())
					messages := resumed.GetAllMessages()
					require.Len(t, messages, 4)
					for i, want := range []string{"original question", "original answer", "follow-up question", "resumed answer"} {
						assert.Equal(t, want, messages[i].Message.Content)
					}

					stored, err := store.GetSessionByOrigin(t.Context(), existing.ID, "a2a")
					require.NoError(t, err)
					assert.Equal(t, tc.limits[0], stored.MaxIterations, "replace the persisted stale iteration limit")
					assert.Equal(t, workingDir, stored.WorkingDir)
					assert.Equal(t, session.SafetyPolicyStrict, stored.GetSafetyPolicy())
				})
			}
		})
	}
}

type a2aResumePromptProvider struct {
	*mockProvider

	requests [][]chat.Message
}

func (p *a2aResumePromptProvider) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, toolList []tools.Tool) (chat.MessageStream, error) {
	p.requests = append(p.requests, messages)
	return p.mockProvider.CreateChatCompletionStream(ctx, messages, toolList)
}

func TestRunDockerAgent_ResumeAppliesToolContentLimitsBeforeProvider(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name      string
				oldTokens int
				resultCap int
			}{
				{name: "old_tool_content", oldTokens: 32},
				{name: "tool_result", resultCap: 32},
				{name: "zero"},
				{name: "disabled", oldTokens: -1, resultCap: -1},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					store := newA2AResumeStore(t, backend)
					_, root := newMockTeam("resumed answer")
					prov := &a2aResumePromptProvider{mockProvider: root.Model(t.Context()).(*mockProvider)}
					selected := dagent.New("analyst", "Selected agent", dagent.WithModel(prov),
						dagent.WithMaxOldToolCallTokens(tc.oldTokens), dagent.WithMaxToolResultTokens(tc.resultCap))
					tm := team.New(team.WithAgents(root, selected))
					existing := session.New(session.WithID("resume-tool-content"), session.WithOrigin("a2a"),
						session.WithTitle("Existing conversation"), session.WithUserMessage("original question"))
					original := "head" + strings.Repeat(" middle ", 128) + "tail"
					existing.AddMessage(&session.Message{AgentName: "analyst", Message: chat.Message{
						Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{
							ID: "read", Type: "function", Function: tools.FunctionCall{Name: "read_file", Arguments: `{}`},
						}},
					}})
					existing.AddMessage(&session.Message{AgentName: "analyst", Message: chat.Message{
						Role: chat.MessageRoleTool, ToolCallID: "read", Content: original,
					}})
					require.NoError(t, store.AddSession(t.Context(), existing))
					// Exercise the same metadata snapshot boundary as a previous runtime turn.
					require.NoError(t, store.Store.UpdateSession(t.Context(), existing))

					events := collectRunEvents(newFakeInvocationContext(t.Context(), existing.ID, "follow-up question"),
						tm, selected, store, session.SafetyPolicyBalanced)
					require.Len(t, events, 2)
					for _, event := range events {
						require.NoError(t, event.err)
					}
					require.Len(t, prov.requests, 1)
					var results []chat.Message
					for _, message := range prov.requests[0] {
						if message.Role == chat.MessageRoleTool {
							results = append(results, message)
						}
					}
					require.Len(t, results, 1)
					assert.Equal(t, "read", results[0].ToolCallID)
					switch {
					case tc.oldTokens > 0:
						assert.Equal(t, "[content truncated]", results[0].Content)
					case tc.resultCap > 0:
						assert.LessOrEqual(t, len(results[0].Content)/4, tc.resultCap)
						assert.Contains(t, results[0].Content, "tool result truncated")
						assert.True(t, strings.HasPrefix(results[0].Content, "head"))
						assert.True(t, strings.HasSuffix(results[0].Content, "tail"))
					default:
						assert.Equal(t, original, results[0].Content)
					}

					stored, err := store.GetSessionByOrigin(t.Context(), existing.ID, "a2a")
					require.NoError(t, err)
					messages := stored.GetAllMessages()
					require.GreaterOrEqual(t, len(messages), 3)
					assert.Equal(t, original, messages[2].Message.Content, "prompt truncation must not rewrite stored history")
				})
			}
		})
	}
}
