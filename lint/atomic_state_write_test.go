package main

import (
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
)

func TestAtomicStateWriteFlagsMarshalledData(t *testing.T) {
	t.Parallel()
	src := `package mypackage
import (
	"encoding/json"
	"os"
)
func save(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
`
	offenses := coptest.Run(t, AtomicStateWrite, src)
	assert.Len(t, offenses, 1)
	assert.Equal(t, "Lint/AtomicStateWrite", offenses[0].CopName)
}

func TestAtomicStateWriteFlagsMarshalDirectly(t *testing.T) {
	t.Parallel()
	src := `package mypackage
import (
	"encoding/json"
	"os"
)
func save(path string, v any) error {
	data, _ := json.Marshal(v)
	return os.WriteFile(path, data, 0o600)
}
`
	assert.Len(t, coptest.Run(t, AtomicStateWrite, src), 1)
}

func TestAtomicStateWriteAllowsNonMarshalledData(t *testing.T) {
	t.Parallel()
	src := `package mypackage
import "os"
func save(path string) error {
	return os.WriteFile(path, []byte("hello\n"), 0o600)
}
`
	assert.Empty(t, coptest.Run(t, AtomicStateWrite, src))
}

func TestAtomicStateWriteAllowsTests(t *testing.T) {
	t.Parallel()
	src := `package mypackage
import (
	"encoding/json"
	"os"
)
func TestSave(t *testing.T) {
	data, _ := json.Marshal(map[string]int{"a": 1})
	os.WriteFile("/tmp/x", data, 0o600)
}
`
	assert.Empty(t, coptest.RunNamed(t, AtomicStateWrite, "pkg/foo/foo_test.go", src))
}

func TestAtomicStateWriteAllowsAtomicfilePackage(t *testing.T) {
	t.Parallel()
	src := `package atomicfile
import (
	"encoding/json"
	"os"
)
func internalSave(path string, v any) error {
	data, _ := json.Marshal(v)
	return os.WriteFile(path, data, 0o600)
}
`
	assert.Empty(t, coptest.RunNamed(t, AtomicStateWrite, "pkg/atomicfile/impl.go", src))
}
