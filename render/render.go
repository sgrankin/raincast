// Package render paints the rain field with tcell and drives the game loop.
//
// The signature rain look comes from a per-cell brightness buffer: each frame
// every cell's brightness decays, then live drops re-stamp their head (full
// brightness) and body (fading upward). Cells a drop has left behind keep their
// glyph and decay toward the background — that dissolving wake, not the drop
// objects alone, is what reads as rain in a grid that has no glow or alpha.
package render

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/gdamore/tcell/v3"

	"github.com/sgrankin/raincast/model"
	"github.com/sgrankin/raincast/sim"
	"github.com/sgrankin/raincast/theme"
)

const (
	decayPerFrame = 0.80 // trail brightness multiplier per frame
	clearBelow    = 0.05 // brightness at which a cell is cleared
	corruptProb   = 0.20 // chance an errored drop's body glyph flickers to noise
)

// cells is the drawing subset of tcell.Screen that painting needs. Abstracting it
// lets the renderer be snapshot-tested against a fake — tcell v3 dropped the
// SimulationScreen that used to fill that role, and a fake is version-independent
// and faster anyway.
type cells interface {
	Size() (width, height int)
	SetContent(x, y int, primary rune, combining []rune, style tcell.Style)
	Clear()
	Show()
}

// vcell is one cell's current glyph, hue, and brightness.
type vcell struct {
	ch  rune
	col theme.RGB
	b   float64
}

// Renderer paints a Sim to a tcell screen.
type Renderer struct {
	screen tcell.Screen
	pal    theme.Palette
	fps    int

	cols, rainRows int
	grid           [][]vcell // [rainRows][cols]
}

// New builds a Renderer over a tcell screen (not yet initialized).
func New(screen tcell.Screen, pal theme.Palette, fps int) *Renderer {
	if fps <= 0 {
		fps = 30
	}
	return &Renderer{screen: screen, pal: pal, fps: fps}
}

func toColor(c theme.RGB) tcell.Color { return tcell.NewRGBColor(int32(c.R), int32(c.G), int32(c.B)) }

// resize recomputes the field geometry from the draw target. Row 0 and the last
// row are HUD; the rain falls in between.
func (r *Renderer) resize(c cells) {
	w, h := c.Size()
	r.cols = w
	r.rainRows = h - 2
	if r.rainRows < 1 {
		r.rainRows = 1
	}
	r.grid = make([][]vcell, r.rainRows)
	for i := range r.grid {
		r.grid[i] = make([]vcell, r.cols)
	}
}

// Run owns the terminal and blocks until the user quits (q/Esc/Ctrl-C) or ctx is
// cancelled. It drains events, advances the sim, and paints at the target fps.
func (r *Renderer) Run(ctx context.Context, events <-chan model.Event, s *sim.Sim) error {
	if err := r.screen.Init(); err != nil {
		return err
	}
	defer r.screen.Fini()
	r.screen.SetStyle(tcell.StyleDefault.Background(toColor(r.pal.Bg)).Foreground(toColor(r.pal.Fg)))
	r.screen.Clear()
	r.resize(r.screen)
	s.Resize(r.cols, r.rainRows)

	eq := r.screen.EventQ() // v3 posts input events here; no PollEvent goroutine needed
	ticker := time.NewTicker(time.Second / time.Duration(r.fps))
	defer ticker.Stop()
	last := time.Now()
	paused := false

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-eq:
			switch ev := ev.(type) {
			case *tcell.EventResize:
				r.screen.Sync()
				r.resize(r.screen)
				s.Resize(r.cols, r.rainRows)
			case *tcell.EventKey:
				switch {
				case ev.Key() == tcell.KeyEscape, ev.Key() == tcell.KeyCtrlC:
					return nil
				case ev.Key() == tcell.KeyRune:
					switch ev.Str() {
					case "q":
						return nil
					case " ":
						paused = !paused
					}
				}
			}
		case now := <-ticker.C:
			dt := now.Sub(last).Seconds()
			last = now
			// Clamp: a stall (slow resize, input burst, GC pause, laptop sleep)
			// shouldn't teleport drops a hundred rows in one frame.
			if dt > 0.1 {
				dt = 0.1
			}
		drain:
			for {
				select {
				case e := <-events:
					s.Ingest(e)
				default:
					break drain
				}
			}
			if !paused {
				s.Advance(dt)
			}
			r.paint(r.screen, s)
		}
	}
}

