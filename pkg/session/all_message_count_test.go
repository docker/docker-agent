package session

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestAllMessageCount_Empty(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0, new(Session).AllMessageCount())
}

func TestAllMessageCount_SystemMessagesExcluded(t *testing.T) {
	t.Parallel()
	s := &Session{Messages: []Item{
		NewMessageItem(SystemMessage("sys")),
		NewMessageItem(UserMessage("u")),
	}}
	assert.Equal(t, 1, s.AllMessageCount())
	assert.Equal(t, len(s.GetAllMessages()), s.AllMessageCount())
}

func TestAllMessageCount_NonMessageItemsIgnored(t *testing.T) {
	t.Parallel()
	s := &Session{Messages: []Item{
		NewErrorItem(&Error{Message: "boom"}),
		{Summary: "summary", FirstKeptEntry: 1},
		NewTerminationItem(&Termination{Reason: TerminationReasonBudgetExceeded}),
		NewMessageItem(UserMessage("u")),
	}}
	assert.Equal(t, 1, s.AllMessageCount())
	assert.Equal(t, len(s.GetAllMessages()), s.AllMessageCount())
}

func TestAllMessageCount_NestedSessions(t *testing.T) {
	t.Parallel()
	grandchild := &Session{Messages: []Item{
		NewMessageItem(UserMessage("gc1")),
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "gc2"}}),
	}}
	child := &Session{Messages: []Item{
		NewMessageItem(UserMessage("c1")),
		NewSubSessionItem(grandchild),
	}}
	parent := &Session{Messages: []Item{
		NewMessageItem(UserMessage("p1")),
		NewSubSessionItem(child),
		NewMessageItem(SystemMessage("sys")),
	}}
	assert.Equal(t, len(parent.GetAllMessages()), parent.AllMessageCount())
	assert.Equal(t, 4, parent.AllMessageCount())
}

func TestAllMessageCount_MalformedSystemPlusSubSession(t *testing.T) {
	t.Parallel()
	child := &Session{Messages: []Item{
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "child"}}),
	}}
	item := NewMessageItem(SystemMessage("sys"))
	item.SubSession = child
	s := &Session{Messages: []Item{item}}
	assert.Equal(t, len(s.GetAllMessages()), s.AllMessageCount())
}

func TestAllMessageCount_MalformedNonSystemPlusSubSession(t *testing.T) {
	t.Parallel()
	child := &Session{Messages: []Item{
		NewMessageItem(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "child"}}),
	}}
	item := NewMessageItem(UserMessage("parent"))
	item.SubSession = child
	s := &Session{Messages: []Item{item}}
	assert.Equal(t, len(s.GetAllMessages()), s.AllMessageCount())
	assert.Equal(t, 1, s.AllMessageCount()) // sub-session not recursed
}

func TestAllMessageCount_Equivalence(t *testing.T) {
	t.Parallel()
	roles := []chat.MessageRole{
		chat.MessageRoleUser, chat.MessageRoleAssistant, chat.MessageRoleSystem, chat.MessageRoleTool,
	}
	var buildTree func(r *rand.Rand, depth, maxItems int) *Session
	buildTree = func(r *rand.Rand, depth, maxItems int) *Session {
		s := &Session{ID: fmt.Sprintf("s-%d", r.Int64())}
		n := r.IntN(maxItems + 1)
		for range n {
			var item Item
			switch k := r.IntN(11); {
			case k <= 3:
				item = NewMessageItem(&Message{Message: chat.Message{
					Role:    roles[r.IntN(len(roles))],
					Content: "x",
				}})
			case k == 4:
				item = Item{}
			case k == 5, k == 6:
				if depth > 0 {
					item = NewSubSessionItem(buildTree(r, depth-1, maxItems))
				} else {
					item = NewMessageItem(UserMessage("leaf"))
				}
			case k == 7:
				item = NewErrorItem(&Error{Message: "e"})
			case k == 8:
				item = Item{Summary: "s", FirstKeptEntry: 1}
			case k == 9:
				// malformed: system + sub-session
				item = NewMessageItem(SystemMessage("sys"))
				if depth > 0 {
					item.SubSession = buildTree(r, depth-1, maxItems)
				}
			default:
				// malformed: non-system + sub-session
				item = NewMessageItem(UserMessage("u"))
				if depth > 0 {
					item.SubSession = buildTree(r, depth-1, maxItems)
				}
			}
			s.Messages = append(s.Messages, item)
		}
		return s
	}

	for seed := range int64(3000) {
		r := rand.New(rand.NewPCG(uint64(seed), 13))
		s := buildTree(r, 3, 12)
		require.Equalf(t, len(s.GetAllMessages()), s.AllMessageCount(), "seed=%d", seed)
	}
}

func TestAllMessageCount_Concurrent(t *testing.T) {
	t.Parallel()

	const iterations = 500
	child := &Session{ID: "child"}
	parent := &Session{ID: "parent"}
	parent.AddSubSession(child)

	var wg sync.WaitGroup
	write := func(s *Session) {
		defer wg.Done()
		for i := range iterations {
			s.AddMessage(&Message{Message: chat.Message{
				Role:    chat.MessageRoleUser,
				Content: fmt.Sprintf("msg-%d", i),
			}})
		}
	}
	read := func(s *Session) {
		defer wg.Done()
		for range iterations {
			_ = s.AllMessageCount()
		}
	}

	wg.Add(4)
	go write(parent)
	go write(child)
	go read(parent)
	go read(child)
	wg.Wait()

	assert.Equal(t, len(parent.GetAllMessages()), parent.AllMessageCount())
}

func BenchmarkAllMessageCount(b *testing.B) {
	mkTree := func(own, subs, subMsgs int) *Session {
		s := &Session{ID: "bench"}
		for i := range own {
			role := chat.MessageRoleUser
			if i%2 == 0 {
				role = chat.MessageRoleAssistant
			}
			s.Messages = append(s.Messages, NewMessageItem(&Message{
				Message: chat.Message{Role: role, Content: "payload"},
			}))
		}
		for j := range subs {
			sub := &Session{ID: fmt.Sprintf("sub-%d", j)}
			for k := range subMsgs {
				sub.Messages = append(sub.Messages, NewMessageItem(&Message{
					Message: chat.Message{Role: chat.MessageRoleAssistant, Content: fmt.Sprintf("m%d", k)},
				}))
			}
			s.Messages = append(s.Messages, NewSubSessionItem(sub))
		}
		return s
	}

	shapes := []struct {
		name           string
		own, subs, sub int
	}{
		{"small_50", 50, 0, 0},
		{"medium_300_5x40", 300, 5, 40},
		{"large_2000_20x100", 2000, 20, 100},
	}
	for _, sh := range shapes {
		s := mkTree(sh.own, sh.subs, sh.sub)
		b.Run(sh.name+"/len_GetAllMessages", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = len(s.GetAllMessages())
			}
		})
		b.Run(sh.name+"/AllMessageCount", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = s.AllMessageCount()
			}
		})
	}
}
