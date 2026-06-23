package profiling

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// StartPprofServer starts an HTTP server exposing Go runtime profiling endpoints
// at /debug/pprof/ on the given addr. It binds the listener synchronously and
// returns an error if the address is unavailable. The server runs in a background
// goroutine and shuts down when ctx is cancelled.
// addr must be a TCP host:port address (e.g. "127.0.0.1:6060"); unix://, npipe://,
// and fd:// schemes are not supported. Prefer a loopback address over a bare port
// (":6060") — the latter binds all interfaces, exposing process memory and arguments
// to the network.
func StartPprofServer(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("pprof: listen on %s: %w", addr, err)
	}

	// ReadHeaderTimeout guards against slow-loris connections on the debug port.
	// WriteTimeout is intentionally omitted: profile/trace captures legitimately
	// run for tens of seconds and would be truncated by a short write deadline.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	slog.InfoContext(ctx, "pprof server listening", "addr", ln.Addr().String())
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok && !tcpAddr.IP.IsLoopback() {
		slog.WarnContext(ctx, "pprof server is listening on a non-loopback address — "+
			"/debug/pprof/cmdline and heap profiles are network-reachable without authentication",
			"addr", tcpAddr.String())
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("pprof server error", "error", err)
		}
	}()

	go func() {
		<-ctx.Done()
		// 5s grace: favors prompt process exit over draining in-flight profile
		// captures. CPU/trace profiles run up to 30s by default; callers should
		// cancel their requests before the process exits rather than relying on
		// this timeout to drain them.
		// context.WithoutCancel preserves ctx values (e.g. trace IDs) without
		// inheriting the cancellation, so the shutdown timeout is not pre-expired.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.WarnContext(shutdownCtx, "pprof server shutdown error", "error", err)
		}
	}()

	return nil
}
