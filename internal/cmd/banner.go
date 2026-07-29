package cmd

import (
	"strings"

	"github.com/gothicframework/cli/v3/internal/output"
	"github.com/gothicframework/cli/v3/internal/termcolor"
)

// brandRamp walks the documentation site's own gradient: the pink link accent
// through the primary purple to the blue code accent. One stop per wordmark
// line, so the CLI opens on the same colours the docs are built from.
var brandRamp = []string{
	"\x1b[38;2;244;114;182m", // #f472b6 pink accent
	"\x1b[38;2;214;102;208m",
	"\x1b[38;2;183;91;234m",
	"\x1b[38;2;154;101;248m",
	"\x1b[38;2;125;133;249m",
	"\x1b[38;2;96;165;250m", // #60a5fa blue accent
}

const wordmark = ` ██████╗  ██████╗ ████████╗██╗  ██╗██╗ ██████╗     █████╗ ██████╗ ██████╗ 
██╔════╝ ██╔═══██╗╚══██╔══╝██║  ██║██║██╔════╝    ██╔══██╗██╔══██╗██╔══██╗
██║  ███╗██║   ██║   ██║   ███████║██║██║         ███████║██████╔╝██████╔╝
██║   ██║██║   ██║   ██║   ██╔══██║██║██║         ██╔══██║██╔═══╝ ██╔═══╝ 
╚██████╔╝╚██████╔╝   ██║   ██║  ██║██║╚██████╗    ██║  ██║██║     ██║     
 ╚═════╝  ╚═════╝    ╚═╝   ╚═╝  ╚═╝╚═╝ ╚═════╝    ╚═╝  ╚═╝╚═╝     ╚═╝     `

// banner renders the startup banner: the wordmark under the brand ramp, then
// the status lines.
func banner() string {
	var b strings.Builder
	reset := termcolor.Code(termcolor.Reset)

	b.WriteString("\n")
	for i, line := range strings.Split(wordmark, "\n") {
		if termcolor.Enabled {
			b.WriteString(brandRamp[i%len(brandRamp)] + line + reset)
		} else {
			b.WriteString(line)
		}
		b.WriteString("\n")
	}

	// A text glyph, not an emoji: terminals paint emoji from the font's own
	// colours and ignore the escape, so an emoji can never take the brand pink.
	bolt := "\u21af"
	if termcolor.Enabled {
		bolt = brandRamp[0] + bolt + reset
	}
	b.WriteString("\n" + bolt + " Gothic App is up and running!\n")
	b.WriteString("\U0001F310 Listening on: " + output.Link("http://127.0.0.1:3000") + reset + "\n")
	b.WriteString("\U0001F525  Mode: HOT RELOAD ENABLED")
	return b.String()
}
