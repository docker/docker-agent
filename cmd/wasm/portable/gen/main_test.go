package main

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCatalogMatchesGo(t *testing.T) {
	var got bytes.Buffer
	require.NoError(t, generate(t.Context(), &got))
	want, err := os.ReadFile("../src/tool_catalog.json")
	require.NoError(t, err)
	require.Equal(t, string(want), got.String(), "regenerate with task generate-wasm-portable-tools")
}
