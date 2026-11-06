//go:build darwin || linux || windows

package desktop

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/docker/secrets-engine/client/dockerhub"
	"github.com/docker/secrets-engine/x/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hubauth"
)

func TestGetTokenFromSecretsEngine(t *testing.T) {
	t.Run("the engine session is preferred to Docker Desktop's backend", func(t *testing.T) {
		fromEngine := makeToken(t, time.Now().Add(time.Hour))
		installFakeBackend(t, &fakeBackend{token: makeToken(t, time.Now().Add(time.Hour))})
		installFakeEngine(t, &fakeEngine{token: fromEngine})

		token, source := GetTokenWithSource(t.Context())
		assert.Equal(t, fromEngine, token)
		assert.Equal(t, SourceSecretsEngine, source)
	})

	t.Run("an engine token is served from memory", func(t *testing.T) {
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

	t.Run("nobody signed in falls back, then picks up a sign-in", func(t *testing.T) {
		fromDesktop := makeToken(t, time.Now().Add(time.Hour))
		installFakeBackend(t, &fakeBackend{token: fromDesktop})
		engine := &fakeEngine{}
		installFakeEngine(t, engine)

		token, source := GetTokenWithSource(t.Context())
		assert.Equal(t, fromDesktop, token)
		assert.Equal(t, SourceDesktop, source)

		fromEngine := makeToken(t, time.Now().Add(time.Hour))
		engine.setToken(fromEngine)
		expireCache()

		token, source = GetTokenWithSource(t.Context())
		assert.Equal(t, fromEngine, token)
		assert.Equal(t, SourceSecretsEngine, source)
	})

	t.Run("a failing engine falls back to Docker Desktop", func(t *testing.T) {
		fromDesktop := makeToken(t, time.Now().Add(time.Hour))
		installFakeBackend(t, &fakeBackend{token: fromDesktop})
		installFakeEngine(t, &fakeEngine{err: secrets.ErrAccessDenied})

		token, source := GetTokenWithSource(t.Context())
		assert.Equal(t, fromDesktop, token)
		assert.Equal(t, SourceDesktop, source)
	})

	t.Run("an expiring engine token falls back to Docker Desktop", func(t *testing.T) {
		fromDesktop := makeToken(t, time.Now().Add(time.Hour))
		installFakeBackend(t, &fakeBackend{token: fromDesktop})
		installFakeEngine(t, &fakeEngine{token: makeToken(t, time.Now().Add(10*time.Second))})

		token, source := GetTokenWithSource(t.Context())
		assert.Equal(t, fromDesktop, token)
		assert.Equal(t, SourceDesktop, source)
	})

	t.Run("an expired engine token falls through to minting", func(t *testing.T) {
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

	t.Run("a token that is not a JWT falls back to Docker Desktop", func(t *testing.T) {
		fromDesktop := makeToken(t, time.Now().Add(time.Hour))
		installFakeBackend(t, &fakeBackend{token: fromDesktop})
		installFakeEngine(t, &fakeEngine{token: "dckr_pat_0123456789abcdef", username: "engine-user"})

		token, source := GetTokenWithSource(t.Context())
		assert.Equal(t, fromDesktop, token)
		assert.Equal(t, SourceDesktop, source)
		require.ErrorIs(t, SecretsEngineError(), dockerhub.ErrInvalidAccessToken)
	})

	for _, tt := range []struct {
		name   string
		engine func(token string) *fakeEngine
	}{
		{name: "a session without claims", engine: func(token string) *fakeEngine {
			return &fakeEngine{token: token, noClaims: true}
		}},
		{name: "a session without an expiry", engine: func(token string) *fakeEngine {
			return &fakeEngine{token: token, noExpiry: true}
		}},
		{name: "a session the engine says is expiring", engine: func(token string) *fakeEngine {
			return &fakeEngine{token: token, expiresAt: time.Now().Add(10 * time.Second)}
		}},
	} {
		t.Run(tt.name+" falls back to Docker Desktop", func(t *testing.T) {
			fromDesktop := makeToken(t, time.Now().Add(time.Hour))
			installFakeBackend(t, &fakeBackend{token: fromDesktop})
			installFakeEngine(t, tt.engine(makeToken(t, time.Now().Add(time.Hour))))

			token, source := GetTokenWithSource(t.Context())
			assert.Equal(t, fromDesktop, token)
			assert.Equal(t, SourceDesktop, source)
		})
	}

	t.Run("a cached engine token is dropped as its session nears expiry", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fromDesktop := makeToken(t, time.Now().Add(time.Hour))
			installFakeBackend(t, &fakeBackend{token: fromDesktop})
			installFakeEngine(t, &fakeEngine{
				token:     makeToken(t, time.Now().Add(time.Hour)),
				expiresAt: time.Now().Add(40 * time.Second),
			})

			_, source := GetTokenWithSource(t.Context())
			require.Equal(t, SourceSecretsEngine, source)

			time.Sleep(15 * time.Second) //nolint:forbidigo // Fake time: the session is within the leeway, the cache is not stale yet.

			token, source := GetTokenWithSource(t.Context())
			assert.Equal(t, fromDesktop, token)
			assert.Equal(t, SourceDesktop, source)
		})
	})
}

func TestGetUserInfoFromSecretsEngine(t *testing.T) {
	desktopAccount := DockerHubInfo{Username: "desktop-user", Email: "desktop@example.com"}
	engineAccount := DockerHubInfo{Username: "engine-user", Email: "engine@example.com"}

	t.Run("names the account of the engine session", func(t *testing.T) {
		backend := &fakeBackend{token: makeIdentityToken(t, "claims-user", "claims@example.com")}
		backend.info = &desktopAccount
		installFakeBackend(t, backend)
		installFakeEngine(t, &fakeEngine{
			token:    makeIdentityToken(t, "jwt-user", "jwt@example.com"),
			username: engineAccount.Username,
			email:    engineAccount.Email,
		})

		assert.Equal(t, engineAccount, GetUserInfo(t.Context()))
	})

	t.Run("names the engine account of a cached engine token", func(t *testing.T) {
		backend := &fakeBackend{}
		backend.info = &desktopAccount
		installFakeBackend(t, backend)
		engine := &fakeEngine{
			token:    makeToken(t, time.Now().Add(time.Hour)),
			username: engineAccount.Username,
			email:    engineAccount.Email,
		}
		installFakeEngine(t, engine)

		_, source := GetTokenWithSource(t.Context())
		require.Equal(t, SourceSecretsEngine, source)
		lookups := engine.lookupCount()

		assert.Equal(t, engineAccount, GetUserInfo(t.Context()))
		assert.Equal(t, lookups, engine.lookupCount())
	})

	t.Run("reads Docker Desktop for an engine session without claims", func(t *testing.T) {
		backend := &fakeBackend{}
		backend.info = &desktopAccount
		installFakeBackend(t, backend)
		installFakeEngine(t, &fakeEngine{token: makeToken(t, time.Now().Add(time.Hour)), noClaims: true})

		assert.Equal(t, desktopAccount, GetUserInfo(t.Context()))
	})

	t.Run("reads Docker Desktop for a cached Docker Desktop token", func(t *testing.T) {
		backend := &fakeBackend{token: makeIdentityToken(t, "claims-user", "claims@example.com")}
		backend.info = &desktopAccount
		installFakeBackend(t, backend)
		engine := &fakeEngine{}
		installFakeEngine(t, engine)

		_, source := GetTokenWithSource(t.Context())
		require.Equal(t, SourceDesktop, source)
		engine.setToken(makeToken(t, time.Now().Add(time.Hour)))

		assert.Equal(t, desktopAccount, GetUserInfo(t.Context()))
	})

	t.Run("reads Docker Desktop for a token that is not a JWT", func(t *testing.T) {
		backend := &fakeBackend{}
		backend.info = &desktopAccount
		installFakeBackend(t, backend)
		installFakeEngine(t, &fakeEngine{token: "dckr_pat_0123456789abcdef", username: engineAccount.Username})

		assert.Equal(t, desktopAccount, GetUserInfo(t.Context()))
	})

	t.Run("reads Docker Desktop before a stalled engine uses up the deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			backend := &fakeBackend{token: makeToken(t, time.Now().Add(-time.Hour)), loggedIn: true}
			backend.info = &desktopAccount
			installFakeBackend(t, backend)
			installFakeEngine(t, &fakeEngine{block: true})

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			assert.Equal(t, desktopAccount, GetUserInfo(ctx))
		})
	})

	t.Run("a refused engine token is not served again", func(t *testing.T) {
		fromEngine := makeToken(t, time.Now().Add(time.Hour))
		fromDesktop := makeToken(t, time.Now().Add(time.Hour))
		installFakeBackend(t, &fakeBackend{token: fromDesktop})
		installFakeEngine(t, &fakeEngine{token: fromEngine})
		require.Equal(t, fromEngine, GetToken(t.Context()))

		InvalidateToken(fromEngine)

		token, source := GetTokenWithSource(t.Context())
		assert.Equal(t, fromDesktop, token)
		assert.Equal(t, SourceDesktop, source)
	})
}

