// Command raincast renders OTLP telemetry as terminal rain. Milestone 1: listen
// for OTLP/gRPC, decode SERVER/CLIENT spans and log records, and print them as a
// themed, glyph-annotated stream (the tcell rain field lands in a later milestone).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sgrankin/raincast/console"
	"github.com/sgrankin/raincast/model"
	"github.com/sgrankin/raincast/receiver"
	"github.com/sgrankin/raincast/theme"
)

func main() {
	listen := flag.String("listen", ":4317", "OTLP/gRPC listen address")
	themeFlag := flag.String("theme", "auto", "color theme: dark, light, or auto (COLORFGBG)")
	colorFlag := flag.String("color", "auto", "color output: always, never, or auto (tty + NO_COLOR)")
	bufSize := flag.Int("buffer", 1024, "event buffer depth before backpressure drops")
	flag.Parse()

	color := colorEnabled(*colorFlag, os.Stdout)
	mode := theme.Detect(*themeFlag, color) // OSC 11 query only when interactive
	pen := theme.NewPen(mode, color)
	printer := console.NewPrinter(os.Stdout, pen)

	// Single consumer goroutine owns all printing — no locks needed.
	events := make(chan model.Event, *bufSize)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			printer.Print(ev)
		}
	}()

	srv := receiver.New(events)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(*listen) }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "raincast listening on %s (OTLP/gRPC) — theme=%s\n", *listen, mode)

	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "\nshutting down…")
	case err := <-errc:
		if err != nil {
			fmt.Fprintln(os.Stderr, "serve error:", err)
		}
	}

	// Stop() drains in-flight RPCs first, so no emit races the channel close.
	srv.Stop()
	close(events)
	<-done
	if d := srv.Dropped(); d > 0 {
		fmt.Fprintf(os.Stderr, "dropped %d events under backpressure\n", d)
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
