package oauthflow

import "net/http"

// otelhttp's server handler links httpsnoop's thousands of ResponseWriter
// wrappers, a megabyte of code for a callback server no browser can reach.
func traceCallback(handler http.Handler) http.Handler {
	return handler
}
