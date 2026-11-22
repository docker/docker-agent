// Package a2astore provides SQLite-backed implementations of the A2A SDK's
// push.ConfigStore and taskstore.Store, so tasks and the webhooks registered
// for them survive an agent restart (the SDK defaults are in-memory).
//
// Push-config authorization (cross-tenant / cross-task isolation) is enforced
// at the a2asrv handler level via the task store; the push-config store is a
// plain persistent CRUD store keyed by (taskID, configID).
package a2astore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/google/uuid"

	"github.com/docker/docker-agent/pkg/sqliteutil"
)

// Store is a SQLite-backed push.ConfigStore. It also owns the database handle
// shared with the TaskStore returned by Tasks. The zero value is not usable;
// construct it with New.
type Store struct {
	db   *sql.DB
	lock *os.File
}

// Ensure Store satisfies the SDK interface at compile time.
var _ push.ConfigStore = (*Store)(nil)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS a2a_push_configs (
	task_id          TEXT NOT NULL,
	config_id        TEXT NOT NULL,
	url              TEXT NOT NULL,
	token            TEXT NOT NULL DEFAULT '',
	tenant           TEXT NOT NULL DEFAULT '',
	auth_scheme      TEXT NOT NULL DEFAULT '',
	auth_credentials TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (task_id, config_id)
);

