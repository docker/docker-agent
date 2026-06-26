package runtime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestFramingValid(t *testing.T) {
	t.Parallel()

	for _, f := range []Framing{"", FramingPlain, FramingInstruction, FramingReplacement} {
		assert.Truef(t, f.Valid(), "framing %q should be valid", f)
	}
	for _, f := range []Framing{"bogus", "Plain", "INSTRUCTION", " ", "instruct"} {
		assert.Falsef(t, f.Valid(), "framing %q should be invalid", f)
	}
}

func TestResolveSteerFraming(t *testing.T) {
	t.Parallel()

	assert.Equal(t, FramingInstruction, resolveSteerFraming(""), "zero value defaults to instruction")
	assert.Equal(t, FramingPlain, resolveSteerFraming(FramingPlain))
	assert.Equal(t, FramingInstruction, resolveSteerFraming(FramingInstruction))
	assert.Equal(t, FramingReplacement, resolveSteerFraming(FramingReplacement))
	assert.Equal(t, FramingPlain, resolveSteerFraming("bogus"),
		"unrecognized values fall back to plain so no envelope is injected unexpectedly")
}

func TestFrameSteeredMessage_Plain(t *testing.T) {
	t.Parallel()

	got := frameSteeredMessage(QueuedMessage{Content: "hello", Framing: FramingPlain})
	assert.Equal(t, "hello", got.Content, "plain framing leaves content untouched")
	assert.Nil(t, got.MultiContent)
}

func TestFrameSteeredMessage_InstructionIsDefault(t *testing.T) {
	t.Parallel()

	// Zero-value framing resolves to instruction.
	got := frameSteeredMessage(QueuedMessage{Content: "also use pytest, not unittest"})
	assert.Equal(t, framingInstructionPreamble+"also use pytest, not unittest"+framingInstructionPostamble, got.Content)
	assert.Contains(t, got.Content, "<system-reminder>")
	assert.Contains(t, got.Content, "finish your current task first")
	assert.Contains(t, got.Content, "also use pytest, not unittest")
}

func TestFrameSteeredMessage_Replacement(t *testing.T) {
	t.Parallel()

	got := frameSteeredMessage(QueuedMessage{
		Content: "actually skip the bug fix, just write a reproducer test",
		Framing: FramingReplacement,
	})
	assert.Equal(t, framingReplacementPreamble+"actually skip the bug fix, just write a reproducer test"+framingReplacementPostamble, got.Content)
	assert.Contains(t, got.Content, "<system-reminder>")
	assert.Contains(t, got.Content, "Abandon your current task")
	assert.NotContains(t, got.Content, "finish your current task first")
}

// TestFrameSteeredMessage_MultiContent verifies the envelope is added as
// leading and trailing text parts so non-text parts (images) stay in place.
func TestFrameSteeredMessage_MultiContent(t *testing.T) {
	t.Parallel()

	sm := QueuedMessage{
		Framing: FramingInstruction,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "https://example.com/a.png"}},
			{Type: chat.MessagePartTypeText, Text: "do this"},
		},
	}
	got := frameSteeredMessage(sm)

	require.Len(t, got.MultiContent, 4, "instruction envelope adds a leading and a trailing text part")
	assert.Equal(t, chat.MessagePartTypeText, got.MultiContent[0].Type)
	assert.Equal(t, framingInstructionPreamble, got.MultiContent[0].Text)
	assert.Equal(t, chat.MessagePartTypeImageURL, got.MultiContent[1].Type, "image part preserved in place")
	assert.Equal(t, "do this", got.MultiContent[2].Text, "original text part preserved")
	assert.Equal(t, chat.MessagePartTypeText, got.MultiContent[3].Type)
	assert.Equal(t, framingInstructionPostamble, got.MultiContent[3].Text)
}

func TestFrameSteeredMessage_UnrecognizedFraming(t *testing.T) {
	t.Parallel()

	// Defense-in-depth: an unrecognized value (the HTTP boundary rejects these,
	// but direct QueuedMessage construction can't) falls back to plain so no
	// envelope is injected unexpectedly.
	got := frameSteeredMessage(QueuedMessage{Content: "hello", Framing: "typo"})
	assert.Equal(t, "hello", got.Content)
	assert.NotContains(t, got.Content, "<system-reminder>")
}

func TestFrameSteeredMessage_PlainMultiContentUnchanged(t *testing.T) {
	t.Parallel()

	sm := QueuedMessage{
		Framing: FramingPlain,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "keep me"},
		},
	}
	got := frameSteeredMessage(sm)
	require.Len(t, got.MultiContent, 1)
	assert.Equal(t, "keep me", got.MultiContent[0].Text)
}

func newFramingTestRuntime(t *testing.T) (*LocalRuntime, *agent.Agent) {
	t.Helper()
	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt, root
}

func drainedUserMessages(sess *session.Session) []string {
	var out []string
	for _, item := range sess.Messages {
		if item.IsMessage() && item.Message.Message.Role == chat.MessageRoleUser {
			out = append(out, item.Message.Message.Content)
		}
	}
	return out
}

