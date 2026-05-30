package receiver

import (
	"context"
	"encoding/hex"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/sgrankin/raincast/model"
)

// traceSvc implements TraceServiceServer. SERVER spans become request drops;
// CLIENT/INTERNAL spans become trailing child droplets once a sim wires them up.
// Child spans frequently lack http.* attributes, so Name carries the operation
// (e.g. "SELECT orders", "→ cache").
type traceSvc struct {
	coltracepb.UnimplementedTraceServiceServer
	s *Server
}

func (t traceSvc) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	for _, rs := range req.GetResourceSpans() {
		if rs == nil {
			continue
		}
		svc := serviceName(rs.GetResource().GetAttributes())
		for _, ss := range rs.GetScopeSpans() {
			if ss == nil {
				continue
			}
			for _, sp := range ss.GetSpans() {
				if sp == nil {
					continue
				}
				a := newAttrs(sp.GetAttributes())
				t.s.emit(model.SpanEvent{
					Service:  svc,
					TraceID:  hex.EncodeToString(sp.TraceId),
					SpanID:   hex.EncodeToString(sp.SpanId),
					ParentID: hex.EncodeToString(sp.ParentSpanId),
					Kind:     int32(sp.Kind),
					Name:     sp.Name,
					Method:   a.str("http.request.method", "http.method"),
					Route:    a.str("http.route", "url.path", "http.target"),
					Status:   int(a.intOr(0, "http.response.status_code", "http.status_code")),
					Bytes:    a.intOr(-1, "http.response.body.size", "http.response_content_length"),
					MS:       float64(sp.EndTimeUnixNano-sp.StartTimeUnixNano) / 1e6,
					IP:       a.str("client.address", "net.peer.ip"),
					Err:      sp.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR,
				})
			}
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}
