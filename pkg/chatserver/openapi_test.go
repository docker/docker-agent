package chatserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAPIEndpoint(t *testing.T) {
	srv, _ := newTestServer("root")
	r := newRouter(srv, Options{})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/openapi.json", http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	assert.Equal(t, "3.1.0", doc["openapi"], "OpenAPI version")
	paths, ok := doc["paths"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, paths, "/v1/chat/completions")
	assert.Contains(t, paths, "/v1/models")
}

func TestOpenAPIEndpoint_BypassesAuth(t *testing.T) {
	// /openapi.json must be reachable without a bearer token even when
	// --api-key is set, so introspection tooling works against locked-
	// down deployments.
	srv, _ := newTestServer("root")
	r := newRouter(srv, Options{APIKey: "secret"})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/openapi.json", http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}
