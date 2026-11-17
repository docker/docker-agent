package streamcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStreamsCompleteOnlyWhenTheLastStreamStops(t *testing.T) {
	t.Parallel()
	var streams Streams
	streams.Start("root")
	streams.Start("root")
	streams.Start("child")
	assert.False(t, streams.Stop("root"), "a nested stream sharing the session keeps it live")
	assert.True(t, streams.Stop("child"))
	assert.True(t, streams.Stop("root"))
	assert.True(t, streams.Stop("unknown"), "a stray stop completes its session")

	streams.Start("root")
	streams.Reset()
	assert.True(t, streams.Stop("root"))
}
