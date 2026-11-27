package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// ErrSuspended is the cancellation cause a host gives a run's context to stop
// the run without ending it: its record stays, so
// [DurableRuntime.ContinueStream] can take the run up again later. Any other
// cancellation ends the run.
var ErrSuspended = errors.New("run suspended")

// maxRunCrashes bounds how often a run that keeps dying at the same step is
// taken up again.
const maxRunCrashes = 5

// checkpointVersion is bumped when a checkpoint written by an earlier version
// can no longer be read.
const checkpointVersion = 1

// DurableRuntime runs agents as [LocalRuntime] does, with a different
// contract towards the session store:
//
//   - A run is on record from before it does anything slow until it is over.
//     The record says where the run stands: after each of its steps the
//     runtime waits until the store holds what the step produced, then saves
//     the run's state with it.
//   - A run that cannot be recorded does not go on. Where a [LocalRuntime]
//     logs a store error and carries on, a durable run stops, and its record
//     stays where it last stood.
//   - A run whose record outlives the process, or whose host suspended it
//     with [ErrSuspended], is taken up at the step it was cut off in by
//     [DurableRuntime.ContinueStream].
//
// Only root runs are recorded. A sub-agent that was working when its run was
// cut off is a tool call of that run, and is treated as one.
//
// A runtime has one current agent, which every session that is not pinned to
// an agent follows, here as on a [LocalRuntime]. A host that runs several
// sessions at once on one runtime pins them with [session.WithAgentName]:
// what a record keeps of a pinned session's agent is the session's own.
type DurableRuntime struct {
	*LocalRuntime
}

// NewDurableRuntime creates a runtime that records its runs in the session
// store given with [WithSessionStore], which must be a [session.RunStore].
func NewDurableRuntime(ctx context.Context, agents *team.Team, opts ...Opt) (*DurableRuntime, error) {
	local, err := NewLocalRuntime(ctx, agents, opts...)
	if err != nil {
		return nil, err
	}
	runs, ok := local.sessionStore.(session.RunStore)
	if !ok {
		return nil, errors.Join(errors.New("a durable runtime needs a session store that records runs"), local.Close())
	}
	local.runs = runs
	return &DurableRuntime{LocalRuntime: local}, nil
}

// ContinueStream takes up the run of a session that was cut off, at the step
// it was cut off in, and returns the session as the store holds it along with
// the run's events. The stored conversation is first cut back to the run's
// last finished step: what a step that never finished had stored is redone.
//
// The store keeps the conversation, not everything a host sets on a session
// before running it. opts are applied to the session once it is loaded and
// stands as it did at the run's last finished step, so they can set what the
// store does not keep, and change what the session lets the run do. They do
// not change where the run stands: the agent the conversation is with and
// the run's iteration limit come from its record, whatever opts say.
//
// It returns an error, and no stream, when the session has no run on record,
// when its record cannot be read, or when the run died too many times in a
// row at the same step and is given up.
//
// A record does not say whether its run is still under way. The host calls
// this, or [DurableRuntime.DiscardRun], only for a session that has no
// stream open: the conversation would be cut back under a run in progress.
func (r *DurableRuntime) ContinueStream(ctx context.Context, sessionID string, opts ...session.Opt) (*session.Session, <-chan Event, error) {
	run, err := r.runs.ResumeRun(ctx, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("continuing the run of session %q: %w", sessionID, err)
	}
	cp, err := readCheckpoint(run)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the run record of session %q: %w", sessionID, err)
	}
	sess, err := r.sessionAt(ctx, sessionID, cp)
	if err != nil {
		return nil, nil, err
	}
	if run.Crashes >= maxRunCrashes {
		return nil, nil, r.giveUp(ctx, sess, fmt.Errorf("the run of session %q died %d times in a row at the same step and was given up", sessionID, run.Crashes))
	}
	for _, opt := range opts {
		opt(sess)
	}
	if err := r.restoreAgent(ctx, sess, cp); err != nil {
		return nil, nil, r.giveUp(ctx, sess, fmt.Errorf("the run of session %q cannot be continued: %w", sessionID, err))
	}
	return sess, r.runStream(ctx, sess, defaultIdleStreamRetryPolicy(), cp), nil
}

