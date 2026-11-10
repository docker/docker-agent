package options

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
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

func TestWithDisableAutoPull_RoundTrips(t *testing.T) {
	t.Parallel()

	src := Apply(WithDisableAutoPull())
	require.True(t, src.DisableAutoPull())

	dst := Apply(FromModelOptions(src)...)
	assert.True(t, dst.DisableAutoPull())
}

func TestWrapTransportUsesDefaultTransportWhenClientTransportNil(t *testing.T) {
	t.Parallel()

	var base http.RoundTripper
	opts := Apply(WithHTTPTransportWrapper(func(got http.RoundTripper) http.RoundTripper {
		base = got
		return &sentinelTransport{base: got}
	}))
	client := &http.Client{}

	opts.WrapTransport(t.Context(), client)

	assert.Equal(t, http.DefaultTransport, base)
	assert.IsType(t, &sentinelTransport{}, client.Transport)
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

func TestFromModelOptions_RoundTripsTokenSource(t *testing.T) {
	t.Parallel()

	source := TokenSource(func(context.Context) (string, error) { return "token", nil })
	var src ModelOptions
	WithTokenSource(source)(&src)

	var dst ModelOptions
	for _, opt := range FromModelOptions(src) {
		opt(&dst)
	}

	require.NotNil(t, dst.TokenSource())
	token, err := dst.TokenSource()(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "token", token)
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

func TestModelOptionsClonePreservesServicesAndExplicitClears(t *testing.T) {
	t.Parallel()
	store := modelsdev.NewDatabaseStore(&modelsdev.Database{})
	wrapper := func(rt http.RoundTripper) http.RoundTripper { return rt }
	source := func(context.Context) (string, error) { return "token", nil }
	m := Apply(WithModelsDevStore(store), WithHTTPTransportWrapper(wrapper), WithTokenSource(source), WithGateway("old"), WithGateway(""), WithStructuredOutput(nil))
	clone, err := m.Clone()
	require.NoError(t, err)
	probe := Apply(FromModelOptions(clone)...)
	assert.Same(t, store, probe.ModelsDevStore())
	assert.Equal(t, reflect.ValueOf(wrapper).Pointer(), reflect.ValueOf(probe.TransportWrapper()).Pointer())
	assert.Equal(t, reflect.ValueOf(source).Pointer(), reflect.ValueOf(probe.TokenSource()).Pointer())
	assert.Empty(t, probe.Gateway())
	assert.True(t, probe.gatewaySet)
	assert.True(t, probe.structuredOutputSet)
	token, err := probe.TokenSource()(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "token", token)
}

func TestModelOptionsClonePreservesNumericTypesAndIsolation(t *testing.T) {
	t.Parallel()
	values := map[string]any{"minimum": int64(9007199254740993), "maximum": uint64(18446744073709551615), "small": int8(-7), "fraction": float32(0.1), "nested": []any{map[string]any{"limit": int64(9007199254740993)}}}
	m := Apply(WithStructuredOutput(&latest.StructuredOutput{Schema: values}), WithProviders(map[string]latest.ProviderConfig{"custom": {ProviderOpts: values}}))
	for range 3 {
		clone, err := m.Clone()
		require.NoError(t, err)
		require.Equal(t, values, clone.StructuredOutput().Schema)
		require.Equal(t, values, clone.Providers()["custom"].ProviderOpts)
		for key, value := range values {
			assert.IsType(t, value, clone.StructuredOutput().Schema[key])
			assert.IsType(t, value, clone.Providers()["custom"].ProviderOpts[key])
		}
		m = Apply(FromModelOptions(clone)...)
	}
	m.StructuredOutput().Schema["nested"].([]any)[0].(map[string]any)["limit"] = int64(0)
	assert.Equal(t, int64(9007199254740993), values["nested"].([]any)[0].(map[string]any)["limit"])
	assert.Equal(t, int64(9007199254740993), m.Providers()["custom"].ProviderOpts["nested"].([]any)[0].(map[string]any)["limit"])
}

func TestMaxTokensPropagationAndRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		opts        []Opt
		want        int64
		inheritable bool
	}{
		{"ordinary", []Opt{WithMaxTokens(64)}, 64, false},
		{"auxiliary", []Opt{WithAuxiliaryMaxTokens(128)}, 128, true},
		{"ordinary last", []Opt{WithAuxiliaryMaxTokens(128), WithMaxTokens(64)}, 64, false},
		{"auxiliary last", []Opt{WithMaxTokens(64), WithAuxiliaryMaxTokens(128)}, 128, true},
		{"ordinary zero unchanged", []Opt{WithMaxTokens(64), WithMaxTokens(0)}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := Apply(tc.opts...)
			cloned, err := original.Clone()
			require.NoError(t, err)
			replayed := FromModelOptions(cloned)
			if tc.want != 0 {
				require.Len(t, replayed, 1)
			} else {
				require.Empty(t, replayed)
			}
			roundTrip := Apply(replayed...)
			for _, m := range []ModelOptions{original, cloned, roundTrip} {
				assert.Equal(t, tc.want, m.MaxTokens())
				assert.Equal(t, tc.inheritable, m.MaxTokensInheritable())
			}
		})
	}
}
