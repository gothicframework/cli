package buildctl

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Status states for one artifact.
const (
	StatusFresh      = "fresh"
	StatusStale      = "stale"
	StatusMidRebuild = "mid-rebuild"
)

// StatusArtifact is the freshness of one build artifact.
type StatusArtifact struct {
	Name string `json:"name"`
	// State is one of the Status* constants.
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// StatusResult is the freshness report of the whole artifact graph.
type StatusResult struct {
	// Rebuilding is true while any stage (foreground or the background WASM
	// compile) is running.
	Rebuilding bool `json:"rebuilding"`
	// Editing is true while an edit session holds the build lock.
	Editing   bool             `json:"editing"`
	Artifacts []StatusArtifact `json:"artifacts"`
}

// BuildStatus reports fresh/stale/mid-rebuild per artifact. The signals are
// the pipeline's own:
//   - templ: the content hash cache (.gothicCli/templ-cache.json)
//   - routes_gen / topic_gen: the generated file's mtime against its inputs
//   - wasm: the content digest gate (.gothicCli/wasm-digest.json)
//   - css / app: the output file's mtime against its inputs
//
// A fresh process restart loses the session completion times, so the mtime
// comparisons carry the weight; the freshness heuristics are cheap and fail
// toward "stale" (never claiming fresh on doubt).
// status applies the shared state precedence — mid-rebuild beats stale beats
// fresh — to one artifact: an artifact whose stage is currently running
// reports mid-rebuild whatever its freshness signal says. Every artifact of
// BuildStatus routes through here.
func (c *Controller) status(name string, bit int32, stale bool, detail string) StatusArtifact {
	state := StatusFresh
	switch {
	case c.busy.Load()&bit != 0:
		state = StatusMidRebuild
	case stale:
		state = StatusStale
	}
	return StatusArtifact{Name: name, State: state, Detail: detail}
}

func (c *Controller) BuildStatus() StatusResult {
	busy := c.busy.Load()

	// templ
	var templ StatusArtifact
	dirty, derr := templDirtyFiles()
	switch {
	case derr != nil:
		templ = c.status(StageTempl, busyTempl, true, fmt.Sprintf("cannot scan templ files: %v", derr))
	case len(dirty) > 0:
		templ = c.status(StageTempl, busyTempl, true, fmt.Sprintf("%d file(s) dirty: %s", len(dirty), truncList(dirty)))
	default:
		templ = c.status(StageTempl, busyTempl, false, "all up to date")
	}

	// routes_gen
	arts := []StatusArtifact{templ, c.routesStatus()}

	// topic_gen
	arts = append(arts, c.topicsStatus())

	// wasm
	arts = append(arts, c.wasmStatus())

	// css
	arts = append(arts, c.cssStatus())

	// app binary
	arts = append(arts, c.appStatus())

	return StatusResult{
		Rebuilding: busy != 0,
		Editing:    c.editing.Load(),
		Artifacts:  arts,
	}
}

// verifiedSince is the freshness floor of one artifact: the later of its
// mtime, the in-session completion time, and the persisted marker (the
// cross-restart twin of the in-session time — see markOK).
func (c *Controller) verifiedSince(name string, fi os.FileInfo) time.Time {
	return maxTime(fi.ModTime(), maxTime(c.lastOKTime(name), verifiedTime(name)))
}

func (c *Controller) routesStatus() StatusArtifact {
	fi := statFile(routesGenPath)
	if fi == nil {
		return c.status(StageRoutesGen, busyRoutesGen, true, "not generated yet")
	}
	newest := newestMtime([]string{pagesDir, componentsDir, "src/api"}, ".templ", ".go")
	if newest.After(c.verifiedSince(StageRoutesGen, fi)) {
		return c.status(StageRoutesGen, busyRoutesGen, true, fmt.Sprintf("sources changed at %s", newest.Format(time.RFC3339)))
	}
	return c.status(StageRoutesGen, busyRoutesGen, false, "up to date")
}

func (c *Controller) topicsStatus() StatusArtifact {
	if _, err := os.Stat("src/topics"); err != nil {
		return c.status(StageTopicGen, busyTopicGen, false, "no topics declared")
	}
	fi := statFile(topicGenPath)
	if fi == nil {
		return c.status(StageTopicGen, busyTopicGen, true, "not generated yet")
	}
	newest := newestMtime([]string{"src/topics"}, ".go")
	if newest.After(c.verifiedSince(StageTopicGen, fi)) {
		return c.status(StageTopicGen, busyTopicGen, true, fmt.Sprintf("topic sources changed at %s", newest.Format(time.RFC3339)))
	}
	return c.status(StageTopicGen, busyTopicGen, false, "up to date")
}

func (c *Controller) wasmStatus() StatusArtifact {
	snap := TakeSnapshot()
	detail := fmt.Sprintf("%d binary(ies) up to date", len(statWasmArtifacts()))
	if snap.Changed {
		detail = "inputs changed since the last build"
	}
	return c.status(StageWasm, busyWasm, snap.Changed, detail)
}

func (c *Controller) cssStatus() StatusArtifact {
	fi := statFile(cssOutPath)
	if fi == nil {
		return c.status(StageCSS, busyCSS, true, "not built yet")
	}
	newest := newestMtime([]string{"src"}, ".css", ".templ", ".go", ".html")
	if newest.After(c.verifiedSince(StageCSS, fi)) {
		return c.status(StageCSS, busyCSS, true, fmt.Sprintf("sources changed at %s", newest.Format(time.RFC3339)))
	}
	return c.status(StageCSS, busyCSS, false, "up to date")
}

func (c *Controller) appStatus() StatusArtifact {
	bin := statFile(c.opts.MainBinary)
	if bin == nil {
		return c.status("app", busyGo, true, "not built yet")
	}
	newest := newestMtime([]string{".", routesGenPath}, ".go", ".mod", ".sum")
	if newest.After(c.verifiedSince(StageGo, bin)) {
		return c.status("app", busyGo, true, fmt.Sprintf("sources changed at %s", newest.Format(time.RFC3339)))
	}
	return c.status("app", busyGo, false, "up to date")
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func truncList(files []string) string {
	const max = 5
	n := min(max, len(files))
	joined := strings.Join(files[:n], ", ")
	if len(files) > max {
		joined += ", …"
	}
	return joined
}
