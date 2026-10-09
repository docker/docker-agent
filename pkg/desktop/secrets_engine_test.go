package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/docker/secrets-engine/client/dockerhub"
	"github.com/docker/secrets-engine/x/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetTokenFromSecretsEngine(t *testing.T) {
	t.Run("the engine session is preferred to Docker Desktop's backend", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fromEngine := makeToken(t, time.Now().Add(time.Hour))
			installFakeBackend(t, &fakeBackend{token: makeToken(t, time.Now().Add(time.Hour))})
			installFakeEngine(t, &fakeEngine{token: fromEngine})

			token, source := GetTokenWithSource(t.Context())
			assert.Equal(t, fromEngine, token)
			assert.Equal(t, SourceSecretsEngine, source)
		})
	})

	t.Run("an engine token is served from memory", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fromEngine := makeToken(t, time.Now().Add(time.Hour))
			installFakeBackend(t, &fakeBackend{})
			engine := &fakeEngine{token: fromEngine}
			installFakeEngine(t, engine)

			require.Equal(t, fromEngine, GetToken(t.Context()))
			lookups := engine.lookupCount()

			token, source := GetTokenWithSource(t.Context())
			assert.Equal(t, fromEngine, token)
			assert.Equal(t, SourceSecretsEngine, source)
			assert.Equal(t, lookups, engine.lookupCount(), "gateway clients call this per request")
		})
	})

	t.Run("nobody signed in falls back without cooling down", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fromDesktop := makeToken(t, time.Now().Add(time.Hour))
			installFakeBackend(t, &fakeBackend{token: fromDesktop})
			engine := &fakeEngine{}
			installFakeEngine(t, engine)

			token, source := GetTokenWithSource(t.Context())
			assert.Equal(t, fromDesktop, token)
			assert.Equal(t, SourceDesktop, source)

			// The user signs in: the next re-check picks it up.
			fromEngine := makeToken(t, time.Now().Add(time.Hour))
			engine.setToken(fromEngine)
			expireCache()

			token, source = GetTokenWithSource(t.Context())
			assert.Equal(t, fromEngine, token)
			assert.Equal(t, SourceSecretsEngine, source)
		})
	})

	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "engine unavailable", err: errors.New("secrets engine is not available: dial unix engine.sock: connect: no such file or directory")},
		{name: "access denied", err: secrets.ErrAccessDenied},
		{name: "lookup failed", err: errors.New("internal error")},
	} {
		t.Run(tt.name+" falls back and cools down", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fromDesktop := makeToken(t, time.Now().Add(time.Hour))
				installFakeBackend(t, &fakeBackend{token: fromDesktop})
				engine := &fakeEngine{err: tt.err}
				installFakeEngine(t, engine)

				token, source := GetTokenWithSource(t.Context())
				assert.Equal(t, fromDesktop, token)
				assert.Equal(t, SourceDesktop, source)
				lookups := engine.lookupCount()
				require.Positive(t, lookups)

				expireCache()
				assert.Equal(t, fromDesktop, GetToken(t.Context()))
				assert.Equal(t, lookups, engine.lookupCount(), "a failing engine is left alone for a while")

				endSecretsEngineCooldown()
				expireCache()
				assert.Equal(t, fromDesktop, GetToken(t.Context()))
				assert.Greater(t, engine.lookupCount(), lookups, "the engine is asked again after the cooldown")
			})
		})
	}

	t.Run("an engine client that can't be created falls back", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fromDesktop := makeToken(t, time.Now().Add(time.Hour))
			installFakeBackend(t, &fakeBackend{token: fromDesktop})
			stubHubAuth(t, func() (dockerhub.ClientAuth, error) {
				return nil, errors.New("could not create the secrets engine client")
			})

			token, source := GetTokenWithSource(t.Context())
			assert.Equal(t, fromDesktop, token)
			assert.Equal(t, SourceDesktop, source)
		})
	})

	t.Run("an expiring engine token falls back to Docker Desktop", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fromDesktop := makeToken(t, time.Now().Add(time.Hour))
			installFakeBackend(t, &fakeBackend{token: fromDesktop})
			installFakeEngine(t, &fakeEngine{token: makeToken(t, time.Now().Add(10*time.Second))})

			token, source := GetTokenWithSource(t.Context())
			assert.Equal(t, fromDesktop, token)
			assert.Equal(t, SourceDesktop, source)
		})
	})

	t.Run("an expired engine token falls through to minting", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// A stuck Desktop refresher leaves both sources stale.
			expired := makeToken(t, time.Now().Add(-time.Hour))
			minted := makeToken(t, time.Now().Add(time.Hour))
			backend := &fakeBackend{token: expired}
			installFakeBackend(t, backend)
			installFakeEngine(t, &fakeEngine{token: expired})
			mintToken = func(context.Context) (string, error) { return minted, nil }

			token, source := GetTokenWithSource(t.Context())
			assert.Equal(t, minted, token)
			assert.Equal(t, SourceMinted, source)
			assert.Equal(t, 0, backend.refreshes())
		})
	})

	t.Run("a refused engine token is not served again", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fromEngine := makeToken(t, time.Now().Add(time.Hour))
			fromDesktop := makeToken(t, time.Now().Add(time.Hour))
			installFakeBackend(t, &fakeBackend{token: fromDesktop})
			installFakeEngine(t, &fakeEngine{token: fromEngine})
			require.Equal(t, fromEngine, GetToken(t.Context()))

			// The engine keeps serving the token Docker refused.
			InvalidateToken(fromEngine)

			token, source := GetTokenWithSource(t.Context())
			assert.Equal(t, fromDesktop, token)
			assert.Equal(t, SourceDesktop, source)
		})
	})

	t.Run("an unresponsive engine delays the lookup by its budget only", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fromDesktop := makeToken(t, time.Now().Add(time.Hour))
			installFakeBackend(t, &fakeBackend{token: fromDesktop})
			engine := &fakeEngine{block: true}
			installFakeEngine(t, engine)

			start := time.Now()
			token, source := GetTokenWithSource(t.Context())
			elapsed := time.Since(start)

			// The engine's deadline must not cut the caller's lookup short.
			assert.Equal(t, fromDesktop, token)
			assert.Equal(t, SourceDesktop, source)
			assert.GreaterOrEqual(t, elapsed, secretsEngineBudget)
			assert.Less(t, elapsed, secretsEngineBudget+time.Second)

			lookups := engine.lookupCount()
			expireCache()
			start = time.Now()
			assert.Equal(t, fromDesktop, GetToken(t.Context()))
			assert.Less(t, time.Since(start), time.Second, "an unresponsive engine is not waited on again right away")
			assert.Equal(t, lookups, engine.lookupCount())
		})
	})
}

