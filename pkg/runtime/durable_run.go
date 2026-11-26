package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
)

// ErrSuspended is the cancellation cause a host gives a run's context to stop
// the run without ending it: its record stays, so [LocalRuntime.ContinueStream]
// can pick the run up later. Any other cancellation ends the run.
var ErrSuspended = errors.New("run suspended")

// maxRunCrashes bounds how often a run that keeps dying is continued.
const maxRunCrashes = 5

// ContinueStream continues the run that was cut off on sess, from its
// transcript and without new input. A transcript that already ends on an
// answer is resumed at what follows an answer, without asking the model again.
//
// The run continues with the agent the runtime resolves for sess, as RunStream
// would: the caller selects the one the run started with
// ([session.Run.Agent]). The stream ends with an error when another agent is
// selected, when the conversation had moved to another agent or ends on
// another agent's answer, when the agent runs in an external harness, or when
// the run died too many times in a row.
func (r *LocalRuntime) ContinueStream(ctx context.Context, sess *session.Session) <-chan Event {
	return r.runStream(ctx, sess, defaultIdleStreamRetryPolicy(), true)
}

// recordContext returns the context the run's record is written with. A
// record is what makes a run durable, so writing it is not cancelled with the
// run, only bounded.
func (r *LocalRuntime) recordContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), r.streamStoppedTimeout())
}

// runStore returns the store recording sess's runs. Only root runs are
// recorded: a sub-session is continued through the run that owns it.
func (r *LocalRuntime) runStore(sess *session.Session) session.RunStore {
	if sess.IsSubSession() {
		return nil
	}
	store, _ := r.sessionStore.(session.RunStore)
	return store
}

// beginRun records the run before it does anything slow, and reports whether
// it is on record. The record stores the prompt the runtime announces, so a
// run on record has its input in the transcript. A host that turns the
// announcement off ([session.Session.SendUserMessage]) stores the input
// itself, as it always had to.
func (r *LocalRuntime) beginRun(ctx context.Context, sess *session.Session, a *agent.Agent, prompt *UserMessageEvent) bool {
	store := r.runStore(sess)
	if store == nil {
		return false
	}
	var input *session.Message
	if prompt != nil {
		input = prompt.sessionMessage()
	}
	ctx, cancel := r.recordContext(ctx)
	defer cancel()
	if err := store.BeginRun(ctx, sess.ID, a.Name(), input); err != nil {
		slog.WarnContext(ctx, "Failed to record run", "session_id", sess.ID, "error", err)
		return false
	}
	if prompt != nil {
		prompt.persisted = true
	}
	return true
}

// continueRun takes the record of the run a is about to continue, when the
// store keeps one. It reports whether the stream now answers for that record,
// and why the run cannot be continued, if it cannot.
func (r *LocalRuntime) continueRun(ctx context.Context, sess *session.Session, a *agent.Agent) (recorded bool, refusal string) {
	var run session.Run
	if store := r.runStore(sess); store != nil {
		var err error
		if run, err = store.ContinueRun(ctx, sess.ID); err != nil {
			// Unread, the record cannot vouch for the run: it is left as it
			// is for a later attempt.
			return false, fmt.Sprintf("The run was not continued: its record could not be read: %v", err)
		}
		recorded = true
	}
	var answeredBy string
	if answer, ok := storedAnswer(sess); ok {
		answeredBy = answer.AgentName
	}
	return recorded, refuseToContinue(run, a, answeredBy)
}

// moveRun records that the run's conversation is now with the agent name. The
// agent a conversation is with lives in memory only, so the record is what
// tells a continued run that it is no longer with the agent that started it.
func (r *LocalRuntime) moveRun(ctx context.Context, sess *session.Session, name string) {
	store := r.runStore(sess)
	if store == nil {
		return
	}
	moveCtx, cancel := r.recordContext(ctx)
	defer cancel()
	err := store.MoveRun(moveCtx, sess.ID, name)
	if err == nil {
		return
	}
	// A record that names the wrong agent would have that agent continue the
	// run: the run goes on without a record instead. Dropping it gets its
	// own time, the move may have used all of its own.
	slog.WarnContext(ctx, "Failed to record the run's agent; the run is no longer on record", "session_id", sess.ID, "agent", name, "error", err)
	endCtx, cancel := r.recordContext(ctx)
	defer cancel()
	if err := store.EndRun(endCtx, sess.ID); err != nil {
		slog.WarnContext(ctx, "Failed to drop run record", "session_id", sess.ID, "error", err)
	}
}

// settleInterruptedCalls gives the tool calls the run was cut off in the
// result they never got. They are not run again: whether to retry is for the
// model to decide. With the result in the transcript, the calls are over for
// everything that reads it, not in flight.
func (r *LocalRuntime) settleInterruptedCalls(sess *session.Session, a *agent.Agent, events EventSink) {
	for _, call := range sess.InterruptedToolCalls() {
		addAgentMessage(sess, a, &chat.Message{
			Role:       chat.MessageRoleTool,
			ToolCallID: call.ID,
			Content:    session.InterruptedToolResult,
			IsError:    true,
			CreatedAt:  r.now().Format(time.RFC3339),
		}, events)
	}
}

