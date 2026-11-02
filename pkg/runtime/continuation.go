package runtime

import (
	"context"
	"log/slog"
	"slices"
	"uuid"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// appendSteerAndEmit adds a steer message to the session and emits the corresponding event.
func (r *LocalRuntime) appendSteerAndEmit(sess *session.Session, sm QueuedMessage, events EventSink) {
	msg := session.UserMessage(sm.Content, sm.MultiContent...)
	msg.Message.MessageID = uuid.NewV4().String()
	pos := sess.AddMessage(msg)
	events.Emit(userMessageEvent(msg.Message, sess.ID, pos))
}

// drainAndEmitSteered defers to drainSteeredQueue unless the session is
// routed (routed sessions drain only at boundaries). drainSteeredQueue drains
// all messages from the steer queue and injects them into the session as
// individual user messages. When multiple messages are drained, a "\n" is
// appended to the content of every non-last message.
// Some chat templates concatenate consecutive user messages without a
// separator before tokenisation, which would cause trailing/leading word
// fragments from adjacent messages to be glued together. The "\n" prevents
// this without merging the messages into one.
//
// It also snapshots the message count before any messages are added and
// returns it alongside the drained flag so the caller can pass it to
// compaction checks without a separate sess.OwnMessageCount() call.
//
// NOTE: the appended \n is persisted in the session message and included in
// UserMessageEvent. This is a deliberate trade-off: because the runtime passes
// chat.Message slices directly to the provider, this is the only injection
// point that doesn't require restructuring. TUI consumers may see a trailing
// newline on non-last steered messages in multi-drain batches.
//
// After appending the drained messages it fires the
// user_steering_messages_submit hook (the steering-queue analogue of
// user_prompt_submit), passing the drained message text. The hook may
// block the run (steerResult.stop) or contribute a transient system
// message (steerResult.contextMsgs) that the caller threads into the
// steered turn only — never persisted, exactly like user_prompt_submit.
//
// Returns drained=true with messageCountBefore set when any messages
// were drained and emitted; otherwise drained=false.
func (r *LocalRuntime) drainAndEmitSteered(ctx context.Context, sess *session.Session, a *agent.Agent, events EventSink) steerResult {
	// A routed invocation keeps later input queued until its boundary, so
	// the steered text cannot change the assessment already made.
	if sess.Routed() {
		return steerResult{}
	}
	return r.drainSteeredQueue(ctx, sess, a, events)
}

// drainSteeredQueue unconditionally moves queued steering messages into the session.
func (r *LocalRuntime) drainSteeredQueue(ctx context.Context, sess *session.Session, a *agent.Agent, events EventSink) steerResult {
	steered := r.steerQueue.Drain(ctx)
	if len(steered) == 0 {
		return steerResult{}
	}
	messageCountBefore := sess.OwnMessageCount()
	contents := make([]string, 0, len(steered))
	for i, sm := range steered {
		contents = append(contents, sm.Content)
		if i < len(steered)-1 {
			sm = appendNewlineToQueuedMessage(sm)
		}
		r.appendSteerAndEmit(sess, sm, events)
	}
	stop, stopMsg, ctxMsgs := r.executeUserSteeringMessagesSubmitHooks(ctx, sess, a, contents, events)
	return steerResult{
		drained:            true,
		messageCountBefore: messageCountBefore,
		stop:               stop,
		stopMsg:            stopMsg,
		contextMsgs:        ctxMsgs,
	}
}

// drainSteeredQueueAtBoundary drains the steering queue where an invocation
// ends; routed conversations only ever drain here.
func (r *LocalRuntime) drainSteeredQueueAtBoundary(ctx context.Context, sess *session.Session, a *agent.Agent, ls *loopState, events EventSink) steerResult {
	if ls.route != nil {
		return r.drainSteeredQueue(ctx, sess, a, events)
	}
	return r.drainAndEmitSteered(ctx, sess, a, events)
}

// steerResult is the outcome of a drainAndEmitSteered call: whether any
// messages were drained, the pre-drain message count (for
// compaction checks), and the user_steering_messages_submit hook verdict
// (a terminating stop and/or a transient context message to thread into
// the steered turn).
type steerResult struct {
	drained            bool
	messageCountBefore int
	stop               bool
	stopMsg            string
	contextMsgs        []chat.Message
}

// appendNewlineToQueuedMessage returns sm with "\n" appended to its text
// content; never mutates the caller's slice contents.
// For plain-text messages Content is extended. For multi-content messages
// only the last part is considered: if it is a text part, "\n" is appended
// to it in a shallow copy of the slice. If the last part is not text type
// (e.g. image), sm is returned unchanged — non-text parts carry their own
// provider envelope that acts as a separator.
func appendNewlineToQueuedMessage(sm QueuedMessage) QueuedMessage {
	if len(sm.MultiContent) == 0 {
		sm.Content += "\n"
		return sm
	}
	// Only act if the last part is a text part.
	last := len(sm.MultiContent) - 1
	if sm.MultiContent[last].Type != chat.MessagePartTypeText {
		return sm
	}
	// Shallow-copy the slice so we don't mutate the original.
	parts := slices.Clone(sm.MultiContent)
	parts[last].Text += "\n"
	sm.MultiContent = parts
	return sm
}

// applySteering commits transient context only when submission hooks allow it.
// Draining, routing restarts, and compaction stay at their distinct loop boundaries.
func (r *LocalRuntime) applySteering(ctx context.Context, sess *session.Session, a *agent.Agent, ls *loopState, sr steerResult, events EventSink) (blocked bool) {
	if sr.stop {
		slog.WarnContext(ctx, "user_steering_messages_submit hook signalled run termination",
			"agent", a.Name(), "session_id", sess.ID, "reason", sr.stopMsg)
		r.emitHookDrivenShutdown(ctx, a, sess, sr.stopMsg, events)
		return true
	}
	ls.userPromptMsgs = sr.contextMsgs
	return false
}
