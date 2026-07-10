package buildtools

import (
	"context"
	"os"
	"testing"
)

// fakeResponse is a single canned outcome for one fakeRunner.Run invocation.
// out is returned as the command's captured stdout; err simulates a failed
// command (non-zero exit or spawn error).
type fakeResponse struct {
	out []byte
	err error
}

// fakeRunner is a commandRunner test double. It records every invocation's full
// argv (command name followed by its arguments) in calls, and returns canned
// results from responses, indexed by call order. When no response is configured
// for a given call it returns (nil, nil), i.e. a successful no-output command.
//
// It is the buildtools-package analogue of the runner seam used by TailwindHelper
// (see runner.go / setRunner), letting tests exercise Build/watch/AWS shell-out
// logic without spawning real processes.
type fakeRunner struct {
	calls     [][]string
	responses []fakeResponse
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	// Capture the full argv (name + args) as an independent slice so later
	// mutations by the caller cannot corrupt the recorded call.
	call := make([]string, 0, len(args)+1)
	call = append(call, name)
	call = append(call, args...)
	idx := len(f.calls)
	f.calls = append(f.calls, call)

	if idx < len(f.responses) {
		r := f.responses[idx]
		return r.out, r.err
	}
	return nil, nil
}

// withWorkDir changes the process working directory to dir for the duration of
// the test t, restoring the original directory (and thus not leaking cwd state
// into sibling tests) via t.Cleanup. Used by the templ Render tests, which scan
// the current directory for .templ files.
func withWorkDir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("withWorkDir: getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("withWorkDir: chdir %q: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(orig); err != nil {
			t.Errorf("withWorkDir: restore cwd %q: %v", orig, err)
		}
	})
}
