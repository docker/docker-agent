package hubauth

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
)

const testToken = tokenPrefix + "pat_secret"

// fakeHub stands in for Docker Hub's token exchange endpoint: it records the
// credentials it is sent and answers with whatever the test asks for.
type fakeHub struct {
	mu     sync.Mutex
	creds  []credentials
	token  string
	status int
	header http.Header
}

type credentials struct {
	username string
	secret   string
}

func (h *fakeHub) received() []credentials {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.creds
}

// serve sets the token the fake hub answers with; an empty one makes the
// exchange fail.
func (h *fakeHub) serve(token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.token = token
}

// fail makes the fake hub answer with the given status, and optionally a
// Retry-After header.
func (h *fakeHub) fail(status int, header http.Header) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status = status
	h.header = header
}

// respondWith sets extra headers the fake hub sends with a successful answer.
func (h *fakeHub) respondWith(header http.Header) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.header = header
}

func installFakeHub(t *testing.T, token string) *fakeHub {
	t.Helper()
	resetState(t)

	hub := &fakeHub{token: token}
	loginEndpoint = newServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		hub.mu.Lock()
		hub.creds = append(hub.creds, credentials{body.Username, body.Password})
		token, status, header := hub.token, hub.status, hub.header
		hub.mu.Unlock()

		maps.Copy(w.Header(), header)
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
	})
	return hub
}

// newServer starts a test server and returns a resolver for its URL, shaped
// like the [loginEndpoint] it replaces.
func newServer(t *testing.T, handler http.HandlerFunc) func() string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return func() string { return server.URL }
}

func installSecret(t *testing.T, secret string) {
	t.Helper()
	lookupCredentials = func() (string, string, error) { return "bob", secret, nil }
}

// expireCredentialCheck ages the cached token past its credential re-check
// window, so the next call consults the credential store again.
func expireCredentialCheck() {
	state.Lock()
	defer state.Unlock()
	state.credCheckedAt = time.Now().Add(-credCheckTTL - time.Second)
}

// expireRenewal marks the cached token as due for renewal. The token shared
// with other processes ages at the same time — it is the very same token.
func expireRenewal() {
	forget()
	state.Lock()
	defer state.Unlock()
	state.renewAt = time.Now().Add(-time.Second)
}

// resetState isolates a test from the developer's machine and from its
// neighbours: fresh in-memory cache, a throw-away shared-token file, no
// inherited clock skew, and the package fakes restored on cleanup.
func resetState(t *testing.T) {
	t.Helper()

	oldEndpoint, oldLookup := loginEndpoint, lookupCredentials
	t.Cleanup(func() {
		loginEndpoint, lookupCredentials = oldEndpoint, oldLookup
	})

	paths.SetCacheDir(t.TempDir())
	t.Cleanup(func() { paths.SetCacheDir("") })

	reset := func() {
		clockSkew.Store(0)
		state.Lock()
		defer state.Unlock()
		state.token, state.renewAt, state.credHash, state.credCheckedAt = "", time.Time{}, "", time.Time{}
		state.lastErr, state.nextAttempt = nil, time.Time{}
	}
	reset()
	t.Cleanup(reset)
}

func longLived(t *testing.T) string {
	t.Helper()
	return makeToken(t, time.Now().Add(10*time.Minute))
}

// makeToken signs a token shaped like the ones Docker issues for Hub.
func makeToken(t *testing.T, exp time.Time, edits ...func(jwt.MapClaims)) string {
	t.Helper()

	claims := jwt.MapClaims{
		"exp": exp.Unix(),
		"iss": trustedIssuers[0],
		"aud": []string{expectedAudience},
		hubClaim: map[string]any{
			"username": "bob",
			"email":    "bob@example.com",
		},
	}
	for _, edit := range edits {
		edit(claims)
	}

	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("secret"))
	require.NoError(t, err)
	return token
}
