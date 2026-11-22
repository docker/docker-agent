package userconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCompactWorkLogDefaultsOffAndHonoursTheSetting(t *testing.T) {
	t.Parallel()

	var nilSettings *Settings
	assert.False(t, nilSettings.GetCompactWorkLog(), "a nil config keeps the upstream ungrouped transcript")

	settings := &Settings{}
	assert.False(t, settings.GetCompactWorkLog(), "an unset value is off")

	on := true
	settings.CompactWorkLog = &on
	assert.True(t, settings.GetCompactWorkLog())
}

func TestVisibleRepliesDefaultsToThreeAndNormalizes(t *testing.T) {
	t.Parallel()

	var nilSettings *Settings
	assert.Equal(t, "3", nilSettings.GetVisibleReplies())
	assert.Equal(t, "3", (&Settings{}).GetVisibleReplies())
	assert.Equal(t, "5", (&Settings{VisibleReplies: "5"}).GetVisibleReplies())
	assert.Equal(t, "latest", (&Settings{VisibleReplies: "latest"}).GetVisibleReplies())
	assert.Equal(t, "3", (&Settings{VisibleReplies: "unknown"}).GetVisibleReplies())
}
