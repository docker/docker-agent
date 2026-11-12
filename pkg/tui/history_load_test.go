package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
)

func TestLoadHistoryCmd(t *testing.T) {
	t.Parallel()

	t.Run("no history", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, (&appModel{ctx: t.Context}).loadHistoryCmd())
	})

	t.Run("reads the lazy history", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		seeded, err := history.New(dir)
		require.NoError(t, err)
		require.NoError(t, seeded.Add("earlier prompt"))

		hist, err := history.New(dir)
		require.NoError(t, err)
		require.Empty(t, hist.Messages)

		cmd := (&appModel{ctx: t.Context, history: hist}).loadHistoryCmd()
		require.NotNil(t, cmd)
		assert.Nil(t, cmd())
		assert.Equal(t, []string{"earlier prompt"}, hist.Messages)
	})
}
