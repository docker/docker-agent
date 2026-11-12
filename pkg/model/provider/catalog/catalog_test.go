package catalog

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedProviders(t *testing.T) {
	t.Parallel()

	var providers []Provider
	decoder := json.NewDecoder(bytes.NewReader(providersJSON))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(&providers))
	require.ErrorIs(t, decoder.Decode(new(any)), io.EOF)
	require.NotEmpty(t, providers)
	assert.Equal(t, Ordered(), providers)
	assert.Len(t, Providers, len(providers))

	for name, alias := range Providers {
		assert.NotEmpty(t, name)
		assert.Equal(t, strings.ToLower(name), name)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, alias, Lookup(name))
			for header, value := range alias.Headers {
				assert.NotEmpty(t, header)
				assert.Equal(t, http.CanonicalHeaderKey(header), header)
				assert.NotContains(t, value, "\r")
				assert.NotContains(t, value, "\n")
			}
			assert.Len(t, slices.Compact(slices.Clone(alias.TokenEnvVars)), len(alias.TokenEnvVars))
			for _, key := range alias.TokenEnvVars {
				assert.NotEmpty(t, key)
			}
			for key, param := range alias.QueryParams {
				assert.NotEmpty(t, key)
				assert.NotEmpty(t, param)
			}
		})
	}
}

func TestCredentialProviders(t *testing.T) {
	t.Parallel()

	providers := CredentialProviders()
	require.NotEmpty(t, providers)
	seen := make(map[string]bool)
	lastIndex := -1
	for _, p := range providers {
		index := slices.IndexFunc(Ordered(), func(entry Provider) bool { return entry.Name == p.Name })
		assert.Greater(t, index, lastIndex, "selection must follow JSON order")
		lastIndex = index
		assert.False(t, seen[p.Name], "duplicate auto provider %q", p.Name)
		seen[p.Name] = true
		assert.NotEmpty(t, Providers[p.Name].APIType, "unknown provider %q", p.Name)
		assert.NotEmpty(t, DefaultModels()[p.Name], "auto provider %q needs a default model", p.Name)
		assert.NotEmpty(t, p.TokenEnvVars)
		assert.NotEmpty(t, p.Hint)
	}
	for name, model := range DefaultModels() {
		assert.NotEmpty(t, model)
		assert.NotEmpty(t, Providers[name].APIType, "unknown default model provider %q", name)
	}
}

func TestTokenKeys(t *testing.T) {
	t.Parallel()

	for name, alias := range Providers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if len(alias.TokenEnvVars) > 0 {
				assert.Equal(t, alias.TokenEnvVars, TokenKeys(name, alias.TokenEnvVars[0]))
			}
			assert.Equal(t, []string{"CUSTOM_TOKEN"}, TokenKeys(name, "CUSTOM_TOKEN"))
			assert.Equal(t, []string{""}, TokenKeys(name, ""))
		})
	}
	assert.Equal(t, Provider{}, Lookup("unknown"))
	assert.Equal(t, []string{"CUSTOM_TOKEN"}, TokenKeys("unknown", "CUSTOM_TOKEN"))
}