// DiscardRun gives up the run of a session that was cut off, for a host that
// will not continue it. The session is left at the run's last finished step,
// as ContinueStream finds it, so it can be loaded and run again without what
// the unfinished step left in it.
func (r *DurableRuntime) DiscardRun(ctx context.Context, sessionID string) error {
	run, err := r.runs.ResumeRun(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("discarding the run of session %q: %w", sessionID, err)
	}
	cp, err := readCheckpoint(run)
	if err != nil {
		// A record that cannot be read says nothing of the session. The
		// conversation is cut back all the same, and the record can go.
		return r.runs.EndRun(ctx, sessionID)
	}
	sess, err := r.sessionAt(ctx, sessionID, cp)
	if err != nil {
		return err
	}
	return r.endRun(ctx, sess)
}

// sessionAt loads a session whose conversation the store has cut back to the
// checkpoint, and takes what the session says of the conversation back with
// it.
func (r *DurableRuntime) sessionAt(ctx context.Context, sessionID string, cp *checkpoint) (*session.Session, error) {
	sess, err := r.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("loading session %q: %w", sessionID, err)
	}
	cp.Session.restore(sess)
	return sess, nil
}

// giveUp ends the run of sess, which cannot be continued, and returns why.
func (r *DurableRuntime) giveUp(ctx context.Context, sess *session.Session, why error) error {
	if err := r.endRun(ctx, sess); err != nil {
		return errors.Join(why, err)
	}
	return why
}

// endRun stores sess, which stands at its run's last finished step, and
// drops the run's record. The record goes last: until the store holds the
// session, the record is all that says where it stood.
func (r *DurableRuntime) endRun(ctx context.Context, sess *session.Session) error {
	if err := r.sessionStore.UpdateSession(ctx, sess); err != nil {
		return fmt.Errorf("storing session %q: %w", sess.ID, err)
	}
	if err := r.runs.EndRun(ctx, sess.ID); err != nil {
		return fmt.Errorf("dropping the run record of session %q: %w", sess.ID, err)
	}
	return nil
}

// readCheckpoint decodes where a run stands, and refuses a checkpoint this
// version cannot take a run up from.
func readCheckpoint(run session.Run) (*checkpoint, error) {
	var cp checkpoint
	if err := json.Unmarshal(run.Checkpoint, &cp); err != nil {
		return nil, err
	}
	if cp.Version != checkpointVersion {
		return nil, fmt.Errorf("it was written by another version (%d, this one reads %d)", cp.Version, checkpointVersion)
	}
	switch cp.Phase {
	case phaseStarting, phaseModel:
	case phaseTools, phaseAnswered:
		if cp.Answer == nil {
			return nil, fmt.Errorf("it stands at step %q without the answer that step works with", cp.Phase)
		}
	default:
		return nil, fmt.Errorf("it stands at an unknown step %q", cp.Phase)
	}
	return &cp, nil
}

// turnAgent returns the agent of the turn a stream starts in. That is the
// agent the conversation is with, except for a run taken up past a turn's
// model step: the turn is still the agent's that made its tool calls,
// wherever those calls have sent the conversation since.
func (r *LocalRuntime) turnAgent(sess *session.Session, resume *checkpoint) *agent.Agent {
	if resume != nil && resume.Answer != nil {
		if owner, err := r.team.Agent(resume.Answer.Agent); err == nil {
			return owner
		}
	}
	return r.resolveSessionAgent(sess)
}

// sessionState is what a session holds besides its conversation that a run
// changes as it goes, and that the store's copy of the session cannot be
// trusted for when the run is taken up:
//
//   - where its instruction context stands in the conversation, and how
//     large the conversation is taken to be, which decides when it is
//     compacted. The store is told of both as the run goes, and a step that
//     never finished can leave them ahead of the conversation it is cut
//     back to;
//   - what its user has let it do: the safety mode they opted into and the
//     tools they allowed from then on, when asked to confirm a call. The
//     store is not told of those by the run at all.
type sessionState struct {
	InstructionContext *session.InstructionContextState `json:"instruction_context,omitempty"`
	InputTokens        int64                            `json:"input_tokens,omitempty"`
	OutputTokens       int64                            `json:"output_tokens,omitempty"`
	SafetyPolicy       session.SafetyPolicy             `json:"safety_policy,omitempty"`
	Permissions        *session.PermissionsConfig       `json:"permissions,omitempty"`
}

func sessionStateOf(sess *session.Session) sessionState {
	state := sessionState{
		InstructionContext: sess.InstructionContextSnapshot(),
		SafetyPolicy:       sess.GetSafetyPolicy(),
		Permissions:        sess.ClonePermissions(),
	}
	state.InputTokens, state.OutputTokens = sess.Usage()
	return state
}