// paint decays the brightness buffer, stamps the live drops, and draws the frame
// plus the HUD. Kept separate from Run (and parameterized by the draw target) so
// it can be driven frame-by-frame against a fake in tests.
func (r *Renderer) paint(c cells, s *sim.Sim) {
	// 1. decay existing cells (the trail wake).
	for y := range r.grid {
		for x := range r.grid[y] {
			cell := &r.grid[y][x]
			cell.b *= decayPerFrame
			if cell.b < clearBelow {
				cell.ch = 0
				cell.b = 0
			}
		}
	}

	// 2. stamp live drops at full/positional brightness. The head leads (bottom),
	//    body glyphs trail upward and dimmer.
	for _, d := range s.Drops() {
		if d.Lane < 0 || d.Lane >= r.cols {
			continue
		}
		col := r.pal.StatusColor(d.Class * 100)
		headRow := int(d.Y)
		for j := 0; j <= len(d.Body); j++ {
			row := headRow - j
			if row < 0 || row >= r.rainRows {
				continue
			}
			var ch rune
			var b float64
			if j == 0 {
				ch, b = d.Head, 1.0
			} else {
				ch = d.Body[j-1]
				b = math.Max(0.15, 1-float64(j)*0.12)
				if d.Err && rand.Float64() < corruptProb {
					ch = sim.NoiseRune()
				}
			}
			r.grid[row][d.Lane] = vcell{ch: ch, col: col, b: b * d.Alpha}
		}
	}

	// 3. draw.
	c.Clear()
	for y := range r.grid {
		for x := range r.grid[y] {
			cell := r.grid[y][x]
			if cell.ch == 0 || cell.b <= 0 {
				continue
			}
			fg := r.pal.Bg.Blend(cell.col, cell.b)
			st := tcell.StyleDefault.Background(toColor(r.pal.Bg)).Foreground(toColor(fg))
			c.SetContent(x, y+1, cell.ch, nil, st)
		}
	}
	r.drawHUD(c, s)
	c.Show()
}

func (r *Renderer) drawHUD(c cells, s *sim.Sim) {
	_, h := c.Size()
	fc := s.Weather()
	bg := toColor(r.pal.Bg)
	base := tcell.StyleDefault.Background(bg).Foreground(toColor(r.pal.Fg))
	dim := tcell.StyleDefault.Background(bg).Foreground(toColor(r.pal.Dim))

	x := r.text(c, 0, 0, " RAINCAST  ", base.Bold(true))
	x = r.text(c, x, 0, fc.Text+"   ", base)
	for _, cl := range []int{2, 3, 4, 5} {
		st := tcell.StyleDefault.Background(bg).Foreground(toColor(r.pal.Status[cl]))
		x = r.text(c, x, 0, fmt.Sprintf("%dxx %d  ", cl, fc.Counts[cl]), st)
	}
	r.text(c, x, 0, fmt.Sprintf("%d/s", fc.RPS), dim)

	bottom := fmt.Sprintf(" sigils: %d   q quit · space pause", s.DictSize())
	r.text(c, 0, h-1, bottom, dim)
}

// text draws a single-width string and returns the next x. Every glyph raincast
// uses (method heads, sigils, half-width katakana noise, HUD text) is
// single-width, so a flat +1 advance keeps the grid aligned.
func (r *Renderer) text(c cells, x, y int, s string, st tcell.Style) int {
	for _, ch := range s {
		if x >= 0 && x < r.cols {
			c.SetContent(x, y, ch, nil, st)
		}
		x++
	}
	return x
}
