// These two accessors are the boundary that keeps busRecord and busRing
// unexported while the dev session's MCP surface drives both planes: marks
// recorded by the injected script and marks injected by a tool call land in
// one ring, on one timeline.
package proxy

import "github.com/gothicframework/cli/v3/internal/output"

// Records copies up to limit newest records out of the dev bus ring (newest
// first; limit < 0 = all). The records are plain maps, shaped as the injected
// script posts them ({t, kind, dir, event, topic, field, payload}).
func (p *ProxyHelper) Records(limit int) []map[string]any {
	return p.Bus.newestFirst(limit)
}

// Ingest adds records to the dev bus ring exactly as the HTTP ingest
// endpoint would: the --verbose one-line-per-record mirrors as well. This is
// the direct path for an in-process writer — the take-mark injector.
func (p *ProxyHelper) Ingest(records []map[string]any) {
	if BusVerbose {
		for _, rec := range records {
			output.Print(busVerboseKey(rec), busVerboseLine(rec))
		}
	}
	p.Bus.add(records)
}