func (state sessionState) restore(sess *session.Session) {
	sess.InstructionContext = state.InstructionContext
	sess.SetUsage(state.InputTokens, state.OutputTokens)
	sess.SetSafetyPolicy(state.SafetyPolicy)
	sess.SetPermissions(state.Permissions)
}

// agentState is what puts a conversation with an agent: the runtime's current
// agent, and what the session holds over it. The store keeps none of it.
type agentState struct {
	// Current is the runtime's current agent.
	Current string `json:"current"`
	// Pinned is the agent the session is pinned to, if it is.
	Pinned string `json:"pinned,omitempty"`
	// Handoffs says the session keeps its handoffs to itself, and Handoff
	// names the agent it was handed to, if it was.
	Handoffs bool   `json:"handoffs,omitempty"`
	Handoff  string `json:"handoff,omitempty"`
	// Routed says the conversation is routed by hooks, and Route names the
	// agent a hook routed it to, if one did.
	Routed bool   `json:"routed,omitempty"`
	Route  string `json:"route,omitempty"`
}

// agentStateOf returns what puts sess's conversation with an agent, given
// the runtime's current agent as the conversation knows it.
func agentStateOf(sess *session.Session, current string) agentState {
	return agentState{
		Current:  current,
		Pinned:   sess.AgentName,
		Handoffs: sess.AllowsAgentHandoffs(),
		Handoff:  sess.HandoffAgent(),
		Routed:   sess.Routed(),
		Route:    sess.RouteAgent(),
	}
}

// restoreAgent puts the conversation back with the agent it was with when
// the checkpoint was taken. It fails when the team no longer has an agent
// the run needs: that one, or the one whose turn the run is taken up in.
func (r *DurableRuntime) restoreAgent(ctx context.Context, sess *session.Session, cp *checkpoint) error {
	state := cp.Agent
	needed := []string{state.Pinned, state.Handoff, state.Route}
	if cp.Answer != nil {
		needed = append(needed, cp.Answer.Agent)
	}
	for _, name := range needed {
		if name == "" {
			continue
		}
		if _, err := r.team.Agent(name); err != nil {
			return err
		}
	}
	// The pin is the record's, also when the record has none: a session
	// that is not pinned follows the runtime's agent.
	session.WithAgentName(state.Pinned)(sess)
	if state.Pinned == "" {
		if err := r.SetCurrentAgent(ctx, state.Current); err != nil {
			return err
		}
	}
	if state.Handoffs {
		session.WithAgentHandoffs()(sess)
		sess.TryAgentHandoff(state.Handoff)
	}
	if state.Routed {
		sess.SetRouted()
		sess.SetRouteAgent(state.Route)
	}
	return nil
}

// checkpoint is where a run stands after one of its steps: the step it takes
// next, and everything that step needs that is not in the transcript.
type checkpoint struct {
	Version int `json:"version"`
	// Phase is the step the run takes next.
	Phase runPhase `json:"phase"`
	// Agent says which agent the conversation is with.
	Agent agentState `json:"agent"`
	// Session is what the session says of the conversation the checkpoint
	// stands for.
	Session sessionState `json:"session"`
	// State is what the run carries from one step to the next.
	State runState `json:"state"`
	// Answer is what the model step of the turn in progress left for the
	// steps after it. It is nil when the next step is to ask the model.
	Answer *modelAnswer `json:"answer,omitempty"`
	// Calls is what the dispatcher's journal keeps of the tool calls of
	// Answer.
	Calls map[string]toolexec.CallRecord `json:"calls,omitempty"`
}

// runRecord is the record a stream keeps of its run. A nil record keeps
// nothing: it is what a runtime that is not durable works with, and every
// method is a no-op on it.
type runRecord struct {
	r    *LocalRuntime
	sess *session.Session
	// transcriptStored waits until the store holds everything the run
	// emitted, and reports what kept it from doing so.
	transcriptStored func(ctx context.Context) error

	// mu guards the fields below: the tool calls of a batch run in parallel
	// and each of them is noted as it starts and as it finishes.
	mu sync.Mutex
	cp checkpoint
	// failed is why the record stopped following the run. From then on
	// nothing more is saved, and the run stops.
	failed error
}

// newRunRecord returns the record this stream keeps of sess's run, or nil
// when the runtime keeps none. resume is the checkpoint the run is taken up
// from, if it is.
func (r *LocalRuntime) newRunRecord(sess *session.Session, events chan<- Event, resume *checkpoint) *runRecord {
	if r.runs == nil || sess.IsSubSession() {
		return nil
	}
	rec := &runRecord{
		r:                r,
		sess:             sess,
		transcriptStored: newObserverFlush(events).transcriptStored,
		cp:               checkpoint{Version: checkpointVersion, Phase: phaseStarting},
	}
	if resume != nil {
		rec.cp = *resume
	}
	return rec
}

