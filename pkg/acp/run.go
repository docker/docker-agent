package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func Run(ctx context.Context, agentFilename string, stdin io.Reader, stdout io.Writer, runConfig *config.RuntimeConfig, sessionDB string) (retErr error) {
	slog.DebugContext(ctx, "Starting ACP server", "agent", agentFilename, "session_db", sessionDB)

	agentSource, err := sources.Resolve(agentFilename, nil)
	if err != nil {
		return err
	}

	// Create SQLite session store for persistent sessions
	sessStore, err := sqlitestore.New(ctx, sessionDB)
	if err != nil {
		return fmt.Errorf("creating session store: %w", err)
	}
	// Close the store on shutdown if it implements io.Closer
	if closer, ok := sessStore.(io.Closer); ok {
		defer func() { retErr = errors.Join(retErr, closer.Close()) }()
	}

	acpAgent := NewAgent(agentSource, runConfig, sessStore)
	conn := acpAgent.NewConnection(stdout, stdin)
	conn.SetLogger(slog.Default())
	defer func() { retErr = errors.Join(retErr, acpAgent.Stop(ctx)) }()

	slog.DebugContext(ctx, "acp started, waiting for conn")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.Done():
		return nil
	}
}
