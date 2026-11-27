package banner

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMainBannerArtwork(t *testing.T) {
	t.Parallel()

	// The original wordmark, including its whitespace, is part of the banner identity.
	want := []string{
		`      ⣿⡿               ⣸⣿⠁                                               ⣤⡶`,
		` ⢠⣶⡿⢷⣶⣿⡇⢀⣴⣾⠿⢷⣦⡀ ⣠⣶⠿⢷⣦⡀⢀⣿⡟⣠⣶⠶⠂⢠⣶⡾⢿⣶⡄ ⣴⣶⣴⡿⠂   ⣠⣶⠿⢷⣦ ⢀⣴⣾⠿⣦⣶⡶ ⣠⣶⠿⢷⣦⡀ ⣶⣶⡶⢾⣷⡆⠠⢾⣿⡷⠆`,
		`⢠⣿⡏  ⣿⣿ ⣾⣿⠁ ⢈⣿⡇⢸⣿⠃  ⠉⠁⢸⣿⣿⣿⡇ ⢠⣿⣯⣤⣤⣿⡷⢀⣿⡟⠁    ⢀⣤⣤⣶⣾⣿ ⣼⣿⠁ ⢸⣿⡇⢸⣿⣧⣤⣤⣿⡇⢰⣿⡏ ⢰⣿⡇ ⣾⣿`,
		`⠘⣿⣧⣀⣴⣿⡏ ⢻⣿⣄⣠⣾⡿⠁⠸⣿⣄⣠⣾⡖ ⣿⡿⠁⢹⣿⡀⠈⢿⣧⣀⣴⣶⠄⣸⣿⠃     ⢺⣿⣄⣤⣿⡏ ⢻⣿⣤⣤⣾⣿ ⠘⣿⣦⣀⣴⡦ ⣼⣿  ⣼⣿ ⠠⣿⣇⡀`,
		` ⠈⠉⠉⠉⠉⠁  ⠉⠉⠉⠉   ⠈⠉⠉⠁  ⠉⠁ ⠈⠉⠁  ⠉⠉⠉⠁ ⠉⠉       ⠉⠉⠁⠉⠁ ⣤⣍⣉⣡⣿⡇  ⠈⠉⠉⠉  ⠉⠉  ⠉⠉  ⠉⠉⠁`,
		`                                                  ⠙⠛⠛⠛⠋`,
	}
	require.Equal(t, want, Lines)
	assert.Equal(t, 76, Width)
	assert.Equal(t, 6, Height)
	width := 0
	for _, line := range Lines {
		width = max(width, ansi.StringWidth(line))
		assert.Equal(t, strings.TrimRight(line, " "), line)
		for _, r := range line {
			assert.True(t, r == ' ' || (r >= '\u2800' && r <= '\u28ff'), "unexpected rune %q", r)
		}
	}
	assert.Equal(t, Width, width)
}

func TestBrailleDotMapping(t *testing.T) {
	t.Parallel()

	assert.Equal(t, [2][4]byte{{1, 2, 4, 64}, {8, 16, 32, 128}}, brailleBits)
	for mask := range 256 {
		var assembled byte
		for x := range 2 {
			for y := range 4 {
				bit := brailleBits[x][y]
				if byte(mask)&bit != 0 {
					assembled |= bit
				}
			}
		}
		assert.Equal(t, byte(mask), assembled)
	}
}
