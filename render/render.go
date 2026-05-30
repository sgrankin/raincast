// SPDX-License-Identifier: MIT

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
	"os"
	"runtime/debug"
	"time"

	"github.com/gdamore/tcell/v3"

	"github.com/sgrankin/raincast/model"
	"github.com/sgrankin/raincast/playout"
	"github.com/sgrankin/raincast/sim"
	"github.com/sgrankin/raincast/theme"
)

// buildRev returns the VCS revision Go embeds at build time, shown in the HUD so
// the running binary is unambiguous when iterating on visuals.
func buildRev() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 8 {
		rev = rev[:8]
	}
	if dirty {
		rev += "+"
	}
	return rev
}

const (
	decayPerFrame = 0.80 // trail brightness multiplier per frame
	clearBelow    = 0.05 // brightness at which a cell is cleared

	// A terminal cell can't glow, so the head must win on brightness alone: it
	// burns at full hue while the body fades from bodyTop (just behind the head)
	// to bodyFloor (the trail's top). The fade is scaled across each drop's own
	// length so even a short drop dims to the floor at its top — a terminal has
	// no canvas persistence to stretch a short trail's fade the way the prototype
	// did.
	headBright = 1.0
	bodyTop    = 0.55
	bodyFloor  = 0.10
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
	rev            string
	minContrast    float64       // match the terminal's minimum-contrast; <=1 disables
	replayDelay    time.Duration // playout buffer depth; <=0 spawns on arrival

	panelH     int         // bottom log/span panel height in rows; 0 disables
	panelLines []panelLine // ring of the most recent formatted event lines
}

// panelLine is one formatted, colored line in the scrolling event panel.
type panelLine struct {
	text string
	col  theme.RGB
}

// Config holds renderer options.
type Config struct {
	FPS int
	// MinContrast should match the terminal's minimum-contrast (e.g. Ghostty's
	// 1.1): a glyph dimmer than this ratio against the background is left blank
	// rather than drawn, so the terminal never contrast-boosts a fading tail
	// toward white. <=1 disables it (trails fade all the way into the background).
	MinContrast float64
	// ReplayDelay is the playout buffer depth: events are held and released at
	// their true relative times delayed by this much, so bursty (batched) exports
	// still render as smooth real-time rain. <=0 spawns on arrival.
	ReplayDelay time.Duration
	// LogPanel reserves this many bottom rows for a scrolling panel that tails
	// decoded events (logs and spans) as text. 0 disables it.
	LogPanel int
}

// New builds a Renderer over a tcell screen (not yet initialized).
func New(screen tcell.Screen, pal theme.Palette, cfg Config) *Renderer {
	if cfg.FPS <= 0 {
		cfg.FPS = 30
	}
	return &Renderer{
		screen: screen, pal: pal, fps: cfg.FPS, rev: buildRev(),
		minContrast: cfg.MinContrast, replayDelay: cfg.ReplayDelay, panelH: cfg.LogPanel,
	}
}

// drawable reports whether a glyph at fg should be drawn, or skipped (left blank)
// because the terminal would contrast-boost it. Skipping keeps a cell as a space,
// which terminals exempt from minimum-contrast.
func (r *Renderer) drawable(fg theme.RGB) bool {
	return r.minContrast <= 1 || fg.Contrast(r.pal.Bg) >= r.minContrast
}

func toColor(c theme.RGB) tcell.Color { return tcell.NewRGBColor(int32(c.R), int32(c.G), int32(c.B)) }

// resize recomputes the field geometry from the draw target. Row 0 and the last
// row are HUD; the rain falls in between.
// effPanelH is the panel height that fits the current screen: capped so at least
// one rain row and both HUD rows survive on a short terminal.
func (r *Renderer) effPanelH(h int) int {
	p := r.panelH
	if max := h - 3; p > max {
		p = max
	}
	if p < 0 {
		p = 0
	}
	return p
}

