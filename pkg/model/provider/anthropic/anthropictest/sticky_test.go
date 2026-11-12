package anthropictest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTruncate(t *testing.T) {
	t.Parallel()
	const reply = "event: message_start\ndata: {}\n\nevent: message_delta\ndata: {}\n\nevent: message_stop\ndata: {}\n\n"
	assert.Equal(t, "event: message_start\ndata: {}\n\n", Truncate(reply, "message_delta"))
	assert.Empty(t, Truncate(reply, "message_start"))
	assert.Equal(t, reply, Truncate(reply, "absent"))
	assert.Equal(t, reply, Truncate(reply, "message"), "event names must match exactly")
}
