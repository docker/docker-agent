package a2astore

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewRejectsStoreInUse(t *testing.T) {
	dir := t.TempDir()
	s1, err := New(t.Context(), dir)
	require.NoError(t, err)

	_, err = New(t.Context(), dir)
	require.ErrorIs(t, err, ErrInUse)

	require.NoError(t, s1.Close())
	s2, err := New(t.Context(), dir)
	require.NoError(t, err, "the lock must be released on Close")
	require.NoError(t, s2.Close())
}
