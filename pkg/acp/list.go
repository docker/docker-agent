package acp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/session"
)

const sessionListPageSize = 50

type sessionListCursor struct {
	Version   int       `json:"v"`
	Cwd       string    `json:"cwd"`
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
}

func parseSessionListCursor(raw, cwd string) (*sessionListCursor, error) {
	data, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil {
		return nil, errors.New("invalid session list cursor")
	}
	var cursor sessionListCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return nil, errors.New("invalid session list cursor")
	}
	// Accept only the canonical shape we emit, including all fields and no extras.
	canonical, err := json.Marshal(cursor)
	if err != nil || !bytes.Equal(data, canonical) || cursor.Version != 1 || cursor.ID == "" || cursor.Cwd != cwd {
		return nil, errors.New("invalid session list cursor or mismatched cwd filter")
	}
	return &cursor, nil
}

func validListWorkingDir(cwd string) bool {
	return filepath.IsAbs(cwd) && !strings.ContainsRune(cwd, 0)
}

// ListSessions implements [acp.Agent] using metadata only, never conversation histories.
func (a *Agent) ListSessions(ctx context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	ctx, span := startACPRequest(ctx, "session/list", params.Meta)
	defer span.End()
	ctx, op, err := a.beginOperation(ctx)
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}
	defer a.finishOperation(op)

	cwd := ""
	if params.Cwd != nil {
		if !validListWorkingDir(*params.Cwd) {
			return acp.ListSessionsResponse{}, acp.NewInvalidParams("cwd must be an absolute path")
		}
		cwd = filepath.Clean(*params.Cwd)
	}
	var cursor *sessionListCursor
	if params.Cursor != nil {
		cursor, err = parseSessionListCursor(*params.Cursor, cwd)
		if err != nil {
			return acp.ListSessionsResponse{}, acp.NewInvalidParams(err.Error())
		}
	}

	summaries, err := a.sessionStore.GetSessionSummaries(ctx)
	if err != nil {
		return acp.ListSessionsResponse{}, fmt.Errorf("failed to list sessions: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return acp.ListSessionsResponse{}, err
	}
	// Stores need not break timestamp ties, and callers may own the returned slice.
	summaries = slices.Clone(summaries)
	slices.SortFunc(summaries, func(a, b session.Summary) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})

	result := acp.ListSessionsResponse{Sessions: make([]acp.SessionInfo, 0, sessionListPageSize)}
	var last session.Summary
	for _, summary := range summaries {
		if err := ctx.Err(); err != nil {
			return acp.ListSessionsResponse{}, err
		}
		if cursor != nil {
			order := summary.CreatedAt.Compare(cursor.CreatedAt)
			if order > 0 || (order == 0 && summary.ID <= cursor.ID) {
				continue
			}
		}
		workingDir, additionalDirs := a.sessionListPaths(summary)
		if summary.ID == "" || !validListWorkingDir(workingDir) {
			continue
		}
		workingDir = filepath.Clean(workingDir)
		if cwd != "" && workingDir != cwd {
			continue
		}
		if len(result.Sessions) == sessionListPageSize {
			data, err := json.Marshal(sessionListCursor{Version: 1, Cwd: cwd, CreatedAt: last.CreatedAt.UTC(), ID: last.ID})
			if err != nil {
				return acp.ListSessionsResponse{}, fmt.Errorf("encoding session list cursor: %w", err)
			}
			next := base64.RawURLEncoding.EncodeToString(data)
			result.NextCursor = &next
			break
		}
		result.Sessions = append(result.Sessions, acp.SessionInfo{
			SessionId: acp.SessionId(summary.ID), Title: &summary.Title,
			Cwd: workingDir, AdditionalDirectories: additionalDirs,
		})
		last = summary
	}
	return result, nil
}

func (a *Agent) sessionListPaths(summary session.Summary) (string, []string) {
	a.mu.Lock()
	active := a.sessions[summary.ID]
	a.mu.Unlock()
	if active != nil {
		return active.workspaceSnapshot()
	}
	return summary.WorkingDir, nil
}
