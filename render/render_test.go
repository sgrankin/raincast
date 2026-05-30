package render

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v3"

	"github.com/sgrankin/raincast/model"
	"github.com/sgrankin/raincast/sim"
	"github.com/sgrankin/raincast/theme"
)

// fakeCells is a headless draw target that records the latest glyph per cell, so
// a frame can be dumped to text and asserted on without a real terminal.
type fakeCells struct {
	w, h  int
	cells map[[2]int]rune
}

func newFakeCells(w, h int) *fakeCells {
	return &fakeCells{w: w, h: h, cells: map[[2]int]rune{}}
}

func (f *fakeCells) Size() (int, int) { return f.w, f.h }
func (f *fakeCells) Clear()           { f.cells = map[[2]int]rune{} }
func (f *fakeCells) Show()            {}
func (f *fakeCells) SetContent(x, y int, primary rune, _ []rune, _ tcell.Style) {
	if x >= 0 && x < f.w && y >= 0 && y < f.h {
		f.cells[[2]int{x, y}] = primary
	}
}

func (f *fakeCells) dump() string {
	var b strings.Builder
	for y := 0; y < f.h; y++ {
		for x := 0; x < f.w; x++ {
			ch := f.cells[[2]int{x, y}]
			if ch == 0 || ch == ' ' {
				b.WriteByte(' ')
			} else {
				b.WriteRune(ch)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func (f *fakeCells) glyphsInRows(y0, y1 int) int {
	n := 0
	for p, ch := range f.cells {
		if p[1] >= y0 && p[1] < y1 && ch != 0 && ch != ' ' {
			n++
		}
	}
	return n
}

func TestPaintRendersRainAndHUD(t *testing.T) {
	c := newFakeCells(90, 14)
	r := New(nil, theme.Of(theme.Dark), 30)
	r.resize(c)
	s := sim.New(sim.Config{LaneKey: "trace", DictCap: 18, MinFall: 4, MaxFall: 16}, r.cols, r.rainRows)

	// A spread of requests across distinct traces (so they take distinct lanes),
	// including a 500 and a 404, then several frames so trails develop.
	routes := []model.SpanEvent{
		{Method: "GET", Route: "/api/users", Status: 200, Bytes: 2400, MS: 40},
		{Method: "POST", Route: "/checkout", Status: 500, Bytes: 0, MS: 1900, Err: true},
		{Method: "GET", Route: "/favicon.ico", Status: 404, Bytes: 0, MS: 3},
		{Method: "PATCH", Route: "/api/cart", Status: 200, Bytes: 210, MS: 60},
		{Method: "GET", Route: "/static/app.js", Status: 200, Bytes: 184000, MS: 8},
	}
	for i, ev := range routes {
		ev.TraceID = fmt.Sprintf("trace-%02d", i)
		s.Ingest(ev)
	}
	// Enough frames for drops to fall in from the spawn stagger and leave trails.
	for f := 0; f < 20; f++ {
		s.Advance(1.0 / 30)
		r.paint(c, s)
	}

	dump := c.dump()
	t.Logf("\n%s", dump)

	if !strings.Contains(dump, "RAINCAST") {
		t.Error("HUD brand line missing")
	}
	if !strings.Contains(dump, "2xx") || !strings.Contains(dump, "5xx") {
		t.Error("HUD status counts missing")
	}
	// Rain area (rows 1..rainRows) should hold the drop heads plus their trails —
	// more lit cells than the 5 heads alone.
	if got := c.glyphsInRows(1, r.rainRows+1); got < 6 {
		t.Errorf("expected several rain glyphs (heads + trails), got %d", got)
	}
}

// The head must be the clear bright leader: brighter than the glyph immediately
// behind it (a terminal can't glow, so brightness alone has to carry it).
func TestHeadBrighterThanTrail(t *testing.T) {
	c := newFakeCells(20, 22)
	r := New(nil, theme.Of(theme.Dark), 30)
	r.resize(c)
	s := sim.New(sim.Config{LaneKey: "trace", MinFall: 4, MaxFall: 16}, r.cols, r.rainRows)
	s.Ingest(model.SpanEvent{Method: "GET", Route: "/x", Status: 200, MS: 100, TraceID: "t"})

	for i := 0; i < 30; i++ {
		s.Advance(1.0 / 30)
		r.paint(c, s)
	}
	d := s.Drops()[0]
	headRow := int(d.Y)
	if headRow < 1 || headRow >= r.rainRows {
		t.Skipf("head off-screen at row %d", headRow)
	}
	head := r.grid[headRow][d.Lane].b
	trail := r.grid[headRow-1][d.Lane].b
	if head <= trail {
		t.Errorf("head (%.2f) should be brighter than the glyph behind it (%.2f)", head, trail)
	}
}

func TestPaintEmptyFieldStillDrawsHUD(t *testing.T) {
	c := newFakeCells(40, 8)
	r := New(nil, theme.Of(theme.Light), 30)
	r.resize(c)
	s := sim.New(sim.Config{}, r.cols, r.rainRows)
	r.paint(c, s)
	if !strings.Contains(c.dump(), "RAINCAST") {
		t.Error("HUD should render even with no traffic")
	}
}
