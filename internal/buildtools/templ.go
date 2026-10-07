package buildtools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"

	templgen "github.com/a-h/templ/cmd/templ/generatecmd"
	"github.com/gothicframework/cli/v4/internal/output"
	templcache "github.com/gothicframework/core/render"
)

// lastGenerateOutput captures the merged stdout+stderr of the most recent
// templ generator invocation, so the build-control layer can translate a
// failure into structured diagnostics with the raw compiler text preserved.
// Each run overwrites the previous one.
var lastGenerateOutput struct {
	mu  sync.Mutex
	raw string
}

func recordGenerateOutput(raw string) {
	lastGenerateOutput.mu.Lock()
	lastGenerateOutput.raw = raw
	lastGenerateOutput.mu.Unlock()
}

// LastTemplOutput returns the raw stdout+stderr of the most recent templ
// generator invocation (including every noise line), for diagnostics
// translation. Empty after a run that produced no output.
func LastTemplOutput() string {
	lastGenerateOutput.mu.Lock()
	defer lastGenerateOutput.mu.Unlock()
	return lastGenerateOutput.raw
}

type TemplHelper struct {
}

// generate is the seam used to invoke the templ generator. It defaults to the
// real templ generatecmd entrypoint; tests replace it to exercise the dirty-file
// and fallback paths without shelling out to the templ toolchain. The args slice
// matches templgen.Run's: e.g. []string{"generate"} or
// []string{"generate", "-f", file}. The default below preserves the exact
// previous behavior (same context, stdout/stderr, and argument forwarding).
var generate = func(args []string) error {
	// The generator narrates its own event loop, which duplicates the phase the
	// hot-reload stream already reports. Only that narration is dropped;
	// diagnostics still reach the terminal — and are captured for the
	// structured-diagnostics layer.
	var collector bytes.Buffer
	out := output.NewLineFilter(io.MultiWriter(os.Stdout, &collector), isTemplNoise)
	errOut := output.NewLineFilter(io.MultiWriter(os.Stderr, &collector), isTemplNoise)
	err := templgen.Run(context.Background(), out, errOut, args)
	recordGenerateOutput(collector.String())
	return err
}

// The generator's progress chatter: a per-event "Post-generation event
// received..." line and the "Complete [...]" summary that follows it, both
// prefixed with a check mark.
var templProgressRe = regexp.MustCompile(`^\(.\)\s*(Post-generation event received|Complete\b)`)

func isTemplNoise(line string) bool {
	// The generator colours its level icon, so the raw line starts with an
	// escape sequence rather than the "(" the pattern anchors on.
	t := strings.TrimSpace(output.StripANSI(line))
	return t == "" || templProgressRe.MatchString(t)
}

func NewTemplHelper() TemplHelper {
	return TemplHelper{}
}

// Render runs `templ generate`, but skips files whose contents are unchanged
// since the last successful run (tracked via .gothicCli/templ-cache.json).
//
// Behavior:
//   - Scans the working directory for .templ files.
//   - If every file is cache-hit (and the matching _templ.go exists), it
//     returns without invoking templ at all.
//   - Otherwise it runs templ per dirty file using the -f flag.
//     If any per-file run fails (e.g. unsupported by the installed templ
//     version), it falls back to a full-project generation.
//   - On success, updates and persists the cache.
func (t *TemplHelper) Render() error {
	cache := templcache.Load()
	files, err := templcache.ScanTemplFiles(".")
	if err != nil {
		// Cache is best-effort — fall back to a full run rather than failing.
		return generate([]string{"generate"})
	}

	dirty := templcache.DirtyFiles(cache, files)
	if len(dirty) == 0 && len(files) > 0 {
		// Everything up to date — skip templ entirely.
		return nil
	}

	if perFileErr := generatePerFile(dirty); perFileErr != nil {
		// Fallback: regenerate everything. We still refresh the cache afterwards
		// so subsequent runs benefit from the optimization.
		if err := generate([]string{"generate"}); err != nil {
			return fmt.Errorf("templ generate (fallback after per-file error %v): %w", perFileErr, err)
		}
	}

	// Refresh hashes for every scanned file and save.
	for _, f := range files {
		if h := templcache.HashFile(f); h != "" {
			cache.Update(f, h)
		}
	}
	_ = cache.Save()
	return nil
}

// generatePerFile invokes `templ generate` once per dirty file using the -f flag.
// If any single invocation fails, the error is returned so the caller can fall
// back to a full-project generation.
func generatePerFile(dirty []string) error {
	for _, f := range dirty {
		if err := generate([]string{"generate", "-f", f}); err != nil {
			return fmt.Errorf("templ generate %s: %w", f, err)
		}
	}
	return nil
}

// RenderFile regenerates a single .templ file (templ generate -f), ignoring
// the dirty-file cache, and refreshes its cache entry on success. Returns the
// raw generator output (stdout+stderr merged) alongside the error, so a
// targeted build can report structured diagnostics.
func (t *TemplHelper) RenderFile(file string) (string, error) {
	var buf bytes.Buffer
	out := output.NewLineFilter(io.MultiWriter(os.Stdout, &buf), isTemplNoise)
	errOut := output.NewLineFilter(io.MultiWriter(os.Stderr, &buf), isTemplNoise)
	err := templgen.Run(context.Background(), out, errOut, []string{"generate", "-f", file})
	if err != nil {
		return buf.String(), fmt.Errorf("templ generate %s: %w", file, err)
	}
	if h := templcache.HashFile(file); h != "" {
		cache := templcache.Load()
		cache.Update(file, h)
		_ = cache.Save()
	}
	return buf.String(), nil
}
