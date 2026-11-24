package app

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type sideQueryPreparer interface {
	PrepareSideQuery(ctx context.Context, sess *session.Session) (*runtime.SideQuery, error)
}

// PrepareSideQuery captures a /btw snapshot of the current session for the
// active runtime. Returns an error when the runtime has no local,
// model-backed support for side queries (e.g. remote sessions, external
// coding harnesses).
func (a *App) PrepareSideQuery(ctx context.Context) (*runtime.SideQuery, error) {
	preparer, ok := a.runtime.(sideQueryPreparer)
	if !ok {
		return nil, errors.New("/btw is not available for this runtime")
	}
	return preparer.PrepareSideQuery(ctx, a.Session())
}
