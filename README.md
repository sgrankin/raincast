# raincast

Watch your service's OpenTelemetry traffic fall as Matrix-style rain in the terminal.

raincast listens for **OTLP/gRPC** telemetry (traces + logs) and renders it as a
field of falling drops: each request is a drop, its downstream spans fall in the
same column (the trace waterfall), its WARN+ logs spark off it in severity colors,
and a weather readout up top tells you — at a glance, from across the room —
whether your service is calm or bleeding 5xx.

No log parsing: the data arrives typed and structured, so `http.route` gives clean
low-cardinality routes and span duration is real server-side latency.

> **Status:** working prototype. See [docs/design.md](docs/design.md) for the full
> design, and [docs/prototype.html](docs/prototype.html) for the browser mock that
> defined the visual language.

## Quick start

Requires Go 1.26+ and a truecolor terminal.

```sh
# terminal 1 — the display (listens on :4317)
go run .

# terminal 2 — synthetic multi-service traffic to watch
go run ./cmd/raintraffic
```

`q`/`Esc` quit, `space` pauses.

### Pointing a real app at it

Local dev — send the SDK straight at raincast:

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317
export OTEL_EXPORTER_OTLP_PROTOCOL=grpc
```

Already shipping to a backend? Don't redirect it — run an OTel Collector and add a
second OTLP exporter pointed at `localhost:4317` to tee a copy, leaving prod
telemetry untouched.

## The encoding

| telemetry | rain |
|---|---|
| HTTP method | head glyph (↓ GET, ▼ POST, ∿ PATCH, …) |
| status code | color (2xx green, 3xx cyan, 4xx amber, 5xx red) |
| route (`http.route`) | a learned sigil after a few hits |
| span duration | fall speed (a slow request is a slow drop) |
| response size | tail length |
| trace id | column — a request's children and logs share its lane |
| downstream spans | child droplets falling in the trace's column |
| logs (WARN+) | severity-colored sparks |
| rps / error rates | the weather forecast up top |

## Useful flags

`raincast`:

- `--theme auto|dark|light` — `auto` detects the terminal background via an OSC 11 query.
- `--min-contrast 1.1` — match your terminal's minimum-contrast setting so dim trail
  glyphs fade out cleanly instead of being boosted toward white. (`raincast --diag`
  shows a color diagnostic if the trails look wrong.)
- `--replay-delay 2s` — reconstruct real request timing from span timestamps, smoothing
  out the app's export batching. Set it `>=` the app's batch interval; `0` spawns on arrival.
- `--log-level warn` — minimum severity to spark (`off|trace|debug|info|warn|error|fatal`).
- `--log-panel N` — reserve N bottom rows to tail decoded events as text.
- also `--children`, `--lane-key trace|client`, `--dict-cap`, `--min-fall`/`--max-fall`,
  `--max-drops`, `--print` (line output instead of rain).

`cmd/raintraffic` (synthetic generator): `--rps`, `--batch` (export batching to mimic a
real app), `--n-plus-one` (an N+1 query burst on `/api/dashboard`), `--time-scale`,
`--duration`. Run either with `--help` for the full list.

## Layout

- `main.go`, `receiver/` — OTLP/gRPC receiver + decode (tolerant of semconv drift)
- `sim/` — render-agnostic drop physics, route→sigil dictionary, weather
- `render/` — tcell game loop, brightness-buffer trails, weather HUD, log panel
- `playout/` — jitter buffer for timestamp-based replay
- `theme/` — dark/light palettes, OSC 11 detection, WCAG contrast
- `console/` — `--print` line renderer
- `cmd/raintraffic/` — synthetic multi-service OTLP generator
