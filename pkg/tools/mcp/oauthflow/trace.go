//go:build !js

package oauthflow

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func traceCallback(handler http.Handler) http.Handler {
	return otelhttp.NewHandler(handler, "oauth.callback")
}
