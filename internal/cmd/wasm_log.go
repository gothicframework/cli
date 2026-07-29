package cmd

import (
	"fmt"

	"github.com/gothicframework/cli/v3/internal/output"
	"github.com/gothicframework/cli/v3/internal/termcolor"
)

// ansi* functions resolve at call time via termcolor.Code so the enable check
// (NO_COLOR, pipe, CI) is honoured on every call.
func ansiReset() string       { return termcolor.Code(termcolor.Reset) }
func ansiBold() string        { return termcolor.Code(termcolor.Bold) }
func ansiRed() string         { return termcolor.Code(termcolor.Red) }
func ansiPurpleLight() string { return termcolor.Code(termcolor.Purple) }
func ansiWhite() string       { return termcolor.Code(termcolor.White) }
func ansiLightGreen() string  { return termcolor.Code(termcolor.Green) }
func ansiGray() string        { return termcolor.Code(termcolor.Gray) }

// wasmTag returns the coloured "WASM" tag.
func wasmTag() string {
	return ansiPurpleLight() + "WASM" + ansiReset()
}

// wasmTimestamp returns the dim timestamp prefix, in the layout every other
// line of the stream uses.
func wasmTimestamp() string {
	return ansiGray() + output.Timestamp() + ansiReset()
}

// wasmLogf prints through the output package so these lines share the one
// stdout mutex and the one dedup state with the rest of the CLI.
func wasmLogf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	output.Print(msg, wasmTimestamp()+" "+wasmTag()+" "+ansiWhite()+msg+ansiReset())
}

// wasmCount formats a count+label pair: the number in the count colour, the
// label back in the message colour so the text around it is unaffected.
func wasmCount(n int, label string) string {
	return fmt.Sprintf(ansiLightGreen()+"%d"+ansiWhite()+" %s", n, label)
}

func wasmErrorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	output.PrintRaw(wasmTimestamp() + " " + wasmTag() + " " + ansiRed() + msg + ansiReset())
}
