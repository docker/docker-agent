package banner

import (
	"math"
	"time"
)

type particle struct {
	homeX, homeY int
	x, y, vx, vy float64
}

// Flock adds spring-bound local motion to the existing dust trajectory.
// Positions and forces use braille dots rather than terminal cells.
type Flock struct {
	dots               []particle
	pointerX, pointerY float64
	hovering           bool
}

func (f *Flock) init() {
	if f.dots != nil {
		return
	}
	for y, line := range Lines {
		for x, r := range []rune(line) {
			if r < '⠀' || r > '⣿' {
				continue
			}
			for dx := range 2 {
				for dy := range 4 {
					if byte(r-'⠀')&brailleBits[dx][dy] != 0 {
						f.dots = append(f.dots, particle{homeX: x*2 + dx, homeY: y*4 + dy})
					}
				}
			}
		}
	}
}

// Point sets a pane-relative pointer; clicking gives nearby particles a kick.
func (f *Flock) Point(x, y float64, click bool, progress float64, width int) {
	f.init()
	f.pointerX, f.pointerY, f.hovering = x*2+0.5, y*4+1.5, true
	if click {
		for i := range f.dots {
			p := &f.dots[i]
			fx, fy := f.force(*p, progress, width)
			p.vx += fx * 0.42
			p.vy += fy * 0.42
		}
	}
}

func (f *Flock) Leave() { f.hovering = false }

func (f *Flock) force(p particle, progress float64, width int) (float64, float64) {
	if !f.hovering {
		return 0, 0
	}
	x, y, _ := dustPosition(p.homeX, p.homeY, width, progress)
	if x < 0 {
		return 0, 0
	}
	dx, dy := float64(x)+p.x-f.pointerX, float64(y)+p.y-f.pointerY
	distance := math.Hypot(dx, dy)
	const radius = 24.0
	if distance >= radius {
		return 0, 0
	}
	// A tiny deterministic bias avoids trapping dots exactly under the pointer.
	if distance < 0.1 {
		angle := float64(dustHash(p.homeX, p.homeY)%628) * 0.01
		dx, dy, distance = math.Cos(angle), math.Sin(angle), 1
	}
	falloff := 1 - distance/radius
	strength := 900 * falloff * falloff / distance
	return dx * strength, dy * strength
}

// Step returns whether motion still needs frames. Small substeps keep the
// spring stable even when terminal rendering delays the shared animation tick.
func (f *Flock) Step(elapsed time.Duration, progress float64, width int) bool {
	remaining := min(elapsed.Seconds(), 0.15)
	for remaining > 0 {
		dt := min(remaining, 1.0/60)
		remaining -= dt
		type velocity struct {
			x, y  float64
			count int
		}
		var groups [Height][Width]velocity
		for _, p := range f.dots {
			g := &groups[p.homeY/4][p.homeX/2]
			g.x += p.vx
			g.y += p.vy
			g.count++
		}
		for i := range f.dots {
			p := &f.dots[i]
			g := groups[p.homeY/4][p.homeX/2]
			fx, fy := f.force(*p, progress, width)
			// Local alignment keeps neighboring dots flowing together without an
			// all-pairs boid simulation; the spring preserves the wordmark identity.
			p.vx += (fx - 9*p.x - 3.8*p.vx + 3*(g.x/float64(g.count)-p.vx)) * dt
			p.vy += (fy - 9*p.y - 3.8*p.vy + 3*(g.y/float64(g.count)-p.vy)) * dt
			p.x += p.vx * dt
			p.y += p.vy * dt
		}
	}
	if f.hovering {
		return true
	}
	for _, p := range f.dots {
		if math.Hypot(p.x, p.y) > 0.03 || math.Hypot(p.vx, p.vy) > 0.1 {
			return true
		}
	}
	*f = Flock{}
	return false
}

// Render draws the displaced dust using the banner's existing colors and bounds.
func (f *Flock) Render(progress float64, width int) []string {
	f.init()
	return renderParticles(progress, width, f)
}
