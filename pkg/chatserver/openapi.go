package chatserver

import (
	_ "embed"
	"net/http"

	"github.com/labstack/echo/v4"
)

// openAPISpec is the static OpenAPI 3.1 document describing the chat
// completions API. Embedding the JSON keeps the schema diffable and
// tractable to review, and means we don't pay a generation step on every
// build.
//
//go:embed openapi.json
var openAPISpec []byte

// handleOpenAPI serves the static OpenAPI document. It is exempted from
// the bearer-auth middleware (see bearerAuthMiddleware) so tooling that
// wants to introspect the API can do so without credentials.
func (s *server) handleOpenAPI(c echo.Context) error {
	return c.Blob(http.StatusOK, "application/json", openAPISpec)
}
