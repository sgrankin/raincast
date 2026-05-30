// Package sim is the render-agnostic simulation behind the rain field. It turns
// normalized telemetry events into falling drops and advances their physics. It
// deliberately has no tcell or color dependency — a renderer reads Drops() and
// maps the logical status class to a palette. A single goroutine (the game loop)
// owns a *Sim, so it holds no locks.
package sim

import (
	"hash/fnv"
	"math"
	"math/rand"
	"strings"
	"time"

	"github.com/sgrankin/raincast/model"
)

// headGlyphs maps HTTP methods to head glyphs (from the prototype).
var headGlyphs = map[string]rune{
	"GET": '↓', "POST": '▼', "PUT": '⇅', "PATCH": '∿',
	"DELETE": '✕', "HEAD": '∘', "OPTIONS": '⌥',
}

// sigils are handed out to hot routes — same pool as the prototype.
var sigils = []rune{'⊕', '♡', '⌑', '◈', '✦', '⟁', '⌬', '⎔', '◉', '⊗', '✧', '⟐', '◌', '⍟', '⌖', '❖', '⊚', '✺'}

// noise glyphs fill the tail and corrupt errored drops (half-width katakana +
// punctuation, all single display-width so the cell grid stays aligned).
var noise = []rune("ｱｲｳｴｵｶｷｸ0101ｿﾀﾁｦﾇ<>=/{}#$%&*")

// noiseRune returns a random filler glyph for drop tails. The set is all single
// display-width, keeping the cell grid aligned.
func noiseRune() rune { return noise[rand.Intn(len(noise))] }

const (
	// evapStartFrac is how far down a 404 drop falls before it evaporates rather
	// than falling all the way.
	evapStartFrac = 0.55
	// spawnStaggerRows is the random head offset above the top at spawn, so a
	// batch of requests arriving in one frame doesn't enter as a flat horizontal
	// rank (the OTLP exporter batches, so arrivals are bursty).
	spawnStaggerRows = 4.0
)

// Drop is one falling request (and, later, child droplets / sparks). Y is a
// float row position (top = 0); the renderer floors it. Color is expressed as a
// logical status Class (2..5) so sim stays theme-free.
type Drop struct {
	Lane  int
	Y     float64
	Vy    float64 // cells/sec
	Head  rune
	Body  []rune
	Class int // status/100; 0 for non-HTTP
	Alpha float64
	Err   bool
	Evap  bool // 404s evaporate partway down
	Child bool // a trailing child droplet (downstream span), not a request
	Sev   int  // >0 marks a log spark, colored by OTLP SeverityNumber

	TraceID string
	Kind    int32
}

// Config tunes lane assignment, the dictionary cap, and the fall-speed range.
type Config struct {
	LaneKey          string // "trace" (default) or "client"
	DictCap          int
	MinFall, MaxFall float64 // cells/sec; slowest, fastest
	Children         bool    // spawn child droplets for downstream (non-HTTP) spans
}

// Sim owns all mutable rain state.
type Sim struct {
	cfg        Config
	cols, rows int
	drops      []*Drop
	dict       map[string]rune // route -> sigil
	hits       map[string]int  // route -> hit count (until assigned)
	index      map[string]*Drop
	weather    *weather
}

// New builds a Sim for a field of cols×rows cells.
func New(cfg Config, cols, rows int) *Sim {
	if cfg.DictCap <= 0 || cfg.DictCap > len(sigils) {
		cfg.DictCap = len(sigils)
	}
	if cfg.MinFall <= 0 {
		cfg.MinFall = 4
	}
	if cfg.MaxFall <= 0 {
		cfg.MaxFall = 16
	}
	if cfg.MinFall > cfg.MaxFall { // tolerate a flipped flag pair
		cfg.MinFall, cfg.MaxFall = cfg.MaxFall, cfg.MinFall
	}
	return &Sim{
		cfg: cfg, cols: cols, rows: rows,
		dict: map[string]rune{}, hits: map[string]int{}, index: map[string]*Drop{},
		weather: newWeather(),
	}
}

// Resize updates the field dimensions. Drops keep their lane; the renderer hides
// any that now fall outside the column range until they evict naturally —
// clamping them here would collapse many lanes onto the last column on a shrink.
func (s *Sim) Resize(cols, rows int) {
	s.cols, s.rows = cols, rows
}

// Drops returns the live drops (read-only for the renderer).
func (s *Sim) Drops() []*Drop { return s.drops }

// Weather returns the current forecast for the HUD.
func (s *Sim) Weather() Forecast { return s.weather.forecast() }

// DictSize is the number of routes that have earned a sigil.
func (s *Sim) DictSize() int { return len(s.dict) }

// Ingest turns one event into rain. For now only HTTP request spans spawn drops;
// child spans and logs land in later milestones.
func (s *Sim) Ingest(ev model.Event) {
	switch e := ev.(type) {
	case model.SpanEvent:
		switch {
		case e.Method != "" || e.Route != "":
			s.spawnRequest(e)
		case s.cfg.Children && e.ParentID != "" && e.TraceID != "":
			// A downstream (non-HTTP) span with a parent: a trailing droplet.
			s.spawnChild(e)
		}
	case model.LogEvent:
		s.spawnLog(e)
	}
}

