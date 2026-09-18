//go:build windows

package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows"
)

func TestIsConnectionRefused_Windows(t *testing.T) {
	t.Parallel()

	assert.True(t, isConnectionRefused(windows.WSAECONNREFUSED))
	assert.False(t, isConnectionRefused(windows.WSAECONNRESET))
}
