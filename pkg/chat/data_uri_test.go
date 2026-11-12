package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDataURI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		uri      string
		mimeType string
		data     string
		err      string
	}{
		{"base64", "data:text/plain;base64,aGk=", "text/plain", "hi", ""},
		{"charset", "data:text/plain;charset=utf-8;base64,aGk=", "text/plain", "hi", ""},
		{"empty mime", "data:;base64,aGk=", "application/octet-stream", "hi", ""},
		{"empty payload", "data:text/plain;base64,", "text/plain", "", ""},
		{"missing prefix", "text/plain;base64,aGk=", "", "", "not a data URI"},
		{"missing comma", "data:text/plain;base64", "", "", "missing comma"},
		{"missing suffix", "data:text/plain,hello", "", "", "not base64-encoded"},
		{"suffix in middle", "data:text/plain;base64;charset=utf-8,aGk=", "", "", "not base64-encoded"},
		{"case sensitive suffix", "data:text/plain;BASE64,aGk=", "", "", "not base64-encoded"},
		{"invalid payload", "data:text/plain;base64,!", "", "", "base64 decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mimeType, data, err := parseDataURI(tc.uri)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				assert.Empty(t, mimeType)
				assert.Nil(t, data)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.mimeType, mimeType)
			assert.Equal(t, tc.data, string(data))
		})
	}
}
