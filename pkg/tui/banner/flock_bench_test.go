package banner

import (
	"testing"
	"time"
)

func BenchmarkFlock(b *testing.B) {
	for _, width := range []int{100, 240} {
		name := "100cols"
		if width == 240 {
			name = "240cols"
		}
		b.Run(name, func(b *testing.B) {
			var f Flock
			f.Point(float64(width/2), 2, true, 1, width)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				f.Point(float64(width/2+i%20-10), 2, i%14 == 0, 1, width)
				_, changed := f.Step(time.Second/14, 1, width)
				if changed {
					_ = f.Render(1, width)
				}
			}
		})
	}
}