// TestDrainAndEmitSteered_DefaultIsInstruction pins that a steer message
// queued with the zero-value framing is drained, persisted, and emitted with
// the instruction envelope.
func TestDrainAndEmitSteered_DefaultIsInstruction(t *testing.T) {
	t.Parallel()

	rt, root := newFramingTestRuntime(t)
	require.NoError(t, rt.Steer(QueuedMessage{Content: "also use pytest"}))

	sess := session.New()
	events := make(chan Event, 16)
	sr := rt.drainAndEmitSteered(t.Context(), sess, root, NewChannelSink(events))
	close(events)

	require.True(t, sr.drained)

	msgs := drainedUserMessages(sess)
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0], "<system-reminder>")
	assert.Contains(t, msgs[0], "finish your current task first")
	assert.Contains(t, msgs[0], "also use pytest")

	var eventMsgs []string
	for ev := range events {
		if ue, ok := ev.(*UserMessageEvent); ok {
			eventMsgs = append(eventMsgs, ue.Message)
		}
	}
	require.Len(t, eventMsgs, 1)
	assert.Equal(t, msgs[0], eventMsgs[0], "the UserMessageEvent must mirror the framed session message")
}

// TestDrainAndEmitSteered_Replacement pins the replacement envelope end-to-end
// through the drain path.
func TestDrainAndEmitSteered_Replacement(t *testing.T) {
	t.Parallel()

	rt, root := newFramingTestRuntime(t)
	require.NoError(t, rt.Steer(QueuedMessage{Content: "stop, the bug is in db.ts", Framing: FramingReplacement}))

	sess := session.New()
	events := make(chan Event, 16)
	sr := rt.drainAndEmitSteered(t.Context(), sess, root, NewChannelSink(events))
	close(events)

	require.True(t, sr.drained)
	msgs := drainedUserMessages(sess)
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0], "Abandon your current task")
	assert.Contains(t, msgs[0], "stop, the bug is in db.ts")
}

// TestDrainAndEmitSteered_MixedFramingPerMessage verifies framing is resolved
// per message within a single drained batch, and that the newline separator
// still lands after a wrapped message's closing tag.
func TestDrainAndEmitSteered_MixedFramingPerMessage(t *testing.T) {
	t.Parallel()

	rt, root := newFramingTestRuntime(t)
	require.NoError(t, rt.Steer(QueuedMessage{Content: "first", Framing: FramingInstruction}))
	require.NoError(t, rt.Steer(QueuedMessage{Content: "second", Framing: FramingPlain}))

	sess := session.New()
	events := make(chan Event, 16)
	sr := rt.drainAndEmitSteered(t.Context(), sess, root, NewChannelSink(events))
	close(events)

	require.True(t, sr.drained)
	msgs := drainedUserMessages(sess)
	require.Len(t, msgs, 2)

	// Non-last message: instruction-wrapped, with the "\n" separator appended
	// after the closing tag.
	assert.Contains(t, msgs[0], "<system-reminder>")
	assert.True(t, strings.HasSuffix(msgs[0], "</system-reminder>\n"),
		"newline separator must follow the closing tag of a non-last wrapped message; got %q", msgs[0])

	// Last message: plain framing, no envelope, no trailing newline.
	assert.Equal(t, "second", msgs[1])
}

// TestSteer_ReplacementFramingReachesModel verifies end-to-end through a full
// RunStream that a replacement-framed steer message is delivered to the model
// with the exact envelope wording — not just persisted in the session.
func TestSteer_ReplacementFramingReachesModel(t *testing.T) {
	t.Parallel()

	stream := newStreamBuilder().AddContent("ok").AddStopWithUsage(5, 3).Build()
	prov := &messageRecordingProvider{id: "test/mock-model", streams: []*mockStream{stream}}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	require.NoError(t, rt.Steer(QueuedMessage{Content: "write a reproducer instead", Framing: FramingReplacement}))

	sess := session.New(session.WithUserMessage("fix the bug"))
	for range rt.RunStream(t.Context(), sess) {
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	require.NotEmpty(t, prov.recordedMessages, "expected at least one model call")

	var found bool
	for _, m := range prov.recordedMessages[0] {
		if strings.Contains(m.Content, "write a reproducer instead") {
			found = true
			assert.Contains(t, m.Content, "<system-reminder>")
			assert.Contains(t, m.Content, "Abandon your current task")
			break
		}
	}
	assert.True(t, found, "model must receive the replacement-framed steer message in its first turn")
}

// TestFollowUpIgnoresFraming pins that follow-up messages always render plain,
// even when a caller mistakenly sets a Framing — follow-ups are unchanged by
// this feature.
func TestFollowUpIgnoresFraming(t *testing.T) {
	t.Parallel()

	// Two turns: the first stops, the runtime dequeues the follow-up and runs a
	// second turn which also stops (queue now empty).
	newStopStream := func() *mockStream {
		return newStreamBuilder().AddContent("ok").AddStopWithUsage(3, 2).Build()
	}
	prov := &queueProvider{
		id:      "test/mock-model",
		streams: []chat.MessageStream{newStopStream(), newStopStream()},
	}
	root := agent.New("root", "test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	require.NoError(t, rt.FollowUp(QueuedMessage{Content: "also do this", Framing: FramingReplacement}))

	sess := session.New(session.WithUserMessage("hi"))
	for range rt.RunStream(t.Context(), sess) {
	}

	var found bool
	for _, content := range drainedUserMessages(sess) {
		if strings.Contains(content, "also do this") {
			found = true
			assert.Equal(t, "also do this", content,
				"follow-ups must render plain regardless of the Framing field")
			assert.NotContains(t, content, "<system-reminder>")
		}
	}
	assert.True(t, found, "expected the follow-up message in the session")
}
