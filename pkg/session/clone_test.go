package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestClone_NilSession(t *testing.T) {
	t.Parallel()
	var s *Session
	assert.Nil(t, s.Clone())
}

func TestClone_CopiesScalarFields(t *testing.T) {
	t.Parallel()
	orig := &Session{
		ID:                      "sess-1",
		Title:                   "title",
		ToolsApproved:           true,
		NonInteractive:          true,
		HideToolResults:         true,
		WorkingDir:              "/work",
		SendUserMessage:         true,
		MaxIterations:           7,
		MaxConsecutiveToolCalls: 3,
		MaxOldToolCallTokens:    99,
		MaxToolResultTokens:     44,
		Starred:                 true,
		InputTokens:             11,
		OutputTokens:            22,
		Cost:                    1.5,
		Permissions:             &PermissionsConfig{Allow: []string{"a"}, Ask: []string{"k"}, Deny: []string{"d"}},
		AgentModelOverrides:     map[string]string{"root": "openai/gpt-4o"},
		CustomModelsUsed:        []string{"openai/gpt-4o"},
		AttachedFiles:           []string{"/abs/path.txt"},
		ExcludedTools:           []string{"run_skill"},
		DisableStructuredOutput: true,
		AgentName:               "root",
		ParentID:                "parent",
		DelegationLineage:       []string{"coordinator"},
	}
	orig.AddMessage(UserMessage("hello"))

	clone := orig.Clone()
	require.NotNil(t, clone)

	// Unlike BranchSession, Clone keeps the original identity and history.
	assert.Equal(t, "sess-1", clone.ID)
	assert.Equal(t, "title", clone.Title)
	assert.True(t, clone.ToolsApproved)
	assert.True(t, clone.NonInteractive)
	assert.True(t, clone.HideToolResults)
	assert.Equal(t, "/work", clone.WorkingDir)
	assert.Equal(t, 7, clone.MaxIterations)
	assert.Equal(t, 3, clone.MaxConsecutiveToolCalls)
	assert.Equal(t, 99, clone.MaxOldToolCallTokens)
	assert.Equal(t, 44, clone.MaxToolResultTokens)
	assert.True(t, clone.Starred)
	assert.Equal(t, int64(11), clone.InputTokens)
	assert.Equal(t, int64(22), clone.OutputTokens)
	assert.InEpsilon(t, 1.5, clone.Cost, 1e-9)
	assert.True(t, clone.DisableStructuredOutput)
	assert.Equal(t, "root", clone.AgentName)
	assert.Equal(t, "parent", clone.ParentID)
	assert.Equal(t, []string{"coordinator"}, clone.DelegationLineage)
	clone.DelegationLineage[0] = "mutated"
	assert.Equal(t, []string{"coordinator"}, orig.DelegationLineage)
	assert.Equal(t, "hello", clone.GetLastUserMessageContent())
	require.NotNil(t, clone.Permissions)
	assert.Equal(t, []string{"a"}, clone.Permissions.Allow)
	assert.Equal(t, []string{"k"}, clone.Permissions.Ask)
	assert.Equal(t, []string{"d"}, clone.Permissions.Deny)
}

func TestClone_DeepCopiesEvalFields(t *testing.T) {
	t.Parallel()
	orig := &Session{
		Evals: &EvalCriteria{
			Relevance:  []string{"is helpful"},
			WorkingDir: "work",
		},
		EvalResult: &EvalResult{
			Passed:    true,
			Successes: []string{"ok"},
			Failures:  []string{"missing"},
			Checks: EvalResultChecks{
				Size: &SizeCheck{Passed: true, Actual: "S", Expected: "M"},
				Relevance: &RelevanceCheck{Results: []RelevanceCriterionResult{{
					Criterion: "is helpful",
					Passed:    true,
				}}},
			},
		},
	}

	clone := orig.Clone()
	require.NotNil(t, clone)

	clone.Evals.Relevance[0] = "mutated"
	clone.EvalResult.Successes[0] = "mutated"
	clone.EvalResult.Failures[0] = "mutated"
	clone.EvalResult.Checks.Size.Actual = "XL"
	clone.EvalResult.Checks.Relevance.Results[0].Criterion = "mutated"

	assert.Equal(t, "is helpful", orig.Evals.Relevance[0])
	assert.Equal(t, "ok", orig.EvalResult.Successes[0])
	assert.Equal(t, "missing", orig.EvalResult.Failures[0])
	assert.Equal(t, "S", orig.EvalResult.Checks.Size.Actual)
	assert.Equal(t, "is helpful", orig.EvalResult.Checks.Relevance.Results[0].Criterion)
}

