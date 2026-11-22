package messages

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func workProjectionMessages(typesList ...types.MessageType) []*types.Message {
	items := make([]*types.Message, len(typesList))
	for i, typ := range typesList {
		items[i] = &types.Message{Type: typ}
	}
	return items
}

// withProse gives the given rows visible prose.
func withProse(items []*types.Message, sources ...int) []*types.Message {
	for _, source := range sources {
		items[source].Content = "answer"
	}
	return items
}

func project(items []*types.Message, visible tuimessages.VisibleReplies) workProjection {
	return projectWorkWithVisibleReplies(items, visible, resolveSessionOpener(items, nil), nil)
}

func TestProjectWorkVisibleRepliesLimitsProseAnswersPerExchange(t *testing.T) {
	// user, then seven prose answers, each separated by a tool call.
	typesList := []types.MessageType{types.MessageTypeUser}
	var answers []int
	for range 7 {
		answers = append(answers, len(typesList))
		typesList = append(typesList, types.MessageTypeAssistant, types.MessageTypeToolCall)
	}
	for _, test := range []struct {
		name    string
		policy  tuimessages.VisibleReplies
		visible int
	}{
		{name: "latest", policy: tuimessages.VisibleRepliesLatest, visible: 1},
		{name: "three", policy: tuimessages.VisibleRepliesThree, visible: 3},
		{name: "five", policy: tuimessages.VisibleRepliesFive, visible: 5},
		{name: "all", policy: tuimessages.VisibleRepliesAll, visible: 7},
		{name: "empty defaults to three", visible: 3},
		{name: "unknown defaults to three", policy: tuimessages.VisibleReplies("future"), visible: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			items := withProse(workProjectionMessages(typesList...), answers...)
			projection := project(items, test.policy)
			require.Len(t, projection.exchanges, 1)
			exchange := projection.exchanges[0]
			wantResponse := answers[len(answers)-test.visible:]
			require.Equal(t, wantResponse, exchange.response, "the newest prose answers stay visible")
			for source := 1; source < len(items); source++ {
				if slices.Contains(wantResponse, source) {
					require.NotContains(t, projection.bySource, source)
					continue
				}
				require.Equal(t, 0, projection.bySource[source], "row %d folds into Work", source)
			}
		})
	}
}

func TestProjectWorkVisibleRepliesCountsGroupsNotParts(t *testing.T) {
	items := withProse(workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant, // second part of the first answer
		types.MessageTypeToolCall,
		types.MessageTypeAssistant,
		types.MessageTypeAssistantReasoningBlock,
		types.MessageTypeToolCall,
		types.MessageTypeAssistant, // one answer made of three parts
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
	), 1, 2, 4, 7, 8, 9)
	projection := project(items, tuimessages.VisibleRepliesLatest)
	require.Equal(t, []int{1, 2, 3, 4, 5, 6}, projection.exchanges[0].work)
	require.Equal(t, []int{7, 8, 9}, projection.exchanges[0].response, "one visible answer is three parts")
}

func TestProjectWorkVisibleRepliesPinsOpenerOutsideTheLimit(t *testing.T) {
	items := withProse(workProjectionMessages(
		types.MessageTypeAssistant,
		types.MessageTypeToolCall,
		types.MessageTypeAssistant,
		types.MessageTypeToolCall,
		types.MessageTypeAssistant,
		types.MessageTypeToolCall,
		types.MessageTypeAssistant,
	), 0, 2, 4, 6)
	projection := project(items, tuimessages.VisibleRepliesLatest)
	require.Equal(t, []int{0, 6}, projection.exchanges[0].response, "the opener and the newest answer stay visible")
	require.Equal(t, []int{1, 2, 3, 4, 5}, projection.exchanges[0].work)
}

func TestProjectWorkVisibleRepliesResetsAtHumanExchanges(t *testing.T) {
	items := withProse(workProjectionMessages(
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
	), 0, 1, 3, 5, 6)
	projection := project(items, tuimessages.VisibleRepliesLatest)
	require.Len(t, projection.exchanges, 3)
	require.Equal(t, []int{0, 1}, projection.exchanges[0].response, "the opener and the answer before the first human row stay visible")
	require.Equal(t, []int{3}, projection.exchanges[1].response)
	require.Equal(t, []int{5, 6}, projection.exchanges[2].response, "a multipart answer is one reply")
}

func TestProjectWorkAnswersWithoutProseAlwaysStayVisible(t *testing.T) {
	items := workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeToolCall,
		types.MessageTypeAssistant,
	)
	projection := project(items, tuimessages.VisibleRepliesLatest)
	require.Equal(t, []int{1, 3}, projection.exchanges[0].response)
	require.Equal(t, []int{2}, projection.exchanges[0].work)
}

