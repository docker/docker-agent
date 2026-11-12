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
	done    chan struct{} // closed once session and err are set
	session dockerhub.UserSession
	err     error
}

// secretsEngineToken returns a usable token from the secrets engine and its account, and caches both.
func secretsEngineToken(ctx context.Context) (string, DockerHubInfo, bool) {
	session, err := defaultSecretsEngine().fetch(ctx)
	token, claims := session.AccessToken, session.Claims
	switch {
	case err != nil:
		slog.DebugContext(ctx, "Skipping the secrets engine", "error", err)
		return "", DockerHubInfo{}, false
	case token == "":
		slog.DebugContext(ctx, "No Docker Hub session in the secrets engine")
		return "", DockerHubInfo{}, false
	case claims.Username == "":
		slog.WarnContext(ctx, "The secrets engine served a session without an account",
			"fingerprint", tokenFingerprint(token))
		return "", DockerHubInfo{}, false
	case claims.ExpiresAt == nil || hubauth.ExpiringAt(claims.ExpiresAt.Time):
		slog.DebugContext(ctx, "The secrets engine served a session that expired, is about to, or has no expiry",
			"fingerprint", tokenFingerprint(token),
			"expires_at", sessionExpiry(session))
		return "", DockerHubInfo{}, false
	case wasRejected(token):
		slog.DebugContext(ctx, "The secrets engine served a token Docker refused",
			"fingerprint", tokenFingerprint(token))
		return "", DockerHubInfo{}, false
	}

	account := DockerHubInfo{Username: claims.Username, Email: claims.Email}
	if !rememberSession(token, SourceSecretsEngine, account, claims.ExpiresAt.Time) {
		return "", DockerHubInfo{}, false
	}
	return token, account, true
}

func sessionExpiry(session dockerhub.UserSession) string {
	if exp := session.Claims.ExpiresAt; exp != nil && !exp.IsZero() {
		return exp.UTC().Format(time.RFC3339)
	}
	return "none"
}

// SecretsEngineError returns the last lookup error, or a timeout error while a lookup is still running.
func SecretsEngineError() error {
	return defaultSecretsEngine().lastError()
}

// fetch waits at most half of ctx's remaining time for the shared lookup.
func (e *secretsEngine) fetch(ctx context.Context) (dockerhub.UserSession, error) {
	lookup, err := e.join(ctx)
	if err != nil {
		return dockerhub.UserSession{}, err
	}

	wait, cancel := context.WithTimeout(ctx, secretsEngineWait(ctx))
	defer cancel()

	select {
	case <-lookup.done:
		return lookup.session, lookup.err
	case <-wait.Done():
		if err := ctx.Err(); err != nil {
			return dockerhub.UserSession{}, err
		}
		return dockerhub.UserSession{}, errSecretsEngineSlow
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
	session, err := e.session(ctx)
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
	lookup.session, lookup.err = session, err
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

// lastError reports failures without waiting for a lookup the caller already stopped waiting for.
func (e *secretsEngine) lastError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inflight != nil {
		return errSecretsEngineSlow
	}
	return e.lastErr
}

// session returns the default account's session, or none when signed out; a missing or non-JWT token fails.
func (e *secretsEngine) session(ctx context.Context) (dockerhub.UserSession, error) {
	if e.clientErr != nil {
		return dockerhub.UserSession{}, e.clientErr
	}

	ctx, cancel := context.WithTimeout(ctx, secretsEngineBudget)
	defer cancel()

	session, err := e.client.GetDefaultSession(ctx)
	switch {
	case err == nil:
		return session, nil
	case errors.Is(err, dockerhub.ErrInvalidAccessToken), errors.Is(err, dockerhub.ErrMissingAccessToken):
		return dockerhub.UserSession{}, err
	case errors.Is(err, dockerhub.ErrNoSession):
		return dockerhub.UserSession{}, nil
	default:
		return dockerhub.UserSession{}, err
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
