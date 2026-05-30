# raincast — OTLP telemetry rain (terminal)

A console program that listens for **OTLP/gRPC** telemetry (traces, logs, metrics)
from your app and renders it as Matrix-style rain. Each inbound request is a
falling drop; its child spans trail behind it; its log lines spark off it in
severity colors; metrics drive the weather readout. No log parsing — the data
arrives typed and structured.

There is a working **browser prototype** ([`prototype.html`](prototype.html),
canvas + glow) that defines the visual language and the synthetic-traffic model.
raincast reuses that model and swaps the render layer for a terminal cell grid
and the data source for an OTLP receiver. The prototype is the visual source of
truth — copy its glyph pools and thresholds verbatim so the terminal and web
versions stay consistent. (It predates the rename; its title still reads
"RAINGREP".)

> This doc is the living design. It started as a prototyping-session braindump
> and has been adjusted to match what's built and the decisions made since.

---

## Status

- ✅ **Receiver** — OTLP/gRPC server for traces **and** logs, decoding into
  normalized events. (Logs were pulled forward from a later milestone: the
  traffic generator emits them, and an unimplemented logs endpoint would just
  spam export errors.)
- ✅ **Console renderer** — a milestone-1 stand-in: one themed, glyph-annotated
  line per event. Same encoding as the rain field, no animation. Kept behind a
  flag as a debug view.
- ✅ **Theme** — dark + light palettes with `--theme auto` detection via an
  OSC 11 background query (COLORFGBG fallback).
- ✅ **Synthetic generator** (`cmd/raintraffic`) — multi-service OTLP traffic on
  the real OTel SDK, for driving the display without a real app.
- 🔨 **Rain display** — the tcell game loop. In progress.
- ⏳ **Metrics** — deferred; span-count rps is good enough for the weather.

---

## Architecture

```
your Go app ──(OTLP/gRPC)──▶  :4317  ──▶  receiver  (N gRPC handler goroutines)
                                            │  normalized model.Events
                                            ▼  buffered chan  (drop-newest)
                                          game loop  (single goroutine)
                                            │  owns ALL drop/sim state — no locks
                                            │  drain events → advance physics → paint
                                            ▼  ~30fps
                                          tcell screen
```

Two layers:

1. **receiver** — a gRPC server implementing the OTLP `Export` RPCs. Decodes
   spans/logs into a small normalized event type and pushes onto a buffered
   channel. **Must return from `Export` fast** — never block on a full channel,
   or you backpressure the instrumented app. Non-blocking send; under flood,
   **drop-newest** (a non-blocking channel send naturally drops the event that
   can't fit, which keeps what's already buffered — see Backpressure).

2. **game loop** — a single goroutine that owns all mutable state (drops, route
   dictionary, trace→drop index, rolling weather window, the per-cell brightness
   buffer). Each tick it drains the event channel non-blocking, advances physics,
   and paints. No mutexes because only this goroutine touches state.

> **Why one loop, not separate sim + render goroutines?** The sim owns the state
> and the renderer needs to read it; splitting them forces either a per-frame
> snapshot handoff or shared locks. Merging removes the channel, the allocation,
> and the question entirely — both run at the same ~30fps anyway. Render-agnostic
> reuse is preserved by keeping the `model` types and `theme` palettes free of any
> tcell dependency (which is what already lets the console renderer and the rain
> field share them).

### Wiring it to the app
- **Local dev:** point the SDK straight at raincast:
  `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317`,
  `OTEL_EXPORTER_OTLP_PROTOCOL=grpc`.
- **App already ships to a real backend:** don't redirect it. Run an **OTel
  Collector** and add a second OTLP exporter to each pipeline pointed at
  `localhost:4317`, so you tee a copy and leave prod telemetry untouched.

---

## The encoding (carried over from the prototype)

The OTel HTTP semantic conventions map almost 1:1 onto the drop model. Two fields
are *better* than an access log: `http.route` is the templated (low-cardinality)
route, and span duration is real server-side latency.

| Drop property | OTel source (with fallbacks) | Visual |
|---|---|---|
| head glyph | `http.request.method` → `http.method` | ↓ GET, ▼ POST, ⇅ PUT, ∿ PATCH, ✕ DELETE, ∘ HEAD, ⌥ OPTIONS |
| color | `http.response.status_code` → `http.status_code` | 2xx green, 3xx cyan, 4xx amber, 5xx red (palette per theme) |
| body sigil | `http.route` → `url.path` | route → assigned sigil after N hits (dictionary) |
| fall speed | span `EndTimeUnixNano − StartTimeUnixNano` | low ms = brisk fall, high ms = slow crawl |
| tail length | `http.response.body.size` | log-scaled, longer tail for bigger responses |
| lane | see Lane assignment | column on screen |
| error flavor | span `Status.Code == ERROR` and/or `exception` events | red bleed + in-place glyph corruption |

