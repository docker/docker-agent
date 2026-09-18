package session

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestGetLastMessageContent_Empty(t *testing.T) {
	t.Parallel()
	s := &Session{}
	assert.Empty(t, s.GetLastAssistantMessageContent())
	assert.Empty(t, s.GetLastUserMessageContent())
}

func TestGetLastMessageContent_Simple(t *testing.T) {
	t.Parallel()
	s := &Session{Messages: []Item{
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "u1"}}),
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "a1"}}),
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "  u2  "}}),
	}}
	assert.Equal(t, "u2", s.GetLastUserMessageContent())
	assert.Equal(t, "a1", s.GetLastAssistantMessageContent())
}

func TestGetLastMessageContent_SystemFiltered(t *testing.T) {
	t.Parallel()
	s := &Session{Messages: []Item{NewMessageItem(SystemMessage("sys"))}}
	assert.Empty(t, s.getLastMessageContentByRole(chat.MessageRoleSystem))
}

// TestGetLastMessageContent_BlankStopsSearch pins that a blank match terminates the
// search — it must NOT fall back to an earlier non-blank message of the same role.
func TestGetLastMessageContent_BlankStopsSearch(t *testing.T) {
	t.Parallel()
	s := &Session{Messages: []Item{
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "earlier"}}),
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "q"}}),
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "  \n"}}),
	}}
	assert.Empty(t, s.GetLastAssistantMessageContent())
}

// TestGetLastMessageContent_BlankStopsSearchInSubSession pins the same blank-stops
// contract when the blank match is inside a child sub-session.
func TestGetLastMessageContent_BlankStopsSearchInSubSession(t *testing.T) {
	t.Parallel()
	child := &Session{Messages: []Item{
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: " "}}),
	}}
	parent := &Session{Messages: []Item{
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "parent"}}),
		NewSubSessionItem(child),
	}}
	assert.Empty(t, parent.GetLastAssistantMessageContent())
}

func TestGetLastMessageContent_NestedSubSession(t *testing.T) {
	t.Parallel()
	grandchild := &Session{Messages: []Item{
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "from-grandchild"}}),
	}}
	child := &Session{Messages: []Item{
		NewSubSessionItem(grandchild),
	}}
	parent := &Session{Messages: []Item{
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "u"}}),
		NewSubSessionItem(child),
	}}
	assert.Equal(t, "from-grandchild", parent.GetLastAssistantMessageContent())
}

// TestGetLastMessageContent_MalformedSystemPlusSub mirrors GetAllMessages' else-if
// ordering: a system-message item with a SubSession set causes recursion into the
// sub-session (system message is filtered, so the else branch fires).
func TestGetLastMessageContent_MalformedSystemPlusSub(t *testing.T) {
	t.Parallel()
	child := &Session{Messages: []Item{
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "from-child"}}),
	}}
	sysPlusSub := NewMessageItem(SystemMessage("sys"))
	sysPlusSub.SubSession = child
	s := &Session{Messages: []Item{
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "parent"}}),
		sysPlusSub,
	}}
	assert.Equal(t, "from-child", s.GetLastAssistantMessageContent())
}

// TestGetLastMessageContent_MalformedNonSystemPlusSub mirrors GetAllMessages' else-if
// ordering: a non-system message item with a SubSession set does NOT recurse.
func TestGetLastMessageContent_MalformedNonSystemPlusSub(t *testing.T) {
	t.Parallel()
	child := &Session{Messages: []Item{
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "from-child"}}),
	}}
	userPlusSub := NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "u"}})
	userPlusSub.SubSession = child
	s := &Session{Messages: []Item{
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "parent"}}),
		userPlusSub,
	}}
	assert.Equal(t, "parent", s.GetLastAssistantMessageContent())
}

// TestGetLastMessageContent_Concurrent confirms no data races or deadlocks under
// concurrent reads and writes on a parent->child->grandchild tree. Run with -race.
func TestGetLastMessageContent_Concurrent(t *testing.T) {
	t.Parallel()

	grandchild := &Session{ID: "gc"}
	child := &Session{ID: "child"}
	child.AddSubSession(grandchild)
	parent := &Session{ID: "parent"}
	parent.AddSubSession(child)

	const iterations = 500
	start := make(chan struct{})
	var wg sync.WaitGroup
	writer := func(s *Session, role chat.MessageRole) {
		defer wg.Done()
		<-start
		for i := range iterations {
			content := fmt.Sprintf("%s-%d", role, i)
			s.AddMessage(&Message{Message: chat.Message{Role: role, Content: content}})
			if i%17 == 0 {
				s.ApplyCompaction(1, 1, Item{Summary: "compact"})
			}
		}
	}
	reader := func(s *Session) {
		defer wg.Done()
		<-start
		for range iterations {
			_ = s.GetLastAssistantMessageContent()
			_ = s.GetLastUserMessageContent()
		}
	}
	for _, s := range []*Session{parent, child, grandchild} {
		wg.Add(4)
		go writer(s, chat.MessageRoleUser)
		go writer(s, chat.MessageRoleAssistant)
		go reader(s)
		go reader(parent)
	}
	close(start)

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: goroutines did not finish")
	}
}

// TestGetLastMessageContent_Equivalence compares the new implementation against
// the GetAllMessages-based baseline on 3000 random session trees.
func TestGetLastMessageContent_Equivalence(t *testing.T) {
	t.Parallel()
	roles := []chat.MessageRole{
		chat.MessageRoleUser, chat.MessageRoleAssistant,
		chat.MessageRoleSystem, chat.MessageRoleTool, "bogus",
	}
	for seed := range int64(3000) {
		s := lmcRandomTree(int(seed), 3, 12)
		for _, role := range roles {
			want := lmcBaseline(s, role)
			got := s.getLastMessageContentByRole(role)
			require.Equalf(t, want, got, "seed=%d role=%s", seed, role)
		}
	}
}

