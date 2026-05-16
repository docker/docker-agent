// Package wire is the (work-in-progress) HTTP transport for
// pkg/runtime. It exposes the [runtime.LoadTeamRequest] and
// [runtime.CreateSessionRequest] payloads as JSON-over-HTTP endpoints
// so a future docker-agent server process can host the same backend a
// docker-agent CLI process today drives in-process.
//
// Today the package contains only the server-side decode/encode
// scaffolding plus an in-process round-trip test. There is no client
// (a future commit adds wire.Client driving runtime.RemoteRuntime),
// no streaming endpoint (a future commit adds /v1/session/run), and
// no authentication (deliberately out of scope for the scaffold).
package wire

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/docker/docker-agent/pkg/runtime"
)

// LoadTeamResponse summarises the result of a LoadTeam call. It carries
// the bare minimum a client needs to tell the user 'the team loaded;
// here are the agents you can talk to'. The server-side LoadResult
// itself is intentionally NOT returned: it holds live toolset handles
// the client cannot use.
type LoadTeamResponse struct {
	AgentNames   []string `json:"agent_names,omitempty"`
	DefaultAgent string   `json:"default_agent,omitempty"`
}

// CreateSessionResponse identifies a freshly created session. The
// session document itself comes back later via the (yet-to-be-added)
// /v1/session/run streaming endpoint.
type CreateSessionResponse struct {
	SessionID string `json:"session_id"`
}

// Backend is the surface a wire.Server expects. It is the wire-side
// mirror of cmd/root.backend without the cleanup-closure return values
// — those are owned server-side and don't cross the wire.
type Backend interface {
	LoadTeam(ctx context.Context, req runtime.LoadTeamRequest) (LoadTeamResponse, error)
	CreateSession(ctx context.Context, req runtime.CreateSessionRequest) (CreateSessionResponse, error)
}

// maxRequestBodyBytes caps the JSON payload size each HTTP handler will
// read before failing the request. The cap exists purely to keep an
// unbounded body from being read into memory; the largest legitimate
// request today (CreateSessionRequest) is a few dozen bytes, so 1 MiB
// is generous by orders of magnitude. Bump it (or make it configurable)
// when a future endpoint legitimately needs more.
const maxRequestBodyBytes = 1 << 20 // 1 MiB

// NewHandler returns an http.Handler that decodes the JSON request
// payloads, dispatches to b, and JSON-encodes the response.
//
// Routes (subject to versioning under /v1/):
//   - POST /v1/team/load     -> Backend.LoadTeam
//   - POST /v1/session/create -> Backend.CreateSession
func NewHandler(b Backend) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/team/load", func(w http.ResponseWriter, r *http.Request) {
		var req runtime.LoadTeamRequest
		if err := decodeJSON(w, r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := b.LoadTeam(r.Context(), req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("POST /v1/session/create", func(w http.ResponseWriter, r *http.Request) {
		var req runtime.CreateSessionRequest
		if err := decodeJSON(w, r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := b.CreateSession(r.Context(), req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
	return mux
}

// decodeJSON reads a JSON request body into v. The body is wrapped in
// an [http.MaxBytesReader] so a malicious or buggy client cannot stream
// an unbounded payload and exhaust server memory. Unknown fields are
// rejected so the wire schema stays a strict contract.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("wire: encode response", "error", err)
	}
}

// writeError translates errors into status codes. ErrUnsupported maps
// to 501 so a client driving RemoteRuntime can surface a clear
// 'feature not supported by the server' message; everything else
// becomes 500.
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, runtime.ErrUnsupported) {
		status = http.StatusNotImplemented
	}
	http.Error(w, err.Error(), status)
}
