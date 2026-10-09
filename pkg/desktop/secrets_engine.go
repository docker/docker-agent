package desktop

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/secrets-engine/client/dockerhub"

	"github.com/docker/docker-agent/pkg/hubauth"
)

const (
	// secretsEngineBudget bounds a single session lookup: the SDK sets no
	// request timeout of its own.
	secretsEngineBudget = 5 * time.Second

	// secretsEngineCooldown is how long a failing engine is skipped.
	secretsEngineCooldown = 30 * time.Second
)

// hubAuth reads Docker Hub sessions from the secrets engine. A var so tests
// can fake it.
var hubAuth = sync.OnceValues(newSecretsEngineHubAuth)

// hubStaging reports whether Docker Hub staging is in use. A var so tests can
// fake it.
var hubStaging = hubauth.Staging

// secretsEngineHubOptions selects the engine realms the session is read from.
// They are production's, unless DOCKER_AGENT_HUB_LOGIN_URL points the token
// exchange at a Docker Hub staging host, such as hub-stage.docker.com: then
// [dockerhub.Staging] reads the staging realms instead.
//
// Limitations:
//   - Docker Desktop doesn't say which environment it is signed in to. A
//     Desktop signed in to staging without that variable is read from the
//     production realms, which hold no session or a production one; with it,
//     production sessions are ignored.
//   - The variable is read once, when the engine client is created.
func secretsEngineHubOptions() []dockerhub.Option {
	if hubStaging() {
		return []dockerhub.Option{dockerhub.Staging()}
	}
	return nil
}

var (
	errSecretsEngineCoolingDown = errors.New("secrets engine lookup failed recently, not retrying yet")
	errSecretsEngineSlow        = errors.New("secrets engine did not answer in time")
)

var secretsEngineState struct {
	sync.Mutex

	nextAttempt time.Time            // earliest time the engine may be asked again
	reported    bool                 // the current run of failures was logged as a warning
	inflight    *secretsEngineLookup // the lookup callers share, nil when none runs
}

// secretsEngineLookup is one session lookup, shared by every caller that
// needs it while it runs.
type secretsEngineLookup struct {
	done  chan struct{} // closed once token and err are set
	token string
	err   error
}

// secretsEngineToken returns a usable token from the secrets engine and caches
// it.
func secretsEngineToken(ctx context.Context) (string, bool) {
	token, err := fetchSecretsEngineToken(ctx)
	switch {
	case err != nil:
		// The lookup logged its own failure: this is only why this caller skips it.
		slog.DebugContext(ctx, "Skipping the secrets engine", "error", err)
		return "", false
	case token == "":
		slog.DebugContext(ctx, "No Docker Hub session in the secrets engine")
		return "", false
	case !usable(token):
		// Fall through so minting or a forced refresh can replace it.
		slog.DebugContext(ctx, "The secrets engine served a token that expired, is about to, or was refused",
			"fingerprint", tokenFingerprint(token),
			"expires_in", expiresIn(token))
		return "", false
	case !remember(token, SourceSecretsEngine):
		// Refused while it was being looked up.
		return "", false
	}
	return token, true
}

// fetchSecretsEngineToken returns the default Docker Hub account's access
// token from the secrets engine, or "" when nobody is signed in.
//
// Concurrent callers share one lookup, which runs on its own budget: a caller
// that gives up doesn't cancel it for the others, and it still records its
// outcome. A caller waits at most half its remaining time, so the fallbacks
// keep the rest.
func fetchSecretsEngineToken(ctx context.Context) (string, error) {
	lookup, err := joinSecretsEngineLookup(ctx)
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

// joinSecretsEngineLookup returns the running lookup, or starts one.
func joinSecretsEngineLookup(ctx context.Context) (*secretsEngineLookup, error) {
	secretsEngineState.Lock()
	defer secretsEngineState.Unlock()

	if lookup := secretsEngineState.inflight; lookup != nil {
		return lookup, nil
	}
	if time.Now().Before(secretsEngineState.nextAttempt) {
		return nil, errSecretsEngineCoolingDown
	}

	lookup := &secretsEngineLookup{done: make(chan struct{})}
	secretsEngineState.inflight = lookup
	go runSecretsEngineLookup(context.WithoutCancel(ctx), lookup)
	return lookup, nil
}

// runSecretsEngineLookup asks the engine for the default session and records
// the outcome: a failure starts the cooldown and is logged.
func runSecretsEngineLookup(ctx context.Context, lookup *secretsEngineLookup) {
	token, err := lookUpSecretsEngineSession(ctx)
	if err != nil {
		logSecretsEngineError(ctx, err)
	}

	secretsEngineState.Lock()
	defer secretsEngineState.Unlock()
	if err != nil {
		secretsEngineState.nextAttempt = time.Now().Add(secretsEngineCooldown)
	} else {
		secretsEngineState.reported = false // the engine answered: a new failure is news
	}
	secretsEngineState.inflight = nil
	lookup.token, lookup.err = token, err
	close(lookup.done)
}

// lookUpSecretsEngineSession returns the default account's access token, or ""
// when nobody is signed in.
func lookUpSecretsEngineSession(ctx context.Context) (string, error) {
	hub, err := hubAuth()
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, secretsEngineBudget)
	defer cancel()

	session, err := hub.GetDefaultSession(ctx)
	switch {
	case err == nil:
		return session.AccessToken, nil
	case errors.Is(err, dockerhub.ErrNoSession):
		return "", nil
	default:
		return "", err
	}
}

// logSecretsEngineError says why the engine couldn't be read. Most machines
// without Docker Desktop have no engine, so that's only logged at debug level.
// Other failures, such as access being denied, mean something is wrong. Only
// the first failure in a run is a warning, so a long-lived process doesn't
// repeat it after every cooldown.
func logSecretsEngineError(ctx context.Context, err error) {
	if secretsEngineUnavailable(err) || secretsEngineFailureReported() {
		slog.DebugContext(ctx, secretsEngineFailureMsg, "error", err)
		return
	}
	slog.WarnContext(ctx, secretsEngineFailureMsg, "error", err)
}

const secretsEngineFailureMsg = "Could not read the Docker Hub session from the secrets engine"

// secretsEngineFailureReported reports whether the current run of failures
// was already logged as a warning, and records that it now is.
func secretsEngineFailureReported() bool {
	secretsEngineState.Lock()
	defer secretsEngineState.Unlock()
	reported := secretsEngineState.reported
	secretsEngineState.reported = true
	return reported
}
