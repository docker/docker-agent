package a2astore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

const (
	defaultListPageSize      = 50
	maxListPageSize          = 100
	defaultListHistoryLength = 100
)

// TaskStore is a SQLite-backed taskstore.Store sharing its database with the
// Store it was obtained from. It mirrors the SDK's in-memory semantics: the
// owner is recorded on Create and an ownership mismatch reads as
// a2a.ErrTaskNotFound (A2A spec §3.3.2).
type TaskStore struct {
	db   *sql.DB
	auth taskstore.Authenticator
	now  func() time.Time
}

var _ taskstore.Store = (*TaskStore)(nil)

// Tasks returns a task store backed by the same database. auth resolves the
// caller identity recorded as task owner; nil means a single anonymous owner.
func (s *Store) Tasks(auth taskstore.Authenticator) *TaskStore {
	if auth == nil {
		auth = func(context.Context) (string, error) { return "", nil }
	}
	return &TaskStore{db: s.db, auth: auth, now: time.Now}
}

func (s *TaskStore) user(ctx context.Context) (string, error) {
	user, err := s.auth(ctx)
	if err != nil {
		return "", fmt.Errorf("taskstore auth failed: %w", err)
	}
	return user, nil
}

type taskRow struct {
	data     string
	state    string
	statusTS sql.NullInt64
}

func encodeTask(task *a2a.Task) (taskRow, error) {
	if task == nil {
		return taskRow{}, errors.New("task cannot be nil")
	}
	data, err := json.Marshal(task)
	if err != nil {
		return taskRow{}, fmt.Errorf("failed to encode task: %w", err)
	}
	row := taskRow{data: string(data), state: string(task.Status.State)}
	if ts := task.Status.Timestamp; ts != nil {
		row.statusTS = sql.NullInt64{Int64: ts.UnixNano(), Valid: true}
	}
	return row, nil
}

func decodeTask(data string) (*a2a.Task, error) {
	var task a2a.Task
	if err := json.Unmarshal([]byte(data), &task); err != nil {
		return nil, fmt.Errorf("failed to decode task: %w", err)
	}
	return &task, nil
}

// Create implements taskstore.Store.
func (s *TaskStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	row, err := encodeTask(task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	user, err := s.user(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}

	const insert = `
INSERT INTO a2a_tasks (task_id, context_id, owner, state, status_ts, version, updated_at, data)
VALUES (?, ?, ?, ?, ?, 1, ?, ?)
ON CONFLICT(task_id) DO NOTHING;`
	res, err := s.db.ExecContext(ctx, insert,
		string(task.ID), task.ContextID, user, row.state, row.statusTS, s.now().UnixNano(), row.data)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("failed to create task: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("failed to create task: %w", err)
	}
	if n == 0 {
		return taskstore.TaskVersionMissing, taskstore.ErrTaskAlreadyExists
	}
	return 1, nil
}

// Update implements taskstore.Store. The version check and bump happen in a
// single statement so concurrent writers cannot both win.
func (s *TaskStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if req == nil {
		return taskstore.TaskVersionMissing, errors.New("update request cannot be nil")
	}
	row, err := encodeTask(req.Task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	user, err := s.user(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}

	const update = `
UPDATE a2a_tasks
SET context_id = ?, state = ?, status_ts = ?, data = ?, updated_at = ?, version = version + 1
WHERE task_id = ? AND owner = ? AND (? = 0 OR version = ?)
RETURNING version;`
	prev := int64(req.PrevVersion)
	var version int64
	err = s.db.QueryRowContext(ctx, update,
		req.Task.ContextID, row.state, row.statusTS, row.data, s.now().UnixNano(),
		string(req.Task.ID), user, prev, prev,
	).Scan(&version)
	if err == nil {
		return taskstore.TaskVersion(version), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return taskstore.TaskVersionMissing, fmt.Errorf("failed to update task: %w", err)
	}

	var owner string
	err = s.db.QueryRowContext(ctx, `SELECT owner FROM a2a_tasks WHERE task_id = ?;`, string(req.Task.ID)).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	case err != nil:
		return taskstore.TaskVersionMissing, fmt.Errorf("failed to update task: %w", err)
	case owner != user:
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	default:
		return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
	}
}

// Get implements taskstore.Store.
func (s *TaskStore) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	var data, owner string
	var version int64
	err := s.db.QueryRowContext(ctx,
		`SELECT data, owner, version FROM a2a_tasks WHERE task_id = ?;`, string(taskID),
	).Scan(&data, &owner, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, a2a.ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get task: %w", err)
	}

	user, err := s.user(ctx)
	if err != nil {
		return nil, err
	}
	if owner != user {
		return nil, a2a.ErrTaskNotFound
	}

	task, err := decodeTask(data)
	if err != nil {
		return nil, err
	}
	return &taskstore.StoredTask{Task: task, Version: taskstore.TaskVersion(version), User: owner}, nil
}

