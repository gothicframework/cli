// Package output provides shared helpers for formatted CLI output with
// consistent timestamping and deduplication of consecutive identical messages.
package output

import (
	"fmt"
	"sync"
	"time"

	"github.com/gothicframework/cli/v3/internal/termcolor"
)

// One mutex and one dedup state for the whole CLI: every line printed through
// this package is serialized against every other, so a concurrent build result
// can never cut into a line being written.
var (
	mu      sync.Mutex
	lastKey string
	repeat  int
)

// TimestampLayout is the timestamp prefix shared by all CLI output. Wall-clock
// only: a hot-reload session is watched live, so the date is the same on every
// line and repeating it costs a column without telling the reader anything.
const TimestampLayout = "15:04:05"

// Timestamp returns the current time in the CLI's standard layout.
func Timestamp() string {
	return time.Now().Format(TimestampLayout)
}

// Print writes line to stdout, appending an " (xN)" suffix when key matches the
// key of the previous Print. The key carries only the message text, so colour
// codes and the timestamp never affect whether two lines are considered equal.
func Print(key, line string) {
	mu.Lock()
	defer mu.Unlock()

	if key == lastKey {
		repeat++
		fmt.Printf("%s (x%d)\n", line, repeat+1)
		return
	}
	repeat = 0
	lastKey = key
	fmt.Printf("%s\n", line)
}

// PrintRaw writes line to stdout without dedup and clears the dedup state. A
// message that repeats across an intervening PrintRaw is no longer consecutive,
// so it starts a fresh count rather than resuming the old one.
func PrintRaw(line string) {
	mu.Lock()
	defer mu.Unlock()

	lastKey = ""
	repeat = 0
	fmt.Printf("%s\n", line)
}

// Println prints a timestamped line with consecutive-identical-message dedup.
//
// Timestamp and message are both dim: a build phase is scaffolding, and the
// timestamp repeats near-identically on every line of a cycle. What matters is
// wrapped in Accent by the caller, so a screen full of these lines has a few
// bright points to read instead of a uniform wall.
func Println(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	dim := termcolor.Code(termcolor.Gray)
	line := dim + Timestamp() + " " + msg + termcolor.Code(termcolor.Reset)
	Print(msg, line)
}

// Accent highlights the payload of a message — a file path, a URL, a duration —
// against the dim line around it. It restores the dim afterwards, so an accented
// span can sit mid-message without bleaching the rest of the line.
func Accent(s string) string {
	if !termcolor.Enabled {
		return s
	}
	return termcolor.Code(termcolor.Yellow) + s + termcolor.Code(termcolor.Reset) + termcolor.Code(termcolor.Gray)
}

// Link highlights a location: a URL, or a path inside the project. Both answer
// "where", so they share one colour.
func Link(s string) string {
	if !termcolor.Enabled {
		return s
	}
	return termcolor.Code(termcolor.Blue) + s + termcolor.Code(termcolor.Reset) + termcolor.Code(termcolor.Gray)
}

// Tag names the subsystem a line comes from, in the brand accent. Plain, not
// bold: the accent alone is loud enough, and bold on a saturated magenta reads
// as shouting.
func Tag(s string) string {
	if !termcolor.Enabled {
		return s
	}
	return termcolor.Code(termcolor.Purple) + s +
		termcolor.Code(termcolor.Reset) + termcolor.Code(termcolor.Gray)
}

// Errorln prints a failure line. It never participates in dedup: two identical
// errors are two events worth seeing.
func Errorln(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	PrintRaw(termcolor.Code(termcolor.Gray) + Timestamp() + " " +
		termcolor.Code(termcolor.Red) + msg + termcolor.Code(termcolor.Reset))
}

// ResetDedup clears the consecutive-message state.
func ResetDedup() {
	mu.Lock()
	lastKey = ""
	repeat = 0
	mu.Unlock()
}
