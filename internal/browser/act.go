package browser

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// Step is one scripted action primitive over the kept-alive page. Actions
// address elements by CSS selector.
type Step struct {
	// Action is one of: "goto", "click", "fill", "hover", "scroll", "key",
	// "select". Unknown actions error.
	Action string
	// Selector is the CSS selector the action targets. Required by click,
	// fill, hover, and select; ignored by goto, scroll, and key.
	Selector string
	// Value is the payload for the action:
	//   fill   — the text to type into the element (replaces existing content)
	//   goto   — the URL (allowlist-gated)
	//   key    — key name(s), e.g. "Enter Escape Tab ArrowDown a"
	//   select — the option value(s) to select, comma-separated
	//   click, hover, scroll — scroll takes it (see scrollLocked), others unused
	Value string
}

// Act runs steps in order over the single kept-alive page, launching the
// browser first if needed. Each step is bounded by the step timeout and the
// whole run by ctx. The first failure aborts the run and is reported with the
// failing step's index and action.
func (m *Manager) Act(ctx context.Context, steps []Step) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	page, err := m.pageLocked()
	if err != nil {
		return err
	}
	for i, step := range steps {
		if ctx.Err() != nil {
			return fmt.Errorf("step %d (%s) aborted: %w", i, step.Action, ctx.Err())
		}
		if err := m.runStepLocked(page, step); err != nil {
			return fmt.Errorf("step %d (%s): %w", i, step.Action, err)
		}
	}
	m.armIdleLocked()
	return nil
}

// runStepLocked executes one action primitive under a step-scoped timeout:
// the query and the interaction share one context, so both together fit the
// budget and neither outlives it. Callers hold m.mu.
func (m *Manager) runStepLocked(page *rod.Page, step Step) error {
	ctx, cancel := context.WithTimeout(m.sessionCtx, m.stepTimeout())
	defer cancel()
	p := page.Context(ctx)

	switch step.Action {
	case "goto":
		if err := m.checkURL(step.Value); err != nil {
			return err
		}
		return m.gotoLocked(page, step.Value)
	case "click":
		el, err := p.Element(step.Selector)
		if err != nil {
			return m.elementErr(step.Selector, err)
		}
		// Left button, single click; Click itself waits for the element to be
		// interactable and enabled first.
		return el.Click(proto.InputMouseButtonLeft, 1)
	case "fill":
		el, err := p.Element(step.Selector)
		if err != nil {
			return m.elementErr(step.Selector, err)
		}
		// Select-then-clear: Input appends to existing content, so emptying
		// first makes fill a replace, which is what callers expect.
		if err := el.SelectAllText(); err != nil {
			return err
		}
		return el.Input(step.Value)
	case "hover":
		el, err := p.Element(step.Selector)
		if err != nil {
			return m.elementErr(step.Selector, err)
		}
		return el.Hover()
	case "scroll":
		return m.scrollLocked(page, step.Value)
	case "key":
		return m.keyLocked(page, step.Value)
	case "select":
		el, err := p.Element(step.Selector)
		if err != nil {
			return m.elementErr(step.Selector, err)
		}
		values := splitCSV(step.Value)
		if len(values) == 0 {
			return fmt.Errorf("select %q requires a value", step.Selector)
		}
		return el.Select(values, true, rod.SelectorTypeCSSSector)
	default:
		return fmt.Errorf("unknown action %q (supported: goto, click, fill, hover, scroll, key, select)", step.Action)
	}
}

// elementErr wraps a failed element lookup with the selector.
func (m *Manager) elementErr(selector string, err error) error {
	return fmt.Errorf("find element %q: %w", selector, err)
}

// scrollLocked scrolls the page. Value forms:
//   - "" or "top" — scroll back to the very top
//   - "down"      — one viewport down
//   - "up"        — one viewport up
//   - "<px>"      — a signed pixel offset relative to the current position
func (m *Manager) scrollLocked(page *rod.Page, value string) error {
	var js string
	switch v := strings.ToLower(strings.TrimSpace(value)); v {
	case "", "top":
		js = "window.scrollTo(0, 0)"
	case "down":
		js = "window.scrollBy(0, window.innerHeight)"
	case "up":
		js = "window.scrollBy(0, -window.innerHeight)"
	default:
		px, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("scroll value %q: use \"\", \"top\", \"up\", \"down\", or a pixel offset", value)
		}
		js = fmt.Sprintf("window.scrollBy(0, %d)", px)
	}
	_, err := page.Eval(js)
	if err != nil {
		return fmt.Errorf("scroll: %w", err)
	}
	return nil
}

// keyLocked presses one or more keys, space-separated from a small named-key
// vocabulary plus single characters ("Enter", "Escape", "Tab", "Backspace",
// "ArrowDown", "a", …). Multiple keys press in sequence.
func (m *Manager) keyLocked(page *rod.Page, value string) error {
	keys := strings.Fields(strings.TrimSpace(value))
	if len(keys) == 0 {
		return fmt.Errorf("key requires a key name, e.g. \"Enter\"")
	}
	ctx, cancel := context.WithTimeout(m.sessionCtx, m.stepTimeout())
	defer cancel()
	kb := page.Context(ctx).Keyboard
	for _, name := range keys {
		key, err := lookupKey(name)
		if err != nil {
			return err
		}
		if err := kb.Press(key); err != nil {
			return fmt.Errorf("press %s: %w", name, err)
		}
		if err := kb.Release(key); err != nil {
			return fmt.Errorf("release %s: %w", name, err)
		}
	}
	return nil
}

// lookupKey maps a key name onto rod's key codes. Named keys cover the common
// control vocabulary; any single character maps through its ASCII code.
func lookupKey(name string) (input.Key, error) {
	switch name {
	case "Enter":
		return input.Enter, nil
	case "Escape":
		return input.Escape, nil
	case "Tab":
		return input.Tab, nil
	case "Backspace":
		return input.Backspace, nil
	case "Delete":
		return input.Delete, nil
	case "Space":
		return input.Space, nil
	case "ArrowUp":
		return input.ArrowUp, nil
	case "ArrowDown":
		return input.ArrowDown, nil
	case "ArrowLeft":
		return input.ArrowLeft, nil
	case "ArrowRight":
		return input.ArrowRight, nil
	case "Home":
		return input.Home, nil
	case "End":
		return input.End, nil
	case "PageUp":
		return input.PageUp, nil
	case "PageDown":
		return input.PageDown, nil
	}
	// Single characters resolve through rod's key map (letters, digits,
	// punctuation).
	r := []rune(name)
	if len(r) == 1 {
		return input.Key(r[0]), nil
	}
	return 0, fmt.Errorf("unsupported key %q", name)
}

// splitCSV splits a comma-separated value list, trimming whitespace around
// each entry.
func splitCSV(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
