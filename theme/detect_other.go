// SPDX-License-Identifier: MIT

//go:build !unix

package theme

// queryBackgroundMode is unsupported off unix (no /dev/tty to query); callers
// fall back to COLORFGBG and then the dark default.
func queryBackgroundMode() (Mode, bool) { return Dark, false }
