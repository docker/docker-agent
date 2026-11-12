package acp

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

type summariesOnlyStore struct {
	session.Store

	summaries []session.Summary
	err       error
}

func (s *summariesOnlyStore) GetSessionSummaries(context.Context) ([]session.Summary, error) {
	return s.summaries, s.err
}

func (*summariesOnlyStore) GetSession(context.Context, string) (*session.Session, error) {
	panic("listing must not load histories")
}

func (*summariesOnlyStore) GetSessions(context.Context) ([]*session.Session, error) {
	panic("listing must not load histories")
}

func listIDs(result acpsdk.ListSessionsResponse) []string {
	ids := make([]string, 0, len(result.Sessions))
	for _, s := range result.Sessions {
		ids = append(ids, string(s.SessionId))
	}
	return ids
}

func TestListSessionsWorkspaceMetadata(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	other := filepath.Join(wd, "other")
	deleted := filepath.Join(wd, "no-longer-exists")
	store := &summariesOnlyStore{summaries: []session.Summary{
		{ID: "other", WorkingDir: other, Title: "other"},
		{ID: "relative", WorkingDir: "relative"},
		{ID: "unknown"},
		{ID: "deleted", WorkingDir: deleted},
		{ID: "clean", WorkingDir: wd + string(filepath.Separator) + ".", Title: "clean"},
		{ID: "active", WorkingDir: wd, Title: "active"},
		{ID: "active-unknown", WorkingDir: ""},
	}}
	a := NewAgent(nil, nil, store)
	extra := t.TempDir()
	a.sessions["active"] = &Session{id: "active", workingDir: wd, additionalDirs: []string{extra}}
	a.sessions["active-unknown"] = &Session{id: "active-unknown"}
	response, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{"active", "clean", "deleted", "other"}, listIDs(response))
	assert.Equal(t, []string{extra}, response.Sessions[0].AdditionalDirectories)
	assert.Nil(t, response.Sessions[0].UpdatedAt)
	assert.Nil(t, response.NextCursor)
	for _, info := range response.Sessions {
		assert.True(t, filepath.IsAbs(info.Cwd))
		assert.Nil(t, info.UpdatedAt)
	}
	response.Sessions[0].AdditionalDirectories[0] = "mutated"
	_, roots := a.sessions["active"].workspaceSnapshot()
	assert.Equal(t, []string{extra}, roots)
	for _, tc := range []struct {
		cwd string
		ids []string
	}{
		{wd, []string{"active", "clean"}},
		{wd + string(filepath.Separator) + ".", []string{"active", "clean"}},
		{deleted, []string{"deleted"}},
		{filepath.Join(wd, "absent"), []string{}},
	} {
		response, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{Cwd: &tc.cwd})
		require.NoError(t, err)
		assert.Equal(t, tc.ids, listIDs(response))
	}
	assert.Equal(t, "other", store.summaries[0].ID, "listing must not reorder store-owned summaries")
}

func TestListSessionsFilterIsLexical(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	upper, lower := filepath.Join(wd, "Project"), filepath.Join(wd, "project")
	store := &summariesOnlyStore{summaries: []session.Summary{{ID: "upper", WorkingDir: upper}, {ID: "lower", WorkingDir: lower}}}
	a := NewAgent(nil, nil, store)
	result, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{Cwd: &upper})
	require.NoError(t, err)
	assert.Equal(t, []string{"upper"}, listIDs(result))
}

func TestListSessionsPagination(t *testing.T) {
	t.Parallel()
	wd, other := t.TempDir(), t.TempDir()
	timestamp := time.Date(2026, 9, 22, 12, 0, 0, 123456789, time.UTC)
	for _, count := range []int{0, 1, sessionListPageSize, sessionListPageSize + 1, sessionListPageSize * 2, sessionListPageSize*2 + 1} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			t.Parallel()
			store := &summariesOnlyStore{}
			want := make([]string, 0, count)
			for i := range count {
				id := fmt.Sprintf("id-%03d", i)
				want = append(want, id)
				store.summaries = append(store.summaries, session.Summary{ID: id, WorkingDir: wd, CreatedAt: timestamp})
			}
			store.summaries = append(store.summaries, session.Summary{ID: "filtered", WorkingDir: other, CreatedAt: timestamp.Add(time.Hour)})
			slices.Reverse(store.summaries)
			a := NewAgent(nil, nil, store)
			request := acpsdk.ListSessionsRequest{Cwd: &wd}
			got := make([]string, 0, count)
			pages := 0
			for {
				result, err := a.ListSessions(t.Context(), request)
				require.NoError(t, err)
				require.NotNil(t, result.Sessions)
				require.LessOrEqual(t, len(result.Sessions), sessionListPageSize)
				got = append(got, listIDs(result)...)
				pages++
				require.LessOrEqual(t, pages, 3)
				if result.NextCursor == nil {
					break
				}
				require.Len(t, result.Sessions, sessionListPageSize)
				request.Cursor = result.NextCursor
			}
			assert.Equal(t, want, got)
		})
	}
}