func TestProjectWorkLeavesInteractionsAndTerminalMessagesOutside(t *testing.T) {
	items := withProse(workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeToolCall,
		types.MessageTypeError,
		types.MessageTypeCancelled,
	), 1)
	items[2].ToolStatus = types.ToolStatusConfirmation
	original := make([]types.Message, len(items))
	for i, item := range items {
		original[i] = *item
	}
	projection := project(items, tuimessages.VisibleRepliesThree)
	require.Equal(t, []int{1}, projection.exchanges[0].response)
	require.Empty(t, projection.exchanges[0].work)
	require.NotContains(t, projection.bySource, 2, "confirmation prompts stay in the transcript")
	require.NotContains(t, projection.bySource, 3, "terminal errors remain outside compact Work")
	require.NotContains(t, projection.bySource, 4, "cancellations remain outside compact Work")
	for i, item := range items {
		require.Equal(t, original[i], *item, "projection leaves source content and metadata untouched")
	}
}

func TestProjectWorkResultsDoNotSplitAnswerGroups(t *testing.T) {
	items := withProse(workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeShellOutput,
		types.MessageTypeToolResult,
		types.MessageTypeAssistant,
	), 1, 4)
	projection := project(items, tuimessages.VisibleRepliesLatest)
	require.Len(t, projection.exchanges, 1)
	// Results do not split an answer group: both answers are one visible reply around the Work rows.
	require.Equal(t, []int{2, 3}, projection.exchanges[0].work)
	require.Equal(t, []int{1, 4}, projection.exchanges[0].response)
}

func TestProjectWorkWithoutMessagesHasNoExchanges(t *testing.T) {
	got := project(nil, tuimessages.VisibleRepliesThree)
	require.Empty(t, got.exchanges)
	require.Empty(t, got.bySource)
}

// Adapted from the source fixtures of the same meaning. RuntimeNotice rows do not
// exist upstream, so the result boundary (ToolResult) stands in for them.

func TestProjectWorkVisibleRepliesWorkArrivalsDoNotEvictLatestAnswer(t *testing.T) {
	items := withProse(workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistantReasoningBlock,
		types.MessageTypeToolCall,
		types.MessageTypeToolResult,
		types.MessageTypeAssistant,
	), 1, 5)
	projection := project(items, tuimessages.VisibleRepliesLatest)
	require.Equal(t, []int{1, 2, 3, 4}, projection.exchanges[0].work)
	require.Equal(t, []int{5}, projection.exchanges[0].response, "work does not consume a reply slot or demote the active answer")
}

func TestProjectWorkVisibleRepliesRetainsNewestAnswerUntilAnotherAnswer(t *testing.T) {
	items := withProse(workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistantReasoningBlock,
		types.MessageTypeToolCall,
		types.MessageTypeToolResult,
	), 1)
	projection := project(items, tuimessages.VisibleRepliesLatest)
	require.Equal(t, []int{2, 3, 4}, projection.exchanges[0].work)
	require.Equal(t, []int{1}, projection.exchanges[0].response, "work does not evict a reply slot")
}

func TestProjectWorkVisibleRepliesIgnoresEmptyAndMediaOnlyReplyGroups(t *testing.T) {
	items := withProse(workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeToolCall,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
		types.MessageTypeToolCall,
		types.MessageTypeAssistant,
	), 1, 6)
	items[4].AssistantMedia = []types.AssistantMedia{{Fallback: "generated image"}}
	projection := project(items, tuimessages.VisibleRepliesLatest)
	require.Equal(t, []int{1, 2, 5}, projection.exchanges[0].work, "the older prose answer folds with its activity")
	require.Equal(t, []int{3, 4, 6}, projection.exchanges[0].response, "empty and media-only groups stay visible without consuming the prose slot")
}

func TestProjectWorkVisibleRepliesKeepsEmptyPartWithItsProseGroup(t *testing.T) {
	items := withProse(workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
	), 2)
	projection := project(items, tuimessages.VisibleRepliesLatest)
	require.Equal(t, []int{1, 2}, projection.exchanges[0].response, "non-prose parts remain attached to the same answer group")
}

func TestProjectWorkKeepsIdentifiedReplyPartsAcrossWork(t *testing.T) {
	items := withProse(workProjectionMessages(types.MessageTypeUser, types.MessageTypeAssistant, types.MessageTypeToolCall, types.MessageTypeAssistant, types.MessageTypeAssistant), 1, 3, 4)
	items[1].MessageID, items[3].MessageID, items[4].MessageID = "one", "one", "two"
	got := project(items, tuimessages.VisibleRepliesLatest)
	require.Equal(t, []int{1, 2, 3}, got.exchanges[0].work)
	require.Equal(t, []int{4}, got.exchanges[0].response)
	items[4].MessageID = "one"
	got = project(items, tuimessages.VisibleRepliesLatest)
	require.Equal(t, []int{2}, got.exchanges[0].work)
	require.Equal(t, []int{1, 3, 4}, got.exchanges[0].response)
}