func TestSecretsEngineCallerDeadline(t *testing.T) {
	t.Run("an unresponsive engine leaves the fallbacks half the caller's time", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// Gateway model discovery gives auth and its request 5s in all.
			fromDesktop := makeToken(t, time.Now().Add(time.Hour))
			installFakeBackend(t, &fakeBackend{token: fromDesktop})
			engine := &fakeEngine{block: true}
			installFakeEngine(t, engine)

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			start := time.Now()
			token, source := GetTokenWithSource(ctx)

			assert.Equal(t, fromDesktop, token)
			assert.Equal(t, SourceDesktop, source)
			assert.Equal(t, 2500*time.Millisecond, time.Since(start))
			require.NoError(t, ctx.Err(), "the caller has time left for its request")
		})
	})

	t.Run("an engine that outlives its callers still cools down", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fromDesktop := makeToken(t, time.Now().Add(time.Hour))
			installFakeBackend(t, &fakeBackend{token: fromDesktop})
			engine := &fakeEngine{block: true}
			installFakeEngine(t, engine)

			lookUp := func() time.Duration {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				expireCache()
				start := time.Now()
				assert.Equal(t, fromDesktop, GetToken(ctx))
				return time.Since(start)
			}

			lookUp()
			lookups := engine.lookupCount()
			time.Sleep(secretsEngineBudget) //nolint:forbidigo // Fake time: the abandoned lookup times out.
			synctest.Wait()

			assert.Zero(t, lookUp(), "the next caller doesn't wait for the engine")
			assert.Equal(t, lookups, engine.lookupCount())
		})
	})

	t.Run("a canceled caller doesn't cancel the lookup for the others", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fromEngine := makeToken(t, time.Now().Add(time.Hour))
			engine := &fakeEngine{token: fromEngine, delay: time.Second}
			installFakeEngine(t, engine)

			canceled, cancel := context.WithCancel(t.Context())
			go func() {
				time.Sleep(100 * time.Millisecond) //nolint:forbidigo // Fake time: cancel while the lookup runs.
				cancel()
			}()
			_, err := fetchSecretsEngineToken(canceled)
			require.ErrorIs(t, err, context.Canceled)

			token, err := fetchSecretsEngineToken(t.Context())
			require.NoError(t, err)
			assert.Equal(t, fromEngine, token)
			assert.Equal(t, 2, engine.lookupCount(), "one profile and one session read")
		})
	})
}

