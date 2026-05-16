package root

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/runregistry"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
)

// startAttachedServer exposes the in-process runtime over HTTP so external
// processes can drive the running TUI (steer, followup, resume, ...). It
// returns an app.Opt that, once the App is constructed, registers the App's
// event stream as the session's event source so /events SSE works. No-op
// when --listen is empty.
func (f *runExecFlags) startAttachedServer(ctx context.Context, out *cli.Printer, rt runtime.Runtime, sess *session.Session) (app.Opt, error) {
	if f.listenAddr == "" {
		return nil, nil
	}

	sm := server.NewSessionManager(ctx, nil, rt.SessionStore(), 0, &f.runConfig)
	sm.AttachRuntime(sess.ID, rt, sess)

	ln, err := server.Listen(ctx, f.listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", f.listenAddr, err)
	}
	context.AfterFunc(ctx, func() { _ = ln.Close() })

	cleanup, err := runregistry.Write(runregistry.Record{
		PID:       os.Getpid(),
		Addr:      "http://" + ln.Addr().String(),
		SessionID: sess.ID,
		Agent:     f.agentName,
		StartedAt: time.Now(),
	})
	if err != nil {
		slog.WarnContext(ctx, "Could not write run registry record", "error", err)
	} else {
		context.AfterFunc(ctx, cleanup)
	}

	out.Println("Control plane listening on", ln.Addr().String())
	warnIfNotLoopback(out, ln.Addr())

	srv := server.NewWithManager(sm, "")
	go func() {
		if err := srv.Serve(ctx, ln); err != nil {
			slog.ErrorContext(ctx, "Control plane server stopped", "error", err)
		}
	}()

	return func(a *app.App) {
		sm.RegisterEventSource(sess.ID, func(ctx context.Context, send func(any)) {
			a.SubscribeWith(ctx, func(msg tea.Msg) {
				if ev, ok := msg.(runtime.Event); ok {
					send(ev)
				}
			})
		})
	}, nil
}
