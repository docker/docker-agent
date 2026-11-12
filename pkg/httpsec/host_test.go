package httpsec

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoopbackHost(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		host string
		want int
	}{
		{"localhost", http.StatusNoContent},
		{"LOCALHOST:8080", http.StatusNoContent},
		{"127.0.0.1:8080", http.StatusNoContent},
		{"127.0.0.2", http.StatusNoContent},
		{"[::1]:8080", http.StatusNoContent},
		{"[::1]", http.StatusNoContent},
		{"[::ffff:127.0.0.1]:8080", http.StatusNoContent},
		{"attacker.example:8080", http.StatusForbidden},
		{"localhost.attacker.example", http.StatusForbidden},
		{"192.168.1.1:8080", http.StatusForbidden},
		{"0.0.0.0:8080", http.StatusForbidden},
		{"[::]:8080", http.StatusForbidden},
		{"_", http.StatusForbidden},
		{"agent", http.StatusForbidden},
		{"", http.StatusForbidden},
		{"localhost@attacker.example", http.StatusForbidden},
		{"attacker.example@localhost", http.StatusForbidden},
		{"localhost/path", http.StatusForbidden},
		{"localhost?query", http.StatusForbidden},
		{"localhost#fragment", http.StatusForbidden},
		{"localhost:bad-port", http.StatusForbidden},
	} {
		t.Run(tc.host, func(t *testing.T) {
			t.Parallel()
			called := false
			handler := LoopbackHost(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
			req.Host = tc.host
			req.Header.Set("Origin", "http://"+tc.host)
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			req.Header.Set("X-Forwarded-Host", "localhost")
			req.Header.Set("Forwarded", "host=localhost")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
			assert.Equal(t, tc.want == http.StatusNoContent, called)
		})
	}
}

func TestValidateListenerAuth(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		addr net.Addr
	}{
		{"IPv4 loopback", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}},
		{"IPv6 loopback", &net.TCPAddr{IP: net.IPv6loopback}},
		{"IPv4 wildcard", &net.TCPAddr{IP: net.IPv4zero}},
		{"IPv6 wildcard", &net.TCPAddr{IP: net.IPv6zero}},
		{"unspecified", &net.TCPAddr{}},
		{"private", &net.TCPAddr{IP: net.ParseIP("192.168.1.1")}},
		{"public", &net.TCPAddr{IP: net.ParseIP("203.0.113.1")}},
		{"unix", &net.UnixAddr{Name: "/tmp/agent.sock", Net: "unix"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateListenerAuth(tc.addr, "")
			if addr, tcp := tc.addr.(*net.TCPAddr); tcp && !addr.IP.IsLoopback() {
				require.ErrorContains(t, err, "require --auth-token")
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, ValidateListenerAuth(tc.addr, "secret"))
		})
	}
}