func TestListSessionsCursorSurvivesDeletedAnchor(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	timestamp := time.Date(2026, 9, 22, 12, 0, 0, 123456789, time.FixedZone("offset", 3600))
	store := &summariesOnlyStore{}
	for i := range sessionListPageSize + 2 {
		store.summaries = append(store.summaries, session.Summary{ID: fmt.Sprintf("id-%03d", i), WorkingDir: wd, CreatedAt: timestamp.Add(-time.Duration(i) * time.Nanosecond)})
	}
	store.summaries = append(store.summaries, session.Summary{ID: "zero", WorkingDir: wd})
	a := NewAgent(nil, nil, store)
	first, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{})
	require.NoError(t, err)
	require.NotNil(t, first.NextCursor)
	anchor := string(first.Sessions[len(first.Sessions)-1].SessionId)
	store.summaries = slices.DeleteFunc(store.summaries, func(s session.Summary) bool { return s.ID == anchor })
	store.summaries = append(store.summaries, session.Summary{ID: "newer", WorkingDir: wd, CreatedAt: timestamp.Add(time.Hour)})
	second, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{Cursor: first.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, []string{"id-050", "id-051", "zero"}, listIDs(second))
	assert.Nil(t, second.NextCursor)
}

func TestListSessionsRejectsInvalidParams(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	a := NewAgent(nil, nil, &summariesOnlyStore{err: errors.New("must reject before store access")})
	for _, cwd := range []string{"", "relative", wd + "\x00"} {
		_, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{Cwd: &cwd})
		var rpcErr *acpsdk.RequestError
		require.ErrorAs(t, err, &rpcErr)
		assert.Equal(t, -32602, rpcErr.Code)
	}
	for _, raw := range []string{
		"", "not-base64!", "null", `{}`,
		`{"cwd":"","createdAt":"0001-01-01T00:00:00Z","id":"a"}`,
		`{"v":1,"createdAt":"0001-01-01T00:00:00Z","id":"a"}`,
		`{"v":1,"cwd":"","id":"a"}`,
		`{"v":1,"cwd":"","createdAt":"0001-01-01T00:00:00Z"}`,
		`{"v":1,"v":1,"cwd":"","createdAt":"0001-01-01T00:00:00Z","id":"a"}`,
		`{"v":1,"cwd":"","createdAt":"0001-01-01T00:00:00Z","id":"a","id":"b"}`,
		`{"v":2,"cwd":"","createdAt":"0001-01-01T00:00:00Z","id":"a"}`, `{"v":1,"cwd":"","createdAt":"invalid","id":"a"}`, `{"v":1,"cwd":"","createdAt":"0001-01-01T00:00:00Z","id":""}`, `{"v":1,"cwd":"","createdAt":"0001-01-01T00:00:00Z","id":"a","extra":true}`, `{"v":1,"cwd":"","createdAt":"0001-01-01T00:00:00Z","id":"a"} {}`,
	} {
		cursor := base64.RawURLEncoding.EncodeToString([]byte(raw))
		_, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{Cursor: &cursor})
		var rpcErr *acpsdk.RequestError
		require.ErrorAs(t, err, &rpcErr)
		assert.Equal(t, -32602, rpcErr.Code)
	}
	data, err := json.Marshal(sessionListCursor{Version: 1, ID: "a", Cwd: wd})
	require.NoError(t, err)
	cursor := base64.RawURLEncoding.EncodeToString(data)
	_, err = a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{Cursor: &cursor})
	require.ErrorContains(t, err, "mismatched cwd")
	_, err = a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{})
	require.ErrorContains(t, err, "must reject before store access")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = a.ListSessions(ctx, acpsdk.ListSessionsRequest{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestListSessionsStoreSummariesExcludeChildren(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			store := session.NewInMemorySessionStore()
			if kind == "sqlite" {
				var err error
				store, err = sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "session.db"))
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			wd := t.TempDir()
			root := session.New(session.WithWorkingDir(wd))
			root.SetTitle("root title")
			child := session.New(session.WithWorkingDir(wd))
			child.ParentID = root.ID
			require.NoError(t, store.AddSession(t.Context(), root))
			require.NoError(t, store.AddSession(t.Context(), child))
			a := NewAgent(nil, nil, store)
			result, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{})
			require.NoError(t, err)
			assert.Equal(t, []string{root.ID}, listIDs(result))
			assert.Equal(t, "root title", *result.Sessions[0].Title)
			assert.Nil(t, result.Sessions[0].UpdatedAt)
		})
	}
}

