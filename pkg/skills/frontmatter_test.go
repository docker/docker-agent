package skills

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitKeyValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line  string
		key   string
		value string
		ok    bool
	}{
		{"name: value", "name", "value", true},
		{"metadata:", "metadata", "", true},
		{":", "", "", true},
		{"ключ:", "ключ", "", true},
		{"key::", "key:", "", true},
		{"key: value:", "key", "value:", true},
		{"", "", "", false},
		{"key", "", "", false},
		{"key:value", "", "", false},
	} {
		t.Run(tc.line, func(t *testing.T) {
			t.Parallel()
			key, value, ok := splitKeyValue(tc.line)
			assert.Equal(t, tc.key, key)
			assert.Equal(t, tc.value, value)
			assert.Equal(t, tc.ok, ok)
		})
	}
}

func TestParseFrontmatterMalformed(t *testing.T) {
	t.Parallel()
	for _, content := range []string{"", "---", "---\n---garbage", "---\nname: test\n---garbage"} {
		t.Run(content, func(t *testing.T) {
			t.Parallel()
			_, ok := parseFrontmatter(content)
			assert.False(t, ok)
		})
	}
	_, ok := parseFrontmatter("---\n---")
	assert.True(t, ok)
}
