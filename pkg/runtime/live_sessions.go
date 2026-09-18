package runtime

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/docker/docker-agent/pkg/session"
)

// LiveSession is one row of the /context team view: a currently running
// RunStream session (or the idle current root session) with its agent
// identity, session identity and context budget. Token counts come from the
// session's provider-reported cumulative usage; ContextLimit is 0 when the
// effective model's window cannot be resolved (harness-backed agents, models
// absent from the catalogue).
type LiveSession struct {
	SessionID    string `json:"session_id"`
	AgentName    string `json:"agent_name"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	ContextLimit int64  `json:"context_limit"`
	// Current marks the caller's current root session. It is listed even
	// while idle (no active stream), unlike child rows which exist only
	// while their RunStream is live.
	Current bool `json:"current"`

	// CompactionModel is the identity ("provider/model") of the row's agent
	// dedicated compaction model, set only when it actually caps ContextLimit
	// below the primary model's own window. Render-unused in the /context
	// dialog (the header's single cap statement is authoritative there); kept
	// on the struct as part of the JSON/API surface for other consumers.
	CompactionModel string `json:"compaction_model,omitempty"`
	// PrimaryContextLimit is the primary model's own context window, set
	// only alongside CompactionModel. Render-unused today (see CompactionModel).
	PrimaryContextLimit int64 `json:"primary_context_limit,omitempty"`
}

// UsedTokens returns the session's current context occupancy estimate.
func (s LiveSession) UsedTokens() int64 {
	return s.InputTokens + s.OutputTokens
}

// ShortID returns the first 8 characters of the session ID, enough to
// disambiguate concurrent runs of the same agent in UI listings.
func (s LiveSession) ShortID() string {
	if len(s.SessionID) <= 8 {
		return s.SessionID
	}
	return s.SessionID[:8]
}

// liveCompactionRequest is one queued explicit compaction of a live session,
// executed on that session's own run goroutine at a safe iteration boundary.
type liveCompactionRequest struct {
	additionalPrompt string
	events           EventSink
}

// liveSessionEntry tracks one active RunStream in the live-session registry.
type liveSessionEntry struct {
	sess *session.Session
	// agentName is resolved once at registration: exact for pinned
	// sessions (background agents) and equal to the current agent at
	// stream start otherwise (transfer_task swaps the current agent
	// before starting the child stream).
	agentName string
	// treeRootID is the ID of the root session of sess's tree, cached at
	// registration while the intermediate parents are still live.
	// LiveSessions scopes by this cached identity instead of re-walking
	// ParentID links through the registry, so a long-running nested
	// background agent stays attributed to its root after its intermediate
	// foreground parent finishes and unregisters. Immutable once the entry
	// is published.
	treeRootID string
	// idleRetry owns the single pre-response idle retry allowance for the
	// complete delegated transfer_task child run.
	idleRetry *idleStreamRetryAllowance
	// compactCh holds at most one pending explicit compaction request.
	// Sends happen only under liveSessionsMu while the entry is still
	// registered; the final drain runs after unregistration, so an
	// accepted request can never be stranded.
	compactCh chan liveCompactionRequest
}

// registerLiveSession adds sess to the live-session registry. Called by
// RunStream before the run goroutine starts so the session is targetable for
// the whole lifetime of its stream.
func (r *LocalRuntime) registerLiveSession(sess *session.Session) *liveSessionEntry {
	return r.registerLiveSessionWithIdleRetry(sess, defaultIdleStreamRetryPolicy())
}

func (r *LocalRuntime) registerLiveSessionWithIdleRetry(sess *session.Session, policy idleStreamRetryPolicy) *liveSessionEntry {
	entry := &liveSessionEntry{
		sess:      sess,
		agentName: r.sessionAgentName(sess),
		idleRetry: policy.allowance(),
		compactCh: make(chan liveCompactionRequest, 1),
	}
	r.liveSessionsMu.Lock()
	defer r.liveSessionsMu.Unlock()
	entry.treeRootID = r.treeRootIDLocked(sess)
	r.liveSessions[sess.ID] = entry
	return entry
}

// treeRootIDLocked resolves the tree root ID cached on a new entry; the
// caller must hold liveSessionsMu so the resolution and the entry's
// publication are one atomic step. A root session is its own root. A child
// whose parent entry is registered inherits the parent's cached root; the
// parent is live at this point in every real registration (transfer_task and
// background tasks start child streams from within the parent's own stream),
// which is what preserves nested lineage after the parent unregisters.
// Otherwise the parent is an unregistered root (typically the idle current
// session, which is only registered while its own stream runs) and the
// parent ID itself is the root.
func (r *LocalRuntime) treeRootIDLocked(sess *session.Session) string {
	if sess.ParentID == "" {
		return sess.ID
	}
	if parent, ok := r.liveSessions[sess.ParentID]; ok {
		return parent.treeRootID
	}
	return sess.ParentID
}

// finishLiveSession removes entry from the registry and then executes any
// already accepted compaction request. Removal happens under the registry
// lock BEFORE the drain: afterwards CompactLiveSession can no longer enqueue
// (it reports the session as not live), so everything the channel holds at
// drain time is processed and a request is either rejected or executed,
// never silently dropped.
func (r *LocalRuntime) finishLiveSession(ctx context.Context, entry *liveSessionEntry) {
	r.liveSessionsMu.Lock()
	if r.liveSessions[entry.sess.ID] == entry {
		delete(r.liveSessions, entry.sess.ID)
	}
	r.liveSessionsMu.Unlock()

	for {
		select {
		case req := <-entry.compactCh:
			r.runLiveCompactionRequest(ctx, entry.sess, req)
		default:
			return
		}
	}
}

// LiveSessions returns the caller's current root session followed by every
// currently live RunStream session that descends from it (foreground
// children and background agent tasks, nested sub-agents included). Descent
// is decided by each entry's tree root ID, cached at registration, so a
// nested background agent stays listed after its intermediate foreground
// parent finished and unregistered. Live sessions rooted outside current's
// tree (e.g. a stale root stream still draining after
// App.NewSession/ReplaceSession swapped the current session, and its
// descendants) are excluded. current may be nil, in which case every live
// entry is listed. Child rows are stable-sorted by agent name then session
// ID, so concurrent runs of the same agent stay distinct and the ordering is
// deterministic. current is listed even while idle.
func (r *LocalRuntime) LiveSessions(ctx context.Context, current *session.Session) []LiveSession {
	r.liveSessionsMu.Lock()
	entries := make([]*liveSessionEntry, 0, len(r.liveSessions))
	for _, entry := range r.liveSessions {
		if current != nil && (entry.sess.ID == current.ID || entry.treeRootID != current.ID) {
			continue
		}
		entries = append(entries, entry)
	}
	r.liveSessionsMu.Unlock()

	var rows []LiveSession
	if current != nil {
		rows = append(rows, r.liveSessionRow(ctx, current, r.sessionAgentName(current), true))
	}

	children := make([]LiveSession, 0, len(entries))
	for _, entry := range entries {
		children = append(children, r.liveSessionRow(ctx, entry.sess, entry.agentName, false))
	}
	slices.SortStableFunc(children, func(a, b LiveSession) int {
		if c := cmp.Compare(a.AgentName, b.AgentName); c != 0 {
			return c
		}
		return cmp.Compare(a.SessionID, b.SessionID)
	})
	return append(rows, children...)
}

// liveSessionRow builds one team-view row from a session and its agent.
func (r *LocalRuntime) liveSessionRow(ctx context.Context, sess *session.Session, agentName string, current bool) LiveSession {
	input, output := sess.Usage()
	row := LiveSession{
		SessionID:    sess.ID,
		AgentName:    agentName,
		InputTokens:  input,
		OutputTokens: output,
		Current:      current,
	}
	if a, err := r.team.Agent(agentName); err == nil && a != nil && !a.HasHarness() {
		modelID := r.getEffectiveModelID(ctx, a)
		row.ContextLimit = r.contextLimitForAgentModel(ctx, a, modelID)
		if a.CompactionModel() != nil {
			compactionModel, primaryLimit, compactionLimit := r.compactionCapAttribution(ctx, a, modelID)
			if compactionCaps(primaryLimit, compactionLimit) {
				row.CompactionModel = compactionModel
				row.PrimaryContextLimit = primaryLimit
			}
		}
	}
	return row
}

// CompactLiveSession queues an explicit manual compaction for the identified
// live session. The request is accepted non-blockingly and executes on the
// target session's own run goroutine at a safe iteration boundary (or during
// its final teardown drain), so it can never interleave with an in-flight
// model turn. Compaction and usage events are emitted to events; when hooks
// veto the compaction before any terminal event, a completed/skipped event
// is synthesized so the requester always observes a terminal signal.
//
// It returns an error when sessionID is not a live session (unknown or
// already finished) or when a request is already pending for it.
func (r *LocalRuntime) CompactLiveSession(_ context.Context, sessionID, additionalPrompt string, events EventSink) error {
	if events == nil {
		events = EventSinkFunc(func(Event) {})
	}

	r.liveSessionsMu.Lock()
	defer r.liveSessionsMu.Unlock()
	entry, ok := r.liveSessions[sessionID]
	if !ok {
		return fmt.Errorf("session %s is not live (unknown or already finished)", sessionID)
	}
	select {
	case entry.compactCh <- liveCompactionRequest{additionalPrompt: additionalPrompt, events: events}:
		return nil
	default:
		return fmt.Errorf("a compaction request is already pending for session %s", sessionID)
	}
}

// runQueuedCompaction executes at most one pending explicit compaction
// request for entry's session. It must only be called from the entry's own
// run goroutine, at iteration boundaries, so the compaction snapshot cannot
// race with messages appended by an active model turn. It drains exactly the
// entry owned by that RunStream, never a registry lookup by session ID,
// which under duplicate IDs would let an older stream steal a newer entry's
// request and compact the wrong in-memory session.
func (r *LocalRuntime) runQueuedCompaction(ctx context.Context, entry *liveSessionEntry) {
	select {
	case req := <-entry.compactCh:
		r.runLiveCompactionRequest(ctx, entry.sess, req)
	default:
	}
}

// runLiveCompactionRequest performs one queued manual compaction, forwarding
// all resulting events to the request's sink. When no terminal compaction
// event was emitted (a pre_compact or before_compaction hook vetoed the run),
// a completed/skipped event is synthesized, mirroring App.CompactSession's
// behavior for the root /compact path.
func (r *LocalRuntime) runLiveCompactionRequest(ctx context.Context, sess *session.Session, req liveCompactionRequest) {
	// With ctx already cancelled (a teardown drain after Ctrl+C), attempting
	// the compaction model call would fail immediately and emit a noisy
	// started/failed pair. Consume the request and report a single terminal
	// skipped event instead, so the requester still observes a terminal
	// signal without a phantom failure.
	if ctx.Err() != nil {
		slog.InfoContext(ctx, "Skipping explicit compaction for live session: context already cancelled",
			"session_id", sess.ID, "agent", r.sessionAgentName(sess))
		req.events.Emit(SessionCompactionCompleted(sess.ID, CompactionOutcomeSkipped, r.sessionAgentName(sess)))
		return
	}

	slog.InfoContext(ctx, "Running explicit compaction for live session",
		"session_id", sess.ID, "agent", r.sessionAgentName(sess))

	completed := false
	sink := EventSinkFunc(func(event Event) {
		if e, ok := event.(*SessionCompactionEvent); ok && e.Status == "completed" {
			completed = true
		}
		req.events.Emit(event)
	})
	r.compactWithReason(ctx, sess, req.additionalPrompt, compactionReasonManual, sink)
	if !completed {
		req.events.Emit(SessionCompactionCompleted(sess.ID, CompactionOutcomeSkipped, r.sessionAgentName(sess)))
	}
}

// sessionAgentName resolves the display agent name for sess, tolerating a
// nil resolution (which resolveSessionAgent's contract makes unexpected).
func (r *LocalRuntime) sessionAgentName(sess *session.Session) string {
	if a := r.resolveSessionAgent(sess); a != nil {
		return a.Name()
	}
	return ""
}
