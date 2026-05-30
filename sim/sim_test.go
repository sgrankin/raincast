package sim

import (
	"testing"

	"github.com/sgrankin/raincast/model"
)

func newTestSim() *Sim {
	return New(Config{LaneKey: "trace", DictCap: 18, MinFall: 4, MaxFall: 16}, 80, 24)
}

func TestSpawnAndAdvance(t *testing.T) {
	s := newTestSim()
	s.Ingest(model.SpanEvent{Method: "GET", Route: "/x", Status: 200, Bytes: 100, MS: 40, TraceID: "abc"})
	if len(s.Drops()) != 1 {
		t.Fatalf("want 1 drop, got %d", len(s.Drops()))
	}
	d := s.Drops()[0]
	if d.Head != '↓' {
		t.Errorf("GET head = %q, want ↓", d.Head)
	}
	if d.Class != 2 {
		t.Errorf("class = %d, want 2", d.Class)
	}
	y0 := d.Y
	s.Advance(1.0) // one second
	if d.Y <= y0 {
		t.Errorf("drop did not fall: y %v -> %v", y0, d.Y)
	}
	// Vy should sit in the configured range.
	if d.Vy < 4 || d.Vy > 16 {
		t.Errorf("Vy = %v, want in [4,16]", d.Vy)
	}
}

func TestFallSpeedMonotonicInLatency(t *testing.T) {
	s := newTestSim()
	fast := s.fall(1)     // low latency -> brisk
	slow := s.fall(10000) // high latency -> crawl
	if !(fast > slow) {
		t.Errorf("expected fast(%v) > slow(%v)", fast, slow)
	}
}

func TestEviction(t *testing.T) {
	s := New(Config{}, 80, 10)
	s.Ingest(model.SpanEvent{Method: "GET", Route: "/x", Status: 200, MS: 1, TraceID: "t"})
	// Run long enough to fall well past the bottom.
	for i := 0; i < 200; i++ {
		s.Advance(0.1)
	}
	if len(s.Drops()) != 0 {
		t.Errorf("drop not evicted: %d remain", len(s.Drops()))
	}
	if _, ok := s.index["t"]; ok {
		t.Error("trace index not cleaned up after eviction")
	}
}

func TestDictionaryAssignsSigilAfterThreeHits(t *testing.T) {
	s := newTestSim()
	ev := model.SpanEvent{Method: "GET", Route: "/hot", Status: 200, TraceID: "t"}
	for i := 0; i < 2; i++ {
		s.Ingest(ev)
	}
	if s.DictSize() != 0 {
		t.Fatalf("sigil assigned too early: dict=%d", s.DictSize())
	}
	s.Ingest(ev) // third hit
	if s.DictSize() != 1 {
		t.Fatalf("sigil not assigned after 3 hits: dict=%d", s.DictSize())
	}
	// The route collapses to its learned sigil as the body's leading glyph
	// (the rest of the body is the byte-size tail, padded with noise).
	d := s.Drops()[len(s.Drops())-1]
	if d.Body[0] != '⊕' {
		t.Errorf("expected leading sigil ⊕, got %q in %q", d.Body[0], string(d.Body))
	}
}

func TestChildSpawnsInParentLane(t *testing.T) {
	s := New(Config{LaneKey: "trace", Children: true, MinFall: 4, MaxFall: 16}, 80, 24)
	// A request and a downstream child sharing the trace id.
	s.Ingest(model.SpanEvent{Method: "GET", Route: "/api/users", Status: 200, TraceID: "t1"})
	s.Ingest(model.SpanEvent{Name: "SELECT users", ParentID: "p", TraceID: "t1", Kind: 3, MS: 12})
	ds := s.Drops()
	if len(ds) != 2 {
		t.Fatalf("want request + child = 2 drops, got %d", len(ds))
	}
	if ds[0].Lane != ds[1].Lane {
		t.Errorf("child lane %d != request lane %d (should share, via trace hash)", ds[1].Lane, ds[0].Lane)
	}
	if !ds[1].Child {
		t.Error("downstream span should be a Child drop")
	}
	if ds[1].Head != childHead {
		t.Errorf("child head = %q, want %q", ds[1].Head, childHead)
	}
}

