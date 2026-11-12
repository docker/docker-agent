//go:build js

package gemini

// The browser has no credential files or metadata server to discover, so a
// Vertex AI client without an explicit token source fails closed instead of
// letting the SDK probe for application default credentials.
const adcSupported = false
