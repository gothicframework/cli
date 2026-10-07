package mcp

// audit.go — the bounded audit tool.
//
// Scope (deliberately bounded): web-vitals metrics read from the browser's
// PerformanceObserver buffered entries, DOM/SEO checks parsed from the
// rendered HTML plus the capture manifest, and axe accessibility rules run
// from the embedded axe-core bundle. Findings only — no score-first output.
//
// Console errors ARE in this audit: the managed browser retains every
// error/warning the page's console emits (a CDP Runtime.consoleAPICalled
// subscription armed at each launch, bounded ring on the Manager), and the
// audit reports the retained messages. Messages emitted before the browser
// launched are not visible.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	_ "embed"
	"github.com/gothicframework/cli/v4/internal/browser"
	"github.com/gothicframework/cli/v4/internal/pagemap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed axe.min.js
var axeSource string

// axeVersion is the pinned axe-core version the embedded bundle carries.
const axeVersion = "4.11.1"

// auditBudget bounds the whole audit run.
const auditBudget = 12 * time.Second

// perfSettle is how long the metrics collector waits for buffered
// PerformanceObserver entries before reporting.
const perfSettle = 1500 * time.Millisecond

// axeRunBudget bounds the axe run itself.
const axeRunBudget = 8 * time.Second

type auditIn struct {
	URL           string `json:"url,omitempty" jsonschema:"navigate here first (optional; default = the tab's current page)"`
	IncludeA11y   bool   `json:"include_a11y,omitempty" jsonschema:"run the axe accessibility pass (the slowest part)"`
	IncludeVitals bool   `json:"include_vitals,omitempty" jsonschema:"collect the buffered web-vitals metrics (needs ~1.5s of settle)"`
}

type auditFinding struct {
	Check   string `json:"check"`
	Status  string `json:"status"` // "pass" | "fail" | "skipped"
	Detail  string `json:"detail,omitempty"`
	FixHint string `json:"fix_hint,omitempty"`
	Impact  string `json:"impact,omitempty"`
}

type auditOut struct {
	OK       bool           `json:"ok"`
	Summary  string         `json:"summary,omitempty"`
	URL      string         `json:"url,omitempty"`
	Fail     int            `json:"fail"`
	Pass     int            `json:"pass"`
	Skipped  int            `json:"skipped"`
	Findings []auditFinding `json:"findings"`
}

