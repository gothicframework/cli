package pagemap

import (
	"fmt"
	"image"
	"math"
	"strconv"
	"strings"
)

// Grid is the raster downsampled to one palette class per cell. The cell
// stores a palette index — the class its pixels BELONG to, never an average:
// averaging a purple button into a dark background is exactly the blend that
// makes the widget unreadable, and the mode keeps it.
type Grid struct {
	Cols, Rows int
	Width      int // source raster size in pixels
	Height     int
	Cells      []int // len = Rows*Cols, row-major palette index per cell
}

// ClassAt returns the palette index of the cell at (row, col).
func (g Grid) ClassAt(row, col int) int { return g.Cells[row*g.Cols+col] }

// buildGrid quantizes the raster to a cols×rows grid of palette classes.
func buildGrid(img image.Image, q quantized, cols, rows int) Grid {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	n := q.pal.Len()
	grid := Grid{Cols: cols, Rows: rows, Width: w, Height: h}
	if w == 0 || h == 0 || cols == 0 || rows == 0 || n == 0 {
		return grid
	}
	counts := make([]int, rows*cols*n)
	for y := 0; y < h; y++ {
		r := (y * rows) / h
		base := r * cols * n
		py := y + b.Min.Y
		for x := 0; x < w; x++ {
			c := (x * cols) / w
			cls := q.clsOf[colorAt(img, x+b.Min.X, py)]
			counts[base+c*n+cls]++
		}
	}
	cells := make([]int, rows*cols)
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			base := (r*cols + c) * n
			best, bestN := 0, -1
			for k := 0; k < n; k++ {
				if cnt := counts[base+k]; cnt > bestN {
					best, bestN = k, cnt
				}
			}
			cells[r*cols+c] = best // ties stay with the smaller index: the palette's more frequent colour
		}
	}
	grid.Cells = cells
	return grid
}

// quietLumDelta is the OKLab lightness within which a palette entry counts as
// "a quiet step above the background" — the dark-theme surface that gets a
// punctuation symbol instead of a loud letter.
const quietLumDelta = 0.08

// paletteSymbols assigns the character printed for each palette entry:
// '.' for the dominant colour, ':' for a runner-up within quietLumDelta of
// that lightness, then letters 'A'..'X' from the third entry on, in the
// palette's frequency order. The legend maps every symbol, so the exact
// choice is cosmetic; determinism is not.
func paletteSymbols(pal Palette) []rune {
	syms := make([]rune, pal.Len())
	if len(syms) == 0 {
		return syms
	}
	syms[0] = '.'
	if len(syms) > 1 &&
		math.Abs(oklab(pal.Entries[0].Color)[0]-oklab(pal.Entries[1].Color)[0]) < quietLumDelta {
		syms[1] = ':'
	}
	next := 'A'
	for i, s := range syms {
		if s != 0 {
			continue
		}
		if next > 'X' {
			syms[i] = '?'
			continue
		}
		syms[i] = next
		next++
	}
	return syms
}

// ruleLine renders the column ruler: a label for every 5th cell, right-aligned
// so its last digit sits over the column it names. The 3-char offset matches
// the "%2d " row prefix.
func ruleLine(cols int) string {
	line := []byte(strings.Repeat(" ", cols+16))
	for c := 0; c < cols; c += 5 {
		s := strconv.Itoa(c)
		end := 3 + c
		copy(line[end-len(s)+1:end+1], s)
	}
	return strings.TrimRight(string(line), " ")
}

// render returns the ruler plus one line per grid row, each prefixed by its
// row number and holding one symbol per cell.
func (g Grid) render(syms []rune) string {
	var b strings.Builder
	b.WriteString(ruleLine(g.Cols))
	b.WriteByte('\n')
	for r := 0; r < g.Rows; r++ {
		fmt.Fprintf(&b, "%2d ", r)
		for c := 0; c < g.Cols; c++ {
			b.WriteRune(syms[g.ClassAt(r, c)])
		}
		b.WriteByte('\n')
	}
	return b.String()
}
