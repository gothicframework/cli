package browser

// SetHeadless switches the managed browser between headless and headful.
//
// Chrome cannot change this at runtime: the flag belongs to the process, so
// toggling restarts the browser. The profile directory (UserDataDir) is
// reused, which is the documented restart contract: cookies, localStorage,
// and session state all survive the toggle. Only the running processes are
// torn down and relaunched; the allowlist and options are kept.
//
// If the browser is not currently running, the toggle just updates the
// options the next launch will use.
func (m *Manager) SetHeadless(headless bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.page == nil {
		// Nothing running: record the mode for the next launch.
		m.opts.Headless = headless
		return nil
	}
	if m.launchOpts.Headless == headless {
		// Already in the requested mode; keep the idle timer as-is.
		return nil
	}

	// Tear down and relaunch with the same profile but the new mode.
	m.closeBrowserLocked()
	m.opts.Headless = headless
	page, err := m.launchLocked()
	if err != nil {
		return err
	}
	m.page = page
	m.armIdleLocked()
	return nil
}
