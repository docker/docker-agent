package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/hubauth"
)

type DockerHubInfo struct {
	Username string `json:"id"`
	Email    string `json:"email,omitempty"`
}

// Source says where a token came from, for diagnostics.
type Source string

const (
	SourceNone    Source = "none"
	SourceDesktop Source = "docker desktop"
	SourceMinted  Source = "minted from the stored access token"
)

// mintToken exchanges the stored access token for a fresh one. A var so tests
// never reach the real credential store or Docker Hub.
var mintToken = hubauth.Token

// cache holds the last token known to be usable. Gateway clients are rebuilt
// for every request and each one asks for a token, so without this every LLM
// call would pay a round-trip to Docker Desktop over its socket.
var cache struct {
	sync.Mutex

	token   string
	source  Source
	staleAt time.Time // time from which the token must be looked up again

	// rejected holds the tokens Docker refused, until they expire. Docker
	// Desktop keeps serving one — it has no idea it was refused, and can't be
	// told — and can regress to an earlier one, so a single tombstone would let
	// a refused token back in.
	rejected map[string]struct{}
}

// cacheTTL bounds how long a token is served from memory before Docker Desktop
// and the credential store are consulted again. Expiry alone would not do: a
// minted token can be valid for hours, and a `docker logout` or an account
// switch in between must not keep the previous account's token in play.
const cacheTTL = 30 * time.Second

// GetToken returns the user's Docker access token, or "" when there is none.
func GetToken(ctx context.Context) string {
	token, _ := GetTokenWithSource(ctx)
	return token
}

// GetTokenWithSource returns the user's Docker access token and where it came
// from. Docker Desktop's newer auth stack (auth v2) serves whatever its
// in-memory token source holds and never refreshes on GET, so a stuck
// background refresher makes it return the same expired JWT forever — or
// nothing at all when its read-time refresh failed. When that happens we mint
// a token ourselves from the access token `docker login` stored, and only then
// fall back to nudging Desktop.
func GetTokenWithSource(ctx context.Context) (string, Source) {
	if token, source, ok := cached(); ok {
		return token, source
	}

	token, err := fetchToken(ctx)
	if err == nil && usable(token) && remember(token, SourceDesktop) {
		return token, SourceDesktop
	}

	logUnusableToken(ctx, token, err)

	// Minting needs no help from Desktop and, unlike a forced refresh, is
	// deterministic: try it first. hubauth keeps its own in-memory copy and
	// re-checks the credential store, so cacheTTL bounds how long a minted
	// token is served without it being asked again.
	minted, mintErr := mintToken(ctx)
	if mintErr == nil && usable(minted) && remember(minted, SourceMinted) {
		return minted, SourceMinted
	}
	slog.DebugContext(ctx, "Could not mint a Docker token from the credential store", "error", mintErr)

	// Signed out: a forced refresh can't help and would delay every caller.
	if token == "" && !isLoggedIn(ctx) {
		return "", SourceNone
	}

	if fresh := forceTokenRefresh(ctx); fresh != "" && remember(fresh, SourceDesktop) {
		slog.InfoContext(ctx, "Recovered a fresh token from Docker Desktop",
			"fingerprint", tokenFingerprint(fresh))
		return fresh, SourceDesktop
	}
	if token == "" || wasRejected(token) {
		slog.WarnContext(ctx, "Token refresh failed, no token available")
		return "", SourceNone
	}
	slog.WarnContext(ctx, "Token refresh failed, sending a token that expired or is about to",
		"fingerprint", tokenFingerprint(token),
		"expires_in", expiresIn(token))
	return token, SourceDesktop
}

// InvalidateToken forgets token, everywhere it may be cached, so the next
// [GetToken] fetches or mints a new one. Called when Docker rejects a token we
// believed to be valid: only the issuer knows for sure.
func InvalidateToken(token string) {
	if token == "" {
		return
	}

	cache.Lock()
	if cache.token == token {
		cache.token, cache.source = "", SourceNone
	}
	if cache.rejected == nil {
		cache.rejected = make(map[string]struct{})
	}
	// An expired token is refused by everyone anyway, and never served from
	// here: forget it rather than grow the set for the life of the process.
	for known := range cache.rejected {
		if hubauth.Expiring(known) {
			delete(cache.rejected, known)
		}
	}
	cache.rejected[token] = struct{}{}
	cache.Unlock()

	hubauth.Invalidate(token)
}

func cached() (string, Source, bool) {
	cache.Lock()
	defer cache.Unlock()

	if cache.token == "" || isRejected(cache.token) {
		return "", SourceNone, false
	}
	if !time.Now().Before(cache.staleAt) || hubauth.Expiring(cache.token) {
		return "", SourceNone, false
	}
	return cache.token, cache.source, true
}

// usable reports whether a token is worth handing to callers: one that dies
// mid-request is no better than none, and one Docker already refused is worse.
func usable(token string) bool {
	return token != "" && !hubauth.Expiring(token) && !wasRejected(token)
}

func wasRejected(token string) bool {
	cache.Lock()
	defer cache.Unlock()

	return isRejected(token)
}

// isRejected must be called with the cache lock held.
func isRejected(token string) bool {
	_, refused := cache.rejected[token]
	return refused
}

// remember caches a token, reporting whether it may be served: the rejection
// check and the write are a single operation, so a token invalidated while it
// was being looked up is never handed out.
func remember(token string, source Source) bool {
	cache.Lock()
	defer cache.Unlock()

	if token == "" || isRejected(token) {
		return false
	}
	cache.token, cache.source, cache.staleAt = token, source, time.Now().Add(cacheTTL)
	return true
}

