// Package theme defines the color language shared by every raincast renderer.
// The same palettes drive the milestone-1 stdout printer and, later, the tcell
// rain field — so the terminal output stays visually consistent across modes and
// with the browser prototype.
package theme

import (
	"fmt"
	"os"
	"strings"
)

// Mode selects a palette tuned for a dark or light terminal background.
type Mode int

const (
	Dark Mode = iota
	Light
)

func (m Mode) String() string {
	if m == Light {
		return "light"
	}
	return "dark"
}

// RGB is a 24-bit truecolor value.
type RGB struct{ R, G, B uint8 }

func (c RGB) ansiFG() string { return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.R, c.G, c.B) }

const reset = "\x1b[0m"

// Palette is the full color table for one mode. Status hues are keyed by the
// leading status digit (Status[2]..Status[5]); severity colors map OTLP
// SeverityNumber ranges.
type Palette struct {
	Status  [6]RGB // index by code/100; 2..5 populated
	SevLow  RGB    // <=12 trace/debug/info
	SevWarn RGB    // 13..16 warn
	SevErr  RGB    // >=17 error/fatal
	Dim     RGB    // labels, secondary text
	Fg      RGB    // primary text
}

// dark carries the browser prototype's neon-on-black hues verbatim.
var dark = Palette{
	Status:  [6]RGB{2: {0x00, 0xff, 0x66}, 3: {0x00, 0xe5, 0xff}, 4: {0xff, 0xb0, 0x00}, 5: {0xff, 0x2b, 0x4e}},
	SevLow:  RGB{0x6f, 0xcf, 0x9a},
	SevWarn: RGB{0xff, 0xb0, 0x00},
	SevErr:  RGB{0xff, 0x2b, 0x4e},
	Dim:     RGB{0x5f, 0x8f, 0x75},
	Fg:      RGB{0x7d, 0xff, 0xb0},
}

// light uses saturated, darker hues that stay legible on a light background —
// neon green/cyan wash out on white, so they shift to deep green and teal.
var light = Palette{
	Status:  [6]RGB{2: {0x0a, 0x7d, 0x33}, 3: {0x00, 0x6f, 0x9a}, 4: {0xa8, 0x5d, 0x00}, 5: {0xc2, 0x00, 0x18}},
	SevLow:  RGB{0x55, 0x55, 0x55},
	SevWarn: RGB{0xa8, 0x5d, 0x00},
	SevErr:  RGB{0xc2, 0x00, 0x18},
	Dim:     RGB{0x8a, 0x8a, 0x8a},
	Fg:      RGB{0x1a, 0x2a, 0x20},
}

// Of returns the palette for a mode.
func Of(m Mode) Palette {
	if m == Light {
		return light
	}
	return dark
}

// StatusColor returns the hue for any HTTP status code (2xx..5xx); other codes
// fall back to the primary foreground.
func (p Palette) StatusColor(code int) RGB {
	if c := code / 100; c >= 2 && c <= 5 {
		return p.Status[c]
	}
	return p.Fg
}

// Severity returns the spark color for an OTLP SeverityNumber (1..24).
func (p Palette) Severity(sev int) RGB {
	switch {
	case sev >= 17:
		return p.SevErr
	case sev >= 13:
		return p.SevWarn
	default:
		return p.SevLow
	}
}

// Detect resolves a mode from an explicit flag value ("dark", "light", or
// "auto"). For "auto" it tries, in order:
//
//  1. an OSC 11 background-color query to the terminal (only when interactive —
//     i.e. we're actually driving a terminal, not a pipe). This is the reliable
//     signal; most terminals answer it.
//  2. COLORFGBG ("fg;bg" or "fg;default;bg"); a trailing background of 7 or 15
//     means light. Legacy, and unset on most modern terminals.
//  3. Dark — the safe default for a full-screen rain animation.
func Detect(flag string, interactive bool) Mode {
	switch strings.ToLower(strings.TrimSpace(flag)) {
	case "light":
		return Light
	case "dark":
		return Dark
	}
	if interactive {
		if m, ok := queryBackgroundMode(); ok {
			return m
		}
	}
	if fgbg := os.Getenv("COLORFGBG"); fgbg != "" {
		parts := strings.Split(fgbg, ";")
		switch strings.TrimSpace(parts[len(parts)-1]) {
		case "7", "15":
			return Light
		}
	}
	return Dark
}

// Pen paints text with truecolor escapes, or passes it through unchanged when
// color is disabled (piped output, NO_COLOR, --color=never).
type Pen struct {
	pal     Palette
	enabled bool
}

func NewPen(m Mode, enabled bool) Pen { return Pen{pal: Of(m), enabled: enabled} }

// Palette exposes the underlying color table for callers that need raw hues.
func (p Pen) Palette() Palette { return p.pal }

// Paint wraps s in c's foreground escape (with reset) when color is enabled.
func (p Pen) Paint(c RGB, s string) string {
	if !p.enabled {
		return s
	}
	return c.ansiFG() + s + reset
}
