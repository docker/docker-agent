package reflectx

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

type testReader struct{}

func (*testReader) Read([]byte) (int, error) { return 0, io.EOF }

func TestIsNil(t *testing.T) {
	var ptr *testReader
	var reader io.Reader = ptr
	var values map[string]string
	var items []string
	var fn func()

	assert.True(t, IsNil(nil))
	assert.True(t, IsNil(ptr))
	assert.True(t, IsNil(reader))
	assert.True(t, IsNil(values))
	assert.True(t, IsNil(items))
	assert.True(t, IsNil(fn))
	assert.False(t, IsNil(&testReader{}))
	assert.False(t, IsNil(42))
	assert.False(t, IsNil(""))
}
