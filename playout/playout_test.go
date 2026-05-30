package playout

import (
	"testing"
	"time"

	"github.com/sgrankin/raincast/model"
)

func span(nanos uint64) model.SpanEvent { return model.SpanEvent{EndNano: nanos} }

func TestReplaysAtRelativeTimes(t *testing.T) {
	const s = uint64(time.Second)
	base := uint64(1_000 * time.Second) // arbitrary epoch
	b := New(time.Second)               // 1s playout delay

	t0 := time.Unix(0, 0)
	// A burst of three events spread 1s apart in event-time, all arriving at once.
	b.Add(span(base), t0)
	b.Add(span(base+1*s), t0)
	b.Add(span(base+2*s), t0)

	// During the initial delay window nothing is due.
	if got := b.Release(t0); len(got) != 0 {
		t.Fatalf("at t0: released %d, want 0 (buffering)", len(got))
	}
	// At +1.5s, only the earliest (base) is due.
	got := b.Release(t0.Add(1500 * time.Millisecond))
	if len(got) != 1 || got[0].When() != base {
		t.Fatalf("at +1.5s: %v, want [base]", times(got, base))
	}
	// At +2.5s, the second.
	got = b.Release(t0.Add(2500 * time.Millisecond))
	if len(got) != 1 || got[0].When() != base+1*s {
		t.Fatalf("at +2.5s: %v, want [base+1s]", times(got, base))
	}
	// At +3.5s, the third.
	got = b.Release(t0.Add(3500 * time.Millisecond))
	if len(got) != 1 || got[0].When() != base+2*s {
		t.Fatalf("at +3.5s: %v, want [base+2s]", times(got, base))
	}
	if b.Pending() != 0 {
		t.Errorf("buffer not drained: %d pending", b.Pending())
	}
}

func TestReleasesInTimeOrderDespiteArrivalOrder(t *testing.T) {
	const s = uint64(time.Second)
	base := uint64(1_000 * time.Second)
	b := New(time.Second)
	t0 := time.Unix(0, 0)
	// Arrive out of order (log exported before its later-ending span, etc.).
	b.Add(span(base+2*s), t0)
	b.Add(span(base), t0)
	b.Add(span(base+1*s), t0)

	var order []uint64
	for _, when := range []time.Duration{1500, 2500, 3500} {
		for _, ev := range b.Release(t0.Add(when * time.Millisecond)) {
			order = append(order, ev.When()-base)
		}
	}
	want := []uint64{0, uint64(time.Second), 2 * uint64(time.Second)}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Errorf("release order (offsets) = %v, want %v", order, want)
	}
}

func times(evs []model.Event, base uint64) []int64 {
	out := make([]int64, len(evs))
	for i, e := range evs {
		out[i] = int64(e.When()) - int64(base)
	}
	return out
}
