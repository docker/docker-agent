package chatserver

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestConversationStore_Disabled(t *testing.T) {
	c := newConversationStore(0, time.Hour)
	c.Put("a", session.New())
	assert.Nil(t, c.Get("a"))
	assert.Equal(t, 0, c.Len())
}

func TestConversationStore_PutGet(t *testing.T) {
	c := newConversationStore(8, time.Hour)
	s := session.New()
	c.Put("a", s)

	got := c.Get("a")
	require.NotNil(t, got)
	assert.Same(t, s, got)
}

func TestConversationStore_TTL(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := newConversationStore(8, time.Minute)
	c.now = func() time.Time { return now }

	c.Put("a", session.New())
	assert.NotNil(t, c.Get("a"))

	now = now.Add(2 * time.Minute)
	assert.Nil(t, c.Get("a"), "entry should be expired")
	assert.Equal(t, 0, c.Len(), "expired entry should be evicted on Get miss")
}

func TestConversationStore_LRUEviction(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := newConversationStore(2, time.Hour)
	c.now = func() time.Time { return now }

	c.Put("a", session.New())
	now = now.Add(time.Second)
	c.Put("b", session.New())
	now = now.Add(time.Second)
	// Touch "a" so it becomes the most-recently-used.
	require.NotNil(t, c.Get("a"))
	now = now.Add(time.Second)
	c.Put("c", session.New())

	// "b" was the LRU when capacity was exceeded, so it should be the
	// one that got evicted.
	assert.Nil(t, c.Get("b"))
	assert.NotNil(t, c.Get("a"))
	assert.NotNil(t, c.Get("c"))
}

func TestConversationStore_Delete(t *testing.T) {
	c := newConversationStore(8, time.Hour)
	c.Put("a", session.New())
	c.Delete("a")
	assert.Nil(t, c.Get("a"))
}

func TestAppendLatestUser(t *testing.T) {
	sess := session.New()
	appendLatestUser(sess, []ChatCompletionMessage{
		{Role: "system", Content: "be helpful"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "ack"},
		{Role: "user", Content: "second"},
		{Role: "tool", Content: "tool result", ToolCallID: "x"},
	})
	assert.Equal(t, "second", sess.GetLastUserMessageContent())
}

func TestAppendLatestUser_NoUserMessage(t *testing.T) {
	sess := session.New()
	appendLatestUser(sess, []ChatCompletionMessage{
		{Role: "system", Content: "be helpful"},
		{Role: "assistant", Content: "ack"},
	})
	assert.Empty(t, sess.GetLastUserMessageContent())
}
