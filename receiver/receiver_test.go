// SPDX-License-Identifier: MIT

package receiver

import (
	"context"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/sgrankin/raincast/model"
)

// A malformed or adversarial OTLP client can encode a nil entry in any repeated
// field. Decoding must skip it, never panic — the receiver's job is to stay up
// while ingesting untrusted telemetry.
func TestExportToleratesNilElements(t *testing.T) {
	events := make(chan model.Event, 16)
	ts := traceSvc{s: &Server{events: events}}
	ls := logSvc{s: &Server{events: events}}

	if _, err := ts.Export(context.Background(), &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			nil,
			{ScopeSpans: []*tracepb.ScopeSpans{nil, {Spans: []*tracepb.Span{nil}}}},
		},
	}); err != nil {
		t.Fatalf("traces Export returned error: %v", err)
	}

	if _, err := ls.Export(context.Background(), &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{
			nil,
			{ScopeLogs: []*logspb.ScopeLogs{nil, {LogRecords: []*logspb.LogRecord{nil}}}},
		},
	}); err != nil {
		t.Fatalf("logs Export returned error: %v", err)
	}
}

// The HTTP semantic conventions were renamed as they stabilized; which key set
// arrives depends on the emitting SDK. The decoder's fallback lists must resolve
// either set. This pins the OLD-key path (http.method / http.status_code).
func TestSpanDecodeSemconvDrift(t *testing.T) {
	events := make(chan model.Event, 4)
	ts := traceSvc{s: &Server{events: events}}

	span := &tracepb.Span{
		Name:              "GET /x",
		Kind:              tracepb.Span_SPAN_KIND_SERVER,
		TraceId:           []byte{0xab, 0xcd},
		StartTimeUnixNano: 1_000_000,
		EndTimeUnixNano:   6_000_000, // 5 ms
		Attributes: []*commonpb.KeyValue{
			kvStr("http.method", "GET"),
			kvInt("http.status_code", 200),
			kvStr("http.target", "/x"),
		},
	}
	if _, err := ts.Export(context.Background(), &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{span}}}},
		},
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	ev := (<-events).(model.SpanEvent)
	if ev.Method != "GET" {
		t.Errorf("Method = %q, want GET", ev.Method)
	}
	if ev.Status != 200 {
		t.Errorf("Status = %d, want 200", ev.Status)
	}
	if ev.Route != "/x" {
		t.Errorf("Route = %q, want /x", ev.Route)
	}
	if ev.MS != 5 {
		t.Errorf("MS = %v, want 5", ev.MS)
	}
	if ev.TraceID != "abcd" {
		t.Errorf("TraceID = %q, want abcd", ev.TraceID)
	}
}

func kvStr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func kvInt(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}
