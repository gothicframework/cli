package helpers

import (
	"fmt"

	"github.com/gothicframework/cli/v3/internal/output"
	"github.com/gothicframework/cli/v3/internal/termcolor"
)

// All WASM lines go through the output package, which owns the single stdout
// mutex and the single dedup state shared with the rest of the CLI.

// ansi* functions resolve at call time via termcolor.Code so the enable check
// (NO_COLOR, pipe, CI) is honoured on every call.
func ansiReset() string       { return termcolor.Code(termcolor.Reset) }
func ansiBold() string        { return termcolor.Code(termcolor.Bold) }
func ansiWhite() string       { return termcolor.Code(termcolor.White) }
func ansiCyan() string        { return termcolor.Code(termcolor.Cyan) }
func ansiRed() string         { return termcolor.Code(termcolor.Red) }
func ansiYellow() string      { return termcolor.Code(termcolor.Yellow) }
func ansiLightGreen() string  { return termcolor.Code(termcolor.Green) }
func ansiBlue() string        { return termcolor.Code(termcolor.Blue) }
func ansiPurpleLight() string { return termcolor.Code(termcolor.Purple) }
func ansiGray() string        { return termcolor.Code(termcolor.Gray) }

// wasmTag returns the coloured "WASM" tag.
func wasmTag() string {
	return ansiPurpleLight() + "WASM" + ansiReset()
}

// wasmTimestamp returns the timestamp prefix. Dim, like every other timestamp
// in the stream: it repeats on each line and is never the point.
func wasmTimestamp() string {
	return ansiGray() + output.Timestamp() + ansiReset()
}

// wasmPath renders an artifact name in the location colour, the same one file
// paths carry elsewhere in the stream: both answer "which one". Resumes the
// message colour so the words after it are unaffected.
func wasmPath(name string) string {
	return ansiBlue() + name + ansiReset() + ansiWhite()
}

// wasmNum renders a quantity in the count colour, resuming the message colour so
// the words around it are unaffected. Mirrors the numbers in the "building N
// page(s)" line, so every count in the stream reads the same.
func wasmNum(n int32) string {
	return ansiLightGreen() + fmt.Sprint(n) + ansiReset() + ansiWhite()
}

// wasmLogf prints a formatted log line with WASM colour and (xN) dedup.
// Dedup is keyed on the message text only, so the timestamp and the colour
// codes never decide whether two lines count as identical.
func wasmLogf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	line := wasmTimestamp() + " " + wasmTag() + " " + ansiWhite() + msg + ansiReset()
	output.Print(msg, line)
}

func wasmErrorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	output.PrintRaw(wasmTimestamp() + " " + wasmTag() + " " + ansiRed() + msg + ansiReset())
}

func wasmWarnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	output.PrintRaw(wasmTimestamp() + " " + wasmTag() + " " + ansiYellow() + msg + ansiReset())
}

// wasmBuildResult prints the coloured build-result line:
//
//	2006/01/02 15:04:05 WASM <name> → <rawSize> → <finalSize> (<compression>)
//
// name: white  raw size: blue  final size: light green  compression: yellow
func wasmBuildResult(name, rawSize, finalSize, compression string) {
	output.PrintRaw(fmt.Sprintf(
		wasmTimestamp()+" "+wasmTag()+" "+
			ansiWhite()+"%s"+ansiReset()+
			ansiGray()+" → "+ansiReset()+
			ansiBlue()+"%s"+ansiReset()+
			ansiGray()+" → "+ansiReset()+
			ansiLightGreen()+"%s"+ansiReset()+
			" "+ansiYellow()+"(%s)"+ansiReset(),
		name, rawSize, finalSize, compression,
	))
}
