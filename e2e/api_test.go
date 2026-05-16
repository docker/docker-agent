package e2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
)

type Session struct {
	Title string `json:"title"`
}

func TestCagentAPI_ListSessions(t *testing.T) {
	type testcase struct {
		db            string
		expectedCount int
	}

	for _, tc := range []testcase{
		{"one-session.db", 1},
		{"two-sessions.db", 2},
		{"transfer-task.db", 1},
		{"session.db", 1},
		{"session-not-found.db", 17},
		{"desktop.db", 2},
	} {
		t.Run(tc.db, func(t *testing.T) {
			socketPath := startCagentAPI(t, filepath.Join("testdata", "db", tc.db))

			client := &http.Client{
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
					},
				},
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost/api/sessions", http.NoBody)
			require.NoError(t, err)
			resp, err := client.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			var sessions []Session
			err = json.NewDecoder(resp.Body).Decode(&sessions)
			require.NoError(t, err)

			assert.Len(t, sessions, tc.expectedCount)
		})
	}
}

func startCagentAPI(t *testing.T, db string) string {
	t.Helper()

	// Get absolute path to db before changing directory
	absDB, err := filepath.Abs(db)
	require.NoError(t, err)

	tmpDir := t.TempDir()
	t.Chdir(tmpDir) // Use relative socket path to avoid Unix socket path length limit

	// Copy database files to temp directory
	dbCopy := tmpDir + "/session.db"
	copyFile(t, dbCopy, absDB)
	if _, err := os.Stat(absDB + "-wal"); err == nil {
		copyFile(t, dbCopy+"-wal", absDB+"-wal")
	}

	ln, err := server.Listen(t.Context(), "unix://cagent.sock")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = ln.Close()
	})

	sessionStore, err := session.NewSQLiteSessionStore(dbCopy)
	require.NoError(t, err)

	srv, err := server.New(t.Context(), sessionStore, &config.RuntimeConfig{}, 0, nil, "")
	require.NoError(t, err)

	go func() {
		_ = srv.Serve(t.Context(), ln)
	}()

	return "cagent.sock"
}

func copyFile(t *testing.T, dst, src string) {
	t.Helper()

	srcFile, err := os.Open(src)
	require.NoError(t, err)
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	require.NoError(t, err)
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	require.NoError(t, err)
}
