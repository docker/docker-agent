package httpsec

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBearerAuth(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		authorization string
		wantStatus    int
		wantCalled    bool
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", authorization: "Basic secret", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", authorization: "Bearer wrong", wantStatus: http.StatusUnauthorized},
		{name: "empty token", authorization: "Bearer ", wantStatus: http.StatusUnauthorized},
		{name: "matching token", authorization: "Bearer secret", wantStatus: http.StatusNoContent, wantCalled: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			called := false
			handler := BearerAuth("secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
			req.Header.Set("Authorization", tc.authorization)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, tc.wantCalled, called)
			if !tc.wantCalled {
				assert.Equal(t, "Bearer", rec.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

func TestParseOrigins(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		spec         string
		wantLiterals []string
		wantPatterns bool
		matches      map[string]bool
		wantErr      string
	}{
		{name: "literal", spec: "https://example.com", wantLiterals: []string{"https://example.com"}},
		{name: "literals with whitespace", spec: " https://example.com, http://localhost:3000 ", wantLiterals: []string{"https://example.com", "http://localhost:3000"}},
		{name: "wildcard", spec: "*", wantLiterals: []string{"*"}},
		{name: "pattern", spec: "~^https://[a-z]+\\.example\\.com$", wantPatterns: true, matches: map[string]bool{"https://app.example.com": true, "https://example.com": false}},
		{name: "literal and pattern", spec: "https://example.com,~^https://[a-z]+\\.example\\.com$", wantLiterals: []string{"https://example.com"}, wantPatterns: true},
		{name: "blank entries", spec: ",, https://example.com, ,", wantLiterals: []string{"https://example.com"}},
		{name: "empty", spec: " , ", wantErr: "no usable CORS origins"},
		{name: "invalid pattern", spec: "~[", wantErr: "invalid CORS regex"},
		{name: "missing scheme", spec: "example.com", wantErr: "scheme must be http or https"},
		{name: "unsupported scheme", spec: "ftp://example.com", wantErr: "scheme must be http or https"},
		{name: "missing host", spec: "https:", wantErr: "missing host"},
		{name: "path", spec: "https://example.com/path", wantErr: "must not include path"},
		{name: "query", spec: "https://example.com?query=value", wantErr: "must not include path"},
		{name: "fragment", spec: "https://example.com#fragment", wantErr: "must not include path"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			matcher, err := ParseOrigins(tc.spec)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantLiterals, matcher.Literals())
			assert.Equal(t, tc.wantPatterns, matcher.HasPatterns())
			for origin, want := range tc.matches {
				assert.Equal(t, want, matcher.MatchPattern(origin))
			}
		})
	}
}

func TestPackageDoesNotDependOnEcho(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-test", ".")
	output, err := cmd.Output()
	require.NoError(t, err)

	for dependency := range strings.FieldsSeq(string(output)) {
		assert.Falsef(t, dependency == "github.com/labstack/echo" || strings.HasPrefix(dependency, "github.com/labstack/echo/"), "httpsec depends on Echo package %s", dependency)
	}
}
