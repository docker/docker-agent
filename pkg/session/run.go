package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Run is the durable record of a run in flight: where the run stood when it
// last finished a step. A record that outlives the process running it marks a
// run that was cut off and can be taken up again.
type Run struct {
	SessionID string
	// Checkpoint is the state of the run after its last step, as the runtime
	// that took it wrote it. It stands for the transcript as it was stored
	// at that moment, not for anything stored since.
	Checkpoint []byte
	// Suspended is set when the host stopped the run on purpose.
	Suspended bool
	// Crashes counts the times in a row the run died at the same step
	// without being suspended.
	Crashes int
}

// RunStore keeps the records of runs in flight. A run it lists that no
// process is running was cut off; the store cannot tell which those are when
// processes share it.
type RunStore interface {
	// BeginRun records a new run of the session, replacing the record of
	// any earlier one, and stores what the run starts from in the same
	// write: the session itself, and its conversation. A session whose
	// conversation the store holds nothing of is stored whole, with
	// whatever conversation it was given; one it holds already gets input
	// added, which is the run's prompt and the last message of sess, if the
	// run has one. The usage of side requests is no part of a conversation:
	// a store that holds only that of a session holds nothing of its
	// conversation.
	BeginRun(ctx context.Context, sess *Session, input *Message, checkpoint []byte) error
	// SaveRun replaces the run's checkpoint, which then stands for the
	// transcript as it is stored now. The run has made progress: its crash
	// count starts over.
	SaveRun(ctx context.Context, sessionID string, checkpoint []byte) error
	// ResumeRun takes the run up again and returns it. What was stored of
	// the transcript after its checkpoint belongs to a step that never
	// finished and is removed, except what is not the run's to redo: the
	// usage of a side request that was paid for in the meantime stays. A
	// run that was not suspended counts one more crash.
	ResumeRun(ctx context.Context, sessionID string) (Run, error)
	// SuspendRun marks the run as stopped on purpose.
	SuspendRun(ctx context.Context, sessionID string) error
	// EndRun removes the record of a run that is over.
	EndRun(ctx context.Context, sessionID string) error
	ListRuns(ctx context.Context) ([]Run, error)
}

// transcriptLength counts the items stored for a session.
const transcriptLength = `(SELECT COALESCE(MAX(position), -1) + 1 FROM session_items WHERE session_id = ?)`

func (s *SQLiteSessionStore) BeginRun(ctx context.Context, sess *Session, input *Message, checkpoint []byte) error {
	if sess.ID == "" {
		return ErrEmptyID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.updateSessionTx(ctx, tx, sess); err != nil {
		return fmt.Errorf("storing session: %w", err)
	}
	var stored, conversation int
	if err := tx.QueryRowContext(ctx,
		"SELECT "+transcriptLength+`, (SELECT COUNT(*) FROM session_items WHERE session_id = ? AND item_type != 'auxiliary_usage')`,
		sess.ID, sess.ID).Scan(&stored, &conversation); err != nil {
		return fmt.Errorf("begin run: %w", err)
	}
	var items []Item
	switch {
	case conversation == 0:
		// What the store holds, if anything, is the usage of side requests
		// made before the session was ever run. The session holds it too,
		// in its place among the rest: the transcript is stored as it is.
		if _, err := tx.ExecContext(ctx, `DELETE FROM session_items WHERE session_id = ?`, sess.ID); err != nil {
			return fmt.Errorf("begin run: %w", err)
		}
		items, stored = sess.MessagesSnapshot(), 0
	case input != nil:
		items = []Item{NewMessageItem(input)}
	}
	for i, item := range items {
		if err := s.addItemTx(ctx, tx, sess.ID, stored+i, item); err != nil {
			return fmt.Errorf("storing what the run starts from: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session_runs (session_id, checkpoint, items) VALUES (?, ?, `+transcriptLength+`)
		 ON CONFLICT(session_id) DO UPDATE SET
			checkpoint = excluded.checkpoint, items = excluded.items, suspended = 0, crashes = 0`,
		sess.ID, string(checkpoint), sess.ID); err != nil {
		return fmt.Errorf("recording run: %w", err)
	}
	return tx.Commit()
}

func (s *SQLiteSessionStore) SaveRun(ctx context.Context, sessionID string, checkpoint []byte) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE session_runs SET checkpoint = ?, items = `+transcriptLength+`, crashes = 0 WHERE session_id = ?`,
		string(checkpoint), sessionID, sessionID)
	if err != nil {
		return fmt.Errorf("saving run: %w", err)
	}
	return requireRow(result)
}

func (s *SQLiteSessionStore) ResumeRun(ctx context.Context, sessionID string) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, fmt.Errorf("resume run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	run := Run{SessionID: sessionID}
	var checkpoint string
	var items int
	err = tx.QueryRowContext(ctx,
		`UPDATE session_runs SET crashes = crashes + (suspended = 0), suspended = 0 WHERE session_id = ?
		 RETURNING checkpoint, items, crashes`,
		sessionID).Scan(&checkpoint, &items, &run.Crashes)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("resuming run: %w", err)
	}
	run.Checkpoint = []byte(checkpoint)

	// Sub-sessions hang off the items that are about to go.
	cut, err := subSessionsFrom(ctx, tx, sessionID, items)
	if err != nil {
		return Run{}, fmt.Errorf("resuming run: %w", err)
	}
	for _, subSessionID := range cut {
		if err := deleteSessionTreeTx(ctx, tx, subSessionID); err != nil && !errors.Is(err, ErrNotFound) {
			return Run{}, fmt.Errorf("resuming run: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM session_items WHERE session_id = ? AND position >= ? AND item_type != 'auxiliary_usage'`,
		sessionID, items); err != nil {
		return Run{}, fmt.Errorf("resuming run: %w", err)
	}
	if err := closeRanks(ctx, tx, sessionID, items); err != nil {
		return Run{}, fmt.Errorf("resuming run: %w", err)
	}
	return run, tx.Commit()
}

// closeRanks moves the items left at or after position up to it, in their
// order, so the transcript has no gaps where items were removed.
func closeRanks(ctx context.Context, tx *sql.Tx, sessionID string, position int) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM session_items WHERE session_id = ? AND position >= ? ORDER BY position, id`,
		sessionID, position)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE session_items SET position = ? WHERE id = ?`, position+i, id); err != nil {
			return err
		}
	}
	return nil
}

// subSessionsFrom lists the sub-sessions a session's items refer to, from
// the item at position on.
func subSessionsFrom(ctx context.Context, tx *sql.Tx, sessionID string, position int) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT subsession_id FROM session_items
		 WHERE session_id = ? AND position >= ? AND item_type = 'subsession' AND subsession_id IS NOT NULL`,
		sessionID, position)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, checkpoint, suspended, crashes FROM session_runs ORDER BY session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		run := Run{}
		var checkpoint string
		if err := rows.Scan(&run.SessionID, &checkpoint, &run.Suspended, &run.Crashes); err != nil {
			return nil, err
		}
		run.Checkpoint = []byte(checkpoint)
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// requireRow reports ErrNotFound when a write that names a session's run
// found none to write to.
func requireRow(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}
