// SPDX-License-Identifier: MIT

package theme

import (
	"strconv"
	"strings"
)

// parseOSC11 extracts the background color from a terminal's OSC 11 reply, which
// looks like `ESC ] 11 ; rgb:RRRR/GGGG/BBBB` terminated by BEL or ST. Each
// component is 1–4 hex digits and is scaled to a 0..1 fraction of its own width
// (so "ff" and "ffff" both mean full intensity).
func parseOSC11(s string) (r, g, b float64, ok bool) {
	i := strings.Index(s, "rgb:")
	if i < 0 {
		return 0, 0, 0, false
	}
	rest := s[i+len("rgb:"):]
	if j := strings.IndexAny(rest, "\x07\x1b"); j >= 0 { // strip BEL or ST/ESC tail
		rest = rest[:j]
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var f [3]float64
	for k, p := range parts {
		v, vok := hexFrac(p)
		if !vok {
			return 0, 0, 0, false
		}
		f[k] = v
	}
	return f[0], f[1], f[2], true
}

func hexFrac(h string) (float64, bool) {
	h = strings.TrimSpace(h)
	if h == "" || len(h) > 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return 0, false
	}
	return float64(v) / float64(uint64(1)<<(4*len(h))-1), true
}

// modeForBackground picks a theme from a background color by relative luminance.
func modeForBackground(r, g, b float64) Mode {
	if 0.2126*r+0.7152*g+0.0722*b > 0.5 {
		return Light
	}
	return Dark
}
