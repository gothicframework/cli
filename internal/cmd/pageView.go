/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gothicframework/cli/v3/internal/browser"
	"github.com/gothicframework/cli/v3/internal/output"
	"github.com/gothicframework/cli/v3/internal/pagemap"
	"github.com/spf13/cobra"
)

// page-view flags. A browser layer writes the screenshot and DOM manifest to
// disk; this command encodes those two files.
var (
	pageViewPNG      string
	pageViewManifest string
	pageViewWidth    int
	pageViewEngine   string
	pageViewGrid     string
)

// pageViewCmd renders the `=== GOTHIC PAGE VIEW ===` payload: grid = geometry
// and colour, legend = identity and content, alerts = the conclusions.
var pageViewCmd = &cobra.Command{
	Use:   "page-view <url>",
	Short: "Encode a rendered page as a symbol grid a text-only model can read",
	Long: `Encodes a rendered page into a compact text payload: the page's own
colour palette as a legend, one letter per grid cell, the DOM regions the
cells land in, and the alerts the raster implies (palette coverage, edge
bleed, overflow).

By default the URL is rendered live in the managed dev browser (headless) —
loopback origins only. Pass --png to encode a screenshot file instead:

  gothic page-view http://localhost:3000
  gothic page-view http://localhost:3000 --width 390 --grid 24x12
  gothic page-view http://localhost:3000 --png shot.png --manifest manifest.json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		url := args[0]

		gw, gh, err := parseGrid(pageViewGrid)
		if err != nil {
			return err
		}

		engine := pageViewEngine
		viewportW := pageViewWidth // 0 = report the raster's own size
		var img image.Image
		var manifest *pagemap.Manifest

		if pageViewPNG == "" {
			// Live capture: the managed dev browser renders the URL and
			// writes BOTH artifacts — the screenshot and the DOM manifest —
			// so the encoding below is the file path's exact same encoder.
			stills, err := capturePageView(cmd.Context(), url)
			if err != nil {
				return err
			}
			img, err = decodePNG(stills.PNGPath)
			if err != nil {
				return err
			}
			data, mErr := os.ReadFile(stills.ManifestPath)
			if mErr == nil {
				manifest, _ = pagemap.ParseManifest(data)
			}
			if engine == "" {
				engine = managedEngineLabel
			}
			if viewportW == 0 {
				viewportW = stills.Width
			}
		} else {
			img, err = decodePNG(pageViewPNG)
			if err != nil {
				return err
			}
			if pageViewManifest != "" {
				data, err := os.ReadFile(pageViewManifest)
				if err != nil {
					return fmt.Errorf("reading --manifest %s: %v", pageViewManifest, err)
				}
				manifest, err = pagemap.ParseManifest(data)
				if err != nil {
					return err
				}
			}
		}

		payload := pagemap.Encode(img, manifest, pagemap.Options{
			URL:       url,
			Engine:    engine,
			ViewportW: viewportW,
			GridW:     gw,
			GridH:     gh,
		})
		// The payload is the product: print it verbatim, no dedup, no colour.
		output.PrintRaw(strings.TrimRight(payload, "\n"))
		return nil
	},
}

func init() {
	pageViewCmd.Flags().StringVar(&pageViewPNG, "png", "",
		"encode a screenshot PNG file instead of rendering the URL live")
	pageViewCmd.Flags().StringVar(&pageViewManifest, "manifest", "",
		"DOM manifest JSON (rects + scroll) to map cells against (--png mode)")
	pageViewCmd.Flags().IntVar(&pageViewWidth, "width", 0,
		"viewport width reported in (and used by the live capture at); 0 = the raster's own width")
	pageViewCmd.Flags().StringVar(&pageViewGrid, "grid", "48x24",
		"grid size as WxH cells (min 4, max 160 per axis)")
	pageViewCmd.Flags().StringVar(&pageViewEngine, "engine", "",
		"browser engine label reported in the payload (e.g. \"chromium 131\")")
	rootCmd.AddCommand(pageViewCmd)
}

// capturePageView renders the URL live in a one-shot managed browser and
// returns the first capture (screenshot + DOM manifest paths). The browser
// is headless, uses its own profile directory (never the dev session's), and
// is closed when the run ends; Ctrl-C cancels it.
func capturePageView(ctx context.Context, rawURL string) (browser.StillResult, error) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	opts := browser.NewOptions()
	// A separate profile keeps this one-shot command out of a running dev
	// session's browser profile (Chrome fails when two processes share one).
	opts.UserDataDir = filepath.Join(".gothicCli", "page-view", "browser-profile")
	mgr := browser.New(ctx, opts)
	defer mgr.Close()

	if u, err := url.Parse(rawURL); err == nil {
		mgr.SetAllowedHostPorts([]string{u.Host})
	}

	var widths []int
	if pageViewWidth > 0 {
		widths = []int{pageViewWidth}
	}
	stills, err := mgr.CaptureStill(ctx, browser.StillOptions{Widths: widths, URL: rawURL})
	if err != nil {
		return browser.StillResult{}, fmt.Errorf("live capture of %s: %w", rawURL, err)
	}
	if len(stills) == 0 {
		return browser.StillResult{}, fmt.Errorf("live capture returned no still")
	}
	return stills[0], nil
}

// parseGrid reads the --grid flag ("48x24") into the two grid dimensions.
func parseGrid(s string) (int, int, error) {
	parts := strings.SplitN(s, "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid --grid %q: expected WxH, e.g. 48x24", s)
	}
	w, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid --grid %q: %v", s, err)
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid --grid %q: %v", s, err)
	}
	const min, max = 4, 160
	if w < min || w > max || h < min || h > max {
		return 0, 0, fmt.Errorf("invalid --grid %q: each axis must be between %d and %d", s, min, max)
	}
	return w, h, nil
}

// decodePNG opens and decodes a screenshot, closing the file eagerly.
func decodePNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening --png %s: %v", path, err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decoding --png %s: %v", path, err)
	}
	return img, nil
}
