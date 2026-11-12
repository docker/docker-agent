package main

import (
	"go/parser"
	"go/token"
	"testing"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutsideFrozenConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"pkg/config/v0/sample.go", false},
		{"pkg/config/v15/nested/sample.go", false},
		{"/repo/pkg/config/v15/sample.go", false},
		{"pkg/config/latest/sample.go", true},
		{"pkg/config/version/sample.go", true},
		{"notpkg/config/v0/sample.go", true},
		{"pkg/config/v/sample.go", true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, tc.path, "package p\n", 0)
			require.NoError(t, err)
			assert.Equal(t, tc.want, outsideFrozenConfig(&cop.Pass{FileSet: fset, File: file}))
		})
	}
}
