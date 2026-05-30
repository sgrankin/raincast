// Command raintraffic is a synthetic OTLP traffic generator for exercising
// raincast. It models a small distributed system — a gateway fronting several
// backend services — and emits real OTLP traces and logs via the OTel SDK, so it
// drives the receiver over the actual wire format with genuine cross-service
// correlation. A few deliberate rough edges mirror production telemetry:
//
//   - the gateway emits NEW HTTP semconv keys, downstream "api" spans emit OLD
//     ones, so the receiver's drift shim gets a real workout;
//   - logs are emitted inside span context, so they carry TraceId/SpanId;
//   - a fraction of requests error, with span status + an error log corroborating;
//   - the request rate jitters and occasionally storms.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// rateLimitedErrors is the OTel global error handler. When raincast isn't
// listening, exports fail continuously; without rate-limiting they'd bury the
// terminal in connection errors. One line every few seconds is enough to tell
// you the receiver is down.
type rateLimitedErrors struct {
	mu   sync.Mutex
	last time.Time
}

func (h *rateLimitedErrors) Handle(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if time.Since(h.last) < 3*time.Second {
		return
	}
	h.last = time.Now()
	fmt.Fprintln(os.Stderr, "raintraffic: export failing (is raincast listening?):", err)
}

// route is one entry in the gateway's weighted endpoint table (ported from the
// browser prototype): a representative method/path with a typical status, body
// size, and latency, plus a selection weight.
type route struct {
	method string
	path   string
	status int
	bytes  int
	ms     int
	weight int
}

var routes = []route{
	{"GET", "/health", 200, 18, 2, 7},
	{"GET", "/api/users", 200, 2400, 45, 9},
	{"GET", "/api/orders", 200, 5100, 120, 6},
	{"POST", "/api/orders", 201, 310, 210, 3},
	{"GET", "/static/app.js", 200, 184000, 8, 5},
	{"GET", "/static/style.css", 200, 42000, 6, 4},
	{"GET", "/favicon.ico", 404, 0, 3, 3},
	{"POST", "/login", 200, 140, 260, 2},
	{"POST", "/login", 401, 90, 180, 2},
	{"POST", "/checkout", 200, 620, 540, 2},
	{"POST", "/checkout", 500, 0, 1900, 1},
	{"GET", "/admin", 403, 0, 12, 1},
	{"GET", "/api/search", 200, 9800, 1400, 3},
	{"GET", "/img/hero.png", 200, 512000, 14, 4},
	{"PATCH", "/api/cart", 200, 210, 60, 2},
	{"DELETE", "/api/cart", 204, 0, 40, 1},
	{"GET", "/old-page", 301, 0, 4, 2},
	{"GET", "/feed", 304, 0, 5, 3},
}

var (
	weightSum   int
	checkout500 route // the failing checkout row, resolved from routes (no duplicate literal)
)

func init() {
	for _, r := range routes {
		weightSum += r.weight
		if r.path == "/checkout" && r.status >= 500 {
			checkout500 = r
		}
	}
}

// pickRoute draws a weighted route; during a storm it heavily favors the failing
// checkout path so 5xx visibly surges.
func pickRoute(storm bool) route {
	if storm && rand.Float64() < 0.6 {
		return checkout500
	}
	n := rand.Intn(weightSum)
	for _, r := range routes {
		if n -= r.weight; n < 0 {
			return r
		}
	}
	return routes[0]
}

// call is a downstream hop made while serving a request.
type call struct {
	service string
	name    string
	ms      int
	errProb float64
	http    bool   // attach OLD-semconv http attrs to the callee span
	method  string // used when http
	target  string // request target path (http.target); just the path, no method
	status  int    // used when http
}

// downstreamsFor expands a route into the backend hops it triggers.
func downstreamsFor(r route, storm bool) []call {
	switch {
	case strings.HasPrefix(r.path, "/api/"), r.path == "/feed":
		return []call{
			{service: "api", name: r.method + " " + r.path, target: r.path, ms: max(1, r.ms/2), http: true, method: r.method, status: r.status},
			{service: "orders-db", name: dbOp(r.path), ms: max(1, r.ms/3), errProb: 0.02},
		}
	case r.path == "/login":
		return []call{{service: "auth", name: "auth.verify", ms: 80, errProb: 0.05}}
	case r.path == "/checkout":
		return []call{
			{service: "api", name: "POST /checkout", target: "/checkout", ms: 120, http: true, method: "POST", status: r.status},
			{service: "orders-db", name: "INSERT orders", ms: 200, errProb: 0.04},
			{service: "cache", name: "SET cart:session", ms: 15},
			{service: "payments", name: "charge card", ms: 400, errProb: pickF(storm, 0.3, 0.08)},
		}
	default:
		return nil
	}
}

