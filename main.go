// Command raincast renders OTLP telemetry as terminal rain. It listens for
// OTLP/gRPC, decodes spans and log records into normalized events, and paints
// them as a falling field (or, with --print, as a themed line stream).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gdamore/tcell/v3"

	"github.com/sgrankin/raincast/console"
	"github.com/sgrankin/raincast/model"
	"github.com/sgrankin/raincast/receiver"
	"github.com/sgrankin/raincast/render"
	"github.com/sgrankin/raincast/sim"
	"github.com/sgrankin/raincast/theme"
)

func main() {
	listen := flag.String("listen", ":4317", "OTLP/gRPC listen address")
	themeFlag := flag.String("theme", "auto", "color theme: dark, light, or auto (OSC 11, then COLORFGBG)")
	colorFlag := flag.String("color", "auto", "--print color: always, never, or auto (tty + NO_COLOR)")
	bufSize := flag.Int("buffer", 1024, "event buffer depth before backpressure drops")
	printMode := flag.Bool("print", false, "print one line per event instead of the rain field")
	fps := flag.Int("fps", 30, "render frames per second")
	laneKey := flag.String("lane-key", "trace", "lane assignment: trace or client")
	dictCap := flag.Int("dict-cap", 18, "max distinct route sigils before the pool fills")
	minFall := flag.Float64("min-fall", 4, "slowest fall (cells/s)")
	maxFall := flag.Float64("max-fall", 16, "fastest fall (cells/s)")
	_ = flag.Bool("children", true, "render child spans as trailing droplets (not yet wired)")
	flag.Parse()

	events := make(chan model.Event, *bufSize)
	srv := receiver.New(events)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(*listen) }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var runErr error
	if *printMode {
		runErr = runPrinter(ctx, events, *themeFlag, *colorFlag)
	} else {
		cfg := sim.Config{LaneKey: *laneKey, DictCap: *dictCap, MinFall: *minFall, MaxFall: *maxFall}
		runErr = runRain(ctx, events, *themeFlag, *fps, cfg)
	}

	// The consumer has returned (quit or signal); drain in-flight RPCs, then close.
	srv.Stop()
	close(events)
	if d := srv.Dropped(); d > 0 {
		fmt.Fprintf(os.Stderr, "dropped %d events under backpressure\n", d)
	}
	select {
	case err := <-errc:
		if err != nil {
			fmt.Fprintln(os.Stderr, "serve error:", err)
		}
	default:
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, runErr)
		os.Exit(1)
	}
}

// runRain owns the terminal and paints the rain field until the user quits or ctx
// is cancelled.
func runRain(ctx context.Context, events <-chan model.Event, themeFlag string, fps int, cfg sim.Config) error {
	mode := theme.Detect(themeFlag, true) // query the terminal before tcell takes over
	scr, err := tcell.NewScreen()
	if err != nil {
		return fmt.Errorf("init terminal: %w (try --print for line output)", err)
	}
	r := render.New(scr, theme.Of(mode), fps)
	s := sim.New(cfg, 0, 0)
	return r.Run(ctx, events, s)
}

// runPrinter drains events to a themed line stream until ctx is cancelled.
func runPrinter(ctx context.Context, events <-chan model.Event, themeFlag, colorFlag string) error {
	color := colorEnabled(colorFlag, os.Stdout)
	mode := theme.Detect(themeFlag, color)
	printer := console.NewPrinter(os.Stdout, theme.NewPen(mode, color))
	fmt.Fprintf(os.Stderr, "raincast: OTLP/gRPC, theme=%s — printing events (ctrl-c to quit)\n", mode)
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-events:
			printer.Print(ev)
		}
	}
}

// colorEnabled decides whether to emit ANSI escapes: an explicit flag wins,
// otherwise auto means "stdout is a terminal and NO_COLOR is unset".
func colorEnabled(flagVal string, f *os.File) bool {
	switch flagVal {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
