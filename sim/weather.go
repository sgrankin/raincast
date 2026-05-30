package sim

import "time"

// windowDur is how far back the forecast looks for status ratios; rps uses the
// most recent second within it.
const (
	windowDur = 5 * time.Second
	rpsDur    = 1 * time.Second
)

// Forecast is the HUD readout derived from recent traffic.
type Forecast struct {
	Text   string // human forecast line
	RPS    int    // requests in the last second
	Counts [6]int // by status class, indices 2..5
}

type wevent struct {
	class int
	t     time.Time
}

// weather is a rolling window of recent request statuses.
type weather struct {
	events []wevent
}

func newWeather() *weather { return &weather{} }

func (w *weather) add(class int) { w.events = append(w.events, wevent{class, time.Now()}) }

// prune drops events older than the window. Called each tick.
func (w *weather) prune(now time.Time) {
	cut := now.Add(-windowDur)
	i := 0
	for i < len(w.events) && !w.events[i].t.After(cut) {
		i++
	}
	if i > 0 {
		w.events = w.events[i:]
	}
}

// forecast classifies the window the same way the prototype does: 5xx rate wins,
// then 4xx rate, then volume.
func (w *weather) forecast() Forecast {
	now := time.Now()
	rcut := now.Add(-rpsDur)
	var counts [6]int
	rps := 0
	for _, e := range w.events {
		if e.class >= 2 && e.class <= 5 {
			counts[e.class]++
		}
		if e.t.After(rcut) {
			rps++
		}
	}
	tot := counts[2] + counts[3] + counts[4] + counts[5]
	if tot == 0 {
		tot = 1
	}
	e5 := float64(counts[5]) / float64(tot)
	e4 := float64(counts[4]) / float64(tot)

	var text string
	switch {
	case e5 > 0.18:
		text = "RED SQUALL · server bleeding"
	case e5 > 0.06:
		text = "scattered 5xx showers"
	case e4 > 0.22:
		text = "amber haze · client errors"
	case rps > 14:
		text = "heavy green downpour"
	case rps < 4:
		text = "clear & calm"
	default:
		text = "steady green drizzle"
	}
	return Forecast{Text: text, RPS: rps, Counts: counts}
}