func TestListSessionsWireMetadata(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	a := NewAgent(nil, nil, &summariesOnlyStore{summaries: []session.Summary{{ID: "known", Title: "title", WorkingDir: wd, CreatedAt: time.Now()}}})
	reader, send := io.Pipe()
	receive, writer := io.Pipe()
	conn := acpsdk.NewAgentSideConnection(a, writer, reader)
	t.Cleanup(func() { _ = send.Close(); _ = receive.Close(); <-conn.Done() })
	encoder, decoder := json.NewEncoder(send), json.NewDecoder(receive)
	for i, filter := range []string{wd, filepath.Join(wd, "absent")} {
		require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": i, "method": "session/list", "params": map[string]any{"cwd": filter}}))
		var response struct {
			Result json.RawMessage `json:"result"`
		}
		require.NoError(t, decoder.Decode(&response))
		text := string(response.Result)
		assert.NotContains(t, text, "updatedAt")
		assert.NotContains(t, text, "nextCursor")
		assert.NotContains(t, text, "additionalDirectories")
		if i == 1 {
			assert.JSONEq(t, `{"sessions":[]}`, text)
		} else {
			assert.Contains(t, text, "known")
		}
	}
}

func TestListCursorRoundTripsZeroTimestamp(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(sessionListCursor{Version: 1, ID: "last"})
	require.NoError(t, err)
	cursor, err := parseSessionListCursor(base64.RawURLEncoding.EncodeToString(data), "")
	require.NoError(t, err)
	assert.True(t, cursor.CreatedAt.IsZero())
	assert.Equal(t, "last", cursor.ID)
	_, err = parseSessionListCursor(strings.Repeat("!", 1024), "")
	require.Error(t, err)
}

func TestListSessionsDoesNotLoadCorruptMessageHistory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sess := session.New(session.WithWorkingDir(t.TempDir()))
	require.NoError(t, store.AddSession(t.Context(), sess))
	id, err := store.AddMessage(t.Context(), sess.ID, session.UserMessage("hello"))
	require.NoError(t, err)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, updateErr := db.ExecContext(t.Context(), "UPDATE session_items SET message_json = ? WHERE id = ?", "not-json", id)
	require.NoError(t, errors.Join(updateErr, db.Close()))
	a := NewAgent(nil, nil, store)
	result, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{sess.ID}, listIDs(result))
}

func TestListSessionsCursorRejectsChangedFilterOnWire(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	store := &summariesOnlyStore{}
	for i := range sessionListPageSize + 1 {
		store.summaries = append(store.summaries, session.Summary{ID: fmt.Sprintf("s-%03d", i), WorkingDir: wd})
	}
	a := NewAgent(nil, nil, store)
	first, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{Cwd: &wd})
	require.NoError(t, err)
	require.NotNil(t, first.NextCursor)
	input, send := io.Pipe()
	receive, output := io.Pipe()
	conn := acpsdk.NewAgentSideConnection(a, output, input)
	t.Cleanup(func() { _ = send.Close(); _ = receive.Close(); <-conn.Done() })
	require.NoError(t, json.NewEncoder(send).Encode(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "session/list", "params": map[string]any{"cursor": *first.NextCursor},
	}))
	var response struct {
		Error *acpsdk.RequestError `json:"error"`
	}
	require.NoError(t, json.NewDecoder(receive).Decode(&response))
	require.NotNil(t, response.Error)
	assert.Equal(t, -32602, response.Error.Code)
}