func TestSecretsEngineHubOptions(t *testing.T) {
	for _, tt := range []struct {
		name    string
		staging bool
		want    string
	}{
		{name: "production by default", want: "docker/auth/metadata/hub/default"},
		{name: "staging when the token exchange targets it", staging: true, want: "docker/auth/metadata/hub-staging/default"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			old := hubStaging
			hubStaging = func() bool { return tt.staging }
			t.Cleanup(func() { hubStaging = old })

			engine := &fakeEngine{}
			_, err := dockerhub.New(engine, secretsEngineHubOptions()...).GetDefaultSession(t.Context())
			require.ErrorIs(t, err, dockerhub.ErrNoSession)
			assert.Equal(t, []string{tt.want}, engine.askedFor())
		})
	}
}

func TestSecretsEngineConcurrentLookups(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fromEngine := makeToken(t, time.Now().Add(time.Hour))
		installFakeBackend(t, &fakeBackend{})
		engine := &fakeEngine{token: fromEngine, delay: time.Second}
		installFakeEngine(t, engine)

		var wg sync.WaitGroup
		for range 32 {
			wg.Go(func() {
				assert.Equal(t, fromEngine, GetToken(t.Context()))
			})
		}
		wg.Wait()

		assert.Equal(t, 2, engine.lookupCount(), "concurrent callers share one lookup")
	})
}

// installFakeEngine serves engine through the real dockerhub accessor.
func installFakeEngine(t *testing.T, engine *fakeEngine) {
	t.Helper()

	resetSecretsEngineState()
	t.Cleanup(resetSecretsEngineState)

	stubHubAuth(t, func() (dockerhub.ClientAuth, error) { return dockerhub.New(engine), nil })
}

// stubHubAuth replaces the secrets engine client for the rest of the test.
func stubHubAuth(t *testing.T, fake func() (dockerhub.ClientAuth, error)) {
	t.Helper()

	old := hubAuth
	hubAuth = fake
	t.Cleanup(func() { hubAuth = old })
	t.Cleanup(awaitSecretsEngineLookup) // runs first: a lookup may outlive its callers
}

// awaitSecretsEngineLookup waits for the running lookup, if any, to finish.
func awaitSecretsEngineLookup() {
	secretsEngineState.Lock()
	lookup := secretsEngineState.inflight
	secretsEngineState.Unlock()
	if lookup != nil {
		<-lookup.done
	}
}

func resetSecretsEngineState() {
	secretsEngineState.Lock()
	defer secretsEngineState.Unlock()
	secretsEngineState.nextAttempt, secretsEngineState.reported = time.Time{}, false
	secretsEngineState.inflight = nil
}

func endSecretsEngineCooldown() {
	secretsEngineState.Lock()
	defer secretsEngineState.Unlock()
	secretsEngineState.nextAttempt = time.Time{}
}

// fakeEngine serves the default account's profile and session.
type fakeEngine struct {
	mu      sync.Mutex
	token   string        // the default account's access token; "" when signed out
	err     error         // returned by every lookup when set
	block   bool          // lookups wait for their context to end
	delay   time.Duration // lookups take this long to answer
	lookups int
	asked   []string // the patterns looked up, in order
}

func (e *fakeEngine) askedFor() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.asked
}

func (e *fakeEngine) setToken(token string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.token = token
}

func (e *fakeEngine) setErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err = err
}

func (e *fakeEngine) lookupCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lookups
}

func (e *fakeEngine) GetSecrets(ctx context.Context, pattern secrets.Pattern) ([]secrets.Envelope, error) {
	e.mu.Lock()
	e.lookups++
	e.asked = append(e.asked, pattern.String())
	token, err, block, delay := e.token, e.err, e.block, e.delay
	e.mu.Unlock()

	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, secrets.ErrNotFound
	}

	var value any
	switch pattern.String() {
	case "docker/auth/metadata/hub/default":
		value = dockerhub.Profile{UserID: "docker/auth/hub/testuser", Username: "testuser"}
	case "docker/auth/hub/testuser":
		value = dockerhub.UserSession{AccessToken: token}
	default:
		return nil, secrets.ErrNotFound
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return []secrets.Envelope{{ID: secrets.MustParseID(pattern.String()), Value: data}}, nil
}