func TestClone_DeepCopiesMessagesAndConfig(t *testing.T) {
	t.Parallel()
	orig := &Session{
		ID:                  "sess-1",
		Permissions:         &PermissionsConfig{Allow: []string{"a"}, Ask: []string{"k"}},
		AgentModelOverrides: map[string]string{"root": "m1"},
		CustomModelsUsed:    []string{"m1"},
		AttachedFiles:       []string{"/abs/a.txt"},
	}
	destructive := true
	orig.AddMessage(&Message{Message: chat.Message{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{{
			Type:     chat.MessagePartTypeImageURL,
			ImageURL: &chat.MessageImageURL{URL: "http://orig"},
		}},
		ToolDefinitions: []tools.Tool{{
			Name: "read_file",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
			},
			OutputSchema: map[string]any{"type": "string"},
			Annotations:  tools.ToolAnnotations{DestructiveHint: &destructive},
		}},
	}})

	clone := orig.Clone()
	require.NotNil(t, clone)

	// Mutate the clone's deep-copied structures; the original must not change.
	clone.Permissions.Allow[0] = "mutated"
	clone.Permissions.Ask[0] = "mutated"
	clone.AgentModelOverrides["root"] = "mutated"
	clone.CustomModelsUsed[0] = "mutated"
	clone.AttachedFiles[0] = "/abs/mutated.txt"
	clone.Messages[0].Message.Message.MultiContent[0].ImageURL.URL = "http://mutated"
	cloneParams := clone.Messages[0].Message.Message.ToolDefinitions[0].Parameters.(map[string]any)
	cloneParams["type"] = "mutated"
	cloneNestedParams := cloneParams["properties"].(map[string]any)["path"].(map[string]any)
	cloneNestedParams["type"] = "mutated"
	cloneOutputSchema := clone.Messages[0].Message.Message.ToolDefinitions[0].OutputSchema.(map[string]any)
	cloneOutputSchema["type"] = "mutated"
	*clone.Messages[0].Message.Message.ToolDefinitions[0].Annotations.DestructiveHint = false

	origParams := orig.Messages[0].Message.Message.ToolDefinitions[0].Parameters.(map[string]any)
	origNestedParams := origParams["properties"].(map[string]any)["path"].(map[string]any)
	origOutputSchema := orig.Messages[0].Message.Message.ToolDefinitions[0].OutputSchema.(map[string]any)

	assert.Equal(t, "a", orig.Permissions.Allow[0])
	assert.Equal(t, "k", orig.Permissions.Ask[0])
	assert.Equal(t, "m1", orig.AgentModelOverrides["root"])
	assert.Equal(t, "m1", orig.CustomModelsUsed[0])
	assert.Equal(t, "/abs/a.txt", orig.AttachedFiles[0])
	assert.Equal(t, "http://orig", orig.Messages[0].Message.Message.MultiContent[0].ImageURL.URL)
	assert.Equal(t, "object", origParams["type"])
	assert.Equal(t, "string", origNestedParams["type"])
	assert.Equal(t, "string", origOutputSchema["type"])
	assert.True(t, *orig.Messages[0].Message.Message.ToolDefinitions[0].Annotations.DestructiveHint)
}

func TestClone_AppendingDoesNotAffectOriginal(t *testing.T) {
	t.Parallel()
	orig := New()
	orig.AddMessage(UserMessage("first"))

	clone := orig.Clone()
	clone.AddMessage(UserMessage("second"))

	assert.Equal(t, 1, orig.MessageCount())
	assert.Equal(t, 2, clone.MessageCount())
	assert.Equal(t, "first", orig.GetLastUserMessageContent())
	assert.Equal(t, "second", clone.GetLastUserMessageContent())
}

func TestClone_PreservesSubSessionAndSummary(t *testing.T) {
	t.Parallel()
	sub := New()
	sub.AddMessage(UserMessage("sub message"))
	sub.AddMessage(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "done", Cost: 0.05}})

	orig := New()
	orig.AddMessage(UserMessage("first"))
	orig.AddLiveSubSession(sub)
	orig.Messages = append(orig.Messages, Item{Summary: "a summary", Cost: 0.25})

	clone := orig.Clone()
	require.Len(t, clone.Messages, 3)

	assert.Equal(t, "first", clone.Messages[0].Message.Message.Content)
	require.NotNil(t, clone.Messages[1].SubSession)
	assert.NotSame(t, sub, clone.Messages[1].SubSession)
	assert.Equal(t, "sub message", clone.Messages[1].SubSession.GetLastUserMessageContent())
	assert.Equal(t, "a summary", clone.Messages[2].Summary)
	assert.InEpsilon(t, 0.25, clone.Messages[2].Cost, 1e-9)

	// Clone preserves identity, including the live-attached provenance of an
	// AddLiveSubSession child: the child reported its own cost through its own
	// TokenUsageEvents, so the clone must keep excluding it from the embedded
	// roll-up — same ID, same live identity, same accounting.
	assert.Equal(t, sub.ID, clone.Messages[1].SubSession.ID)
	assert.Zero(t, clone.EmbeddedSubSessionCost(),
		"a live-attached child must stay excluded from the clone's embedded cost")
	assert.InDelta(t, 0.30, clone.TotalCost(), 1e-9, "the total still counts the live child")
}

// TestClone_PreservesItemValueFields guards against a clone that rebuilds
// items field-by-field and silently drops the per-item Cost / FirstKeptEntry
// that can ride alongside a message.
func TestClone_PreservesItemValueFields(t *testing.T) {
	t.Parallel()
	orig := New()
	orig.Messages = []Item{{
		Message:        UserMessage("hello"),
		Cost:           0.5,
		FirstKeptEntry: 3,
	}}

	clone := orig.Clone()
	require.Len(t, clone.Messages, 1)
	assert.Equal(t, "hello", clone.Messages[0].Message.Message.Content)
	assert.InEpsilon(t, 0.5, clone.Messages[0].Cost, 1e-9)
	assert.Equal(t, 3, clone.Messages[0].FirstKeptEntry)
}

// TestClone_DeepCopiesErrorItem guards against the clone sharing the *Error
// pointer with the original, which would let a mutation on one leak into the
// other.
func TestClone_DeepCopiesErrorItem(t *testing.T) {
	t.Parallel()
	orig := New()
	orig.AddError(&Error{Message: "boom", Code: "model_error", AgentName: "root"})

	clone := orig.Clone()
	require.Len(t, clone.Messages, 1)
	require.True(t, clone.Messages[0].IsError())
	assert.Equal(t, "boom", clone.Messages[0].Error.Message)

	clone.Messages[0].Error.Message = "mutated"
	assert.Equal(t, "boom", orig.Messages[0].Error.Message, "Error must be deep-copied")
}
