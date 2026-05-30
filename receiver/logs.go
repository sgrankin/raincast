// SPDX-License-Identifier: MIT

package receiver

import (
	"context"
	"encoding/hex"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"

	"github.com/sgrankin/raincast/model"
)

// logSvc implements LogsServiceServer. Correlation is in the data: each
// LogRecord carries top-level TraceId/SpanId, populated when the log was emitted
// inside an active span. An empty TraceId means the log fell outside any span.
type logSvc struct {
	collogspb.UnimplementedLogsServiceServer
	s *Server
}

func (l logSvc) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	for _, rl := range req.GetResourceLogs() {
		if rl == nil {
			continue
		}
		svc := serviceName(rl.GetResource().GetAttributes())
		for _, sl := range rl.GetScopeLogs() {
			if sl == nil {
				continue
			}
			for _, lr := range sl.GetLogRecords() {
				if lr == nil {
					continue
				}
				l.s.emit(model.LogEvent{
					Service:  svc,
					TraceID:  hex.EncodeToString(lr.TraceId),
					SpanID:   hex.EncodeToString(lr.SpanId),
					Sev:      int(lr.SeverityNumber),
					Body:     anyStr(lr.Body),
					TimeNano: lr.TimeUnixNano,
				})
			}
		}
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}
