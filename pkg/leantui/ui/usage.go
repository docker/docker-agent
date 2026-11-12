package ui

// UsageSnapshot is the per-session token and cost usage summarized in the
// status footer.
type UsageSnapshot struct {
	ContextLength       int64
	ContextLimit        int64
	Tokens              int64
	Cost                float64
	CompactionThreshold float64
}

// UsageTracker aggregates per-session token usage so the footer can show the
// active session's context window alongside the total cost of the whole run
// (the root session plus any nested agent sessions). It keeps a stack of
// in-flight sessions so the "active" session is whichever stream is on top.
type UsageTracker struct {
	bySession       map[string]UsageSnapshot
	rootSessionID   string
	latestSessionID string
	stack           []string
}

func NewUsageTracker() *UsageTracker {
	return &UsageTracker{bySession: map[string]UsageSnapshot{}}
}

func (u *UsageTracker) Reset() {
	u.bySession = map[string]UsageSnapshot{}
	u.rootSessionID = ""
	u.latestSessionID = ""
	u.stack = nil
}

// RecoverIdle restores root context selection without discarding accounted usage.
func (u *UsageTracker) RecoverIdle(sessionID string) {
	u.stack = nil
	if sessionID != "" {
		u.rootSessionID = sessionID
	}
}

// StreamStarted pushes a newly-started session onto the active stack, adopting
// the first one as the root session.
func (u *UsageTracker) StreamStarted(sessionID string) {
	if sessionID == "" {
		return
	}
	if len(u.stack) == 0 {
		u.rootSessionID = sessionID
	}
	u.stack = append(u.stack, sessionID)
}

// StreamStopped pops the most recently-started session off the active stack.
func (u *UsageTracker) StreamStopped() {
	if n := len(u.stack); n > 0 {
		u.stack = u.stack[:n-1]
	}
}

// Record stores usage for a session, adopting the first session seen as the
// root when no stream has started yet.
func (u *UsageTracker) Record(sessionID string, snapshot UsageSnapshot) {
	if u.rootSessionID == "" && len(u.bySession) == 0 {
		u.rootSessionID = sessionID
	}
	u.bySession[sessionID] = snapshot
	u.latestSessionID = sessionID
}

func (u *UsageTracker) Empty() bool { return len(u.bySession) == 0 }

func (u *UsageTracker) TotalCost() float64 {
	var total float64
	for _, usage := range u.bySession {
		total += usage.Cost
	}
	return total
}

// Active returns the usage of the session whose context the footer should show:
// the top of the active stack, else the root, else the most recent, else the
// sole recorded session.
func (u *UsageTracker) Active() (UsageSnapshot, bool) {
	if n := len(u.stack); n > 0 {
		usage, ok := u.bySession[u.stack[n-1]]
		return usage, ok
	}
	if u.rootSessionID != "" {
		usage, ok := u.bySession[u.rootSessionID]
		return usage, ok
	}
	if u.latestSessionID != "" {
		usage, ok := u.bySession[u.latestSessionID]
		return usage, ok
	}
	if len(u.bySession) == 1 {
		for _, usage := range u.bySession {
			return usage, true
		}
	}
	return UsageSnapshot{}, false
}

func (u *UsageTracker) RootSessionID() string {
	return u.rootSessionID
}