// List implements taskstore.Store. Tasks are ordered by last update, newest
// first, like the SDK's in-memory store.
func (s *TaskStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	user, err := s.auth(ctx)
	if user == "" || err != nil {
		return nil, a2a.ErrUnauthenticated
	}

	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = defaultListPageSize
	} else if pageSize < 1 || pageSize > maxListPageSize {
		return nil, fmt.Errorf("page size must be between 1 and %d inclusive, got %d: %w", maxListPageSize, pageSize, a2a.ErrInvalidRequest)
	}

	filter := []string{"owner = ?"}
	args := []any{user}
	if req.ContextID != "" {
		filter = append(filter, "context_id = ?")
		args = append(args, req.ContextID)
	}
	if req.Status != a2a.TaskStateUnspecified {
		filter = append(filter, "state = ?")
		args = append(args, string(req.Status))
	}
	if req.StatusTimestampAfter != nil {
		filter = append(filter, "(status_ts IS NULL OR status_ts >= ?)")
		args = append(args, req.StatusTimestampAfter.UnixNano())
	}
	where := strings.Join(filter, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM a2a_tasks WHERE "+where, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("failed to count tasks: %w", err)
	}

	pageWhere, pageArgs := where, args
	if req.PageToken != "" {
		cursorTime, cursorID, err := decodePageToken(req.PageToken)
		if err != nil {
			return nil, err
		}
		pageWhere += " AND (updated_at < ? OR (updated_at = ? AND task_id < ?))"
		pageArgs = append(slices.Clone(args), cursorTime, cursorTime, cursorID)
	}
	pageArgs = append(pageArgs, pageSize+1)

	page, err := s.listPage(ctx, pageWhere, pageArgs)
	if err != nil {
		return nil, err
	}

	var nextPageToken string
	if len(page) > pageSize {
		page = page[:pageSize]
		last := page[len(page)-1]
		nextPageToken = encodePageToken(last.updatedAt, last.taskID)
	}

	historyLength := defaultListHistoryLength
	if req.HistoryLength != nil {
		historyLength = *req.HistoryLength
	}
	tasks := make([]*a2a.Task, 0, len(page))
	for _, l := range page {
		task, err := decodeTask(l.data)
		if err != nil {
			return nil, err
		}
		if historyLength <= 0 {
			task.History = []*a2a.Message{}
		} else if len(task.History) > historyLength {
			task.History = task.History[len(task.History)-historyLength:]
		}
		if !req.IncludeArtifacts {
			task.Artifacts = nil
		}
		tasks = append(tasks, task)
	}

	return &a2a.ListTasksResponse{
		Tasks:         tasks,
		TotalSize:     total,
		PageSize:      pageSize,
		NextPageToken: nextPageToken,
	}, nil
}

type listedTask struct {
	data      string
	taskID    string
	updatedAt int64
}

// listPage collects rows before decoding: MaxOpenConns=1 forbids other
// queries while rows are open.
func (s *TaskStore) listPage(ctx context.Context, where string, args []any) ([]listedTask, error) {
	//nolint:gosec // G202: where is built from constant fragments; values are bound parameters.
	query := "SELECT data, task_id, updated_at FROM a2a_tasks WHERE " + where + " ORDER BY updated_at DESC, task_id DESC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks: %w", err)
	}
	defer rows.Close()

	var page []listedTask
	for rows.Next() {
		var l listedTask
		if err := rows.Scan(&l.data, &l.taskID, &l.updatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan task: %w", err)
		}
		page = append(page, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate tasks: %w", err)
	}
	return page, nil
}

func encodePageToken(updatedAt int64, taskID string) string {
	return base64.URLEncoding.EncodeToString(fmt.Appendf(nil, "%d_%s", updatedAt, taskID))
}

func decodePageToken(token string) (int64, string, error) {
	decoded, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return 0, "", a2a.ErrParseError
	}
	ts, taskID, ok := strings.Cut(string(decoded), "_")
	if !ok {
		return 0, "", a2a.ErrParseError
	}
	updatedAt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return 0, "", a2a.ErrParseError
	}
	return updatedAt, taskID, nil
}

// FailInterrupted marks tasks a previous process left submitted or working as
// failed: their execution died with it, so clients would otherwise wait
// forever. It returns the updated tasks.
func (s *Store) FailInterrupted(ctx context.Context, reason string) (_ []*a2a.Task, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to recover interrupted tasks: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	tasks, err := interruptedTasks(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, task := range tasks {
		task.Status = a2a.TaskStatus{
			State:     a2a.TaskStateFailed,
			Message:   a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart(reason)),
			Timestamp: &now,
		}
		row, err := encodeTask(task)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE a2a_tasks SET state = ?, status_ts = ?, data = ?, updated_at = ?, version = version + 1
WHERE task_id = ?;`, row.state, row.statusTS, row.data, now.UnixNano(), string(task.ID)); err != nil {
			return nil, fmt.Errorf("failed to fail interrupted task: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to recover interrupted tasks: %w", err)
	}
	return tasks, nil
}

func interruptedTasks(ctx context.Context, tx *sql.Tx) ([]*a2a.Task, error) {
	rows, err := tx.QueryContext(ctx, `SELECT data FROM a2a_tasks WHERE state IN (?, ?);`,
		string(a2a.TaskStateSubmitted), string(a2a.TaskStateWorking))
	if err != nil {
		return nil, fmt.Errorf("failed to list interrupted tasks: %w", err)
	}
	defer rows.Close()

	var tasks []*a2a.Task
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("failed to scan interrupted task: %w", err)
		}
		task, err := decodeTask(data)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to list interrupted tasks: %w", err)
	}
	return tasks, nil
}