func dbOp(path string) string {
	switch {
	case strings.Contains(path, "orders"):
		return "SELECT orders"
	case strings.Contains(path, "users"):
		return "SELECT users"
	case strings.Contains(path, "cart"):
		return "SELECT cart"
	case strings.Contains(path, "search"):
		return "SELECT products WHERE name LIKE ?"
	default:
		return "SELECT 1"
	}
}

func pickF(cond bool, a, b float64) float64 {
	if cond {
		return a
	}
	return b
}

var heavyIPs = []string{"203.0.113.7", "185.7.2.91", "45.33.1.8", "74.12.9.3"}

func clientIP() string {
	if rand.Float64() < 0.3 { // recurring "firehose" clients
		return heavyIPs[rand.Intn(len(heavyIPs))]
	}
	return fmt.Sprintf("%d.%d.%d.%d", rand.Intn(223)+1, rand.Intn(256), rand.Intn(256), rand.Intn(256))
}

var bots = []string{"bingbot/2.0", "GPTBot/1.0", "python-requests/2.31", "curl/8.4"}

func userAgent() string {
	if rand.Float64() < 0.18 {
		return bots[rand.Intn(len(bots))]
	}
	return "Mozilla/5.0"
}

// gen owns one tracer + logger per simulated service and the traffic shape.
type gen struct {
	tracers   map[string]trace.Tracer
	loggers   map[string]otellog.Logger
	baseRPS   float64
	timeScale float64
}

// request serves one synthetic request as a full cross-service trace rooted at
// the gateway, with correlated logs and proportional latency.
func (g *gen) request(storm bool) {
	r := pickRoute(storm)
	ctx, span := g.tracers["gateway"].Start(context.Background(), r.path,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			// NEW (stable) HTTP semconv keys on the gateway.
			attribute.String("http.request.method", r.method),
			attribute.String("http.route", r.path),
			attribute.String("url.path", r.path),
			attribute.String("client.address", clientIP()),
			attribute.String("user_agent.original", userAgent()),
		),
	)
	g.emitLog("gateway", ctx, otellog.SeverityInfo, "INFO", fmt.Sprintf("%s %s", r.method, r.path))

	g.sleep(max(1, r.ms/4)) // gateway's own work before fanning out
	for _, c := range downstreamsFor(r, storm) {
		g.downstream(ctx, c)
	}

	span.SetAttributes(
		attribute.Int("http.response.status_code", r.status),
		attribute.Int("http.response.body.size", r.bytes),
	)
	switch {
	case r.status >= 500:
		span.SetStatus(codes.Error, "internal server error")
		span.RecordError(errors.New("unhandled exception in handler"))
		g.emitLog("gateway", ctx, otellog.SeverityError, "ERROR",
			fmt.Sprintf("%s %s -> 500 unhandled exception", r.method, r.path))
	case r.status >= 400:
		g.emitLog("gateway", ctx, otellog.SeverityWarn, "WARN",
			fmt.Sprintf("%s %s -> %d", r.method, r.path, r.status))
	}
	span.End()
}

// downstream models a single backend hop: a CLIENT span on the caller wrapping a
// SERVER span on the callee, sharing the trace so children trail their parent.
func (g *gen) downstream(ctx context.Context, c call) {
	ctx, client := g.tracers["gateway"].Start(ctx, "→ "+c.service, trace.WithSpanKind(trace.SpanKindClient))
	cctx, server := g.tracers[c.service].Start(ctx, c.name, trace.WithSpanKind(trace.SpanKindServer))
	if c.http {
		// OLD semconv keys on purpose — this is the drift the receiver shim handles.
		server.SetAttributes(
			attribute.String("http.method", c.method),
			attribute.String("http.target", c.target), // path only — span name keeps the method
			attribute.Int("http.status_code", c.status),
		)
	}
	g.emitLog(c.service, cctx, otellog.SeverityDebug, "DEBUG", c.name)
	g.sleep(c.ms)
	if rand.Float64() < c.errProb {
		server.SetStatus(codes.Error, "downstream failure")
		server.RecordError(fmt.Errorf("%s failed", c.name))
		g.emitLog(c.service, cctx, otellog.SeverityError, "ERROR", c.name+" failed")
	}
	server.End()
	client.End()
}

// emitLog emits a log record in span context; the SDK fills in TraceId/SpanId
// from ctx, which is how the log correlates back to its request.
func (g *gen) emitLog(service string, ctx context.Context, sev otellog.Severity, sevText, body string) {
	logger, ok := g.loggers[service]
	if !ok {
		return
	}
	var rec otellog.Record
	rec.SetTimestamp(time.Now())
	rec.SetSeverity(sev)
	rec.SetSeverityText(sevText)
	rec.SetBody(otellog.StringValue(body))
	logger.Emit(ctx, rec)
}

