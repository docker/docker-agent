package server

import (
	"net/http"
	"testing"
)

// Keep parallel requests independent of httptest.Server.Close's global pool cleanup.
func newTestHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}
