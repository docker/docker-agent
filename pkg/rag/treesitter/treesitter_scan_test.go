//go:build cgo

package treesitter

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScanPackageNameLongLine(t *testing.T) {
	t.Parallel()

	// A single line over bufio.Scanner's 64 KiB token limit must not hide a
	// package clause further down the file.
	content := []byte(strings.Repeat("x", 70_000) + "\npackage main\n")

	assert.Equal(t, "main", scanPackageName(content))
}

func TestScanPackageNameAcrossBufferBoundary(t *testing.T) {
	t.Parallel()

	// A bufio.Reader returns 4096-byte fragments, so both of these lines are
	// split by the reader buffer while staying below the old 64 KiB limit.
	t.Run("leading whitespace longer than the reader buffer", func(t *testing.T) {
		t.Parallel()

		content := []byte(strings.Repeat(" ", 5_000) + "package main\nfunc F() {}\n")

		assert.Equal(t, "main", scanPackageName(content))
	})

	t.Run("identifier longer than the reader buffer", func(t *testing.T) {
		t.Parallel()

		name := strings.Repeat("x", 5_000)
		content := []byte("package " + name + "\n")

		assert.Equal(t, name, scanPackageName(content))
	})
}
