package session

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// ---- helpers ----------------------------------------------------------------

func ownCountMsg(role chat.MessageRole) Item {
	return NewMessageItem(&Message{Message: chat.Message{Role: role, Content: "x"}})
}

// ---- edge-case tests --------------------------------------------------------

func TestOwnMessageCount_Nil(t *testing.T) {
	t.Parallel()
	s := &Session{}
	assert.Equal(t, 0, s.OwnMessageCount())
	assert.Equal(t, len(s.OwnMessages()), s.OwnMessageCount())
}

func TestOwnMessageCount_EmptyItems(t *testing.T) {
	t.Parallel()
	s := &Session{Messages: []Item{{}, {}, {}}}
	assert.Equal(t, 0, s.OwnMessageCount())
	assert.Equal(t, len(s.OwnMessages()), s.OwnMessageCount())
}

func TestOwnMessageCount_AllRoles(t *testing.T) {
	t.Parallel()
	s := &Session{Messages: []Item{
		ownCountMsg(chat.MessageRoleUser),
		ownCountMsg(chat.MessageRoleAssistant),
		ownCountMsg(chat.MessageRoleTool),
		ownCountMsg(chat.MessageRoleSystem), // excluded
	}}
	assert.Equal(t, 3, s.OwnMessageCount(), "system messages must not be counted")
	assert.Equal(t, len(s.OwnMessages()), s.OwnMessageCount())
}

func TestOwnMessageCount_SystemExcluded(t *testing.T) {
	t.Parallel()
	s := &Session{Messages: []Item{
		ownCountMsg(chat.MessageRoleSystem),
		ownCountMsg(chat.MessageRoleSystem),
	}}
	assert.Equal(t, 0, s.OwnMessageCount())
	assert.Equal(t, len(s.OwnMessages()), s.OwnMessageCount())
}

func TestOwnMessageCount_ErrorsAndSummaries(t *testing.T) {
	t.Parallel()
	s := &Session{Messages: []Item{
		NewErrorItem(&Error{Message: "boom"}),
		{Summary: "compacted", FirstKeptEntry: 1},
		NewTerminationItem(&Termination{Reason: TerminationReasonBudgetExceeded}),
		ownCountMsg(chat.MessageRoleUser),
		ownCountMsg(chat.MessageRoleAssistant),
	}}
	assert.Equal(t, 2, s.OwnMessageCount())
	assert.Equal(t, len(s.OwnMessages()), s.OwnMessageCount())
}

func TestOwnMessageCount_SubSessionExcluded(t *testing.T) {
	t.Parallel()
	child := &Session{Messages: []Item{
		ownCountMsg(chat.MessageRoleAssistant),
		ownCountMsg(chat.MessageRoleAssistant),
	}}
	s := &Session{Messages: []Item{
		ownCountMsg(chat.MessageRoleUser),
		NewSubSessionItem(child),
	}}
	// Sub-session messages do NOT count — only direct messages in s.
	assert.Equal(t, 1, s.OwnMessageCount())
	assert.Equal(t, len(s.OwnMessages()), s.OwnMessageCount())
}

func TestOwnMessageCount_Mixed(t *testing.T) {
	t.Parallel()
	child := &Session{Messages: []Item{ownCountMsg(chat.MessageRoleAssistant)}}
	s := &Session{Messages: []Item{
		ownCountMsg(chat.MessageRoleUser),
		ownCountMsg(chat.MessageRoleAssistant),
		ownCountMsg(chat.MessageRoleSystem), // excluded
		NewErrorItem(&Error{Message: "e"}),
		NewSubSessionItem(child), // sub-session not counted
		{Summary: "sum", FirstKeptEntry: 2},
		ownCountMsg(chat.MessageRoleTool),
	}}
	assert.Equal(t, 3, s.OwnMessageCount())
	assert.Equal(t, len(s.OwnMessages()), s.OwnMessageCount())
}