func TestSecretsEngineCooldown(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "engine not running", err: errors.New("secrets engine is not running at engine.sock: dial unix engine.sock: connect: no such file or directory")},
		{name: "access denied", err: secrets.ErrAccessDenied},
		{name: "lookup failed", err: errors.New("internal error")},
	} {
		t.Run(tt.name+" cools down", func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				fake := &fakeEngine{err: tt.err}
				engine := newTestEngine(t, fake)

				_, err := engine.fetch(t.Context())
				require.ErrorIs(t, err, tt.err)
				lookups := fake.lookupCount()
				require.Positive(t, lookups)

				_, err = engine.fetch(t.Context())
				require.ErrorIs(t, err, errSecretsEngineCoolingDown)
				assert.Equal(t, lookups, fake.lookupCount(), "a failing engine is left alone for a while")

				endCooldown(engine)
				_, err = engine.fetch(t.Context())
				require.ErrorIs(t, err, tt.err)
				assert.Greater(t, fake.lookupCount(), lookups, "the engine is asked again after the cooldown")
			})
		})
	}

	t.Run("nobody signed in does not cool down", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fake := &fakeEngine{}
			engine := newTestEngine(t, fake)

			session, err := engine.fetch(t.Context())
			require.NoError(t, err)
			assert.Empty(t, session.AccessToken)
			lookups := fake.lookupCount()

			_, err = engine.fetch(t.Context())
			require.NoError(t, err)
			assert.Greater(t, fake.lookupCount(), lookups)
		})
	})

	t.Run("an engine client that can't be created fails the lookup", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			errClient := errors.New("could not create the secrets engine client")
			engine := &secretsEngine{clientErr: errClient, logger: slog.New(slog.DiscardHandler)}

			_, err := engine.fetch(t.Context())
			require.ErrorIs(t, err, errClient)
		})
	})
}