// refuseToContinue says why the selected agent cannot continue run, or returns
// "" when it can. answeredBy names the agent whose answer the run's transcript
// ends on, if it ends on one.
func refuseToContinue(run session.Run, selected *agent.Agent, answeredBy string) string {
	switch {
	case run.Crashes >= maxRunCrashes:
		return fmt.Sprintf("The run was cut off %d times in a row and was not continued.", run.Crashes)
	case run.Agent != "" && run.Agent != selected.Name():
		return fmt.Sprintf("The run belongs to agent %q and was not continued by agent %q.", run.Agent, selected.Name())
	case run.Current != run.Agent:
		// The state that put the conversation with that agent is gone.
		return fmt.Sprintf("The conversation had moved to agent %q, and a run that changes agents cannot be continued.", run.Current)
	case answeredBy != "" && answeredBy != selected.Name():
		// What follows an answer is its agent's to do. The conversation is
		// back with the selected agent, but the note that handed it over
		// was never stored.
		return fmt.Sprintf("The run was cut off on an answer from agent %q, and agent %q cannot continue it.", answeredBy, selected.Name())
	case selected.HasHarness():
		// The harness does its work outside the transcript: handing it the
		// prompt again would start that work over.
		return fmt.Sprintf("Agent %q runs in an external harness, and its run cannot be continued.", selected.Name())
	default:
		return ""
	}
}

// settleRun drops the record of a run that is over, and keeps the record of
// one its host suspended. Only the stream that took the record settles it.
//
// The host's word is enough to keep the record: under cancellation the loop
// can report any stop reason, a hook failing as if it had blocked the run for
// one. A run that had in fact ended is found to be so when it is continued.
func (r *LocalRuntime) settleRun(ctx context.Context, sess *session.Session) {
	store := r.runStore(sess)
	if store == nil {
		return
	}
	suspended := errors.Is(context.Cause(ctx), ErrSuspended)

	flushCtx, cancel := r.recordContext(ctx)
	defer cancel()
	// The record goes only once the store holds what the run emitted: until
	// then it is what lets the run be found again.
	err := flushObservers(flushCtx)
	switch {
	case suspended:
		// A run stopped on purpose is noted as such whatever became of the
		// flush, and in its own time: it must not count as a crash.
		suspendCtx, cancel := r.recordContext(ctx)
		defer cancel()
		err = errors.Join(err, store.SuspendRun(suspendCtx, sess.ID))
	case err == nil:
		err = store.EndRun(flushCtx, sess.ID)
	}
	if err != nil {
		slog.WarnContext(ctx, "Failed to settle run record", "session_id", sess.ID, "suspended", suspended, "error", err)
	}
}

// storedAnswer returns the answer sess's transcript ends on, and reports false
// when it ends on input instead. The transcript is read as stored: the
// assembled context may have trimmed the answer away.
func storedAnswer(sess *session.Session) (session.Message, bool) {
	if sess.Unanswered() {
		return session.Message{}, false
	}
	tail := sess.Tail(1)
	if len(tail) == 0 {
		return session.Message{}, false
	}
	return tail[0], true
}

// endedByRuntime reports whether sess's transcript ends on the runtime's own
// last word rather than on a model's answer: the message that says a limit
// was reached or that the run failed. Such a run was over and only its record
// had yet to go, so nothing follows that message. It is told from an answer
// by naming no model.
func endedByRuntime(sess *session.Session) bool {
	answer, ok := storedAnswer(sess)
	return ok && answer.Message.Model == ""
}

// resumeAfterAnswer picks a run up where its transcript ends on an answer: the
// model had stopped, and the run was cut off before what follows an answer
// was done. It reports false when the transcript ends on input instead.
//
// What follows an answer depends on the model that gave it, which a fallback
// or a routing rule may have chosen over primary, the agent's own.
func (r *LocalRuntime) resumeAfterAnswer(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	primary modelsdev.ID,
	contextLimit int64,
	ls *loopState,
	events EventSink,
) (turnControl, bool) {
	stored, ok := storedAnswer(sess)
	if !ok {
		return turnContinue, false
	}
	last := stored.Message
	answer := stoppedAnswer{
		content:      last.Content,
		hasMedia:     len(last.MultiContent) > 0,
		finishReason: last.FinishReason,
		validated:    structuredOutputEnabled(sess, a) && isStructuredOutput(a, last.Content),
	}
	servingModel := primary
	if id, err := modelsdev.ParseID(last.Model); err == nil {
		servingModel = id
	}
	ctrl, reason := r.afterAnswer(ctx, sess, a, answer, servingModel.Model, contextLimit, sess.OwnMessageCount(), ls, events)
	ls.exitReason = reason
	return ctrl, true
}
