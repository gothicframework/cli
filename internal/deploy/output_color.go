package tofu

import (
	"bytes"
	"io"
	"strings"

	"github.com/gothicframework/cli/v3/internal/termcolor"
)

// Shorthands sourced from the shared Gothic palette (pkg/helpers/termcolor), so
// the recolored OpenTofu stream matches the deploy output and WASM build lines.
// terraform-exec runs tofu with a piped (non-TTY) stdout, so tofu emits no color
// of its own and we add it here — honoring the shared enable check.
const (
	tReset  = termcolor.Reset
	tBold   = termcolor.Bold
	tGreen  = termcolor.Green  // + create
	tRed    = termcolor.Red    // - destroy / errors
	tYellow = termcolor.Yellow // ~ update / replace
	tDim    = termcolor.Gray   // # resource headers
)

// tofuColorWriter wraps an io.Writer and tints OpenTofu's plan/apply/destroy
// output by its leading diff marker. It line-buffers; because tofu's output is
// newline-terminated the buffer always drains, so no explicit flush is needed.
type tofuColorWriter struct {
	w   io.Writer
	buf []byte
}

// newTofuColorWriter returns w unwrapped when color is disabled, otherwise a
// colorizing wrapper.
func newTofuColorWriter(w io.Writer) io.Writer {
	if !termcolor.Enabled {
		return w
	}
	return &tofuColorWriter{w: w}
}

func (t *tofuColorWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		line := string(t.buf[:i+1])
		t.buf = t.buf[i+1:]
		if _, err := io.WriteString(t.w, colorizeTofuLine(line)); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// colorizeTofuLine tints a single line (trailing newline preserved outside the
// color codes) by its first non-space character, the way `tofu plan`/`apply`
// colors its diff. Unrecognized lines pass through unchanged.
func colorizeTofuLine(line string) string {
	trimmed := strings.TrimLeft(line, " \t")

	var code string
	switch {
	case strings.HasPrefix(trimmed, "+ ") || trimmed == "+\n":
		code = tGreen
	case strings.HasPrefix(trimmed, "- ") || trimmed == "-\n":
		code = tRed
	case strings.HasPrefix(trimmed, "~ ") || strings.HasPrefix(trimmed, "-/+") || strings.HasPrefix(trimmed, "+/-"):
		code = tYellow
	case strings.HasPrefix(trimmed, "# "):
		code = tDim
	case strings.Contains(line, "Apply complete!") || strings.Contains(line, "Destroy complete!"):
		code = tBold + tGreen
	case strings.HasPrefix(trimmed, "Error:") || strings.HasPrefix(trimmed, "│ Error") || strings.HasPrefix(trimmed, "╷"):
		code = tRed
	case strings.HasPrefix(trimmed, "Plan:") || strings.HasPrefix(trimmed, "Outputs:") || strings.HasPrefix(trimmed, "Changes to Outputs:"):
		code = tBold
	default:
		return line
	}

	body, nl := line, ""
	if strings.HasSuffix(line, "\n") {
		body, nl = line[:len(line)-1], "\n"
	}
	return code + body + tReset + nl
}
