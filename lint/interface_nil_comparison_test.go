package main

import (
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInterfaceNilComparisonReportsInterfaceNilChecks(t *testing.T) {
	offenses := coptest.RunTyped(t, InterfaceNilComparison, `package sample

type Worker interface { Work() }

func f(w Worker) {
	if w == nil {
	}
	if nil != w {
	}
}
`)

	require.Len(t, offenses, 2)
	assert.Equal(t, "Lint/InterfaceNilComparison", offenses[0].CopName)
	assert.Contains(t, offenses[0].Message, "reflectx.IsNil")
}

func TestInterfaceNilComparisonIgnoresErrorAnyAndConcreteTypes(t *testing.T) {
	offenses := coptest.RunTyped(t, InterfaceNilComparison, `package sample

func f(err error, value any, ptr *int) {
	if err == nil {
	}
	if value == nil {
	}
	if ptr == nil {
	}
}
`)

	assert.Empty(t, offenses)
}
