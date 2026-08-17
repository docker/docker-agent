package options

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// sentinelTransport is a minimal http.RoundTripper used only for identity checks.
type sentinelTransport struct{ base http.RoundTripper }

func (s *sentinelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return s.base.RoundTrip(req)
}

func TestWithHTTPTransportWrapper_SetAndGet(t *testing.T) {
	t.Parallel()
	var called bool
	wrapFn := func(base http.RoundTripper) http.RoundTripper {
		called = true
		return &sentinelTransport{base: base}
	}

	var opts ModelOptions
	WithHTTPTransportWrapper(wrapFn)(&opts)

	got := opts.TransportWrapper()
	require.NotNil(t, got)

	// Verify invoking the returned wrapper marks called=true and returns a non-nil transport.
	result := got(http.DefaultTransport)
	assert.True(t, called)
	assert.NotNil(t, result)
}

func TestTransportWrapper_NilByDefault(t *testing.T) {
	t.Parallel()
	var opts ModelOptions
	assert.Nil(t, opts.TransportWrapper())
}

func TestWithStructuredOutput_ModeHandling(t *testing.T) {
	t.Parallel()

	schema := map[string]any{"type": "object"}

	tests := []struct {
		name      string
		so        *latest.StructuredOutput
		wantNativ bool
	}{
		{name: "nil clears", so: nil, wantNativ: false},
		{name: "mode absent forwarded", so: &latest.StructuredOutput{Name: "x", Schema: schema}, wantNativ: true},
		{name: "mode native forwarded", so: &latest.StructuredOutput{Name: "x", Schema: schema, Mode: latest.StructuredOutputModeNative}, wantNativ: true},
		{name: "mode tool suppressed", so: &latest.StructuredOutput{Name: "x", Schema: schema, Mode: latest.StructuredOutputModeTool}, wantNativ: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := Apply(WithStructuredOutput(tt.so))
			if tt.wantNativ {
				require.Same(t, tt.so, opts.StructuredOutput())
			} else {
				assert.Nil(t, opts.StructuredOutput(), "providers must not receive native structured output")
			}
		})
	}
}

// TestWithStructuredOutput_ToolModeOverridesEarlier proves a later tool-mode
// opt clears a previously set native structured output, matching the
// "later opts override earlier ones" contract.
func TestWithStructuredOutput_ToolModeOverridesEarlier(t *testing.T) {
	t.Parallel()

	native := &latest.StructuredOutput{Name: "x", Schema: map[string]any{"type": "object"}}
	toolMode := &latest.StructuredOutput{Name: "x", Schema: map[string]any{"type": "object"}, Mode: latest.StructuredOutputModeTool}

	opts := Apply(WithStructuredOutput(native), WithStructuredOutput(toolMode))
	assert.Nil(t, opts.StructuredOutput())
}

func TestFromModelOptions_RoundTripsTransportWrapper(t *testing.T) {
	t.Parallel()
	var wrapperInvoked bool
	wrapFn := func(base http.RoundTripper) http.RoundTripper {
		wrapperInvoked = true
		return &sentinelTransport{base: base}
	}

	var src ModelOptions
	WithHTTPTransportWrapper(wrapFn)(&src)

	opts := FromModelOptions(src)
	require.NotEmpty(t, opts)

	var dst ModelOptions
	for _, o := range opts {
		o(&dst)
	}

	got := dst.TransportWrapper()
	require.NotNil(t, got)

	result := got(http.DefaultTransport)
	assert.True(t, wrapperInvoked)
	assert.NotNil(t, result)
}

func TestFromModelOptions_RoundTripsCompacting(t *testing.T) {
	t.Parallel()
	var src ModelOptions
	WithCompacting()(&src)
	require.True(t, src.Compacting())

	var dst ModelOptions
	for _, o := range FromModelOptions(src) {
		o(&dst)
	}
	assert.True(t, dst.Compacting())
	assert.False(t, dst.GeneratingTitle())
}

func TestFromModelOptions_NilWrapperNotIncluded(t *testing.T) {
	t.Parallel()
	// A ModelOptions with no transport wrapper should not add a
	// WithHTTPTransportWrapper opt, so TransportWrapper() stays nil.
	var src ModelOptions
	opts := FromModelOptions(src)

	var dst ModelOptions
	for _, o := range opts {
		o(&dst)
	}

	assert.Nil(t, dst.TransportWrapper())
}