func TestSecretsEngineCallerDeadline(t *testing.T) {
	t.Parallel()

	t.Run("an unresponsive engine delays the lookup by its budget only", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fake := &fakeEngine{block: true}
			engine := newTestEngine(t, fake)

			start := time.Now()
			_, err := engine.fetch(t.Context())
			require.Error(t, err)
			assert.Equal(t, secretsEngineBudget, time.Since(start))
			synctest.Wait()

			lookups := fake.lookupCount()
			start = time.Now()
			_, err = engine.fetch(t.Context())
			require.ErrorIs(t, err, errSecretsEngineCoolingDown)
			assert.Zero(t, time.Since(start), "an unresponsive engine is not waited on again right away")
			assert.Equal(t, lookups, fake.lookupCount())
		})
	})

	t.Run("an unresponsive engine leaves the fallbacks half the caller's time", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			engine := newTestEngine(t, &fakeEngine{block: true})

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			start := time.Now()
			_, err := engine.fetch(ctx)

			require.ErrorIs(t, err, errSecretsEngineSlow)
			assert.Equal(t, 2500*time.Millisecond, time.Since(start))
			require.NoError(t, ctx.Err(), "the caller has time left for its request")
		})
	})

	t.Run("an engine that outlives its callers still cools down", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fake := &fakeEngine{block: true}
			engine := newTestEngine(t, fake)

			lookUp := func() time.Duration {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				start := time.Now()
				_, _ = engine.fetch(ctx)
				return time.Since(start)
			}

			lookUp()
			lookups := fake.lookupCount()
			time.Sleep(secretsEngineBudget) //nolint:forbidigo // Fake time: the abandoned lookup times out.
			synctest.Wait()

			assert.Zero(t, lookUp(), "the next caller doesn't wait for the engine")
			assert.Equal(t, lookups, fake.lookupCount())
		})
	})

	t.Run("a canceled caller doesn't cancel the lookup for the others", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fromEngine := makeToken(t, time.Now().Add(time.Hour))
			fake := &fakeEngine{token: fromEngine, delay: time.Second}
			engine := newTestEngine(t, fake)

			canceled, cancel := context.WithCancel(t.Context())
			go func() {
				time.Sleep(100 * time.Millisecond) //nolint:forbidigo // Fake time: cancel while the lookup runs.
				cancel()
			}()
			_, err := engine.fetch(canceled)
			require.ErrorIs(t, err, context.Canceled)

			session, err := engine.fetch(t.Context())
			require.NoError(t, err)
			assert.Equal(t, fromEngine, session.AccessToken)
			assert.Equal(t, 2, fake.lookupCount(), "one profile and one session read")
		})
	})
}

