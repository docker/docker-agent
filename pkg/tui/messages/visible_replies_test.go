package messages

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseVisibleReplies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value string
		want  VisibleReplies
	}{
		{"latest", VisibleRepliesLatest},
		{"3", VisibleRepliesThree},
		{"5", VisibleRepliesFive},
		{"all", VisibleRepliesAll},
		{"", VisibleRepliesThree},
		{"invalid", VisibleRepliesThree},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			assert.Equal(t, tt.want, ParseVisibleReplies(tt.value))
		})
	}
}
