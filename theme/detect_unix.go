// SPDX-License-Identifier: MIT

//go:build unix

package theme

import (
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// queryBackgroundMode asks the terminal for its background color via an OSC 11
// query and classifies it by luminance. Returns ok=false when there's no
// controlling terminal or it doesn't answer within the timeout. This is the
// signal that actually works on modern terminals (Ghostty, kitty, iTerm2,
// xterm, …) — most of them never set COLORFGBG, so without this query "auto"
// can only ever guess the default.
func queryBackgroundMode() (Mode, bool) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return Dark, false
	}
	defer tty.Close()
	fd := int(tty.Fd())
	if !term.IsTerminal(fd) {
		return Dark, false
	}
	// Raw mode so the reply isn't echoed or line-buffered; restored before return.
	old, err := term.MakeRaw(fd)
	if err != nil {
		return Dark, false
	}
	defer term.Restore(fd, old)

	if _, err := tty.WriteString("\x1b]11;?\x07"); err != nil {
		return Dark, false
	}

	ch := make(chan string, 1)
	go func() {
		buf := make([]byte, 0, 64)
		tmp := make([]byte, 32)
		for {
			n, err := tty.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
				s := string(buf)
				// Stop as soon as a full OSC 11 reply has arrived.
				if strings.Contains(s, "rgb:") && (strings.IndexByte(s, '\x07') >= 0 || strings.Contains(s, "\x1b\\")) {
					break
				}
			}
			if err != nil {
				break
			}
		}
		ch <- string(buf)
	}()

	select {
	case data := <-ch:
		if r, g, b, ok := parseOSC11(data); ok {
			return modeForBackground(r, g, b), true
		}
		return Dark, false
	case <-time.After(300 * time.Millisecond):
		// Terminal ignored the query. The read goroutine stays parked on /dev/tty
		// until the process exits — harmless for a one-shot startup probe.
		return Dark, false
	}
}