// ---- MessageCount divergence: proves MessageCount is NOT a substitute -------

func TestOwnMessageCount_NotEquivalentToMessageCount(t *testing.T) {
	t.Parallel()
	s := &Session{Messages: []Item{
		ownCountMsg(chat.MessageRoleUser),
		ownCountMsg(chat.MessageRoleSystem),
		ownCountMsg(chat.MessageRoleAssistant),
	}}
	assert.Equal(t, 3, s.MessageCount(), "MessageCount includes system")
	assert.Equal(t, 2, s.OwnMessageCount(), "OwnMessageCount excludes system")
}

// ---- randomized equivalence -------------------------------------------------

var ownCountRoles = []chat.MessageRole{
	chat.MessageRoleUser, chat.MessageRoleAssistant,
	chat.MessageRoleTool, chat.MessageRoleSystem,
}

func ownCountRandomSession(r *rand.Rand, depth, maxItems int) *Session {
	s := &Session{ID: fmt.Sprintf("s-%d", r.Int64())}
	n := r.IntN(maxItems + 1)
	for range n {
		switch k := r.IntN(8); {
		case k <= 3:
			role := ownCountRoles[r.IntN(len(ownCountRoles))]
			s.Messages = append(s.Messages, ownCountMsg(role))
		case k == 4:
			s.Messages = append(s.Messages, Item{})
		case k == 5:
			if depth > 0 {
				s.Messages = append(s.Messages, NewSubSessionItem(ownCountRandomSession(r, depth-1, maxItems)))
			}
		case k == 6:
			s.Messages = append(s.Messages, NewErrorItem(&Error{Message: "e"}))
		default:
			s.Messages = append(s.Messages, Item{Summary: "s", FirstKeptEntry: 1})
		}
	}
	return s
}

func TestOwnMessageCount_Equivalence(t *testing.T) {
	t.Parallel()
	sawSystemDivergence := false
	for seed := range int64(3000) {
		r := rand.New(rand.NewPCG(uint64(seed), 11))
		s := ownCountRandomSession(r, 2, 16)
		require.Equalf(t, len(s.OwnMessages()), s.OwnMessageCount(), "seed=%d", seed)
		if s.MessageCount() != s.OwnMessageCount() {
			sawSystemDivergence = true
		}
	}
	// Confirms MessageCount() is not a valid substitute (it includes system messages).
	assert.True(t, sawSystemDivergence)
}

// ---- benchmark: proves allocations removed ----------------------------------

func BenchmarkOwnMessageCount(b *testing.B) {
	payload := "tool output line\n"
	mk := func(role chat.MessageRole, i int) Item {
		m := &Message{AgentName: "root", Message: chat.Message{Role: role, Content: payload}}
		m.Message.ToolCalls = []tools.ToolCall{{ID: fmt.Sprintf("tc-%d", i)}}
		m.Message.MultiContent = []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: payload}}
		m.Message.Usage = &chat.Usage{InputTokens: 10}
		return NewMessageItem(m)
	}

	sizes := []int{50, 300}
	for _, n := range sizes {
		s := &Session{ID: "bench"}
		for i := range n {
			role := chat.MessageRoleUser
			switch i % 3 {
			case 1:
				role = chat.MessageRoleAssistant
			case 2:
				role = chat.MessageRoleTool
			}
			s.Messages = append(s.Messages, mk(role, i))
		}
		// add a sub-session to confirm it's not traversed
		sub := &Session{ID: "sub"}
		for j := range 40 {
			sub.Messages = append(sub.Messages, mk(chat.MessageRoleAssistant, j))
		}
		s.Messages = append(s.Messages, NewSubSessionItem(sub))

		b.Run(fmt.Sprintf("n=%d/len_OwnMessages", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = len(s.OwnMessages())
			}
		})
		b.Run(fmt.Sprintf("n=%d/OwnMessageCount", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = s.OwnMessageCount()
			}
		})
	}
}