func (s *surface) auditTool(ctx context.Context, _ *mcp.CallToolRequest, in auditIn) (*mcp.CallToolResult, auditOut, error) {
	c, err := s.currentCap()
	if err != nil {
		return nil, auditOut{}, err
	}
	b, err := c.needBrowser()
	if err != nil {
		return nil, auditOut{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, auditBudget)
	defer cancel()

	if strings.TrimSpace(in.URL) != "" {
		if err := b.Open(in.URL); err != nil {
			return nil, auditOut{}, fmt.Errorf("navigate to %s: %w", in.URL, err)
		}
	}

	out := auditOut{OK: true}

	// The page identity comes from the still capture's manifest (url, DOM).
	stills, err := b.CaptureStill(ctx, browser.StillOptions{})
	if err != nil {
		return nil, auditOut{}, fmt.Errorf("capture the page: %w", err)
	}
	if len(stills) == 0 {
		return nil, auditOut{}, fmt.Errorf("capture returned no stills")
	}
	still := stills[0]
	out.URL = still.URL
	var manifest *pagemap.Manifest
	if data, err := os.ReadFile(still.ManifestPath); err == nil {
		manifest, _ = pagemap.ParseManifest(data)
	}

	// 1. DOM/SEO checks — from the rendered HTML (fetched over HTTP) and the
	//    capture manifest.
	html, hErr := fetchHTML(ctx, still.URL)
	if hErr != nil {
		out.finding(auditFinding{Check: "page-fetch", Status: "skipped",
			Detail: fmt.Sprintf("cannot fetch %s for the DOM/SEO checks: %v", still.URL, hErr)})
	} else {
		domFindings(html, manifest, &out)
	}

	// 2. Web vitals — buffered PerformanceObserver entries in the page.
	if in.IncludeVitals {
		vitals, vErr := collectVitals(ctx, b)
		if vErr != nil {
			out.finding(auditFinding{Check: "web-vitals", Status: "skipped", Detail: vErr.Error()})
		} else {
			vitalsFindings(vitals, &out)
		}
	}

	// 3. Console errors — the page's retained console output (the errors and
	//    warnings subscribed via CDP since the browser launched).
	consoles := b.Console(0)
	if len(consoles) == 0 {
		out.finding(auditFinding{Check: "console-errors", Status: "pass",
			Detail: "no console errors or warnings retained since the browser launched"})
	} else {
		samples := make([]string, 0, 3)
		for i, e := range consoles {
			if i >= 3 {
				break
			}
			samples = append(samples, fmt.Sprintf("[%s] %s", e.Type, oneLine(e.Text)))
		}
		out.finding(auditFinding{Check: "console-errors", Status: "fail",
			Detail:  fmt.Sprintf("%d console error/warning message(s), newest first: %s", len(consoles), strings.Join(samples, " | ")),
			FixHint: "fix the reported console errors; the page's JavaScript emitted them"})
	}

	// 4. Accessibility — the embedded axe bundle, run in the page.
	if in.IncludeA11y {
		axFindings, axErr := runAxe(ctx, b)
		if axErr != nil {
			out.finding(auditFinding{Check: "a11y", Status: "skipped",
				Detail: fmt.Sprintf("axe could not run: %v", axErr)})
		} else {
			for _, f := range axFindings {
				out.finding(f)
			}
		}
	}

	out.Summary = fmt.Sprintf("%d fail, %d pass, %d skipped (axe-core %s embedded)", out.Fail, out.Pass, out.Skipped, axeVersion)
	return nil, out, nil
}

// finding appends one finding and updates the counters.
func (o *auditOut) finding(f auditFinding) {
	o.Findings = append(o.Findings, f)
	switch f.Status {
	case "fail":
		o.Fail++
	case "pass":
		o.Pass++
	default:
		o.Skipped++
	}
}

// ── DOM/SEO checks ─────────────────────────────────────────────────────────

// domFindings runs the DOM/SEO checks over the fetched HTML plus the capture
// manifest. Everything is a plain regexp/string check on the document —
// bounded, no full HTML parse.
func domFindings(html string, m *pagemap.Manifest, out *auditOut) {
	title := regexpSearch(html, `<title[^>]*>([^<]*)</title>`)
	if title == "" {
		out.finding(auditFinding{Check: "seo-title", Status: "fail", FixHint: "add a <title> to the page's layout head"})
	} else {
		out.finding(auditFinding{Check: "seo-title", Status: "pass", Detail: fmt.Sprintf("%q", title)})
	}

	if regexpSearch(html, `<meta[^>]+name=["']description["'][^>]*>`) == "" {
		out.finding(auditFinding{Check: "seo-description", Status: "fail", FixHint: "add <meta name=\"description\"> to the head"})
	} else {
		out.finding(auditFinding{Check: "seo-description", Status: "pass"})
	}

	if !regexpMatch(html, `<link[^>]+rel=["']canonical["']`) {
		out.finding(auditFinding{Check: "seo-canonical", Status: "fail", FixHint: "add <link rel=\"canonical\"> to the head"})
	} else {
		out.finding(auditFinding{Check: "seo-canonical", Status: "pass"})
	}

	if regexpMatch(html, `<meta[^>]+name=["']viewport["']`) {
		out.finding(auditFinding{Check: "seo-viewport", Status: "pass"})
	} else {
		out.finding(auditFinding{Check: "seo-viewport", Status: "fail", FixHint: "add <meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">"})
	}

	h1Count := regexpCount(html, `<h1[\s>]`)
	switch {
	case h1Count == 0:
		out.finding(auditFinding{Check: "dom-h1", Status: "fail", FixHint: "the page has no h1"})
	case h1Count > 1:
		out.finding(auditFinding{Check: "dom-h1", Status: "fail",
			Detail: fmt.Sprintf("%d h1 elements", h1Count), FixHint: "keep one h1 per page"})
	default:
		out.finding(auditFinding{Check: "dom-h1", Status: "pass"})
	}

	if !regexpMatch(html, `<html[^>]+lang=`) {
		out.finding(auditFinding{Check: "a11y-lang", Status: "fail", FixHint: "set the lang attribute on <html>"})
	} else {
		out.finding(auditFinding{Check: "a11y-lang", Status: "pass"})
	}

	imgTags := regexpCount(html, `<img[\s>]`)
	altImgs := regexpCount(html, `<img[^>]+alt=`)
	if imgTags > 0 && altImgs < imgTags {
		out.finding(auditFinding{Check: "a11y-img-alt", Status: "fail",
			Detail:  fmt.Sprintf("%d of %d <img> elements carry alt", altImgs, imgTags),
			FixHint: "give every img an alt (decorative ones alt=\"\")"})
	} else {
		out.finding(auditFinding{Check: "a11y-img-alt", Status: "pass"})
	}

	// Layout checks from the capture manifest: overflowing elements.
	if m != nil {
		var overflow []string
		for _, r := range m.Rects {
			if r.Overflows {
				overflow = append(overflow, r.Selector)
				if len(overflow) >= 5 {
					break
				}
			}
		}
		if len(overflow) > 0 {
			out.finding(auditFinding{Check: "dom-overflow", Status: "fail",
				Detail:  fmt.Sprintf("%d element(s) overflow horizontally, e.g. %s", len(overflow), strings.Join(overflow, ", ")),
				FixHint: "the element's content is wider than its box; check max-width/word-break"})
		} else {
			out.finding(auditFinding{Check: "dom-overflow", Status: "pass"})
		}
	}
}

func regexpSearch(html, pattern string) string {
	re := regexp.MustCompile(pattern)
	if m := re.FindStringSubmatch(html); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func regexpMatch(html, pattern string) bool {
	return regexp.MustCompile(pattern).MatchString(html)
}

func regexpCount(html, pattern string) int {
	n := regexp.MustCompile(pattern).FindAllStringIndex(html, -1)
	return len(n)
}

// oneLine flattens a console message for a finding detail (whitespace to
// single spaces) and bounds its length.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const cap = 160
	if len(s) > cap {
		s = s[:cap] + "…"
	}
	return s
}

// fetchHTML fetches the page over HTTP (the dev origin is loopback).
func fetchHTML(ctx context.Context, url string) (string, error) {
	if !strings.HasPrefix(url, "http://127.0.0.1") && !strings.HasPrefix(url, "http://localhost") {
		return "", fmt.Errorf("non-loopback URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body := make([]byte, 0, 64<<10)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil || len(body) > 1<<20 {
			break
		}
	}
	return string(body), nil
}

// ── web vitals ─────────────────────────────────────────────────────────────

// vitalsMetrics is the metrics snapshot the page reports.
type vitalsMetrics struct {
	FCP float64 `json:"fcp"`
	LCP float64 `json:"lcp"`
	CLS float64 `json:"cls"`
	TBT float64 `json:"tbt"`
	INP float64 `json:"inp"`
}

// collectVitalsJS reads the buffered entries and resolves after a settle
// window. INP here is the worst buffered interaction (a lower bound on the
// real INP, which needs full-session event tracking).
const collectVitalsJS = `() => new Promise(resolve => {
  const out = {fcp: 0, lcp: 0, cls: 0, tbt: 0, inp: 0};
  try {
    new PerformanceObserver(list => {
      for (const e of list.getEntries()) {
        if (e.name === "first-contentful-paint") out.fcp = Math.round(e.startTime);
      }
    }).observe({type: "paint", buffered: true});
    new PerformanceObserver(list => {
      for (const e of list.getEntries()) { out.lcp = Math.round(e.startTime); }
    }).observe({type: "largest-contentful-paint", buffered: true});
    new PerformanceObserver(list => {
      for (const e of list.getEntries()) { out.cls += e.value; }
    }).observe({type: "layout-shift", buffered: true});
    new PerformanceObserver(list => {
      for (const e of list.getEntries()) { out.tbt += Math.max(0, e.duration - 50); }
    }).observe({type: "longtask", buffered: true});
    new PerformanceObserver(list => {
      for (const e of list.getEntries()) {
        if (e.interactionId && e.duration > out.inp) out.inp = Math.round(e.duration);
      }
    }).observe({type: "event", buffered: true, durationThreshold: 16});
  } catch (err) {}
  setTimeout(() => { out.cls = Math.round(out.cls * 1000) / 1000; resolve(JSON.stringify(out)); }, 1500);
})`

// collectVitals runs the metrics collector in the page.
func collectVitals(ctx context.Context, b BrowserPort) (vitalsMetrics, error) {
	val, err := b.Eval(collectVitalsJS)
	if err != nil {
		return vitalsMetrics{}, fmt.Errorf("the metrics collector failed: %v", err)
	}
	var m vitalsMetrics
	if err := json.Unmarshal([]byte(val), &m); err != nil {
		return vitalsMetrics{}, fmt.Errorf("decoding the metrics: %v", err)
	}
	return m, nil
}

// vitalsFindings turns the metrics into findings (thresholds per Google's
// Core Web Vitals assessment).
func vitalsFindings(m vitalsMetrics, out *auditOut) {
	check := func(name string, v float64, good, poor float64, unit string) {
		switch {
		case v <= 0:
			out.finding(auditFinding{Check: name, Status: "skipped", Detail: "no observation (nothing buffered during the settle window)"})
		case v <= good:
			out.finding(auditFinding{Check: name, Status: "pass", Detail: fmt.Sprintf("%.0f%s", v, unit)})
		case v <= poor:
			out.finding(auditFinding{Check: name, Status: "fail", Detail: fmt.Sprintf("%.0f%s (needs improvement)", v, unit)})
		default:
			out.finding(auditFinding{Check: name, Status: "fail", Detail: fmt.Sprintf("%.0f%s (poor)", v, unit)})
		}
	}
	check("vitals-fcp", m.FCP, 1800, 3000, "ms")
	check("vitals-lcp", m.LCP, 2500, 4000, "ms")
	check("vitals-cls", m.CLS, 0.1, 0.25, "")
	check("vitals-tbt", m.TBT, 200, 600, "ms")
	check("vitals-inp", m.INP, 200, 500, "ms")
}

// ── axe ────────────────────────────────────────────────────────────────────

// axeInjectJS injects the embedded bundle as a script tag (source inline; the
// dev origin carries no CSP in the default setup) and resolves when axe is
// usable.
const axeInjectJS = `() => new Promise((resolve, reject) => {
  if (window.axe) { resolve("present"); return; }
  const src = __AXE_SOURCE__;
  try {
    const s = document.createElement("script");
    s.textContent = src;
    s.onload = () => resolve("loaded");
    s.onerror = (e) => reject(new Error("axe script tag failed to load"));
    document.head.appendChild(s);
    if (window.axe) resolve("loaded");
    else setTimeout(() => window.axe ? resolve("loaded") : reject(new Error("axe did not evaluate")), 1000);
  } catch (err) { reject(err); }
})`

// runAxe injects and runs the axe bundle, mapping violations to findings.
func runAxe(ctx context.Context, b BrowserPort) ([]auditFinding, error) {
	inject := strings.Replace(axeInjectJS, "__AXE_SOURCE__", jsonStringLiteral(axeSource), 1)
	if _, err := b.Eval(inject); err != nil {
		return nil, fmt.Errorf("inject axe %s: %v", axeVersion, err)
	}
	runJS := fmt.Sprintf(`() => new Promise((resolve, reject) => {
    const budget = setTimeout(() => reject(new Error("axe run exceeded %dms")), %d);
    window.axe.run(document, {resultTypes: ["violations"]}).then(results => {
      clearTimeout(budget);
      const out = [];
      for (const v of results.violations || []) {
        out.push({
          id: v.id,
          impact: v.impact || "",
          help: v.help,
          nodes: (v.nodes || []).length,
          first: ((v.nodes || [])[0] || {}).target || []
        });
      }
      resolve(JSON.stringify(out));
    }).catch(err => { clearTimeout(budget); reject(err); });
  }))`, axeRunBudget.Milliseconds(), axeRunBudget.Milliseconds())
	val, err := b.Eval(runJS)
	if err != nil {
		return nil, fmt.Errorf("axe run: %v", err)
	}
	var violations []struct {
		ID     string   `json:"id"`
		Impact string   `json:"impact"`
		Help   string   `json:"help"`
		Nodes  int      `json:"nodes"`
		First  []string `json:"first"`
	}
	if err := json.Unmarshal([]byte(val), &violations); err != nil {
		return nil, fmt.Errorf("decoding the axe report: %v", err)
	}
	out := make([]auditFinding, 0, len(violations))
	for _, v := range violations {
		status := "fail"
		if v.Impact == "" {
			status = "pass" // "incomplete" rules report without impact
		}
		detail := fmt.Sprintf("%s — %d node(s)", v.Help, v.Nodes)
		if len(v.First) > 0 {
			detail += fmt.Sprintf(", first: %s", strings.Join(v.First, " "))
		}
		out = append(out, auditFinding{
			Check:   "a11y:" + v.ID,
			Status:  status,
			Detail:  detail,
			FixHint: "see the axe rule " + v.ID + " at https://dequeuniversity.com/rules/axe/" + axeVersion,
			Impact:  v.Impact,
		})
	}
	return out, nil
}

// jsonStringLiteral encodes a string as a JS string literal (JSON escaping is
// valid JS escaping for this subset).
func jsonStringLiteral(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