func TestSecretsEngineConcurrentLookups(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		fromEngine := makeToken(t, time.Now().Add(time.Hour))
		fake := &fakeEngine{token: fromEngine, delay: time.Second}
		engine := newTestEngine(t, fake)

		sessions := make([]dockerhub.UserSession, 32)
		errs := make([]error, 32)
		var wg sync.WaitGroup
		for i := range sessions {
			wg.Go(func() {
				sessions[i], errs[i] = engine.fetch(t.Context())
			})
		}
		wg.Wait()

		require.NoError(t, errors.Join(errs...))
		for _, session := range sessions {
			assert.Equal(t, fromEngine, session.AccessToken)
		}

		assert.Equal(t, 2, fake.lookupCount(), "concurrent callers share one lookup")
	})
}

func TestSecretsEngineLastError(t *testing.T) {
	t.Parallel()

	t.Run("a failed lookup is recorded until the engine answers", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fake := &fakeEngine{err: secrets.ErrAccessDenied}
			engine := newTestEngine(t, fake)
			require.NoError(t, engine.lastError())

			_, _ = engine.fetch(t.Context())
			require.ErrorIs(t, engine.lastError(), secrets.ErrAccessDenied)

			fake.setErr(nil)
			endCooldown(engine)
			_, _ = engine.fetch(t.Context())
			require.NoError(t, engine.lastError())
		})
	})

	t.Run("a slow lookup is reported without waiting for it", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			engine := newTestEngine(t, &fakeEngine{block: true})

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			_, err := engine.fetch(ctx)
			require.ErrorIs(t, err, errSecretsEngineSlow)

			start := time.Now()
			require.ErrorIs(t, engine.lastError(), errSecretsEngineSlow)
			assert.Zero(t, time.Since(start))
			require.NoError(t, ctx.Err())

			engine.wait()
			require.ErrorIs(t, engine.lastError(), context.DeadlineExceeded)
		})
	})
}

func TestSecretsEngineDiagnosticsPreserveLegacyAccount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		account := DockerHubInfo{Username: "legacy-user", Email: "legacy@example.com"}
		fromDesktop := makeToken(t, time.Now().Add(time.Hour))
		installFakeBackend(t, &fakeBackend{token: fromDesktop, info: &account})
		installFakeEngine(t, &fakeEngine{block: true})

		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		start := time.Now()

		token, source := GetTokenWithSource(ctx)
		require.Equal(t, fromDesktop, token)
		require.Equal(t, SourceDesktop, source)
		require.Equal(t, time.Second, time.Since(start))

		require.ErrorIs(t, SecretsEngineError(), errSecretsEngineSlow)
		require.NoError(t, ctx.Err())
		assert.Equal(t, account, GetUserInfo(ctx))
		assert.Equal(t, time.Second, time.Since(start))
	})
}

func newTestEngine(t *testing.T, fake *fakeEngine) *secretsEngine {
	t.Helper()

	engine := &secretsEngine{client: dockerhub.New(fake), logger: slog.New(slog.DiscardHandler)}
	t.Cleanup(engine.wait)
	return engine
}

func installNoSecretsEngine(t *testing.T) {
	t.Helper()
	installFakeEngine(t, &fakeEngine{})
}

func installFakeEngine(t *testing.T, fake *fakeEngine) {
	t.Helper()

	engine := newTestEngine(t, fake)
	old := defaultSecretsEngine
	defaultSecretsEngine = func() *secretsEngine { return engine }
	t.Cleanup(func() { defaultSecretsEngine = old })
}

func endCooldown(engine *secretsEngine) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.nextAttempt = time.Time{}
}

type fakeEngine struct {
	mu        sync.Mutex
	token     string
	username  string
	email     string
	expiresAt time.Time
	noClaims  bool
	noExpiry  bool
	err       error
	block     bool
	delay     time.Duration
	lookups   int
}

// claims are what the engine stores alongside the token: its account, and the token's expiry unless overridden.
func (e *fakeEngine) claims(token string) dockerhub.Claims {
	if e.noClaims {
		return dockerhub.Claims{}
	}
	claims := dockerhub.Claims{Username: cmp.Or(e.username, "testuser"), Email: e.email}
	if e.noExpiry {
		return claims
	}
	exp := e.expiresAt
	if exp.IsZero() {
		exp, _ = hubauth.Expiry(token)
	}
	if !exp.IsZero() {
		claims.ExpiresAt = &dockerhub.NumericDate{Time: exp}
	}
	return claims
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
	token, err, block, delay := e.token, e.err, e.block, e.delay
	claims := e.claims(token)
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
		value = dockerhub.UserSession{AccessToken: token, Claims: claims}
	default:
		return nil, secrets.ErrNotFound
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return []secrets.Envelope{{ID: secrets.MustParseID(pattern.String()), Value: data}}, nil
}