func (s *Sim) spawnRequest(e model.SpanEvent) {
	if s.cols <= 0 {
		return
	}
	head := headGlyphs[e.Method]
	if head == 0 {
		head = '·'
	}

	var body []rune
	if sig := s.sigilFor(e.Route); sig != 0 {
		body = []rune{sig}
	} else {
		r := []rune(strings.TrimPrefix(e.Route, "/"))
		if len(r) > 9 {
			r = r[:9]
		}
		if len(r) == 0 {
			r = []rune{'/'}
		}
		body = r
	}
	// tail grows (log-scaled) with response size
	bytes := e.Bytes
	if bytes < 0 {
		bytes = 0
	}
	tail := 1 + int(math.Round(math.Log10(float64(bytes)+10)))
	if tail > 14 {
		tail = 14
	}
	for len(body) < tail {
		body = append(body, noiseRune())
	}

	d := &Drop{
		Lane:    s.lane(e.TraceID, e.IP),
		Y:       -rand.Float64() * spawnStaggerRows,
		Vy:      s.fall(e.MS),
		Head:    head,
		Body:    body,
		Class:   e.Status / 100,
		Alpha:   1,
		Err:     e.Err,
		Evap:    e.Status == 404,
		TraceID: e.TraceID,
		Kind:    e.Kind,
	}
	s.drops = append(s.drops, d)
	if e.TraceID != "" {
		s.index[e.TraceID] = d
	}
	s.weather.add(d.Class)
}

// childHead leads a child droplet — a small dot, since downstream spans have no
// HTTP method to glyph. sparkGlyph leads a log spark.
const (
	childHead  = '·'
	sparkGlyph = '✦'
)

// spawnLog adds a log spark: a single severity-colored glyph that falls in its
// trace's lane (so it sparks down the same column as its request), or a random
// lane when the log has no trace (an orphan log scattered into the field).
// Replay time-orders logs with their spans, so no separate late-arrival buffer
// is needed. Logs don't count toward the weather.
func (s *Sim) spawnLog(e model.LogEvent) {
	if s.cols <= 0 {
		return
	}
	sev := e.Sev
	if sev <= 0 {
		sev = 9 // default to INFO so Sev>0 reliably marks a spark
	}
	s.drops = append(s.drops, &Drop{
		Lane:    s.lane(e.TraceID, ""), // trace's column, or random if orphan
		Y:       -rand.Float64() * spawnStaggerRows,
		Vy:      s.fall(0), // sparks fall fast (no duration)
		Head:    sparkGlyph,
		Body:    nil, // a single bright cell
		Sev:     sev,
		Alpha:   1,
		TraceID: e.TraceID,
	})
}

// spawnChild adds a trailing droplet for a downstream span. It shares the
// parent's lane (both hash the same trace_id), so a request and its children
// fall in one column — the trace waterfall. Its body is the span name (e.g.
// "SELECT orders", "→ cache"). Color is muted (the renderer dims it) unless the
// span errored. Children don't count toward the weather (that's requests only).
func (s *Sim) spawnChild(e model.SpanEvent) {
	if s.cols <= 0 {
		return
	}
	name := e.Name
	if name == "" {
		name = "·"
	}
	body := []rune(strings.ReplaceAll(name, " ", "·")) // no gaps in the vertical stack
	if len(body) > 12 {
		body = body[:12]
	}
	for len(body) < 3 { // a small minimum droplet
		body = append(body, noiseRune())
	}
	class := 0
	if e.Err {
		class = 5
	}
	d := &Drop{
		Lane:    s.lane(e.TraceID, e.IP),
		Y:       -rand.Float64() * spawnStaggerRows,
		Vy:      s.fall(e.MS),
		Head:    childHead,
		Body:    body,
		Class:   class,
		Alpha:   1,
		Err:     e.Err,
		Child:   true,
		TraceID: e.TraceID,
		Kind:    e.Kind,
	}
	s.drops = append(s.drops, d)
}

// fall maps latency to fall speed: low ms = brisk, high ms = a slow crawl. Units
// are cells/sec (corrected from the spec's px/sec). See docs/design.md.
func (s *Sim) fall(ms float64) float64 {
	frac := math.Log10(ms+1) / 3.3
	if frac < 0 {
		frac = 0
	} else if frac > 1 {
		frac = 1
	}
	return s.cfg.MinFall + (1-frac)*(s.cfg.MaxFall-s.cfg.MinFall)
}

// lane hashes the lane key to a column. trace_id (default) spreads requests
// evenly and keeps a trace's children/logs together; client.address makes a
// hammering client a visible firehose.
func (s *Sim) lane(traceID, ip string) int {
	key := traceID
	if s.cfg.LaneKey == "client" && ip != "" {
		key = ip
	}
	if key == "" {
		return rand.Intn(s.cols)
	}
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32() % uint32(s.cols))
}

// sigilFor assigns a sigil after a route is seen a few times, capped at DictCap.
// Once assigned (or the pool is full) it stops counting, bounding the hits map.
func (s *Sim) sigilFor(route string) rune {
	if route == "" {
		return 0
	}
	if r, ok := s.dict[route]; ok {
		return r
	}
	if len(s.dict) >= s.cfg.DictCap {
		return 0
	}
	if s.hits[route]++; s.hits[route] >= 3 {
		r := sigils[len(s.dict)]
		s.dict[route] = r
		delete(s.hits, route)
		return r
	}
	return 0
}

// Advance steps physics by dt seconds and evicts finished drops.
func (s *Sim) Advance(dt float64) {
	s.weather.prune(time.Now())
	kept := s.drops[:0]
	for _, d := range s.drops {
		d.Y += d.Vy * dt
		if d.Evap && d.Y > float64(s.rows)*evapStartFrac {
			d.Alpha -= 0.06
		}
		// Keep a drop until its whole tail has cleared the bottom, so trails fall
		// off-screen rather than vanishing the instant the head exits.
		offBottom := d.Y-float64(len(d.Body)+1) > float64(s.rows)
		if offBottom || d.Alpha <= 0 {
			if d.TraceID != "" && s.index[d.TraceID] == d {
				delete(s.index, d.TraceID)
			}
			continue
		}
		kept = append(kept, d)
	}
	s.drops = kept
}
