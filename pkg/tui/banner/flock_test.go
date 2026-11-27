package banner

import (
	"image"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFlockRepelsAndReturns(t *testing.T) {
	const width = 100
	var hover, click Flock
	hover.Point(50, 2, false, 1, width)
	click.Point(50, 2, true, 1, width)
	displacement := func(f *Flock) float64 {
		var total float64
		for _, p := range f.dots {
			total += math.Hypot(p.x, p.y)
		}
		return total
	}
	before := hover.Composite("", 1, width, image.Point{}, image.Rect(0, 0, width, Height), nil)
	hover.Step(70*time.Millisecond, 1, width)
	click.Step(70*time.Millisecond, 1, width)
	assert.Positive(t, displacement(&hover))
	assert.Greater(t, displacement(&click), displacement(&hover))
	assert.NotEqual(t, before, hover.Composite("", 1, width, image.Point{}, image.Rect(0, 0, width, Height), nil))
	for _, p := range hover.dots {
		x, y, _ := dustPosition(p.homeX, p.homeY, width, 1)
		if p.x != 0 || p.y != 0 {
			assert.Greater(t, p.x*(float64(x)-hover.pointerX)+p.y*(float64(y)-hover.pointerY), 0.0)
		}
	}
	hover.Leave()
	active := true
	for range 100 {
		active, _ = hover.Step(70*time.Millisecond, 1, width)
	}
	assert.False(t, active)
	assert.Empty(t, hover.dots)
}
