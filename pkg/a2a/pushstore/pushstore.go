// Package pushstore provides a SQLite-backed implementation of the A2A
// push.ConfigStore interface. It persists per-task push notification
// configurations so that webhooks registered via tasks/pushNotificationConfig/set
// survive an agent restart, unlike the SDK's in-memory default store.
//
// Authorization (cross-tenant / cross-task isolation) is enforced at the
// a2asrv handler level via the task store; this store is a plain persistent
// CRUD store keyed by (taskID, configID).
package pushstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/google/uuid"

	"github.com/docker/docker-agent/pkg/sqliteutil"
)

// Store is a SQLite-backed push.ConfigStore. The zero value is not usable;
// construct it with New.
type Store struct {
	db *sql.DB
}

// Ensure Store satisfies the SDK interface at compile time.
var _ push.ConfigStore = (*Store)(nil)

const createTableSQL = `
CREATE TABLE IF NOT EXISTS a2a_push_configs (
	task_id          TEXT NOT NULL,
	config_id        TEXT NOT NULL,
	url              TEXT NOT NULL,
	token            TEXT NOT NULL DEFAULT '',
	tenant           TEXT NOT NULL DEFAULT '',
	auth_scheme      TEXT NOT NULL DEFAULT '',
	auth_credentials TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (task_id, config_id)
);`

// New opens (creating if necessary) a SQLite database at path and ensures the
// push-config table exists. Pass ":memory:" for an ephemeral store (useful in
// tests). The caller owns the returned Store and must call Close.
func New(ctx context.Context, path string) (*Store, error) {
	db, err := sqliteutil.OpenDB(ctx, path)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, createTableSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to create a2a_push_configs table: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	return sqliteutil.CloseDB(s.db)
}

// validateConfig mirrors the validation the SDK's in-memory store performs so
// that callers get identical a2a.ErrInvalidParams behaviour regardless of the
// backing store.
func validateConfig(config *a2a.PushConfig) error {
	if config == nil {
		return errors.New("push config cannot be nil")
	}
	if config.URL == "" {
		return errors.New("push config endpoint cannot be empty")
	}
	if _, err := url.ParseRequestURI(config.URL); err != nil {
		return fmt.Errorf("invalid push config endpoint URL: %w", err)
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
