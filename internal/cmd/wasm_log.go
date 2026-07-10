package cmd

import (
	"fmt"
	"time"

	"github.com/gothicframework/cli/v3/internal/termcolor"
)

// Shorthands sourced from the shared Gothic palette (pkg/helpers/termcolor) so
// every colored CLI log draws from one place.
const (
	ansiReset       = termcolor.Reset
	ansiBold        = termcolor.Bold
	ansiWhite       = termcolor.White
	ansiRed         = termcolor.Red
	ansiPurpleLight = termcolor.Purple
	ansiCyan        = termcolor.Cyan
	ansiLightGreen  = termcolor.Green
)

const wasmTag = ansiBold + ansiPurpleLight + "WASM" + ansiReset

func wasmTimestamp() string {
	return ansiWhite + time.Now().Format("2006/01/02 15:04:05") + ansiReset
}

func wasmLogf(format string, args ...any) {
	fmt.Printf(wasmTimestamp()+" "+wasmTag+" "+ansiCyan+format+ansiReset+"\n", args...)
}

// wasmCount formats a count+label pair: number in light green, label in cyan.
// Ends in cyan so surrounding text in wasmLogf stays cyan after substitution.
func wasmCount(n int, label string) string {
	return fmt.Sprintf(ansiLightGreen+"%d"+ansiCyan+" %s", n, label)
}

func wasmErrorf(format string, args ...any) {
	fmt.Printf(wasmTimestamp()+" "+wasmTag+" "+ansiRed+format+ansiReset+"\n", args...)
}