func (r *Renderer) resize(c cells) {
	w, h := c.Size()
	r.cols = w
	r.rainRows = h - 2 - r.effPanelH(h) // top HUD + bottom HUD + optional panel
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
	pb := playout.New(r.replayDelay)
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
					pb.Add(e, now) // buffer arrivals; release at their playout time
				default:
					break drain
				}
			}
			for _, e := range pb.Release(now) {
				s.Ingest(e)
				r.pushPanel(e)
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
		// Log sparks take severity color (overriding status — surfaces "200 OK but
		// logged ERROR"); children are muted unless they errored; requests take
		// their status hue.
		var col theme.RGB
		switch {
		case d.Sev > 0:
			col = r.pal.Severity(d.Sev)
		case d.Child && d.Err:
			col = r.pal.Status[5]
		case d.Child:
			col = r.pal.Dim
		default:
			col = r.pal.StatusColor(d.Class * 100)
		}
		headRow := int(d.Y)
		for j := 0; j <= len(d.Body); j++ {
			row := headRow - j
			if row < 0 || row >= r.rainRows {
				continue
			}
			var ch rune
			var b float64
			if j == 0 {
				ch, b = d.Head, headBright // leading glyph, full hue
			} else {
				ch = d.Body[j-1]
				// Fade across the whole body: j=1 (behind head) is brightest,
				// j=len (trail top) reaches the floor — independent of length.
				frac := 0.0
				if n := len(d.Body); n > 1 {
					frac = float64(n-j) / float64(n-1)
				}
				b = bodyFloor + (bodyTop-bodyFloor)*frac
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
			if !r.drawable(fg) {
				continue // too dim — leave blank so the terminal can't boost it
			}
			st := tcell.StyleDefault.Background(toColor(r.pal.Bg)).Foreground(toColor(fg))
			c.SetContent(x, y+1, cell.ch, nil, st)
		}
	}
	r.drawHUD(c, s)
	r.drawPanel(c)
	c.Show()
}

// pushPanel formats an event and appends it to the panel ring (newest last),
// keeping only the visible rows. No-op when the panel is disabled.
func (r *Renderer) pushPanel(ev model.Event) {
	if r.panelH <= 0 {
		return
	}
	text, col := panelFormat(ev, r.pal)
	if text == "" {
		return
	}
	r.panelLines = append(r.panelLines, panelLine{text, col})
	if len(r.panelLines) > r.panelH {
		r.panelLines = r.panelLines[len(r.panelLines)-r.panelH:]
	}
}

// drawPanel renders the event panel just above the bottom HUD, oldest line at
// the top so new lines scroll up from the bottom.
func (r *Renderer) drawPanel(c cells) {
	_, h := c.Size()
	ph := r.effPanelH(h)
	if ph <= 0 {
		return
	}
	top := h - 1 - ph // rows [top, h-2]; h-1 is the bottom HUD
	bg := toColor(r.pal.Bg)
	n := len(r.panelLines)
	for i := 0; i < ph; i++ {
		idx := n - ph + i
		if idx < 0 || idx >= n {
			continue
		}
		ln := r.panelLines[idx]
		st := tcell.StyleDefault.Background(bg).Foreground(toColor(ln.col))
		r.text(c, 1, top+i, ln.text, st)
	}
}

// panelFormat renders one event as a colored text line for the panel.
func panelFormat(ev model.Event, pal theme.Palette) (string, theme.RGB) {
	switch e := ev.(type) {
	case model.SpanEvent:
		if e.Method != "" || e.Route != "" { // request
			line := fmt.Sprintf("%c %3d  %-8s %-6s %-24s %6.0fms",
				sim.HeadGlyph(e.Method), e.Status, ptrunc(e.Service, 8), e.Method, ptrunc(e.Route, 24), e.MS)
			return line, pal.StatusColor(e.Status)
		}
		col := pal.Dim // child span
		if e.Err {
			col = pal.Status[5]
		}
		return fmt.Sprintf("  └ %-8s %-28s %6.0fms", ptrunc(e.Service, 8), ptrunc(e.Name, 28), e.MS), col
	case model.LogEvent:
		return fmt.Sprintf("✦ %-5s %-8s %s", panelSev(e.Sev), ptrunc(e.Service, 8), ptrunc(e.Body, 40)), pal.Severity(e.Sev)
	}
	return "", pal.Fg
}

