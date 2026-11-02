package runtime

import (
	"sync"
	"testing"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// TestAppendSteerAndEmitConcurrentWithAddMessage pins the data-race fix for
// continuation.go's appendSteerAndEmit (issue #3590): the emitted
// UserMessageEvent.SessionPosition must come from AddMessage's own atomic
// return value, not a separate len(sess.Messages)-1 read taken after the
// fact. Run with -race: without the fix, reading len(sess.Messages)
// unlocked races the concurrent AddMessage goroutine simulating a live HTTP
// AddMessage/compaction on the same session. Beyond race-freedom, this also
// asserts the emitted position always resolves back to the message that
// call actually appended, which a racy read cannot guarantee.
func TestAppendSteerAndEmitConcurrentWithAddMessage(t *testing.T) {
	t.Parallel()

	rt, _ := newTestRuntime(t)
	sess := session.New()

	const n = 100
	events := make(chan Event, n)
	sink := NewChannelSink(events)

	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			rt.appendSteerAndEmit(sess, QueuedMessage{Content: "steer"}, sink)
		})
		wg.Go(func() {
			sess.AddMessage(session.UserMessage("concurrent-http-add"))
		})
	}
	wg.Wait()
	close(events)

	items := sess.MessagesSnapshot()
	for ev := range events {
		um, ok := ev.(*UserMessageEvent)
		if !ok {
			t.Fatalf("unexpected event type %T", ev)
		}
		if um.SessionPosition < 0 || um.SessionPosition >= len(items) {
			t.Fatalf("SessionPosition %d out of range [0,%d)", um.SessionPosition, len(items))
		}
		item := items[um.SessionPosition]
		if !item.IsMessage() || item.Message.Message.Content != um.Message {
			t.Fatalf("SessionPosition %d does not point at the emitted message %q", um.SessionPosition, um.Message)
		}
	}
}

// TestEmitStartupInfoConcurrentWithAddMessage pins the data-race fix for
// runtime.go's session-restore LastMessage reconstruction (issue #3590): it
// must iterate a MessagesSnapshot rather than sess.Messages directly, so
// restoring startup info for a session cannot race a concurrent
// AddMessage/ApplyCompaction (e.g. a live HTTP AddMessage or the runtime's
// own compaction). Run with -race; before the fix, slices.Backward over the
// live sess.Messages slice races the concurrent appends below.
func TestEmitStartupInfoConcurrentWithAddMessage(t *testing.T) {
	t.Parallel()

	rt, _ := newTestRuntime(t)
	sess := session.New()
	sess.SetUsage(10, 20)
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "hi",
	}))

	sink := EventSinkFunc(func(Event) {})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			sess.AddMessage(session.UserMessage("concurrent-restore-add"))
		}
	}()

	rt.EmitStartupInfo(t.Context(), sess, sink)
	<-done
}
