package banner

import (
	"math"
	"time"
)

const influenceRadius = 24.0

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

// Leave reports whether displaced particles need to return home.
func (f *Flock) Leave() bool {
	f.hovering = false
	return len(f.dots) != 0
}

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
	if distance >= influenceRadius {
		return 0, 0
	}
	// A tiny deterministic bias avoids trapping dots exactly under the pointer.
	if distance < 0.1 {
		angle := float64(dustHash(p.homeX, p.homeY)%628) * 0.01
		dx, dy, distance = math.Cos(angle), math.Sin(angle), 1
	}
	falloff := 1 - distance/influenceRadius
	strength := 900 * falloff * falloff / distance
	return dx * strength, dy * strength
}

// Step reports whether motion needs frames and whether any dot changed cells.
// Small substeps keep the spring stable when terminal rendering delays ticks.
func (f *Flock) Step(elapsed time.Duration, progress float64, width int) (active, changed bool) {
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
			ax := fx - 9*p.x - 3.8*p.vx + 3*(g.x/float64(g.count)-p.vx)
			ay := fy - 9*p.y - 3.8*p.vy + 3*(g.y/float64(g.count)-p.vy)
			p.vx += ax * dt
			p.vy += ay * dt
			if remaining == 0 && (math.Hypot(p.vx, p.vy) > 0.1 || math.Hypot(ax, ay) > 0.3) {
				active = true
			}
			oldX, oldY := math.Round(p.x), math.Round(p.y)
			p.x += p.vx * dt
			p.y += p.vy * dt
			changed = changed || math.Round(p.x) != oldX || math.Round(p.y) != oldY
		}
	}
	if f.hovering {
		return active, changed
	}
	for _, p := range f.dots {
		if math.Hypot(p.x, p.y) > 0.03 || math.Hypot(p.vx, p.vy) > 0.1 {
			return true, changed
		}
	}
	*f = Flock{}
	return false, true
}

// Displaced reports whether the flock owns a frame, including a resting hover.
func (f *Flock) Displaced() bool { return len(f.dots) != 0 }
