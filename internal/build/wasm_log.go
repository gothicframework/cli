package helpers

import (
	"fmt"
	"time"

	"github.com/gothicframework/cli/v3/internal/termcolor"
)

// Shorthands sourced from the shared Gothic palette (pkg/helpers/termcolor): the
// deploy output, the OpenTofu colorizer and these WASM build lines all draw from
// one definition, so the whole CLI shares a single palette.
const (
	ansiReset       = termcolor.Reset
	ansiBold        = termcolor.Bold
	ansiWhite       = termcolor.White  // timestamp
	ansiCyan        = termcolor.Cyan   // name
	ansiRed         = termcolor.Red
	ansiYellow      = termcolor.Yellow // compression method
	ansiLightGreen  = termcolor.Green  // final size / success
	ansiBlue        = termcolor.Blue   // raw size
	ansiPurpleLight = termcolor.Purple // WASM tag / accent
	ansiGray        = termcolor.Gray   // dim — arrows
)

const wasmTag = ansiBold + ansiPurpleLight + "WASM" + ansiReset

func wasmTimestamp() string {
	return ansiWhite + time.Now().Format("2006/01/02 15:04:05") + ansiReset
}

func wasmUpToDate(name string) {
	fmt.Printf(wasmTimestamp()+" "+wasmTag+" "+ansiCyan+"%s"+ansiReset+ansiGray+" → "+ansiReset+ansiLightGreen+"up to date"+ansiReset+"\n", name)
}

func wasmLogf(format string, args ...any) {
	fmt.Printf(wasmTimestamp()+" "+wasmTag+" "+ansiCyan+format+ansiReset+"\n", args...)
}

func wasmErrorf(format string, args ...any) {
	fmt.Printf(wasmTimestamp()+" "+wasmTag+" "+ansiRed+format+ansiReset+"\n", args...)
}

func wasmWarnf(format string, args ...any) {
	fmt.Printf(wasmTimestamp()+" "+wasmTag+" "+ansiYellow+format+ansiReset+"\n", args...)
}

// wasmBuildResult prints the coloured build-result line:
//
//	2006/01/02 15:04:05 WASM <name> → <rawSize> → <finalSize> (<compression>)
//
// name: white  raw size: blue  final size: light green  compression: yellow
func wasmBuildResult(name, rawSize, finalSize, compression string) {
	fmt.Printf(
		wasmTimestamp()+" "+wasmTag+" "+
			ansiCyan+"%s"+ansiReset+
			ansiGray+" → "+ansiReset+
			ansiBlue+"%s"+ansiReset+
			ansiGray+" → "+ansiReset+
			ansiLightGreen+"%s"+ansiReset+
			" "+ansiYellow+"(%s)"+ansiReset+
			"\n",
		name, rawSize, finalSize, compression,
	)
}
