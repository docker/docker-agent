package mcpcatalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

//go:embed servers.json
var serversJSON []byte

// Catalog mirrors the on-disk JSON file. It exposes the curated subset of
// the Docker MCP Catalog that consists exclusively of remote servers
// reachable over the streamable-http transport — the format docker-agent
// can talk to directly without a local subprocess or the MCP gateway.
type Catalog struct {
	Source        string   `json:"source"`
	SourceURL     string   `json:"source_url"`
	SchemaVersion int      `json:"schema_version"`
	Filter        string   `json:"filter"`
	Count         int      `json:"count"`
	Servers       []Server `json:"servers"`
}

// Server is a curated, on-demand-activatable remote MCP server description.
//
// The catalog only carries servers reachable over streamable-http with an
// auth model docker-agent can drive itself: either no credentials at all
// ("none") or an OAuth flow ("oauth") handled by the underlying mcp.Toolset.
type Server struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	URL         string   `json:"url"`
	Transport   string   `json:"transport"`
	Category    string   `json:"category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Icon        string   `json:"icon,omitempty"`
	Readme      string   `json:"readme,omitempty"`
	Auth        Auth     `json:"auth"`

	allowPrivateIPs bool
}

// Auth describes how to authenticate against a remote MCP server.
//
// Type is one of:
//   - "none"  — no credentials required
//   - "oauth" — OAuth flow handled by the underlying mcp.Toolset
type Auth struct {
	Type      string          `json:"type"`
	Providers []OAuthProvider `json:"providers,omitempty"`
}

type OAuthProvider struct {
	Provider string `json:"provider"`
	Env      string `json:"env"`
	Secret   string `json:"secret"`
}

// loadOnce caches the parsed catalog. The embedded JSON is immutable for
// the lifetime of the binary, so we decode it exactly once and hand out
// shallow copies to callers that intend to mutate it (tests do).
var loadOnce = sync.OnceValues(func() (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(serversJSON, &c); err != nil {
		return nil, fmt.Errorf("decoding embedded MCP catalog: %w", err)
	}
	return &c, nil
})

// Load returns the embedded catalog. The first call decodes the JSON;
// subsequent calls return a shallow copy so callers can append synthetic
// servers (notably in tests) without affecting later callers.
func Load() (*Catalog, error) {
	cached, err := loadOnce()
	if err != nil {
		return nil, err
	}
	// Shallow copy with a fresh Servers slice so test-only appends don't
	// leak across Load() calls. Server values themselves are immutable.
	cloned := *cached
	cloned.Servers = append([]Server(nil), cached.Servers...)
	return &cloned, nil
}

// MustLoad is like Load but panics on error. Intended for package init.
func MustLoad() *Catalog {
	c, err := Load()
	if err != nil {
		panic(err)
	}
	return c
}
