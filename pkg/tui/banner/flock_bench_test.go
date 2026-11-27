package banner

import (
	"fmt"
	"image"
	"strings"
	"testing"
	"time"
)

func BenchmarkFlock(b *testing.B) {
	for _, size := range []image.Point{{X: 100, Y: 30}, {X: 160, Y: 50}, {X: 240, Y: 80}} {
		b.Run(fmt.Sprintf("%dx%d", size.X, size.Y), func(b *testing.B) {
			var f Flock
			f.Point(float64(size.X/2), 2, true, 1, size.X)
			base := strings.Repeat(strings.Repeat(" ", size.X)+"\n", size.Y-1) + strings.Repeat(" ", size.X)
			viewport := image.Rectangle{Max: size}
			origin := image.Pt(0, size.Y/2-Height/2)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				f.Point(float64(size.X/2+i%20-10), 2, i%14 == 0, 1, size.X)
				_, changed := f.Step(time.Second/14, 1, size.X)
				if changed {
					_ = f.Composite(base, 1, size.X, origin, viewport, nil)
				}
			}
		})
	}
}
