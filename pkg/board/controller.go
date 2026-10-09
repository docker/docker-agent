package board

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// sessionClient is the slice of the control-plane client the controller
// needs, as an interface so tests can inject a fake without real sockets.
type sessionClient interface {
	Snapshot(ctx context.Context) (snapshot, error)
	StreamEvents(ctx context.Context, since uint64, onEvent func(event) bool) error
	Followup(ctx context.Context, idempotencyKey, message string) error
}

const (
	// retryDelay paces reconnect and relaunch attempts.
	retryDelay = 500 * time.Millisecond
	// cappedRetryDelay paces the watcher of a card whose agent keeps
	// crashing at startup (see maxLaunchFailures): the card is red and only
	// a user action revives it, so polling every retryDelay would waste a
	// tmux fork per tick for nothing.
	cappedRetryDelay = 5 * time.Second
	// snapshotTimeout bounds a single snapshot request so a wedged server
	// cannot block a watcher forever.
	snapshotTimeout = 10 * time.Second
	// followupTimeout bounds a single follow-up delivery.
	followupTimeout = 10 * time.Second
	// readyProbeTimeout bounds the control-plane probe behind "attach" so
	// the user gets quick feedback instead of hanging.
	readyProbeTimeout = 2 * time.Second
	// maxLaunchFailures is how many consecutive agent deaths — before the
	// control plane ever answers — the watcher tolerates. At the cap the
	// agent is crashing deterministically (bad config, missing key…):
	// relaunching again would only kill the dead pane holding the error
	// output, so the watcher goes red and leaves the pane for inspection.
	// A successful snapshot or a user-initiated (prompt-bearing) relaunch
	// resets the count.
	maxLaunchFailures = 3
)

// controller keeps each card in sync with its agent's control plane. One
// watcher goroutine per card tails the session event stream and mirrors the
// running/waiting status and the title into the store, reconnecting — and
// relaunching the tmux session if the agent died — as needed.
type controller struct {
	// ctx is the board-lifetime context watchers derive from; they are
	// started lazily (Start) after construction, so it is held here rather
	// than passed.
	ctx       context.Context //nolint:containedctx // base context for background watchers
	store     *Store
	sessions  sessionManager
	onChanged func()
	clientFor func(socket, session string) sessionClient

	mu       sync.Mutex
	watchers map[string]*watcher
	// cards holds per-card recovery state. It lives on the controller — not
	// in the watcher loop — so user-initiated relaunches (SendPrompt), which
	// run outside the watcher, can read and update it too.
	cards map[string]*cardState

	// relaunchMu serializes session relaunches. A watcher's background
	// resume and a prompt-bearing relaunch (SendPrompt) can otherwise race:
	// one kills the session the other just created, dropping its prompt.
	relaunchMu sync.Mutex
}

// watcher tracks a running watch goroutine so it can be cancelled and waited on.
type watcher struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// cardState is the controller's per-card recovery state, guarded by c.mu.
type cardState struct {
	// expectTurn marks a card whose latest launch carried an initial prompt:
	// a first turn is imminent, so the watcher keeps it "starting" until the
	// event stream reports it instead of flashing "ready" first.
	expectTurn bool
	// launchFailures counts consecutive agent deaths before the control
	// plane ever answered. A user-initiated relaunch resets it to give the
	// agent a fresh set of attempts after the cap tripped.
	launchFailures int
	// launchError keeps the last failed relaunch's error. When a session
	// cannot even be recreated there is no dead pane to attach to and read,
	// so this is the only record of why the card went red.
	launchError error
}

func newController(ctx context.Context, store *Store, sessions sessionManager, onChanged func()) *controller {
	return &controller{
		ctx:       ctx,
		store:     store,
		sessions:  sessions,
		onChanged: onChanged,
		clientFor: func(socket, session string) sessionClient { return newClient(socket, session) },
		watchers:  make(map[string]*watcher),
		cards:     make(map[string]*cardState),
	}
}

// ReconcileAll starts a watcher for every existing card. Called on startup
// so the board reattaches to sessions still running in tmux.
func (c *controller) ReconcileAll() {
	for _, card := range c.store.ListCards() {
		c.Start(card)
	}
}

// Start ensures a watcher is running for the card. Idempotent.
func (c *controller) Start(card *Card) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.watchers[card.ID]; ok {
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	w := &watcher{cancel: cancel, done: make(chan struct{})}
	c.watchers[card.ID] = w
	go func() {
		defer close(w.done)
		c.watch(ctx, card.ID)
	}()
}

// ExpectTurn records that the card's agent was just launched with an
// initial prompt, so its first turn is imminent.
func (c *controller) ExpectTurn(cardID string) {
	c.setExpectTurn(cardID, true)
}

