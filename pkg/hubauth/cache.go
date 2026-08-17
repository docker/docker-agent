package hubauth

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/paths"
)

// Minted tokens are shared between docker-agent processes through a file in
// the cache directory: a `docker agent` invocation, the MCP server it spawns
// and a sandbox helper all authenticate as the same user, and re-exchanging the
// PAT in each of them costs a credential-helper exec plus a round-trip to Hub.
//
// The file holds a bearer token, so it is owner-only inside a directory of its
// own, also owner-only, and it is tied to a fingerprint of the credentials that
// minted it: after a `docker logout` or an account switch, the entry is simply
// ignored. Every failure here is non-fatal — the token is re-minted instead.
//
// File modes are POSIX-only: on Windows the token is left to the ACLs it
// inherits from the user's profile, like every other secret docker-agent
// caches (see the atomicfile package).

type cacheEntry struct {
	Credentials string `json:"credentials"`
	Token       string `json:"token"`
}

// cachePath keeps the token in a subdirectory of this package's own rather
// than in the cache root: MkdirAll applies its mode only to the directories it
// creates, and the shared cache root usually already exists, world-readable.
func cachePath() string {
	return filepath.Join(paths.GetCacheDir(), "hubauth", "hub-token.json")
}

// load returns a token minted from the given credentials by this or another
// process, when one is cached and not due for renewal.
func load(credHash string) (string, bool) {
	data, err := os.ReadFile(cachePath())
	if err != nil {
		return "", false
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return "", false
	}
	if entry.Credentials != credHash || entry.Token == "" {
		return "", false
	}
	// The same checks a fresh exchange goes through: a file that somehow holds
	// a token from another issuer, for another audience, or one due for
	// renewal, is no more trustworthy than a response off the wire.
	if err := validate(entry.Token); err != nil {
		slog.Debug("Ignoring the cached Docker token", "error", err)
		return "", false
	}
	return entry.Token, true
}

// store publishes a minted token for other processes to reuse.
func store(credHash, token string) {
	data, err := json.Marshal(cacheEntry{Credentials: credHash, Token: token})
	if err != nil {
		return
	}

	path := cachePath()
	// 0700 on the directory keeps the token unreadable during the window
	// between atomicfile's rename and its chmod.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Debug("Could not create the Docker token cache directory", "error", err)
		return
	}
	if err := atomicfile.Write(path, bytes.NewReader(data), 0o600); err != nil {
		slog.Debug("Could not cache the Docker token", "error", err)
	}
}

// forget removes the shared token, so no process keeps using one that this one
// found to be unusable.
func forget() {
	if err := os.Remove(cachePath()); err != nil && !os.IsNotExist(err) {
		slog.Debug("Could not remove the cached Docker token", "error", err)
	}
}
