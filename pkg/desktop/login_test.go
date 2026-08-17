package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetToken(t *testing.T) {
	valid := makeToken(t, time.Now().Add(time.Hour))
	expired := makeToken(t, time.Now().Add(-time.Hour))

	t.Run("valid token returned as-is", func(t *testing.T) {
		backend := &fakeBackend{token: valid}
		installFakeBackend(t, backend)

		assert.Equal(t, valid, GetToken(t.Context()))
		assert.Equal(t, 0, backend.refreshes())
	})

	t.Run("expired token replaced by a minted one", func(t *testing.T) {
		backend := &fakeBackend{token: expired}
		installFakeBackend(t, backend)
		mintToken = func(context.Context) (string, error) { return valid, nil }

		token, source := GetTokenWithSource(t.Context())
		assert.Equal(t, valid, token)
		assert.Equal(t, SourceMinted, source)
		assert.Equal(t, 0, backend.refreshes(), "minting makes nudging Desktop unnecessary")
	})

	t.Run("a usable token is served from memory", func(t *testing.T) {
		backend := &fakeBackend{token: valid}
		installFakeBackend(t, backend)

		token, source := GetTokenWithSource(t.Context())
		assert.Equal(t, valid, token)
		assert.Equal(t, SourceDesktop, source)

		// Desktop is not asked again: gateway clients call this per request.
		backend.setFailTokenFetch(true)
		assert.Equal(t, valid, GetToken(t.Context()))
	})

	t.Run("an invalidated token is fetched again", func(t *testing.T) {
		backend := &fakeBackend{token: valid}
		installFakeBackend(t, backend)
		require.Equal(t, valid, GetToken(t.Context()))

		other := makeToken(t, time.Now().Add(time.Hour))
		backend.setToken(other)
		InvalidateToken(valid)

		assert.Equal(t, other, GetToken(t.Context()))
	})

	t.Run("a refused token is not served again", func(t *testing.T) {
		// Docker Desktop keeps serving the token Docker refused: it has no way
		// of knowing, so minting is the only way out.
		backend := &fakeBackend{token: valid}
		installFakeBackend(t, backend)
		require.Equal(t, valid, GetToken(t.Context()))

		minted := makeToken(t, time.Now().Add(time.Hour))
		mintToken = func(context.Context) (string, error) { return minted, nil }
		InvalidateToken(valid)

		token, source := GetTokenWithSource(t.Context())
		assert.Equal(t, minted, token)
		assert.Equal(t, SourceMinted, source)
	})

	t.Run("a refused token is not served again when minting is unavailable", func(t *testing.T) {
		// The forced refresh polls Docker Desktop, which serves the refused
		// token until it renews its session: accepting it would send Docker a
		// token it just refused, and pin it in the cache for its whole life.
		backend := &fakeBackend{token: valid, loggedIn: true}
		installFakeBackend(t, backend) // minting unavailable: no PAT, or Hub is down
		require.Equal(t, valid, GetToken(t.Context()))

		InvalidateToken(valid)

		token, source := GetTokenWithSource(t.Context())
		assert.Empty(t, token, "a refused token must never be served again")
		assert.Equal(t, SourceNone, source)

		// Desktop eventually renews its session: the next token is served.
		fresh := makeToken(t, time.Now().Add(time.Hour))
		backend.setToken(fresh)
		assert.Equal(t, fresh, GetToken(t.Context()))
	})

	t.Run("a refused token is not reused from the last refresh result", func(t *testing.T) {
		// The refresh is rate-limited, and its result is reused while it lasts:
		// not once Docker has refused that token.
		backend := &fakeBackend{token: expired, loggedIn: true}
		backend.onRefresh = func() { backend.setToken(valid) }
		installFakeBackend(t, backend)
		require.Equal(t, valid, GetToken(t.Context()))
		require.Equal(t, 1, backend.refreshes())

		backend.setToken(expired)
		InvalidateToken(valid)

		// The last resort is the stale token Desktop still serves, never the
		// refused one.
		assert.Equal(t, expired, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes(), "still rate-limited")
	})

	t.Run("expired token triggers forced refresh", func(t *testing.T) {
		backend := &fakeBackend{token: expired}
		backend.onRefresh = func() { backend.setToken(valid) }
		installFakeBackend(t, backend)

		assert.Equal(t, valid, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("stale token returned when refresh does not help", func(t *testing.T) {
		backend := &fakeBackend{token: expired}
		installFakeBackend(t, backend)

		assert.Equal(t, expired, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("backoff prevents repeated refresh nudges", func(t *testing.T) {
		backend := &fakeBackend{token: expired}
		installFakeBackend(t, backend)

		assert.Equal(t, expired, GetToken(t.Context()))
		assert.Equal(t, expired, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("rate-limited caller reuses last refresh result", func(t *testing.T) {
		backend := &fakeBackend{token: expired}
		backend.onRefresh = func() { backend.setToken(valid) }
		installFakeBackend(t, backend)

		assert.Equal(t, valid, GetToken(t.Context()))

		// Desktop regressed to an expired token, but a new nudge is
		// rate-limited: the cached result of the last refresh is reused.
		backend.setToken(makeToken(t, time.Now().Add(-time.Minute)))
		assert.Equal(t, valid, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("concurrent callers share a single refresh", func(t *testing.T) {
		backend := &fakeBackend{token: expired}
		backend.onRefresh = func() { backend.setToken(valid) }
		installFakeBackend(t, backend)

		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				assert.Equal(t, valid, GetToken(t.Context()))
			})
		}
		wg.Wait()
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("canceled caller returns promptly with stale token", func(t *testing.T) {
		backend := &fakeBackend{token: expired}
		installFakeBackend(t, backend)
		refreshBudget = time.Second

		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()

		start := time.Now()
		assert.Equal(t, expired, GetToken(ctx))
		assert.Less(t, time.Since(start), 500*time.Millisecond)
	})

	t.Run("non-JWT token returned as-is", func(t *testing.T) {
		backend := &fakeBackend{token: "not-a-jwt"}
		installFakeBackend(t, backend)

		assert.Equal(t, "not-a-jwt", GetToken(t.Context()))
		assert.Equal(t, 0, backend.refreshes())
	})

	t.Run("empty token while signed out returns without refresh", func(t *testing.T) {
		backend := &fakeBackend{}
		installFakeBackend(t, backend)

		assert.Empty(t, GetToken(t.Context()))
		assert.Equal(t, 0, backend.refreshes())
	})

	t.Run("empty token while signed in triggers forced refresh", func(t *testing.T) {
		backend := &fakeBackend{loggedIn: true}
		backend.onRefresh = func() { backend.setToken(valid) }
		installFakeBackend(t, backend)

		assert.Equal(t, valid, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("failed token fetch while signed in triggers forced refresh", func(t *testing.T) {
		backend := &fakeBackend{loggedIn: true, failTokenFetch: true}
		backend.onRefresh = func() {
			backend.setToken(valid)
			backend.setFailTokenFetch(false)
		}
		installFakeBackend(t, backend)

		assert.Equal(t, valid, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes())
	})
}

func TestGetTokenSignedOutStillMints(t *testing.T) {
	valid := makeToken(t, time.Now().Add(time.Hour))

	// A `docker login` PAT works even when Docker Desktop is signed out or
	// not running at all.
	backend := &fakeBackend{}
	installFakeBackend(t, backend)
	mintToken = func(context.Context) (string, error) { return valid, nil }

	assert.Equal(t, valid, GetToken(t.Context()))
	assert.Equal(t, 0, backend.refreshes())
}

// TestCachedTokenIsRecheckedPeriodically covers a `docker logout` or an account while a minted token — which can be valid for hours — is cached:
// waiting for its expiry would keep the previous account's token in play.
func TestCachedTokenIsRecheckedPeriodically(t *testing.T) {
	minted := makeToken(t, time.Now().Add(4*time.Hour))

	// Docker Desktop signed out, or not running at all: the token can only
	// come from the credential store.
	installFakeBackend(t, &fakeBackend{})
	mints := 0
	mintToken = func(context.Context) (string, error) {
		mints++
		return minted, nil
	}

	require.Equal(t, minted, GetToken(t.Context()))
	require.Equal(t, minted, GetToken(t.Context()))
	require.Equal(t, 1, mints, "a fresh token is served from memory")

	expireCache()
	mintToken = func(context.Context) (string, error) {
		return "", errors.New("no Docker access token in the credential store")
	}

	assert.Empty(t, GetToken(t.Context()), "a logout must stop the previous account's token from being served")
}

// TestTokenInvalidatedDuringLookupIsNotServed covers the window between
// checking a token and caching it: another request's 401 lands in between, so
// the token must not be handed out even though it looked fine when fetched.
func TestTokenInvalidatedDuringLookupIsNotServed(t *testing.T) {
	valid := makeToken(t, time.Now().Add(time.Hour))
	installFakeBackend(t, &fakeBackend{token: valid, loggedIn: true})

	require.True(t, usable(valid))
	InvalidateToken(valid) // the gateway answered 401 to a concurrent request

	assert.False(t, remember(valid, SourceDesktop),
		"a token refused while it was being looked up must not be served")

	token, source := GetTokenWithSource(t.Context())
	assert.Empty(t, token)
	assert.Equal(t, SourceNone, source)
}

// TestEveryRefusedTokenStaysRefused covers Docker Desktop regressing to a token
// refused before the one it serves now: a single tombstone would let the older
// one back in.
func TestEveryRefusedTokenStaysRefused(t *testing.T) {
	first := makeToken(t, time.Now().Add(time.Hour))
	second := makeToken(t, time.Now().Add(time.Hour))

	backend := &fakeBackend{token: first, loggedIn: true}
	installFakeBackend(t, backend)

	require.Equal(t, first, GetToken(t.Context()))
	InvalidateToken(first)

	backend.setToken(second)
	require.Equal(t, second, GetToken(t.Context()))
	InvalidateToken(second)

	backend.setToken(first)
	assert.Empty(t, GetToken(t.Context()), "the first refused token must stay refused")
}

func TestGetUserInfo(t *testing.T) {
	t.Run("prefers what Docker Desktop reports", func(t *testing.T) {
		backend := &fakeBackend{token: makeIdentityToken(t, "claims-user", "claims@example.com")}
		backend.info = &DockerHubInfo{Username: "desktop-user", Email: "desktop@example.com"}
		installFakeBackend(t, backend)

		assert.Equal(t, DockerHubInfo{Username: "desktop-user", Email: "desktop@example.com"}, GetUserInfo(t.Context()))
	})

	t.Run("falls back to the token claims", func(t *testing.T) {
		// Docker Desktop is not around (or not signed in): the token itself
		// says who we are.
		backend := &fakeBackend{token: makeIdentityToken(t, "claims-user", "claims@example.com")}
		installFakeBackend(t, backend)

		assert.Equal(t, DockerHubInfo{Username: "claims-user", Email: "claims@example.com"}, GetUserInfo(t.Context()))
	})

	t.Run("reports nothing without a token", func(t *testing.T) {
		installFakeBackend(t, &fakeBackend{})

		assert.Equal(t, DockerHubInfo{}, GetUserInfo(t.Context()))
	})
}

// expireCache ages the cached token past its re-check window, so the next call
// consults Docker Desktop and the credential store again.
func expireCache() {
	cache.Lock()
	defer cache.Unlock()
	cache.staleAt = time.Now().Add(-time.Second)
}

// tokenSerial keeps successive tokens distinct: Docker never issues the same
// JWT twice, and a test that tells two tokens apart must not depend on the
// second they were signed in.
var tokenSerial atomic.Int64

func makeToken(t *testing.T, exp time.Time, claims ...func(jwt.MapClaims)) string {
	t.Helper()
	mapClaims := jwt.MapClaims{"exp": exp.Unix(), "jti": tokenSerial.Add(1)}
	for _, claim := range claims {
		claim(mapClaims)
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, mapClaims).SignedString([]byte("secret"))
	require.NoError(t, err)
	return token
}

// makeIdentityToken signs a token carrying an account, the way Docker's tokens
// do.
func makeIdentityToken(t *testing.T, username, email string) string {
	t.Helper()
	return makeToken(t, time.Now().Add(time.Hour), func(c jwt.MapClaims) {
		c["https://hub.docker.com"] = map[string]any{"username": username, "email": email}
	})
}

// fakeBackend emulates Docker Desktop's backend API: GET /registry/token
// serves the current token; GET /registry/is-logged-in reports session state;
// POST /registry/credstore-updated triggers onRefresh (Desktop's async
// AutoLogin).
type fakeBackend struct {
	mu             sync.Mutex
	token          string
	info           *DockerHubInfo
	loggedIn       bool
	failTokenFetch bool
	refreshCalls   int
	onRefresh      func()
}

func (b *fakeBackend) setToken(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token = token
}

func (b *fakeBackend) setFailTokenFetch(fail bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failTokenFetch = fail
}

func (b *fakeBackend) refreshes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refreshCalls
}

func (b *fakeBackend) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /registry/token", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		token, fail := b.token, b.failTokenFetch
		b.mu.Unlock()
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(token)
	})
	mux.HandleFunc("GET /registry/is-logged-in", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		loggedIn := b.loggedIn
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(loggedIn)
	})
	mux.HandleFunc("GET /registry/info", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		info := b.info
		b.mu.Unlock()
		if info == nil {
			http.Error(w, "not signed in", http.StatusNotFound)
			return
		}
		// Docker Desktop reports the username in an "id" field.
		_ = json.NewEncoder(w).Encode(map[string]string{"id": info.Username, "email": info.Email})
	})
	mux.HandleFunc("POST /registry/credstore-updated", func(http.ResponseWriter, *http.Request) {
		b.mu.Lock()
		b.refreshCalls++
		onRefresh := b.onRefresh
		b.mu.Unlock()
		if onRefresh != nil {
			onRefresh()
		}
	})
	return mux
}

func installFakeBackend(t *testing.T, backend *fakeBackend) {
	t.Helper()

	// Minting is exercised on its own in pkg/hubauth; here it must never
	// reach the developer's credential store or the real Docker Hub.
	oldMint := mintToken
	mintToken = func(context.Context) (string, error) {
		return "", errors.New("no Docker access token in the credential store")
	}
	t.Cleanup(func() { mintToken = oldMint })

	clearCache := func() {
		cache.Lock()
		defer cache.Unlock()
		cache.token, cache.source, cache.staleAt, cache.rejected = "", SourceNone, time.Time{}, nil
	}
	clearCache()
	t.Cleanup(clearCache)

	ln := newMemListener()
	server := &http.Server{Handler: backend.handler()}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = ln.Close()
	})

	oldClient := ClientBackend
	ClientBackend = newRawClient(ln.dial)
	t.Cleanup(func() { ClientBackend = oldClient })

	oldCooldown, oldBackoff, oldBudget, oldInterval := refreshCooldown, refreshFailureBackoff, refreshBudget, refreshPollInterval
	refreshCooldown = time.Hour
	refreshFailureBackoff = time.Hour
	refreshBudget = 150 * time.Millisecond
	refreshPollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		refreshCooldown, refreshFailureBackoff, refreshBudget, refreshPollInterval = oldCooldown, oldBackoff, oldBudget, oldInterval
	})

	func() {
		refreshState.Lock()
		defer refreshState.Unlock()
		refreshState.nextAttempt = time.Time{}
		refreshState.inflight = nil
		refreshState.result = ""
	}()

	// Runs first on cleanup (LIFO): a detached refresh goroutine must finish
	// before the fake backend and globals are torn down.
	t.Cleanup(func() { drainInflightRefresh(t) })
}

func drainInflightRefresh(t *testing.T) {
	t.Helper()

	refreshState.Lock()
	inflight := refreshState.inflight
	refreshState.Unlock()

	if inflight == nil {
		return
	}
	select {
	case <-inflight:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight token refresh did not finish")
	}
}

// memListener is an in-memory net.Listener fed by its dial method, so the
// RawClient can talk to a fake backend without a real socket.
type memListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newMemListener() *memListener {
	return &memListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *memListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "mem", Net: "unix"}
}

func (l *memListener) dial(context.Context) (net.Conn, error) {
	clientSide, serverSide := net.Pipe()
	select {
	case l.conns <- serverSide:
		return clientSide, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
