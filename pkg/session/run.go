package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Run is the durable record of a root run in flight. A record that outlives
// the process running it marks a run that was cut off and can be continued.
type Run struct {
	SessionID string
	// Agent is the agent the run started with.
	Agent string
	// Current is the agent the run's conversation is with now. It differs
	// from Agent once the run has handed the conversation on.
	Current string
	// Suspended is set when the host stopped the run on purpose.
	Suspended bool
	// Crashes counts the times in a row the run died without being suspended.
	Crashes int
}

// RunStore records runs in flight. Runs it lists that no process is running
// were cut off; the store cannot tell which those are when processes share it.
type RunStore interface {
	// BeginRun records a new run by agent and stores its input, if any, in
	// the same write.
	BeginRun(ctx context.Context, sessionID, agent string, input *Message) error
	// MoveRun records that the run's conversation is now with agent.
	MoveRun(ctx context.Context, sessionID, agent string) error
	// ContinueRun records that the session's run is being continued and
	// returns it. A run that was not suspended counts one more crash.
	ContinueRun(ctx context.Context, sessionID string) (Run, error)
	// SuspendRun keeps the record of a run its host stopped on purpose.
	SuspendRun(ctx context.Context, sessionID string) error
	// EndRun removes the record of a run that is over.
	EndRun(ctx context.Context, sessionID string) error
	ListRuns(ctx context.Context) ([]Run, error)
}

func (s *InMemorySessionStore) BeginRun(ctx context.Context, sessionID, agent string, input *Message) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	if _, exists := s.sessions.Load(sessionID); !exists {
		return ErrNotFound
	}
	if input != nil {
		if _, err := s.AddMessage(ctx, sessionID, input); err != nil {
			return err
		}
	}
	s.setRunLocked(Run{SessionID: sessionID, Agent: agent, Current: agent})
	return nil
}

func (s *InMemorySessionStore) MoveRun(_ context.Context, sessionID, agent string) error {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	if run, recorded := s.runs[sessionID]; recorded {
		run.Current = agent
		s.runs[sessionID] = run
	}
	return nil
}

func (s *InMemorySessionStore) ContinueRun(_ context.Context, sessionID string) (Run, error) {
	if sessionID == "" {
		return Run{}, ErrEmptyID
	}
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	if _, exists := s.sessions.Load(sessionID); !exists {
		return Run{}, ErrNotFound
	}
	run, recorded := s.runs[sessionID]
	if recorded && !run.Suspended {
		run.Crashes++
	}
	run.SessionID = sessionID
	run.Suspended = false
	s.setRunLocked(run)
	return run, nil
}

func (s *InMemorySessionStore) SuspendRun(_ context.Context, sessionID string) error {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	if run, recorded := s.runs[sessionID]; recorded {
		run.Suspended = true
		s.runs[sessionID] = run
	}
	return nil
}

func (s *InMemorySessionStore) EndRun(_ context.Context, sessionID string) error {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	delete(s.runs, sessionID)
	return nil
}

func (s *InMemorySessionStore) ListRuns(context.Context) ([]Run, error) {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	runs := make([]Run, 0, len(s.runs))
	for _, run := range s.runs {
		runs = append(runs, run)
	}
	slices.SortFunc(runs, func(a, b Run) int { return strings.Compare(a.SessionID, b.SessionID) })
	return runs, nil
}

// forgetRuns drops the records of deleted sessions.
func (s *InMemorySessionStore) forgetRuns(deleted map[string]struct{}) {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	for id := range deleted {
		delete(s.runs, id)
	}
}

func (s *InMemorySessionStore) setRunLocked(run Run) {
	if s.runs == nil {
		s.runs = make(map[string]Run)
	}
	s.runs[run.SessionID] = run
}

// recordRun inserts the record of an existing session's run; callers append
// what to set when the session already has one.
const recordRun = `INSERT INTO session_runs (session_id, agent, current_agent)
	SELECT ?, ?, ? WHERE EXISTS (SELECT 1 FROM sessions WHERE id = ?)
	ON CONFLICT(session_id) DO UPDATE SET `

func (s *SQLiteSessionStore) BeginRun(ctx context.Context, sessionID, agent string, input *Message) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx,
		recordRun+`agent = excluded.agent, current_agent = excluded.current_agent, suspended = 0, crashes = 0`,
		sessionID, agent, agent, sessionID)
	if err != nil {
		return fmt.Errorf("recording run: %w", err)
	}
	recorded, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if recorded == 0 {
		return ErrNotFound
	}
	if input != nil {
		if _, err := insertMessage(ctx, tx, sessionID, input); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteSessionStore) ContinueRun(ctx context.Context, sessionID string) (Run, error) {
	if sessionID == "" {
		return Run{}, ErrEmptyID
	}
	run := Run{SessionID: sessionID}
	err := s.db.QueryRowContext(ctx,
		recordRun+`crashes = crashes + (suspended = 0), suspended = 0 RETURNING agent, current_agent, crashes`,
		sessionID, "", "", sessionID).Scan(&run.Agent, &run.Current, &run.Crashes)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("continuing run: %w", err)
	}
	return run, nil
}

func (s *SQLiteSessionStore) MoveRun(ctx context.Context, sessionID, agent string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_runs SET current_agent = ? WHERE session_id = ?`, agent, sessionID)
	return err
}

func (s *SQLiteSessionStore) SuspendRun(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_runs SET suspended = 1 WHERE session_id = ?`, sessionID)
	return err
}

func (s *SQLiteSessionStore) EndRun(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM session_runs WHERE session_id = ?`, sessionID)
	return err
}

func (s *SQLiteSessionStore) ListRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, agent, current_agent, suspended, crashes FROM session_runs ORDER BY session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		var run Run
		if err := rows.Scan(&run.SessionID, &run.Agent, &run.Current, &run.Suspended, &run.Crashes); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}
