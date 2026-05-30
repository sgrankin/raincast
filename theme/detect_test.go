// SPDX-License-Identifier: MIT

package theme

import "testing"

func TestParseOSC11(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Mode
		ok   bool
	}{
		{"black, 16-bit, BEL", "\x1b]11;rgb:0000/0000/0000\x07", Dark, true},
		{"white, 16-bit, ST", "\x1b]11;rgb:ffff/ffff/ffff\x1b\\", Light, true},
		{"near-white, 8-bit", "\x1b]11;rgb:fa/fa/fa\x07", Light, true},
		{"dark gray", "\x1b]11;rgb:2020/2020/2020\x07", Dark, true},
		{"solarized-ish dark teal", "\x1b]11;rgb:0026/002b/0036\x07", Dark, true},
		{"no rgb", "\x1b]11;?\x07", Dark, false},
		{"garbage", "not a reply at all", Dark, false},
		{"wrong field count", "\x1b]11;rgb:ffff/ffff\x07", Dark, false},
	}
	for _, c := range cases {
		r, g, b, ok := parseOSC11(c.in)
		if ok != c.ok {
			t.Errorf("%s: ok=%v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok {
			if got := modeForBackground(r, g, b); got != c.want {
				t.Errorf("%s: mode=%v, want %v", c.name, got, c.want)
			}
		}
	}
}