// stateLocked returns the card's state, creating it on first use. Callers
// must hold c.mu.
func (c *controller) stateLocked(cardID string) *cardState {
	s, ok := c.cards[cardID]
	if !ok {
		s = &cardState{}
		c.cards[cardID] = s
	}
	return s
}

func (c *controller) setExpectTurn(cardID string, expect bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stateLocked(cardID).expectTurn = expect
}

func (c *controller) turnExpected(cardID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.cards[cardID]
	return s != nil && s.expectTurn
}

// launchFailed records one more agent death before the control plane ever
// answered, and returns the updated consecutive count.
func (c *controller) launchFailed(cardID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stateLocked(cardID)
	s.launchFailures++
	return s.launchFailures
}

func (c *controller) resetLaunchFailures(cardID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.cards[cardID]; s != nil {
		s.launchFailures = 0
	}
}

func (c *controller) setLaunchError(cardID string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stateLocked(cardID).launchError = err
}

// LaunchError returns the last failed relaunch's error for the card, or nil.
// It explains why an errored card may have no session left to attach to.
func (c *controller) LaunchError(cardID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.cards[cardID]; s != nil {
		return s.launchError
	}
	return nil
}

// Stop cancels the card's watcher and waits for it to exit. Waiting matters:
// it guarantees the watcher cannot relaunch the session after the caller
// goes on to tear it down (kill the tmux session, remove the worktree),
// which would otherwise leave an orphaned session. The card's controller
// state is dropped only after the wait, so a watcher mid-relaunch cannot
// re-add entries behind the cleanup.
func (c *controller) Stop(cardID string) {
	c.mu.Lock()
	w, ok := c.watchers[cardID]
	delete(c.watchers, cardID)
	c.mu.Unlock()
	if ok {
		w.cancel()
		<-w.done
	}
	c.forget(cardID)
}

// forget drops all per-card controller state.
func (c *controller) forget(cardID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cards, cardID)
}