type runRecordKey struct{}

// withRunRecord gives the stream's own code the record of its run. Every
// stream sets it, to nil when it keeps no record, so a sub-agent's stream
// never finds its parent's.
func withRunRecord(ctx context.Context, rec *runRecord) context.Context {
	return context.WithValue(ctx, runRecordKey{}, rec)
}

func runRecordFrom(ctx context.Context) *runRecord {
	rec, _ := ctx.Value(runRecordKey{}).(*runRecord)
	return rec
}

// begin puts a new run on record, before it does anything slow, and stores
// what it starts from in the same write: the session, and the conversation
// the store does not hold yet. The write is bounded but not cancelled with
// the run: a run suspended as it starts is on record too.
func (rec *runRecord) begin(ctx context.Context, prompt *UserMessageEvent) error {
	if rec == nil {
		return nil
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.cp.Agent.Current = rec.r.currentAgentName()
	data, err := rec.marshal()
	if err != nil {
		return rec.fail(ctx, err)
	}
	var input *session.Message
	if prompt != nil {
		input = prompt.sessionMessage()
	}
	writeCtx, cancel := rec.r.recordContext(ctx)
	defer cancel()
	if err := rec.r.runs.BeginRun(writeCtx, rec.sess, input, data); err != nil {
		return rec.fail(ctx, err)
	}
	if prompt != nil {
		prompt.stored = true
	}
	return nil
}

// commit records that the run has finished a step and takes phase next,
// given the answer of the turn in progress if there is one.
func (rec *runRecord) commit(ctx context.Context, phase runPhase, ls *loopState, answer *modelAnswer) error {
	if rec == nil {
		return nil
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.cp.Phase = phase
	// Between steps no sub-agent is at work: the runtime's current agent is
	// the conversation's.
	rec.cp.Agent.Current = rec.r.currentAgentName()
	rec.cp.State = ls.runState
	rec.cp.Answer = nil
	if answer != nil {
		// The steps to come change the answer they are given.
		kept := *answer
		rec.cp.Answer = &kept
	}
	rec.cp.Calls = nil
	return rec.save(ctx)
}

// save stores the checkpoint once the store holds the transcript it stands
// for. A cancelled run saves nothing more: its record stays at its last step.
func (rec *runRecord) save(ctx context.Context) error {
	if rec.failed != nil {
		return rec.failed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := rec.transcriptStored(ctx); err != nil {
		return rec.fail(ctx, err)
	}
	data, err := rec.marshal()
	if err != nil {
		return rec.fail(ctx, err)
	}
	if err := rec.r.runs.SaveRun(ctx, rec.sess.ID, data); err != nil {
		return rec.fail(ctx, err)
	}
	return nil
}

// marshal encodes the checkpoint with what the session holds now: what it
// says of the agent the conversation is with, and of the conversation.
// The runtime's current agent is not read here. While tool calls run it can
// be a sub-agent at work, which the conversation is not handed to.
func (rec *runRecord) marshal() ([]byte, error) {
	rec.cp.Agent = agentStateOf(rec.sess, rec.cp.Agent.Current)
	rec.cp.Session = sessionStateOf(rec.sess)
	return json.Marshal(rec.cp)
}

// currentAgentIs notes that sess's conversation was handed to the agent
// name by making it the runtime's current agent. The next save keeps it.
func (rec *runRecord) currentAgentIs(sess *session.Session, name string) {
	if rec == nil || rec.sess != sess {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.cp.Agent.Current = name
}

// finishCall has record put in the session the result of a tool call the
// runtime answers itself, and notes the call as finished, unless it already
// is. A failure stops the run, like any other of the record's.
func (rec *runRecord) finishCall(ctx context.Context, callID string, record func()) {
	if rec == nil {
		record()
		return
	}
	if rec.Call(callID).Progress == toolexec.CallFinished {
		return
	}
	_ = rec.CallFinished(ctx, callID, toolexec.CallOutcome{}, record)
}

// fail remembers why the record stopped following the run. A write that
// the run's own context cut short, cancelled or out of time, is no failure
// of the store's: that run is over, and its record is not kept for it.
func (rec *runRecord) fail(ctx context.Context, err error) error {
	if rec.failed == nil && ctx.Err() == nil {
		rec.failed = err
	}
	return err
}

// failure returns why the record stopped following the run, if it did.
func (rec *runRecord) failure() error {
	if rec == nil {
		return nil
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.failed
}

// journal returns the record as the journal of the run's tool calls.
func (rec *runRecord) journal() toolexec.Journal {
	if rec == nil {
		return nil
	}
	return rec
}

// Call implements [toolexec.Journal].
func (rec *runRecord) Call(callID string) toolexec.CallRecord {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.cp.Calls[callID]
}

// CallStarted implements [toolexec.Journal].
func (rec *runRecord) CallStarted(ctx context.Context, callID string, call tools.ToolCall) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.cp.Calls[callID].Progress == toolexec.CallStarted {
		// A call that is run again got no further than it had: the record
		// stands, and so does the count of the times the run died here.
		return rec.failed
	}
	return rec.noteCall(ctx, callID, toolexec.CallRecord{Progress: toolexec.CallStarted, Call: &call})
}

// CallRecorded implements [toolexec.Journal]. The record is locked while the
// result goes to the session, so every checkpoint holds the results of
// exactly the calls it says have theirs.
func (rec *runRecord) CallRecorded(ctx context.Context, callID string, result *tools.ToolCallResult, record func()) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	record()
	return rec.noteCall(ctx, callID, toolexec.CallRecord{
		Progress: toolexec.CallRecorded,
		Call:     rec.cp.Calls[callID].Call,
		Result:   result,
	})
}

// CallFinished implements [toolexec.Journal].
func (rec *runRecord) CallFinished(ctx context.Context, callID string, outcome toolexec.CallOutcome, record func()) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if record != nil {
		record()
	}
	return rec.noteCall(ctx, callID, toolexec.CallRecord{Progress: toolexec.CallFinished, Outcome: outcome})
}

// noteCall saves what the journal keeps of a call. The caller holds rec.mu.
func (rec *runRecord) noteCall(ctx context.Context, callID string, call toolexec.CallRecord) error {
	if rec.cp.Calls == nil {
		rec.cp.Calls = make(map[string]toolexec.CallRecord)
	}
	rec.cp.Calls[callID] = call
	return rec.save(ctx)
}

// settle drops the record of a run that is over, once the store holds all
// the run emitted. It keeps the record of a run its host suspended, noting
// that it did not crash, and of a run the store could not follow to its end:
// both can be continued. It returns what kept it from settling a run that
// got to its end: that run's record stays too, and its host is to know.
func (rec *runRecord) settle(ctx context.Context) error {
	if rec == nil {
		return nil
	}
	suspended := errors.Is(context.Cause(ctx), ErrSuspended)
	cancelled := ctx.Err() != nil
	ctx, cancel := rec.r.recordContext(ctx)
	defer cancel()

	var err error
	switch {
	case suspended:
		err = rec.r.runs.SuspendRun(ctx, rec.sess.ID)
	case rec.failure() != nil:
		// Whoever stops listening once the run has reported its error, the
		// record stays: the run can be taken up from where it last stood.
		return nil
	case cancelled:
		// A run its host cancelled is over, whatever of it reached the store.
		err = rec.r.runs.EndRun(ctx, rec.sess.ID)
	default:
		// Until the store holds the run's last words, the record is what
		// lets the run be found again.
		if err = rec.transcriptStored(ctx); err == nil {
			err = rec.r.runs.EndRun(ctx, rec.sess.ID)
		}
		return err
	}
	if err != nil {
		// The run was stopped: there is no one left to tell.
		slog.WarnContext(ctx, "Failed to settle run record", "session_id", rec.sess.ID, "suspended", suspended, "error", err)
	}
	return nil
}

// recordContext returns the context a run's record is written with when the
// write must not be cancelled with the run, only bounded.
func (r *LocalRuntime) recordContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), r.streamStoppedTimeout())
}

// stopUnrecorded reports that the run stops because its progress could not
// be recorded, and returns the reason it ends with. A cancelled run is
// stopping already and has nothing to report.
func (r *LocalRuntime) stopUnrecorded(ctx context.Context, sess *session.Session, a *agent.Agent, err error, events EventSink) string {
	if ctx.Err() != nil {
		return turnEndReasonCanceled
	}
	msg := fmt.Sprintf("The run stopped because its progress could not be recorded: %v", err)
	slog.ErrorContext(ctx, "Run record failed", "agent", a.Name(), "session_id", sess.ID, "error", err)
	events.Emit(ErrorForSession(sess.ID, msg))
	r.notifyError(ctx, a, sess.ID, msg)
	return turnEndReasonError
}
