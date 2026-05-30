// Package console renders normalized events as colorized, glyph-annotated lines.
// It is the milestone-1 stand-in for the tcell rain field: the same encoding
// (method → head glyph, status → hue, route → learned sigil, severity → spark
// color), just one line per event instead of a falling drop. A single goroutine
// owns a Printer, so it holds no locks.
package console

import (
	"fmt"
	"io"
	"strings"

	"github.com/sgrankin/raincast/model"
	"github.com/sgrankin/raincast/theme"
)

// methodGlyph maps HTTP methods to head glyphs (carried over from the prototype).
var methodGlyph = map[string]string{
	"GET": "↓", "POST": "▼", "PUT": "⇅", "PATCH": "∿",
	"DELETE": "✕", "HEAD": "∘", "OPTIONS": "⌥",
}

// sigils are handed out to hot routes — same pool as the browser prototype so the
// terminal and web views stay visually consistent.
var sigils = []string{"⊕", "♡", "⌑", "◈", "✦", "⟁", "⌬", "⎔", "◉", "⊗", "✧", "⟐", "◌", "⍟", "⌖", "❖", "⊚", "✺"}

// Printer formats events to an io.Writer using a theme Pen.
type Printer struct {
	w    io.Writer
	pen  theme.Pen
	dict map[string]string // route -> assigned sigil
	hits map[string]int     // route -> hit count
}

func NewPrinter(w io.Writer, pen theme.Pen) *Printer {
	return &Printer{w: w, pen: pen, dict: map[string]string{}, hits: map[string]int{}}
}

// sigilFor assigns a sigil to a route after it has been seen a few times, capped
// at the pool size. Dictionary coding: hot paths collapse to one glyph. Once a
// route is assigned (or the pool is exhausted) it stops counting hits, so the
// hits map can't grow unbounded under a flood of distinct (high-cardinality)
// routes — e.g. http.route missing and url.path used as the fallback.
func (p *Printer) sigilFor(route string) string {
	if route == "" {
		return ""
	}
	if s := p.dict[route]; s != "" {
		return s
	}
	if len(p.dict) >= len(sigils) {
		return "" // pool exhausted — don't track further routes
	}
	if p.hits[route]++; p.hits[route] >= 3 {
		p.dict[route] = sigils[len(p.dict)]
		delete(p.hits, route) // assigned; no longer need the counter
		return p.dict[route]
	}
	return ""
}

// Print renders one event.
func (p *Printer) Print(ev model.Event) {
	switch e := ev.(type) {
	case model.SpanEvent:
		p.printSpan(e)
	case model.LogEvent:
		p.printLog(e)
	}
}

func (p *Printer) printSpan(e model.SpanEvent) {
	pal := p.pen.Palette()
	tid := p.pen.Paint(pal.Dim, short(e.TraceID))

	// HTTP span (method or route present) → a request drop.
	if e.Method != "" || e.Route != "" {
		glyph := methodGlyph[e.Method]
		if glyph == "" {
			glyph = "·"
		}
		hue := pal.StatusColor(e.Status)
		route := e.Route
		if sig := p.sigilFor(e.Route); sig != "" {
			route = sig + " " + e.Route
		}
		head := p.pen.Paint(hue, fmt.Sprintf("%s %3d", glyph, e.Status))
		errMark := ""
		if e.Err {
			errMark = p.pen.Paint(hue, " ✗")
		}
		fmt.Fprintf(p.w, "%s  %s  %-6s %-28s %8.1fms  %8s  %s%s\n",
			head,
			p.pen.Paint(pal.Dim, pad(e.Service, 9)),
			e.Method,
			pad(route, 28),
			e.MS,
			bytesLabel(e.Bytes),
			tid,
			errMark,
		)
		return
	}

	// Non-HTTP span (DB/cache/downstream) → trailing child droplet, shown by name.
	hue := pal.Dim
	if e.Err {
		hue = pal.StatusColor(500)
	}
	fmt.Fprintf(p.w, "   %s  %s  %-38s %8.1fms  %s\n",
		p.pen.Paint(hue, pad("└ "+kindShort(e.Kind), 11)),
		p.pen.Paint(pal.Dim, pad(e.Service, 9)),
		truncate(e.Name, 38),
		e.MS,
		tid,
	)
}

func (p *Printer) printLog(e model.LogEvent) {
	pal := p.pen.Palette()
	hue := pal.Severity(e.Sev)
	corr := p.pen.Paint(pal.Dim, short(e.TraceID))
	if e.TraceID == "" {
		corr = p.pen.Paint(pal.Dim, "(orphan)")
	}
	fmt.Fprintf(p.w, "%s  %s  %-38s %s\n",
		p.pen.Paint(hue, fmt.Sprintf("✺ %-5s", sevTag(e.Sev))),
		p.pen.Paint(pal.Dim, pad(e.Service, 9)),
		truncate(e.Body, 38),
		corr,
	)
}

// short returns the first 8 hex chars of an id, or a placeholder for empties.
func short(id string) string {
	if id == "" {
		return "--------"
	}
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

func kindShort(k int32) string {
	switch k {
	case 3:
		return "client"
	case 1:
		return "internal"
	case 4:
		return "producer"
	case 5:
		return "consumer"
	default:
		return "server"
	}
}

// sevTag labels an OTLP SeverityNumber range (severity is numeric, 1..24).
func sevTag(sev int) string {
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

func bytesLabel(b int64) string {
	switch {
	case b < 0:
		return "-"
	case b >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// pad right-pads (or truncates) s to n runes. Operates on the uncolored string,
// so it must be called before Paint wraps it in escape codes.
func pad(s string, n int) string {
	r := []rune(s)
	if len(r) >= n {
		return string(r[:n])
	}
	return s + strings.Repeat(" ", n-len(r))
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