// logUnusableToken records why Docker Desktop's token can't be used as-is,
// so gateway auth failures can be attributed from logs.
func logUnusableToken(ctx context.Context, token string, err error) {
	switch {
	case err != nil:
		slog.WarnContext(ctx, "Failed to fetch a token from Docker Desktop", "error", err)
	case token == "":
		slog.WarnContext(ctx, "Docker Desktop served an empty token")
	case wasRejected(token):
		slog.WarnContext(ctx, "Docker Desktop served a token Docker refused",
			"fingerprint", tokenFingerprint(token))
	default:
		attrs := []any{"fingerprint", tokenFingerprint(token), "expires_in", expiresIn(token)}
		if exp, ok := hubauth.Expiry(token); ok {
			attrs = append(attrs, "expires_at", exp.UTC().Format(time.RFC3339))
		}
		slog.WarnContext(ctx, "Docker Desktop served a token that expired or is about to", attrs...)
	}
}

// tokenFingerprint returns a short non-reversible identifier, safe to log.
func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:4])
}

// expiresIn returns how long the token has left, negative once its exp claim
// has passed.
func expiresIn(token string) string {
	exp, ok := hubauth.Expiry(token)
	if !ok {
		return "unknown"
	}
	return time.Until(exp).Round(time.Second).String()
}

// GetUserInfo returns the signed-in account. Docker Desktop knows it best, but
// it is not always around: the token itself carries the same information.
func GetUserInfo(ctx context.Context) DockerHubInfo {
	var info DockerHubInfo
	_ = ClientBackend.Get(ctx, "/registry/info", &info)
	if info.Username != "" {
		return info
	}

	identity, ok := hubauth.IdentityFromToken(GetToken(ctx))
	if !ok {
		return info
	}
	return DockerHubInfo{Username: identity.Username, Email: identity.Email}
}

func fetchToken(ctx context.Context) (string, error) {
	var token string
	err := ClientBackend.Get(ctx, "/registry/token", &token)
	return token, err
}

func isLoggedIn(ctx context.Context) bool {
	var loggedIn bool
	if err := ClientBackend.Get(ctx, "/registry/is-logged-in", &loggedIn); err != nil {
		return false
	}
	return loggedIn
}

var refreshState struct {
	sync.Mutex

	nextAttempt time.Time     // earliest time a new refresh may start
	inflight    chan struct{} // closed when the in-flight refresh completes
	result      string        // token produced by the last refresh, "" if none
}

// Vars, not consts, so tests can shorten them.
var (
	refreshCooldown       = 30 * time.Second
	refreshFailureBackoff = 2 * time.Minute
	refreshBudget         = 10 * time.Second
	refreshPollInterval   = 500 * time.Millisecond
)

// forceTokenRefresh nudges Docker Desktop to reload its session from the OS
// credential store and refresh it — the same hook the Docker CLI triggers
// after `docker login`. Desktop handles it asynchronously, so we poll until a
// non-expired token shows up. Returns "" if no fresh token was obtained.
//
// Refreshes are singleflighted: one goroutine talks to Desktop while
// concurrent callers wait for its result (or bail out when their own context
// is canceled). Attempts are rate-limited so a persistently stale Desktop
// doesn't stall every request.
func forceTokenRefresh(ctx context.Context) string {
	refreshState.Lock()

	if inflight := refreshState.inflight; inflight != nil {
		refreshState.Unlock()
		return awaitRefresh(ctx, inflight)
	}

	if time.Now().Before(refreshState.nextAttempt) {
		// Rate-limited, but a refresh may have just completed: reuse its
		// result if still valid.
		token := refreshState.result
		refreshState.Unlock()
		if usable(token) {
			return token
		}
		return ""
	}

	done := make(chan struct{})
	refreshState.inflight = done
	refreshState.Unlock()

	go func() {
		// Detached from the caller: the refresh benefits all requests, so a
		// single canceled request must not abort it. refreshBudget bounds the
		// whole attempt (POST + polling).
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshBudget)
		defer cancel()

		token := runTokenRefresh(ctx)

		refreshState.Lock()
		defer refreshState.Unlock()
		refreshState.result = token
		backoff := refreshCooldown
		if token == "" {
			backoff = refreshFailureBackoff
		}
		refreshState.nextAttempt = time.Now().Add(backoff)
		refreshState.inflight = nil
		close(done)
	}()

	return awaitRefresh(ctx, done)
}

func awaitRefresh(ctx context.Context, done <-chan struct{}) string {
	select {
	case <-done:
		refreshState.Lock()
		defer refreshState.Unlock()
		return refreshState.result
	case <-ctx.Done():
		return ""
	}
}

func runTokenRefresh(ctx context.Context) string {
	slog.WarnContext(ctx, "Forcing a Docker Desktop token refresh")
	if err := postRefreshNudge(ctx); err != nil {
		slog.WarnContext(ctx, "Failed to trigger Docker Desktop token refresh", "error", err)
		return ""
	}

	ticker := time.NewTicker(refreshPollInterval)
	defer ticker.Stop()

	for {
		// Check right away: Desktop may have refreshed synchronously. A token
		// Docker refused doesn't count: Desktop serves it until it renews its
		// session, and accepting it here would cache it for its whole life.
		if token, err := fetchToken(ctx); err == nil && usable(token) {
			return token
		}
		select {
		case <-ctx.Done():
			slog.WarnContext(ctx, "Docker Desktop did not deliver a fresh token in time")
			return ""
		case <-ticker.C:
		}
	}
}

// postRefreshNudge caps the POST to a slice of the refresh budget so a slow
// Desktop can't consume it all and starve the polling loop that follows.
func postRefreshNudge(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, refreshBudget/3)
	defer cancel()
	return ClientBackend.Post(ctx, "/registry/credstore-updated")
}
