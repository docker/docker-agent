//go:build !js

package datetime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetDatetimeLocalTimezone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 7, 23, 4, 5, 0, time.UTC)
	ts := &ToolSet{now: func() time.Time { return now }}
	for _, timezone := range []string{"", "Local"} {
		result, err := ts.get(t.Context(), Args{Format: time.RFC3339, Timezone: timezone})
		require.NoError(t, err)
		require.False(t, result.IsError)
		assert.Equal(t, now.In(time.Local).Format(time.RFC3339), result.Output)
	}
}
