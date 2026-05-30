// Package receiver implements an OTLP/gRPC receiver. It decodes inbound traces
// and logs into normalized model.Events and forwards them on a channel. The
// Export RPCs must return fast — a slow receiver backpressures the instrumented
// app's export queue — so emit never blocks: under flood it drops the incoming
// event (drop-newest) and counts it rather than stall the caller.
package receiver

import (
	"context"
	"net"
	"sync/atomic"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sgrankin/raincast/model"
)

// Server hosts the gRPC endpoint and owns the outbound event channel. The two
// OTLP services are registered as distinct handler types (traceSvc, logSvc)
// because both declare an Export method — Go has no overloading, so a single
// type cannot implement both service interfaces.
type Server struct {
	events  chan<- model.Event
	grpc    *grpc.Server
	dropped atomic.Uint64
}

// New builds a Server that forwards decoded events to the given channel.
func New(events chan<- model.Event) *Server {
	// recoverUnary backstops the decoders: a malformed/adversarial OTLP message
	// must fail its one RPC, never panic the whole receiver. The decode loops
	// also skip nil elements, but this guards anything they miss.
	s := &Server{events: events, grpc: grpc.NewServer(grpc.UnaryInterceptor(recoverUnary))}
	coltracepb.RegisterTraceServiceServer(s.grpc, traceSvc{s: s})
	collogspb.RegisterLogsServiceServer(s.grpc, logSvc{s: s})
	return s
}

func recoverUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = status.Errorf(codes.Internal, "raincast: recovered from panic decoding %s: %v", info.FullMethod, r)
		}
	}()
	return handler(ctx, req)
}

// Serve listens on addr and blocks, serving until Stop is called.
func (s *Server) Serve(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.grpc.Serve(lis)
}

// Stop drains in-flight RPCs and stops serving. After it returns, no handler is
// running, so the owner can safely close the event channel.
func (s *Server) Stop() { s.grpc.GracefulStop() }

// emit performs a non-blocking send. A full buffer means the consumer can't keep
// up; we drop the new event and count it rather than block the Export RPC.
func (s *Server) emit(ev model.Event) {
	select {
	case s.events <- ev:
	default:
		s.dropped.Add(1)
	}
}

// Dropped reports how many events were discarded under backpressure.
func (s *Server) Dropped() uint64 { return s.dropped.Load() }
