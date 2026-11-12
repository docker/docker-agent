package main

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopRegistrations(t *testing.T) {
	t.Parallel()
	require.Len(t, cops, 30)
	require.Len(t, programCops, 36)
	counts := make(map[string]int)
	for _, c := range cops {
		counts[c.Name()]++
	}
	for _, c := range programCops {
		counts[c.Name()]++
	}
	assert.Len(t, counts, 66)
	for _, name := range []string{"Lint/ContextFirstParameter", "Lint/NoContextField", "Lint/SplitGoroutineLifetime"} {
		assert.NotContains(t, counts, name)
	}
	for name, count := range counts {
		assert.Equal(t, 1, count, name)
	}
	for _, name := range []string{
		"HTTPRequestWithContext", "NoFatalOutsideMain",
		"SlogContextual", "ConstructorPurity", "ConstructorNetworkIO", "WrapErrors",
		"ErrorStringMatching", "DeferMutexUnlock", "NewExpr", "NoStdoutInLibraries", "ConstructorCommandExec",
	} {
		registeredCop(t, cops, "Lint/"+name)
	}
	for _, name := range []string{
		"PointerHelper", "ReflectFields", "StdlibUUID", "URLClone", "JSONMarshalWrite",
		"BenchmarkLoop", "SplitTrimJoin", "FieldsSeq", "StreamCloseSafety", "SlicesClone",
		"CutPrefix", "CutSuffix", "FieldsSeqLookup", "SortStableFunc",
		"ErrorsAsType", "MapsCopy", "MapsClone", "SlicesContains", "SlicesEqual",
		"SplitSeq", "SortedMapKeys", "WaitGroupGo", "HTTPTestRequestWithContext",
	} {
		registeredCop(t, programCops, "Lint/"+name)
	}
}

func registeredCop[T interface{ Name() string }](t *testing.T, registered []T, name string) T {
	t.Helper()
	i := slices.IndexFunc(registered, func(c T) bool { return c.Name() == name })
	require.NotEqual(t, -1, i, "missing registered cop %s", name)
	return registered[i]
}
