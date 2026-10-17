//go:build darwin || linux || windows

package desktop

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	secretsengine "github.com/docker/secrets-engine/client"
	"github.com/docker/secrets-engine/client/dockerhub"
	"github.com/docker/secrets-engine/x/api"

	"github.com/docker/docker-agent/pkg/hubauth"
)

const (
	// secretsEngineBudget bounds a session lookup: the SDK has no default timeout.
	secretsEngineBudget = 5 * time.Second

	// secretsEngineCooldown is how long a failing engine is skipped.
	secretsEngineCooldown = 30 * time.Second
)

var (
	errSecretsEngineCoolingDown = errors.New("secrets engine lookup failed recently, not retrying yet")
	errSecretsEngineSlow        = errors.New("secrets engine did not answer in time")
)

const secretsEngineFailureMsg = "Could not read the Docker Hub session from the secrets engine"

// defaultSecretsEngine is the engine [GetTokenWithSource] reads, created on first use.
var defaultSecretsEngine = sync.OnceValue(newSecretsEngine)

// newSecretsEngine connects to Docker Desktop's secrets engine, reading staging sessions on staging.
func newSecretsEngine() *secretsEngine {
	client, err := secretsengine.New(secretsengine.WithSocketPath(api.DesktopSocketPath()))
	if err != nil {
		return &secretsEngine{clientErr: err}
	}
	var opts []dockerhub.Option
	if hubauth.Staging() {
		opts = append(opts, dockerhub.Staging())
	}
	return &secretsEngine{client: client.HubAuth(opts...)}
}

// secretsEngine reads Docker Hub sessions from the secrets engine, sharing lookups and cooling down after failures.
type secretsEngine struct {
	client    dockerhub.ClientAuth
	clientErr error
	logger    *slog.Logger // nil logs to slog.Default()

	mu          sync.Mutex
	nextAttempt time.Time            // earliest time the engine may be asked again
	reported    bool                 // the current run of failures was logged as a warning
	inflight    *secretsEngineLookup // the lookup callers share, nil when none runs
	lastErr     error                // why the last lookup failed, nil when it didn't
}

// secretsEngineLookup is one session lookup shared by concurrent callers.
type secretsEngineLookup struct {
	done  chan struct{} // closed once token and err are set
	token string
	err   error
}

// secretsEngineToken returns a usable token from the secrets engine and caches it.
func secretsEngineToken(ctx context.Context) (string, bool) {
	token, err := defaultSecretsEngine().fetch(ctx)
	switch {
	case err != nil:
		slog.DebugContext(ctx, "Skipping the secrets engine", "error", err)
		return "", false
	case token == "":
		slog.DebugContext(ctx, "No Docker Hub session in the secrets engine")
		return "", false
	case !usable(token):
		slog.DebugContext(ctx, "The secrets engine served a token that expired, is about to, or was refused",
			"fingerprint", tokenFingerprint(token),
			"expires_in", expiresIn(token))
		return "", false
	case !remember(token, SourceSecretsEngine):
		return "", false
	}
	return token, true
}

// SecretsEngineError returns why the last secrets engine lookup failed, or nil, once any running lookup ends.
func SecretsEngineError() error {
	return defaultSecretsEngine().lastError()
}

// fetch waits at most half of ctx's remaining time for the shared lookup.
func (e *secretsEngine) fetch(ctx context.Context) (string, error) {
	lookup, err := e.join(ctx)
	if err != nil {
		return "", err
	}

	wait, cancel := context.WithTimeout(ctx, secretsEngineWait(ctx))
	defer cancel()

	select {
	case <-lookup.done:
		return lookup.token, lookup.err
	case <-wait.Done():
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", errSecretsEngineSlow
	}
}

// secretsEngineWait is how long ctx's caller may wait for the engine.
func secretsEngineWait(ctx context.Context) time.Duration {
	wait := secretsEngineBudget
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline)/2)
	}
	return wait
}

// join returns the running lookup, or starts one.
func (e *secretsEngine) join(ctx context.Context) (*secretsEngineLookup, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if lookup := e.inflight; lookup != nil {
		return lookup, nil
	}
	if time.Now().Before(e.nextAttempt) {
		return nil, errSecretsEngineCoolingDown
	}

	lookup := &secretsEngineLookup{done: make(chan struct{})}
	e.inflight = lookup
	go e.run(context.WithoutCancel(ctx), lookup)
	return lookup, nil
}

// run reads the session; a failure is logged and starts the cooldown.
func (e *secretsEngine) run(ctx context.Context, lookup *secretsEngineLookup) {
	token, err := e.session(ctx)
	if err != nil {
		e.logError(ctx, err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.nextAttempt = time.Now().Add(secretsEngineCooldown)
	} else {
		e.reported = false
	}
	e.lastErr = err
	e.inflight = nil
	lookup.token, lookup.err = token, err
	close(lookup.done)
}

// wait blocks until the running lookup, if any, ends.
func (e *secretsEngine) wait() {
	e.mu.Lock()
	lookup := e.inflight
	e.mu.Unlock()
	if lookup != nil {
		<-lookup.done
	}
}

// lastError returns why the last lookup failed, or nil, once any running lookup ends.
func (e *secretsEngine) lastError() error {
	e.wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastErr
}

// session returns the default account's access token, or "" when signed out.
func (e *secretsEngine) session(ctx context.Context) (string, error) {
	if e.clientErr != nil {
		return "", e.clientErr
	}

	ctx, cancel := context.WithTimeout(ctx, secretsEngineBudget)
	defer cancel()

	session, err := e.client.GetDefaultSession(ctx)
	switch {
	case err == nil:
		return session.AccessToken, nil
	case errors.Is(err, dockerhub.ErrNoSession):
		return "", nil
	default:
		return "", err
	}
}

// logError warns once per run of failures, except for a missing engine.
func (e *secretsEngine) logError(ctx context.Context, err error) {
	level := slog.LevelWarn
	if errors.Is(err, secretsengine.ErrSecretsEngineNotRunning) || e.failureReported() {
		level = slog.LevelDebug
	}
	args := []any{"error", err}
	if connErr, ok := errors.AsType[*secretsengine.ConnectError](err); ok && connErr.Hint() != "" {
		args = append(args, "hint", connErr.Hint())
	}
	cmp.Or(e.logger, slog.Default()).Log(ctx, level, secretsEngineFailureMsg, args...)
}

// failureReported reports whether this run of failures was already warned about.
func (e *secretsEngine) failureReported() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	reported := e.reported
	e.reported = true
	return reported
}