func TestChildrenDisabled(t *testing.T) {
	s := New(Config{LaneKey: "trace"}, 80, 24) // Children: false
	s.Ingest(model.SpanEvent{Name: "SELECT users", ParentID: "p", TraceID: "t1", Kind: 3})
	if len(s.Drops()) != 0 {
		t.Errorf("children disabled: want 0 drops, got %d", len(s.Drops()))
	}
}

func TestLogSparkInTraceLane(t *testing.T) {
	s := New(Config{LaneKey: "trace", MinFall: 4, MaxFall: 16}, 80, 24)
	s.Ingest(model.SpanEvent{Method: "GET", Route: "/x", Status: 200, TraceID: "t1"})
	s.Ingest(model.LogEvent{TraceID: "t1", Sev: 17, Body: "boom"}) // ERROR in the same trace
	ds := s.Drops()
	if len(ds) != 2 {
		t.Fatalf("want request + spark = 2, got %d", len(ds))
	}
	spark := ds[1]
	if spark.Sev != 17 {
		t.Errorf("spark Sev = %d, want 17", spark.Sev)
	}
	if spark.Head != sparkGlyph {
		t.Errorf("spark head = %q, want %q", spark.Head, sparkGlyph)
	}
	if spark.Lane != ds[0].Lane {
		t.Errorf("spark lane %d should match its trace's request lane %d", spark.Lane, ds[0].Lane)
	}
	// Logs don't drive the weather.
	if c := s.Weather().Counts; c[2] != 1 {
		t.Errorf("weather should count the request only: %v", c)
	}
}

func TestSigilLRUEviction(t *testing.T) {
	s := New(Config{DictCap: 2}, 80, 24)
	hit3 := func(route string) rune {
		var g rune
		for i := 0; i < 3; i++ {
			g = s.sigilFor(route)
		}
		return g
	}
	ga, gb := hit3("/a"), hit3("/b")
	if ga == 0 || gb == 0 || ga == gb {
		t.Fatalf("/a,/b should get distinct sigils, got %q %q", ga, gb)
	}
	if s.DictSize() != 2 {
		t.Fatalf("dict size %d, want 2", s.DictSize())
	}
	s.sigilFor("/b")       // touch /b so /a becomes least-recently-used
	gc := hit3("/c")       // /c earns a sigil → evicts /a, reuses its glyph
	if s.DictSize() != 2 { // pool stays capped
		t.Errorf("dict size %d after eviction, want 2", s.DictSize())
	}
	if gc != ga {
		t.Errorf("evicted /a's glyph should be reused for /c: gc=%q ga=%q", gc, ga)
	}
	if g := s.sigilFor("/a"); g != 0 {
		t.Errorf("/a was evicted, should return 0 until re-earned, got %q", g)
	}
}

func TestFloodCap(t *testing.T) {
	s := New(Config{MaxDrops: 3}, 80, 24)
	for i := 0; i < 10; i++ {
		s.Ingest(model.SpanEvent{Method: "GET", Route: "/x", Status: 200, TraceID: "t"})
	}
	if got := len(s.Drops()); got != 3 {
		t.Errorf("flood cap MaxDrops=3: %d drops, want 3", got)
	}
}

func TestForecastReactsTo5xx(t *testing.T) {
	s := newTestSim()
	for i := 0; i < 20; i++ {
		s.Ingest(model.SpanEvent{Method: "POST", Route: "/c", Status: 500, Err: true, TraceID: "t"})
	}
	f := s.Weather()
	if f.Counts[5] == 0 {
		t.Fatal("no 5xx counted")
	}
	if f.Text != "RED SQUALL · server bleeding" {
		t.Errorf("forecast = %q, want red squall", f.Text)
	}
}
