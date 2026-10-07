// console.go — page-console retention for the managed browser.
//
// Every launch subscribes the kept-alive tab to Runtime.consoleAPICalled and
// retains the error and warning messages in a bounded ring, so the audit's
// console-errors plane and the logs tool can report what the page's
// JavaScript said after the fact. The ring is session history, not
// per-browser state: it survives idle closes and headless toggles, and only
// the subscription is re-armed per launch.
package browser

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// ConsoleEntry is one retained console message from the managed page.
type ConsoleEntry struct {
	Type string `json:"type"` // "error" or "warning"
	Text string `json:"text"`
	// TimestampMs is epoch milliseconds; CDP's Runtime.Timestamp counts
	// seconds since the epoch (mirrors the screencast frame timestamps), so
	// ordering across entries is stable but the field is wall-clock, not
	// relative to the page's time origin.
	TimestampMs int64 `json:"timestamp_ms"`
}

// consoleRingCap bounds retention: the newest messages win. Well above what
// any audit or logs read cares about, and enough that a chatty page cannot
// grow the ring.
const consoleRingCap = 200

// Console returns the retained console messages, newest first. last <= 0
// returns the whole ring (at most consoleRingCap entries).
func (m *Manager) Console(last int) []ConsoleEntry {
	m.consoleMu.Lock()
	defer m.consoleMu.Unlock()
	n := len(m.console)
	if last > 0 && last < n {
		n = last
	}
	out := make([]ConsoleEntry, n)
	for i := 0; i < n; i++ {
		out[i] = m.console[len(m.console)-n+i]
	}
	return out
}

// appendConsole retains one message in the ring (oldest dropped past the cap).
func (m *Manager) appendConsole(e ConsoleEntry) {
	m.consoleMu.Lock()
	defer m.consoleMu.Unlock()
	m.console = append(m.console, e)
	if len(m.console) > consoleRingCap {
		m.console = m.console[len(m.console)-consoleRingCap:]
	}
}

// subscribeConsole arms the per-launch console subscription on the page:
// every error/warning the page's console emits is retained in the ring.
// rod only starts delivering events once the wait func eachEvent returns is
// run, so the delivery loop goes in its own goroutine. The loop ends by
// itself when the browser tears down: the CDP connection close propagates
// through rod's event channel, and eachEvent's own cleanup disables the
// Runtime domain again.
func (m *Manager) subscribeConsole(page *rod.Page) {
	wait := page.EachEvent(func(e *proto.RuntimeConsoleAPICalled) {
		if e.Type != proto.RuntimeConsoleAPICalledTypeError && e.Type != proto.RuntimeConsoleAPICalledTypeWarning {
			return
		}
		m.appendConsole(ConsoleEntry{
			Type:        string(e.Type),
			Text:        consoleArgsText(e.Args),
			TimestampMs: int64(e.Timestamp * 1000),
		})
	})
	go wait()
}

// consoleArgsText renders a console call's arguments: primitive values by
// value, everything else by CDP's description (objects arrive as
// "Uncaught …" strings or "[object …]" shapes).
func consoleArgsText(args []*proto.RuntimeRemoteObject) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		if a == nil {
			continue
		}
		text := ""
		if v := a.Value.Raw(); v != nil {
			// gson may hand back the JSON value still encoded as bytes
			// (a JSON string keeps its quotes).
			switch t := v.(type) {
			case string:
				text = t
			case []byte:
				var s string
				if err := json.Unmarshal(t, &s); err == nil {
					text = s
				} else {
					text = string(t)
				}
			default:
				text = fmt.Sprintf("%v", t)
			}
		}
		if text == "" {
			text = a.Description
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return "(no text)"
	}
	return strings.Join(parts, " ")
}