CREATE TABLE IF NOT EXISTS a2a_tasks (
	task_id    TEXT PRIMARY KEY,
	context_id TEXT NOT NULL,
	owner      TEXT NOT NULL,
	state      TEXT NOT NULL,
	status_ts  INTEGER,
	version    INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	data       TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS a2a_tasks_owner_updated ON a2a_tasks (owner, updated_at, task_id);`

// dbFile is the database New opens inside its directory.
const dbFile = "a2a.db"

// DirFor returns the store directory for a session database, so servers using
// different session databases in the same directory never share tasks.
func DirFor(sessionDB string) string {
	return sessionDB + "-a2a"
}

// New opens (creating if necessary) the store in dir and ensures the
// push-config and task tables exist. The database holds webhook callback
// tokens and credentials in plaintext, so dir is made private to the current
// user before SQLite creates or opens any file in it. Only one Store may be
// open per dir: New fails with ErrInUse while another one holds it. The caller
// owns the returned Store and must call Close.
func New(ctx context.Context, dir string) (*Store, error) {
	path := filepath.Join(dir, dbFile)
	if err := preparePrivate(dir, path); err != nil {
		return nil, fmt.Errorf("failed to secure a2a store directory: %w", err)
	}
	lock, err := acquireLock(dir)
	if err != nil {
		return nil, err
	}
	db, err := sqliteutil.OpenDB(ctx, path)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		_ = db.Close()
		_ = lock.Close()
		return nil, fmt.Errorf("failed to create a2a tables: %w", err)
	}
	return &Store{db: db, lock: lock}, nil
}

// sqliteFiles lists the database and every sidecar SQLite may create for it.
func sqliteFiles(path string) []string {
	return []string{path, path + "-wal", path + "-shm", path + "-journal"}
}

// Close releases the database handle, then the store lock.
func (s *Store) Close() error {
	return errors.Join(sqliteutil.CloseDB(s.db), s.lock.Close())
}

// validateConfig rejects configs the sender could never deliver to, reporting
// a2a.ErrInvalidParams like the SDK's in-memory store.
func validateConfig(config *a2a.PushConfig) error {
	if config == nil {
		return errors.New("push config cannot be nil")
	}
	if config.URL == "" {
		return errors.New("push config endpoint cannot be empty")
	}
	// Don't wrap the parse error: it embeds the URL, which may carry secrets.
	// Destination IP checks happen at dial time in the sender.
	u, err := url.Parse(config.URL)
	if err != nil {
		return errors.New("invalid push config endpoint URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("push config endpoint URL must use http or https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return errors.New("push config endpoint URL must include a host")
	}
	return nil
}

// Save creates or updates a push configuration for a task. When config.ID is
// empty a time-ordered UUIDv7 is generated so a task can carry multiple
// callbacks. It returns a copy of the stored configuration.
func (s *Store) Save(ctx context.Context, taskID a2a.TaskID, config *a2a.PushConfig) (*a2a.PushConfig, error) {
	if err := validateConfig(config); err != nil {
		return nil, fmt.Errorf("%w: %w", a2a.ErrInvalidParams, err)
	}

	saved := *config
	saved.TaskID = taskID
	if saved.ID == "" {
		saved.ID = uuid.Must(uuid.NewV7()).String()
	}

	var scheme, credentials string
	if saved.Auth != nil {
		scheme = saved.Auth.Scheme
		credentials = saved.Auth.Credentials
	}

	const upsert = `
INSERT INTO a2a_push_configs (task_id, config_id, url, token, tenant, auth_scheme, auth_credentials)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(task_id, config_id) DO UPDATE SET
	url = excluded.url,
	token = excluded.token,
	tenant = excluded.tenant,
	auth_scheme = excluded.auth_scheme,
	auth_credentials = excluded.auth_credentials;`

	if _, err := s.db.ExecContext(ctx, upsert,
		string(taskID), saved.ID, saved.URL, saved.Token, saved.Tenant, scheme, credentials,
	); err != nil {
		return nil, fmt.Errorf("failed to save push config: %w", err)
	}

	return scanCopy(&saved), nil
}

// Get returns the push configuration with the given configID for a task, or
// push.ErrPushConfigNotFound if none is stored.
func (s *Store) Get(ctx context.Context, taskID a2a.TaskID, configID string) (*a2a.PushConfig, error) {
	const query = `
SELECT url, token, tenant, auth_scheme, auth_credentials
FROM a2a_push_configs WHERE task_id = ? AND config_id = ?;`

	row := s.db.QueryRowContext(ctx, query, string(taskID), configID)
	config, err := scanRow(row, taskID, configID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, push.ErrPushConfigNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get push config: %w", err)
	}
	return config, nil
}

// List returns all push configurations registered for a task. It returns an
// empty (non-nil) slice when none are stored.
func (s *Store) List(ctx context.Context, taskID a2a.TaskID) ([]*a2a.PushConfig, error) {
	const query = `
SELECT config_id, url, token, tenant, auth_scheme, auth_credentials
FROM a2a_push_configs WHERE task_id = ? ORDER BY config_id;`

	rows, err := s.db.QueryContext(ctx, query, string(taskID))
	if err != nil {
		return nil, fmt.Errorf("failed to list push configs: %w", err)
	}
	defer rows.Close()

	result := make([]*a2a.PushConfig, 0)
	for rows.Next() {
		var configID, u, token, tenant, scheme, credentials string
		if err := rows.Scan(&configID, &u, &token, &tenant, &scheme, &credentials); err != nil {
			return nil, fmt.Errorf("failed to scan push config: %w", err)
		}
		result = append(result, buildConfig(taskID, configID, u, token, tenant, scheme, credentials))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate push configs: %w", err)
	}
	return result, nil
}

// Delete removes a single push configuration. It is idempotent: deleting a
// configuration that does not exist is not an error.
func (s *Store) Delete(ctx context.Context, taskID a2a.TaskID, configID string) error {
	const del = `DELETE FROM a2a_push_configs WHERE task_id = ? AND config_id = ?;`
	if _, err := s.db.ExecContext(ctx, del, string(taskID), configID); err != nil {
		return fmt.Errorf("failed to delete push config: %w", err)
	}
	return nil
}

// DeleteAll removes every push configuration registered for a task.
func (s *Store) DeleteAll(ctx context.Context, taskID a2a.TaskID) error {
	const del = `DELETE FROM a2a_push_configs WHERE task_id = ?;`
	if _, err := s.db.ExecContext(ctx, del, string(taskID)); err != nil {
		return fmt.Errorf("failed to delete push configs: %w", err)
	}
	return nil
}

// scanRow builds a PushConfig from a single-row query holding the non-key
// columns. taskID and configID come from the caller's lookup keys.
func scanRow(row *sql.Row, taskID a2a.TaskID, configID string) (*a2a.PushConfig, error) {
	var u, token, tenant, scheme, credentials string
	if err := row.Scan(&u, &token, &tenant, &scheme, &credentials); err != nil {
		return nil, err
	}
	return buildConfig(taskID, configID, u, token, tenant, scheme, credentials), nil
}

// buildConfig assembles a PushConfig, reconstructing the optional Auth block
// only when authentication details were stored.
func buildConfig(taskID a2a.TaskID, configID, u, token, tenant, scheme, credentials string) *a2a.PushConfig {
	config := &a2a.PushConfig{
		TaskID: taskID,
		ID:     configID,
		URL:    u,
		Token:  token,
		Tenant: tenant,
	}
	if scheme != "" || credentials != "" {
		config.Auth = &a2a.PushAuthInfo{Scheme: scheme, Credentials: credentials}
	}
	return config
}

// scanCopy returns a deep copy of config so callers cannot mutate stored state
// through the returned pointer.
func scanCopy(config *a2a.PushConfig) *a2a.PushConfig {
	cp := *config
	if config.Auth != nil {
		auth := *config.Auth
		cp.Auth = &auth
	}
	return &cp
}
