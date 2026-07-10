// Package termcolor is the single source of truth for the Gothic CLI's terminal
// colors. Every colored CLI log — the WASM build lines, the deploy output, and the
// OpenTofu stream colorizer — draws from this one palette and this one enable
// check, so the whole tool reads as a single, coherent surface.
package termcolor

import "os"

// Enabled reports whether ANSI color should be emitted. It is resolved once at
// process start: true only for an interactive terminal, and false under NO_COLOR /
// GOTHIC_NO_COLOR or when stdout is redirected (a pipe, a file, CI) so those logs
// stay clean.
var Enabled = supported()

func supported() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("GOTHIC_NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// The canonical Gothic palette. 256-color codes are used for the accent, dim,
// green and blue so the look is consistent across capable terminals; the primary
// colors stay on the 16-color codes for maximum compatibility.
const (
	Reset = "\033[0m"
	Bold  = "\033[1m"
	Under = "\033[4m"

	White  = "\033[37m"       // timestamps / neutral
	Cyan   = "\033[36m"       // names, values, URLs
	Red    = "\033[31m"       // errors, destroy
	Yellow = "\033[33m"       // warnings, update/replace
	Green  = "\033[38;5;120m" // success, create
	Blue   = "\033[38;5;75m"  // secondary numbers
	Purple = "\033[38;5;183m" // Gothic accent / rules / the WASM tag
	Gray   = "\033[38;5;244m" // dim / secondary text / diff comments
)

// Paint wraps s in the given ANSI codes, or returns it unchanged when color is
// disabled.
func Paint(codes, s string) string {
	if !Enabled {
		return s
	}
	return codes + s + Reset
}

// Code returns the ANSI code when color is enabled, or "" when it is not. It lets
// callers that build format strings with inline escapes (rather than wrapping a
// finished string) still honor the enable check: an empty code is a no-op.
func Code(code string) string {
	if !Enabled {
		return ""
	}
	return code
}