Glyph/sigil pools and forecast thresholds live in the prototype (and are mirrored
in `console/console.go`); copy them verbatim.

### Fall speed — units (corrected)

The prototype's `vy` is **60–200 px/s** at a ~13px font, i.e. roughly **5–15
cells/s** in terminal terms. The original spec mis-carried these as "60–200
cells/s" — at 60 cells/s a drop crosses a 40-row terminal in under a second, far
too fast to read. Use cells/s directly:

```
frac = clamp(log10(ms+1) / 3.3, 0, 1)        // 0 = instant, 1 = very slow
fall = minFall + (1-frac) * (maxFall-minFall) // cells/s; low ms → fast
```

Defaults: `--min-fall 4` (slowest), `--max-fall 16` (fastest) cells/s.

### Lane assignment (configurable, `--lane-key`)
- **`trace` (default):** hash `trace_id` → column. Each request gets its own lane
  → even spread → the "field of rain" look. Also keeps a request's children and
  logs in one column for free.
- **`client`:** hash `client.address` → column. One client = one lane → a
  hammering client becomes a visible firehose. `client.address` is sometimes
  missing/masked, and dev traffic from few IPs clusters into a few columns.

---

## Rendering the rain (terminal specifics)

A cell grid has none of the canvas tricks the prototype leans on — no `shadowBlur`
glow, no sub-cell vertical position, no alpha blending of overlapping drops, and
critically no `fillRect` persistence fade for trails. The signature look has to be
rebuilt from cell primitives:

- **Decaying brightness buffer.** Keep a `[][]float` (or a small struct per cell)
  that the head writes bright and that decays each frame. Trailing cells render by
  sampling this buffer through the palette — that dissolving wake, not the drop
  objects alone, is what reads as *rain*. Without it you get falling text.
- **Integer rows, float physics.** Advance `y` as a float (cells/s × dt) and floor
  it for the row to draw.
- **Truecolor + 256 fallback.** Detect truecolor (`COLORTERM=truecolor`); fall back
  to a 256-color palette otherwise.
- **Resize.** On a tcell resize event, recompute column count; clamp/retire drops in
  now-out-of-range lanes; resize the brightness buffer.

### Logs as sparks
- **Has an in-flight parent drop** (resolve `TraceID` in the trace→drop index):
  emit the log glyph **in the parent's lane, near its current y**, colored by
  severity. Severity color **overrides** the request's status hue — surfacing the
  "200 OK but logged an ERROR inside" case the status code alone can't show.
- **No resolvable parent** (orphan, or span not arrived yet): fall as a **lone
  severity-colored glyph in a random lane**, scattered into the field. A FATAL with
  no trace is the app screaming on the way down — give it a fat red drop.
- **Late arrival:** logs sometimes export *before* their span (batching reorders).
  Buffer a log with a non-empty `TraceID` but unknown parent for ~1–2s and retry the
  index lookup before releasing it to the field. Optionally hash its `TraceID`→lane
  so a late log still shares its request's column.

Severity is numeric (`SeverityNumber` 1–24), **not** a string: ≤12 dim, 13–16
amber, ≥17 red.

### Child spans as trailing droplets
SERVER spans are request drops. CLIENT/INTERNAL spans with a parent in an
in-flight trace are child droplets trailing the parent (the trace waterfall).
They frequently lack http.* attrs — use `span.Name` (`SELECT orders`,
`→ cache`) as the body text. Gate behind `--children` (default on).

---

## Theme

Dark and light palettes (`theme/`). `--theme dark|light|auto`. **auto** resolves
in order:

1. **OSC 11 query** — ask the terminal for its background color and classify by
   luminance. Only when output is interactive. This is the signal that actually
   works; most modern terminals (Ghostty, iTerm2, kitty, …) never set COLORFGBG.
2. **COLORFGBG** — legacy fallback (`fg;bg`; trailing 7/15 ⇒ light).
3. **Dark** — default.

Light mode uses saturated, darker hues (neon green/cyan wash out on white).

---

## Backpressure

Never block `Export`. Buffered channel + non-blocking send; under flood, the send
fails and we **drop-newest** and count it. (A non-blocking channel send drops the
incoming event, not the oldest buffered one — which is what we want: keep the
backlog, shed the surplus. The original spec said "drop oldest"; that would need a
ring buffer for no real benefit here.) The receiver returning slowly = your real
app's export queue backing up, so fast-return is the whole game.

---

## Metrics (deferred)

The stable `http.server.request.duration` histogram could give real rps for the
weather. But its temporality matters: **cumulative** (the common SDK default) means
count is a monotonic total you must diff between points; **delta** means per-interval
counts you sum. Counting SERVER spans over a rolling window gives rps without that
ambiguity, so metrics are deferred until span-count rps proves insufficient.

---

