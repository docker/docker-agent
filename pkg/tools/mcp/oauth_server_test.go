package mcp

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestCallbackServer_Port(t *testing.T) {
	cs, err := NewCallbackServer()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Shutdown(t.Context()) }()

	port := cs.Port()
	if port <= 0 || port > 65535 {
		t.Fatalf("Port() = %d, want a valid TCP port", port)
	}

	// Port() should agree with what's embedded in GetRedirectURI().
	if !strings.Contains(cs.GetRedirectURI(), ":"+strconv.Itoa(port)+"/callback") {
		t.Errorf("GetRedirectURI() = %q does not contain port %d", cs.GetRedirectURI(), port)
	}
}

// TestBuildRedirectURI tests the pure string-substitution logic without
// needing to open a listener.
func TestBuildRedirectURI(t *testing.T) {
	const fallback = "http://127.0.0.1:12345/callback"
	const port = 54321

	tests := []struct {
		name     string
		override string
		want     string
	}{
		{
			name:     "empty override falls back",
			override: "",
			want:     fallback,
		},
		{
			name:     "no placeholder returns override verbatim",
			override: "https://oauth.example.com/callback",
			want:     "https://oauth.example.com/callback",
		},
		{
			name:     "single placeholder is substituted",
			override: "https://oauth.example.com/redirect?port=${callbackPort}",
			want:     fmt.Sprintf("https://oauth.example.com/redirect?port=%d", port),
		},
		{
			name:     "multiple placeholders all substituted",
			override: "https://host:${callbackPort}/x/${callbackPort}",
			want:     fmt.Sprintf("https://host:%d/x/%d", port, port),
		},
		{
			name:     "unrelated dollar sequences are left alone",
			override: "https://x.example/cb?s=$other&p=${callbackPort}",
			want:     fmt.Sprintf("https://x.example/cb?s=$other&p=%d", port),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRedirectURI(tt.override, fallback, port)
			if got != tt.want {
				t.Errorf("buildRedirectURI(%q, %q, %d) = %q, want %q", tt.override, fallback, port, got, tt.want)
			}
		})
	}
}

// TestCallbackServer_ResolveRedirectURI exercises the method wrapper end-to-end
// to make sure it stitches GetRedirectURI() and Port() together correctly.
func TestCallbackServer_ResolveRedirectURI(t *testing.T) {
	cs, err := NewCallbackServer()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Shutdown(t.Context()) }()

	if got := cs.resolveRedirectURI(""); got != cs.GetRedirectURI() {
		t.Errorf("resolveRedirectURI(\"\") = %q, want %q", got, cs.GetRedirectURI())
	}

	want := fmt.Sprintf("https://host.example/cb?port=%d", cs.Port())
	if got := cs.resolveRedirectURI("https://host.example/cb?port=${callbackPort}"); got != want {
		t.Errorf("resolveRedirectURI with placeholder = %q, want %q", got, want)
	}
}