// watch keeps one card mirrored to its control plane: snapshot to resync,
// then tail events; on a drop, reconnect; if the agent is gone, relaunch and
// resume.
func (c *controller) watch(ctx context.Context, cardID string) {
	for ctx.Err() == nil {
		card, err := c.store.GetCard(cardID)
		if errors.Is(err, ErrCardNotFound) {
			return // card deleted
		}
		if err != nil {
			if sleep(ctx, retryDelay) {
				return
			}
			continue
		}
		client := c.clientFor(socketPath(card.AgentSession), card.AgentSession)

		sctx, scancel := context.WithTimeout(ctx, snapshotTimeout)
		snap, err := client.Snapshot(sctx)
		scancel()
		if err != nil {
			// The control plane is unreachable. If the agent's tmux pane is
			// gone, relaunch to resume; otherwise it is still starting, so
			// just wait and retry. An agent that keeps dying before ever
			// answering is crashing at startup: surface the failure instead
			// of silently relaunching forever, and stop killing the dead
			// pane so the user can attach and read the agent's last output.
			delay := retryDelay
			if alive, aerr := c.sessions.Alive(card.Session); aerr == nil && !alive {
				if c.launchFailed(cardID) >= maxLaunchFailures {
					c.setStatus(cardID, StatusError)
					delay = cappedRetryDelay
				} else {
					c.resume(card)
				}
			} else if card.Status.StartingUp() {
				// The agent is up but has not answered yet: report how far
				// its startup got, so a stuck launch shows where it stopped.
				c.setStatus(cardID, startupPhase(card))
			}
			if sleep(ctx, delay) {
				return
			}
			continue
		}
		c.resetLaunchFailures(cardID)

		if snap.Title != "" {
			c.setTitle(cardID, snap.Title)
		}

		// The control plane answers: the agent has started. If the card is
		// still in a startup phase, default to waiting; the event replay
		// below promptly corrects it if a turn is already underway. Checking
		// the loop-top read is safe: besides this watcher, the only other
		// status writer (relaunch) writes StatusStarting, a startup phase.
		// Exception: a launch that carried an initial prompt is about to run
		// its first turn, so the card stays "starting" until stream_started
		// flips it to running — flashing "ready" before the first turn would
		// misreport when the card is really done.
		if card.Status.StartingUp() && !c.turnExpected(cardID) {
			c.setStatus(cardID, StatusWaiting)
		}

		// Derive the running state from the event stream. Tail from the
		// start of the buffer (since 0) so the whole backlog is replayed: a
		// turn that began before this watcher connected is still seen and
		// keeps the card running.
		//
		// A turn can spawn nested streams: every sub-agent and skill emits
		// its own stream_started/stream_stopped pair. The depth keeps the
		// card running until the outermost stream stops. Delivery of stream
		// events is best-effort, so user_message — emitted only for real
		// user turns, right before the turn's outermost stream_started — is
		// the recovery point that resets a drifted depth.
		depth := 0
		// failed marks that the current turn emitted an error event. It is
		// applied immediately (the error event is delivered reliably, the
		// stream_stopped that follows is not), and cleared when the
		// outermost stop reports a "normal" completion or a new turn begins.
		failed := false
		// paused marks that the run loop is blocked on /pause. There is no
		// matching resume event, so any subsequent event — the loop emits
		// nothing while blocked — means the session resumed.
		paused := false

		// Events at or below the snapshot's seq are replayed history. Their
		// intermediate statuses must not be broadcast on every reconnect — a
		// long-resolved error would flash the card red each time — so they
		// only update the derived state, which is applied once, when the
		// replay catches up with the snapshot. Replayed titles are dropped
		// entirely: the snapshot's title already reflects them.
		replaying := snap.LastEventSeq > 0
		var replayStatus CardStatus
		flushReplay := func() {
			replaying = false
			if replayStatus != "" {
				c.setStatus(cardID, replayStatus)
			}
		}
		setStatus := func(status CardStatus) {
			if replaying {
				replayStatus = status
			} else {
				c.setStatus(cardID, status)
			}
		}

		exited := false
		_ = client.StreamEvents(ctx, 0, func(ev event) bool {
			if replaying && (ev.Seq == 0 || ev.Seq > snap.LastEventSeq) {
				flushReplay() // past the snapshot: this event is live
			}
			switch ev.Type {
			case eventGap:
				return false // resume point evicted: reconnect and re-snapshot
			case eventSessionExited:
				exited = true
				return false
			case eventUserMessage:
				// A new user turn begins: any leftover depth is drift from
				// dropped stream events. Resync here so one lost stop cannot
				// leave the card stuck running forever.
				depth = 0
				failed = false
			case eventStreamStarted:
				failed = false
				paused = false
				c.setExpectTurn(cardID, false) // the expected turn arrived
				depth++
				setStatus(StatusRunning)
			case eventError:
				failed = true
				paused = false
				setStatus(StatusError)
			case eventStreamStopped:
				paused = false
				if depth > 0 {
					depth--
				}
				if depth == 0 {
					// The outermost stream ended: a "normal" reason means
					// the turn completed even if a nested sub-agent errored
					// along the way, so the sticky error is cleared. Any
					// other reason leaves a failed turn red.
					if ev.Reason == reasonNormal {
						failed = false
					}
					if !failed {
						setStatus(StatusWaiting)
					}
				}
			case eventRuntimePaused:
				paused = true
				setStatus(StatusPaused)
			case eventSessionTitle:
				if !replaying {
					c.setTitle(cardID, ev.Title)
				}
			default:
				// The run loop emits nothing while blocked on /pause, so any
				// other event means the session resumed mid-turn.
				if paused {
					paused = false
					setStatus(StatusRunning)
				}
			}
			if replaying && ev.Seq == snap.LastEventSeq {
				flushReplay() // caught up with the snapshot
			}
			return true
		})

		if exited && ctx.Err() == nil {
			// The agent process ended; resume it so the card stays usable.
			c.resume(card)
		}
		if sleep(ctx, retryDelay) {
			return
		}
	}
}

// startupPhase derives how far a launching agent got from the milestones it
// materializes on disk: the worktree first, then the control-plane socket.
// relaunch removes the stale socket before starting, so within one launch
// the phase only moves forward (a watcher racing a concurrent relaunch may
// report a phase one poll stale, which the next poll corrects).
func startupPhase(card *Card) CardStatus {
	if _, err := os.Stat(socketPath(card.AgentSession)); err == nil {
		return StatusAttaching
	}
	if _, err := os.Stat(card.Worktree); err == nil {
		return StatusLoading
	}
	return StatusStarting
}

// resume relaunches the card's session in the background recovery paths
// (dead pane, session_exited). A session that cannot even be recreated
// (tmux failure, missing worktree…) is surfaced as an errored card rather
// than left "starting" forever; a deleted card is not stamped.
func (c *controller) resume(card *Card) {
	if err := c.relaunch(card, ""); err != nil && !errors.Is(err, ErrCardNotFound) {
		c.setStatus(card.ID, StatusError)
	}
}

// setStatus writes only the status field, and only on change, notifying so
// the UI refreshes.
func (c *controller) setStatus(cardID string, status CardStatus) {
	if changed, err := c.store.UpdateCardStatus(cardID, status); err == nil && changed {
		c.onChanged()
	}
}

// setTitle writes only the title field, and only on change.
func (c *controller) setTitle(cardID, title string) {
	if changed, err := c.store.UpdateCardTitle(cardID, title); err == nil && changed {
		c.onChanged()
	}
}