## Sim state (game-loop owned)

- `drops []*Drop` — active request drops and their child droplets.
- `index map[string]*Drop` — `trace_id` → in-flight request drop; TTL-evicted
  shortly after the drop leaves the screen, so late children/logs can still attach.
- `dict` — `route` → sigil; assigned after N hits; **capped + LRU-evicted** so a
  flood of distinct routes (e.g. `http.route` missing → `url.path` with IDs) can't
  blow up cardinality.
- `weather` — rolling window of statuses + rps → forecast string.
- `orphanLogs` — short buffer for logs awaiting their span.
- `bright [][]float` — per-cell brightness buffer driving the trails.

---

## Edge cases / gotchas

- **Never block `Export`.** Non-blocking send, drop-newest, count drops.
- **Nil elements in repeated fields.** A malformed/adversarial OTLP message can
  encode a nil entry at any level (ResourceSpans/ScopeSpans/Span/…). Decode loops
  skip nils; a gRPC recovery interceptor backstops anything missed — the receiver
  must stay up while ingesting untrusted telemetry.
- **Semconv drift.** Always use fallback key lists (`receiver/attrs.go`). HTTP
  conventions were renamed (`http.method`→`http.request.method`, etc.); which set
  you get depends on the emitting SDK.
- **Severity is a number**, not `"ERROR"` — map ranges.
- **Log-before-span reordering** — buffer + retry, then field.
- **Missing `http.route`** — falls back to `url.path` (high cardinality) → rely on
  the dictionary cap.
- **`client.address` masked/absent** — affects `--lane-key client`; default `trace`.
- **Terminal resize** — recompute columns, clamp/retire out-of-range drops, resize
  brightness buffer.
- **Color support** — detect truecolor; 256-color fallback.

---

## Remaining milestones

1. ✅ Receiver (traces + logs) → normalized events → console.
2. 🔨 **Rain display** — tcell game loop: port encoding + physics, brightness-buffer
   trails, lane assignment, dictionary, weather HUD. Consumes the live event stream
   directly (the receiver + generator already exist, so there's no synthetic-only
   phase). *Verify:* it rains, routes collapse to sigils, weather updates, resize works.
3. **Logs as sparks** — off parents via the trace index; orphan/late logs as field
   glyphs by severity.
4. **Child spans as trailing droplets** — the trace waterfall.
5. **Polish** — sampling under flood, 256-color fallback, pause, graceful shutdown,
   LRU dictionary eviction.
6. **Metrics → real rps** (only if span-count rps proves insufficient).

---

## CLI

Built (`raincast`):
```
--listen   :4317              OTLP/gRPC listen address
--theme    auto|dark|light    palette (auto = OSC 11 query, then COLORFGBG, then dark)
--color    auto|always|never  ANSI output (auto = tty + NO_COLOR)
--buffer   1024               event buffer depth before drop-newest
--print                       (planned) use the line printer instead of the rain field
```

Planned for the rain field:
```
--lane-key trace|client       lane assignment (default trace)
--fps      30                  render frames per second
--children true                render child spans as trailing droplets
--dict-cap 18                  max distinct route sigils before LRU eviction
--min-fall 4                   slowest fall (cells/s)
--max-fall 16                  fastest fall (cells/s)
```

Generator (`cmd/raintraffic`):
```
--endpoint   localhost:4317   OTLP/gRPC endpoint
--rps        12               base requests/sec (before jitter/storms)
--duration   0                run duration; 0 = until interrupted
--time-scale 1                divide simulated latencies (speed up wall-clock)
```

---

## Layout (as built)

```
raincast/
  main.go               flags; wire receiver + consumer; signal handling
  model/event.go        render-agnostic SpanEvent / LogEvent
  receiver/
    server.go           grpc server, register services, drop-newest emit, recover interceptor
    traces.go           Export → SpanEvent (nil-safe)
    logs.go             Export → LogEvent (nil-safe)
    attrs.go            semconv-drift attribute shim (fallback key lists)
    receiver_test.go    nil-tolerance + drift decode
  theme/
    theme.go            Mode, palettes, Pen, Detect
    detect.go           OSC 11 reply parsing + luminance classification
    detect_unix.go      /dev/tty OSC 11 query (build-tagged unix)
    detect_other.go     no-op fallback
    detect_test.go      OSC 11 parser tests
  console/console.go    milestone-1 line renderer (debug view)
  sim/                  (planned) drop state, physics, dictionary, weather, frame builder
  render/               (planned) tcell loop, brightness buffer, palette → cells, input
  cmd/raintraffic/      synthetic OTLP traffic generator
```

The end state: a request falls, its DB/cache/downstream child droplets trail it
in-lane, its log lines spark off it in severity colors, and if it 500s the whole
column goes red at once — span status, error logs, and recorded exception all
corroborating each other as it hits the floor.