func lmcBaseline(s *Session, role chat.MessageRole) string {
	messages := s.GetAllMessages()
	for _, message := range slices.Backward(messages) {
		if message.Message.Role == role {
			return strings.TrimSpace(message.Message.Content)
		}
	}
	return ""
}

func lmcRandomTree(seed, maxDepth, maxItems int) *Session {
	rng := newLmcRng(int64(seed))
	return lmcBuildTree(rng, maxDepth, maxItems)
}

type lmcRng struct{ state int64 }

func newLmcRng(seed int64) *lmcRng {
	return &lmcRng{state: seed*6364136223846793005 + 1442695040888963407}
}

func (r *lmcRng) next() int64 {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return r.state
}
func (r *lmcRng) intn(n int) int { return int(uint64(r.next())>>1) % n }

var (
	lmcRoles    = []chat.MessageRole{chat.MessageRoleUser, chat.MessageRoleAssistant, chat.MessageRoleSystem, chat.MessageRoleTool}
	lmcContents = []string{"", " ", "\n\t ", "hello", "  padded  ", "multi\nline", "x"}
)

func lmcMsg(rng *lmcRng, role chat.MessageRole) *Message {
	m := &Message{
		AgentName: "a",
		Message: chat.Message{
			Role:    role,
			Content: lmcContents[rng.intn(len(lmcContents))],
		},
	}
	if rng.intn(4) == 0 {
		m.Message.ToolCalls = []tools.ToolCall{{ID: "tc"}}
	}
	return m
}

func lmcBuildTree(rng *lmcRng, depth, maxItems int) *Session {
	s := &Session{}
	n := rng.intn(maxItems + 1)
	for range n {
		var item Item
		switch k := rng.intn(13); {
		case k <= 3:
			item = NewMessageItem(lmcMsg(rng, lmcRoles[rng.intn(len(lmcRoles))]))
		case k == 4:
			item = Item{}
		case k == 5 || k == 6:
			if depth > 0 {
				item = NewSubSessionItem(lmcBuildTree(rng, depth-1, maxItems))
			} else {
				item = NewMessageItem(lmcMsg(rng, chat.MessageRoleAssistant))
			}
		case k == 7:
			item = NewErrorItem(&Error{Message: "boom"})
		case k == 8:
			item = Item{Summary: "summary", FirstKeptEntry: 1}
		case k == 9:
			item = NewTerminationItem(&Termination{Reason: TerminationReasonBudgetExceeded})
		case k == 10:
			item = NewMessageItem(lmcMsg(rng, chat.MessageRoleSystem))
			if depth > 0 {
				item.SubSession = lmcBuildTree(rng, depth-1, maxItems)
			}
		case k == 11:
			item = NewMessageItem(lmcMsg(rng, lmcRoles[rng.intn(2)]))
			if depth > 0 {
				item.SubSession = lmcBuildTree(rng, depth-1, maxItems)
			}
		default:
			item = NewMessageItem(lmcMsg(rng, chat.MessageRoleUser))
			item.Error = &Error{Message: "e"}
		}
		s.Messages = append(s.Messages, item)
	}
	return s
}

func BenchmarkGetLastAssistantMessageContent(b *testing.B) {
	shapes := []struct {
		name               string
		own, subs, subMsgs int
	}{
		{"small_50", 50, 0, 0},
		{"medium_300", 300, 5, 40},
		{"large_2000", 2000, 20, 100},
	}

	for _, sh := range shapes {
		s := lmcBenchTree(sh.own, sh.subs, sh.subMsgs)
		b.Run(sh.name+"/baseline_GetAllMessages", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = lmcBaseline(s, chat.MessageRoleAssistant)
			}
		})
		b.Run(sh.name+"/reverse_walk", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = s.GetLastAssistantMessageContent()
			}
		})
	}
}

func lmcBenchTree(ownMsgs, subSessions, subMsgs int) *Session {
	s := &Session{ID: "bench"}
	payload := strings.Repeat("tool output line\n", 40)
	mk := func(role chat.MessageRole, i int) *Message {
		m := &Message{AgentName: "root", Message: chat.Message{Role: role, Content: payload, CreatedAt: "t"}}
		m.Message.ToolCalls = []tools.ToolCall{{ID: fmt.Sprintf("tc-%d", i)}}
		m.Message.Usage = &chat.Usage{InputTokens: 10}
		return m
	}
	for i := range ownMsgs {
		role := chat.MessageRoleUser
		switch i % 3 {
		case 1:
			role = chat.MessageRoleAssistant
		case 2:
			role = chat.MessageRoleTool
		}
		s.Messages = append(s.Messages, NewMessageItem(mk(role, i)))
		if subSessions > 0 && i%(ownMsgs/subSessions+1) == 0 {
			sub := &Session{ID: fmt.Sprintf("sub-%d", i)}
			for j := range subMsgs {
				sub.Messages = append(sub.Messages, NewMessageItem(mk(chat.MessageRoleAssistant, j)))
			}
			s.Messages = append(s.Messages, NewSubSessionItem(sub))
		}
	}
	s.Messages = append(s.Messages, NewMessageItem(mk(chat.MessageRoleUser, -1)))
	return s
}