// Ready reports whether the card's agent control plane answers, i.e. the
// agent process has really started and its UI is worth attaching to;
// otherwise the session still shows the bare launch command.
func (c *controller) Ready(card *Card) bool {
	client := c.clientFor(socketPath(card.AgentSession), card.AgentSession)
	ctx, cancel := context.WithTimeout(c.ctx, readyProbeTimeout)
	defer cancel()
	_, err := client.Snapshot(ctx)
	return err == nil
}

// SendPrompt delivers a prompt to the card's agent through the control
// plane. The follow-up carries an idempotency key so the control plane can
// dedupe a retried delivery. If the follow-up fails only because the agent
// (or its tmux session) is gone, the session is relaunched with the prompt
// as its next message; any other failure (busy, queue full, timeout) is
// surfaced rather than destroying a live session.
func (c *controller) SendPrompt(card *Card, prompt string) error {
	if prompt == "" {
		return nil
	}

	client := c.clientFor(socketPath(card.AgentSession), card.AgentSession)
	ctx, cancel := context.WithTimeout(c.ctx, followupTimeout)
	defer cancel()
	if err := client.Followup(ctx, newID(), prompt); err == nil {
		return nil
	} else if alive, aerr := c.sessions.Alive(card.Session); aerr != nil || alive {
		return fmt.Errorf("deliver prompt: %w", err)
	}

	return c.relaunch(card, prompt)
}

// relaunch recreates the card's tmux session under the same name, resuming
// the same docker-agent session (and its worktree) on the same control-plane
// socket. A non-empty prompt is delivered as the resumed session's next
// message. Launching from the worktree keeps the agent isolated even if
// docker-agent's own worktree reattachment does not happen.
func (c *controller) relaunch(card *Card, prompt string) error {
	c.relaunchMu.Lock()
	defer c.relaunchMu.Unlock()

	// The card may have been deleted while this relaunch was pending
	// (SendPrompt runs outside the watcher, so Stop does not cover it).
	// Teardown holds the same lock, so after this check the session cannot
	// be resurrected behind a delete.
	if _, err := c.store.GetCard(card.ID); err != nil {
		return err
	}

	// A concurrent relaunch may have already resurrected the session; a
	// plain resume must not kill it (and drop its queued prompt) just to
	// start over. A prompt-bearing relaunch proceeds: its prompt must be
	// delivered, and the session it kills is one that just rejected the
	// follow-up.
	if prompt == "" {
		if alive, err := c.sessions.Alive(card.Session); err == nil && alive {
			return nil
		}
	}

	_ = c.sessions.KillSession(card.Session)
	socket := socketPath(card.AgentSession)
	// A killed agent leaves its control-plane socket file behind. Remove it
	// so the resumed run can bind --listen; otherwise the new agent fails to
	// start and the card stays stuck "starting".
	_ = os.Remove(socket)
	// docker agent creates the worktree on the first launch; if that launch
	// died before it did, resuming from the worktree directory would fail.
	// Launch from the repository again so --worktree (re)creates it. Only a
	// confirmed absence takes the fallback: recreating an existing worktree
	// fails, so a transient stat error must not reroute the launch.
	workDir, worktreeName, worktreeBase := card.Worktree, "", ""
	if _, statErr := os.Stat(card.Worktree); card.Worktree != "" && errors.Is(statErr, fs.ErrNotExist) {
		workDir = card.RepoPath
		worktreeName = filepath.Base(card.Worktree)
		worktreeBase = upstreamBase(c.ctx, card.RepoPath)
	}
	err := c.sessions.NewSession(
		card.Session, workDir, card.Agent, card.AgentSession,
		socket, worktreeName, worktreeBase, prompt,
	)
	c.setLaunchError(card.ID, err)
	if err == nil {
		// The agent is launching again: show it as starting until its
		// control plane answers and the event stream drives the status.
		c.setExpectTurn(card.ID, prompt != "")
		if prompt != "" {
			// A user asked for this relaunch: give the agent a fresh set of
			// startup attempts even if the crash-loop cap tripped before.
			c.resetLaunchFailures(card.ID)
		}
		c.setStatus(card.ID, StatusStarting)
	}
	return err
}

// Teardown kills the card's tmux session under the relaunch lock, so an
// in-flight relaunch cannot recreate a session the caller is tearing down.
// The caller must have removed the card from the store first: that is what
// makes a later relaunch abort instead of resurrecting the session. The
// card's controller state is dropped again here: a SendPrompt relaunch runs
// outside the watcher (so Stop does not wait for it) and may have re-added
// entries; holding the lock guarantees it has finished.
func (c *controller) Teardown(card *Card) {
	c.relaunchMu.Lock()
	defer c.relaunchMu.Unlock()
	_ = c.sessions.KillSession(card.Session)
	c.forget(card.ID)
}

// sleep waits for d or until ctx is done, reporting whether ctx was done.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}
