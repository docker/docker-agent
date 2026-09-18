//go:build !js

package gemini

// adcSupported reports whether the Vertex AI backend may fall back to the
// SDK's application default credentials when no token source is given.
const adcSupported = true
