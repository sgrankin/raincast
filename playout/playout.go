// Package playout is a jitter/playout buffer for telemetry events. Instrumented
// apps export spans in batches, so events arrive in bursts that don't reflect
// the real request timing. The buffer accepts events as they arrive and releases
// them at their true relative times, delayed by a fixed depth: as long as an
// event arrives within the delay of its own timestamp, it plays out at the right
// moment. Later arrivals are released immediately (the signal that the delay is
// too short). A delay <= 0 disables buffering (events play on arrival).
package playout

import (
	"container/heap"
	"time"

	"github.com/sgrankin/raincast/model"
)

// Buffer reorders and time-shifts events into real-time playout.
type Buffer struct {
	delay time.Duration
	pq    eventHeap
	have  bool
	base  uint64    // first event's timestamp (unix nanos) — the event-time origin
	wall  time.Time // wall clock when the first event arrived — the wall-time origin
}

// New returns a buffer with the given playout delay.
func New(delay time.Duration) *Buffer { return &Buffer{delay: delay} }

// Add buffers an event that arrived at wall time now.
func (b *Buffer) Add(ev model.Event, now time.Time) {
	if !b.have {
		b.have, b.base, b.wall = true, ev.When(), now
	}
	heap.Push(&b.pq, ev)
}

// Release pops every event whose scheduled display time has arrived, in
// event-time order. An event with timestamp T displays at wall
// origin + delay + (T - base), so at wall=now everything with
// T <= base + (now-origin) - delay is due.
func (b *Buffer) Release(now time.Time) []model.Event {
	if !b.have {
		return nil
	}
	thresh := int64(b.base) + now.Sub(b.wall).Nanoseconds() - b.delay.Nanoseconds()
	var out []model.Event
	for b.pq.Len() > 0 && int64(b.pq[0].When()) <= thresh {
		out = append(out, heap.Pop(&b.pq).(model.Event))
	}
	return out
}

// Pending reports how many events are still buffered.
func (b *Buffer) Pending() int { return b.pq.Len() }

// eventHeap is a min-heap of events ordered by timestamp.
type eventHeap []model.Event

func (h eventHeap) Len() int           { return len(h) }
func (h eventHeap) Less(i, j int) bool { return h[i].When() < h[j].When() }
func (h eventHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x any)        { *h = append(*h, x.(model.Event)) }
func (h *eventHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