func (g *gen) sleep(ms int) {
	if d := time.Duration(float64(ms) * float64(time.Millisecond) / g.timeScale); d > 0 {
		time.Sleep(d)
	}
}

// run drives requests at a jittering rate, occasionally storming. Each request
// runs in its own goroutine (it sleeps to simulate latency); a semaphore caps
// concurrency so a storm can't spawn unbounded goroutines.
func (g *gen) run(ctx context.Context) {
	const maxConcurrent = 512
	sem := make(chan struct{}, maxConcurrent)
	var stormUntil time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		if now.After(stormUntil) && rand.Float64() < 0.0025 {
			stormUntil = now.Add(time.Duration(3+rand.Intn(4)) * time.Second)
			fmt.Fprintln(os.Stderr, "raintraffic: ⛈ storm rolling in")
		}
		storm := now.Before(stormUntil)

		rps := g.baseRPS * (0.7 + 0.6*rand.Float64()) // ±30% jitter
		if storm {
			rps *= 3
		}
		if rps < 1 {
			rps = 1
		}

		select {
		case sem <- struct{}{}:
			go func(storm bool) {
				defer func() { <-sem }()
				g.request(storm)
			}(storm)
		default: // at the concurrency cap — skip this tick
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(float64(time.Second) / rps)):
		}
	}
}

func main() {
	endpoint := flag.String("endpoint", "localhost:4317", "OTLP/gRPC endpoint (host:port)")
	rps := flag.Float64("rps", 12, "base requests per second (before jitter and storms)")
	dur := flag.Duration("duration", 0, "run duration; 0 = until interrupted")
	timeScale := flag.Float64("time-scale", 1, "divide simulated latencies by this to speed up wall-clock")
	batch := flag.Duration("batch", time.Second, "exporter batch interval (simulates a real app's export batching; OTel's default is 5s)")
	flag.Parse()
	if *timeScale <= 0 {
		*timeScale = 1
	}

	// Quiet, rate-limited reporting when the receiver is down.
	otel.SetErrorHandler(&rateLimitedErrors{})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Retry disabled: this is a dev generator pointed at a local tool, so a dead
	// endpoint should fail fast (and shut down fast) rather than back off for a
	// minute.
	traceExp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(*endpoint),
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: false}),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "trace exporter:", err)
		os.Exit(1)
	}
	logExp, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(*endpoint),
		otlploggrpc.WithInsecure(),
		otlploggrpc.WithRetry(otlploggrpc.RetryConfig{Enabled: false}),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "log exporter:", err)
		os.Exit(1)
	}

	services := []string{"gateway", "api", "auth", "orders-db", "cache", "payments"}
	g := &gen{
		tracers:   map[string]trace.Tracer{},
		loggers:   map[string]otellog.Logger{},
		baseRPS:   *rps,
		timeScale: *timeScale,
	}
	var flushes, shutdowns []func(context.Context) error
	for _, s := range services {
		res := resource.NewSchemaless(attribute.String("service.name", s))
		// Batch interval simulates a real app's export batching. raincast's playout
		// buffer (--replay-delay) reconstructs real timing from span timestamps, so
		// this no longer needs to be artificially short to avoid wave artifacts.
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(traceExp, sdktrace.WithBatchTimeout(*batch)),
			sdktrace.WithResource(res),
		)
		lp := sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp, sdklog.WithExportInterval(*batch))),
			sdklog.WithResource(res),
		)
		g.tracers[s] = tp.Tracer("raintraffic")
		g.loggers[s] = lp.Logger("raintraffic")
		flushes = append(flushes, tp.ForceFlush, lp.ForceFlush)
		shutdowns = append(shutdowns, tp.Shutdown, lp.Shutdown)
	}

	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}

	fmt.Fprintf(os.Stderr, "raintraffic → %s  base=%.0frps  services=%v\n", *endpoint, *rps, services)
	g.run(ctx)

	// Release the SIGINT handler now that run has returned: if the flush below
	// hangs on a dead endpoint, a second Ctrl-C should force-quit (default
	// behavior) rather than be swallowed by NotifyContext.
	stop()

	// Flush every provider through the shared exporters before shutting anything
	// down (one Shutdown per provider would otherwise close a shared exporter
	// out from under its siblings). Note: in-flight request goroutines (capped at
	// maxConcurrent, mostly mid-sleep) may End a few spans in the window between
	// flush and shutdown; for a generator that small tail loss is acceptable.
	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, f := range flushes {
		_ = f(shutCtx)
	}
	for _, sd := range shutdowns {
		_ = sd(shutCtx)
	}
}
