package httpsec

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// LoopbackHost rejects DNS-rebinding hosts on unauthenticated TCP listeners.
// Local sockets should not use it: their clients may use synthetic hostnames.
func LoopbackHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := url.Parse("//" + r.Host)
		if err != nil || u.Host != r.Host || u.User != nil || !isLoopbackHost(u.Hostname()) {
			http.Error(w, "invalid Host header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}

// ValidateListenerAuth requires authentication when a TCP listener is not local.
// Checking the bound address also covers inherited file-descriptor listeners.
func ValidateListenerAuth(addr net.Addr, token string) error {
	if tcpAddr, ok := addr.(*net.TCPAddr); ok && !tcpAddr.IP.IsLoopback() && token == "" {
		return errors.New("non-loopback TCP listeners require --auth-token; use a loopback address or local socket for unauthenticated access")
	}
	return nil
}