func panelSev(sev int) string {
	switch {
	case sev >= 21:
		return "FATAL"
	case sev >= 17:
		return "ERROR"
	case sev >= 13:
		return "WARN"
	case sev >= 9:
		return "INFO"
	case sev >= 5:
		return "DEBUG"
	default:
		return "TRACE"
	}
}

func ptrunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
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

	bottom := fmt.Sprintf(" sigils: %d   q quit · space pause   build %s", s.DictSize(), r.rev)
	r.text(c, 0, h-1, bottom, dim)
}

// Diag renders a static color/brightness diagnostic and blocks until the user
// quits. For each status class it draws two strips representing one drop's trail
// from head (bright, left) to tail (dim, right): "blend" is the current color
// math (fade toward background), "scale" is an alternative (scale the hue by
// brightness). Comparing them in the real terminal reveals whether a dim hue
// renders as expected or washes to white, and which model avoids it.
func (r *Renderer) Diag(ctx context.Context) error {
	if err := r.screen.Init(); err != nil {
		return err
	}
	defer r.screen.Fini()
	bg := toColor(r.pal.Bg)
	r.screen.SetStyle(tcell.StyleDefault.Background(bg).Foreground(toColor(r.pal.Fg)))

	draw := func() {
		r.screen.Clear()
		base := tcell.StyleDefault.Background(bg).Foreground(toColor(r.pal.Fg))
		dim := tcell.StyleDefault.Background(bg).Foreground(toColor(r.pal.Dim))
		r.text(r.screen, 0, 0, fmt.Sprintf(" RAINCAST DIAG · TERM=%s COLORTERM=%s · build %s · q quits",
			os.Getenv("TERM"), os.Getenv("COLORTERM"), r.rev), base)
		r.text(r.screen, 0, 1, fmt.Sprintf(" one drop's trail: bright (left) → dim (right). --min-contrast=%.2g", r.minContrast), dim)
		r.text(r.screen, 0, 2, " block: reference · text: raw (boosts at dim end) · fix: clears below the contrast floor", dim)

		labels := map[int]string{2: "2xx", 3: "3xx", 4: "4xx", 5: "5xx"}
		const sw = 40
		for ci, cl := range []int{2, 3, 4, 5} {
			col := r.pal.Status[cl]
			row := 4 + ci*4
			r.text(r.screen, 0, row, labels[cl]+" block", base)
			r.text(r.screen, 0, row+1, "    text ", dim)
			r.text(r.screen, 0, row+2, "    fix  ", dim)
			for i := 0; i < sw; i++ {
				b := 1.0 - float64(i)/float64(sw-1) // 1.0 → 0.0
				fg := r.pal.Bg.Blend(col, b)
				st := tcell.StyleDefault.Background(bg).Foreground(toColor(fg))
				// Identical color via the exact rain path, drawn three ways: a block
				// char (contrast-boost-exempt), a raw text glyph (boosted at the dim
				// end), and the fix (text, but skipped once below the contrast floor).
				r.screen.SetContent(11+i, row, '█', nil, st)
				r.screen.SetContent(11+i, row+1, 'ｷ', nil, st)
				if r.drawable(fg) {
					r.screen.SetContent(11+i, row+2, 'ｷ', nil, st)
				}
			}
		}
		r.screen.Show()
	}

	draw()
	eq := r.screen.EventQ()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-eq:
			switch ev := ev.(type) {
			case *tcell.EventResize:
				r.screen.Sync()
				draw()
			case *tcell.EventKey:
				if ev.Key() == tcell.KeyEscape || ev.Key() == tcell.KeyCtrlC || ev.Str() == "q" {
					return nil
				}
			}
		}
	}
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
