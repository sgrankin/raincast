// Package model holds the normalized, render-agnostic telemetry events that the
// receiver produces and a renderer consumes. Keeping these free of any OTLP or
// rendering dependency is what lets the same events feed a stdout printer today
// and a tcell rain field later.
package model

// Event is one normalized piece of telemetry. The concrete types are SpanEvent
// and LogEvent; a consumer type-switches on them.
type Event interface{ isEvent() }

// SpanEvent is a single decoded span. The receiver's field lookups tolerate
// semconv drift, so these values resolve across SDK/semconv versions.
type SpanEvent struct {
	Service  string  // resource service.name
	TraceID  string  // hex; groups a request's spans and logs
	SpanID   string  // hex
	ParentID string  // hex, empty for roots
	Kind     int32   // OTLP span kind (1=internal, 2=server, 3=client, ...)
	Name     string  // span name — used when http.* attrs are absent (DB/cache/downstream)
	Method   string  // HTTP request method, empty for non-HTTP spans
	Route    string  // templated route (low cardinality), or url.path fallback
	Status   int     // HTTP response status code, 0 if absent
	Bytes    int64   // response body size, -1 if absent
	MS       float64 // server-side duration in milliseconds
	IP       string  // client.address, empty if masked/absent
	Err      bool    // span status == ERROR
}

func (SpanEvent) isEvent() {}

// LogEvent is a single decoded log record. TraceID/SpanID are populated when the
// log was emitted inside an active span, which is how logs correlate to drops.
type LogEvent struct {
	Service  string
	TraceID  string // empty if emitted outside a span
	SpanID   string
	Sev      int    // OTLP SeverityNumber, 1..24 (a number, NOT a string)
	Body     string
	TimeNano uint64
}

func (LogEvent) isEvent() {}
