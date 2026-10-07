// eval.go — the JS-read seam the dev session's MCP surface needs: audit and
// probe run read-style expressions inside the kept-alive page. This is the
// only place outside this package that reaches the page's JS context; the
// expression contract ("() => …" returning a serializable value) keeps the
// surface read-shaped.
package browser

// Eval runs one rod-style JS function expression ("() => …") in the
// kept-alive page and returns its value serialized to a string. Promise
// results are awaited by rod's Eval shortcut. Like every other entry point,
// it launches the browser first if needed and re-arms the idle timer.
func (m *Manager) Eval(js string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	page, err := m.pageLocked()
	if err != nil {
		return "", err
	}
	m.armIdleLocked()
	res, err := page.Eval(js)
	if err != nil {
		return "", err
	}
	return res.Value.Str(), nil
}
