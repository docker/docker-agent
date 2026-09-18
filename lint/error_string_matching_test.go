package main

import (
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrorStringMatchingFlagsContains(t *testing.T) {
	t.Parallel()
	src := `package p
import "strings"
func f(err error) bool { return strings.Contains(err.Error(), "queue full") }
`
	offenses := coptest.RunTyped(t, ErrorStringMatching, src)
	require.Len(t, offenses, 1)
	assert.Equal(t, "Lint/ErrorStringMatching", offenses[0].CopName)
	assert.Contains(t, offenses[0].Message, "errors.Is/errors.As")
}

func TestErrorStringMatchingFlagsPrefixAndSuffix(t *testing.T) {
	t.Parallel()
	for _, function := range []string{"HasPrefix", "HasSuffix"} {
		src := `package p
import "strings"
func f(err error) bool { return strings.` + function + `(err.Error(), "failure") }
`
		require.Len(t, coptest.RunTyped(t, ErrorStringMatching, src), 1, function)
	}
}

func TestErrorStringMatchingFlagsConcreteErrorType(t *testing.T) {
	t.Parallel()
	src := `package p
import "strings"
type customError struct{}
func (*customError) Error() string { return "failure" }
func f(err *customError) bool { return strings.Contains(err.Error(), "failure") }
`
	require.Len(t, coptest.RunTyped(t, ErrorStringMatching, src), 1)
}

func TestErrorStringMatchingAllowsOrdinaryErrorRendering(t *testing.T) {
	t.Parallel()
	src := `package p
func f(err error) string { return err.Error() }
`
	assert.Empty(t, coptest.RunTyped(t, ErrorStringMatching, src))
}

func TestErrorStringMatchingAllowsNonErrorErrorMethod(t *testing.T) {
	t.Parallel()
	src := `package p
import "strings"
type response struct{}
func (response) Error(int) string { return "failure" }
func f(r response) bool { return strings.Contains(r.Error(1), "failure") }
`
	assert.Empty(t, coptest.RunTyped(t, ErrorStringMatching, src))
}

func TestErrorStringMatchingAllowsStrings(t *testing.T) {
	t.Parallel()
	src := `package p
import "strings"
func f(message string) bool { return strings.Contains(message, "failure") }
`
	assert.Empty(t, coptest.RunTyped(t, ErrorStringMatching, src))
}
