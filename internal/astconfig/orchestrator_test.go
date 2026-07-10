package astconfig

import (
	"strings"
	"testing"

	config "github.com/gothicframework/core/config"
)

// These tests compile and run a generated Go program via `go run`, which needs
// the Go toolchain and may resolve modules on first run. They are real
// integration tests of the orchestrator round-trip.

func TestGenerateOrchestratorRoundTrip(t *testing.T) {
	gctx := &config.GothicContext{
		Stage:       "dev",
		ProjectName: "orchapp",
		Outputs:     map[string]string{},
	}
	got, err := GenerateOrchestrator("testdata/orchestrator", "BeforeDeploy", gctx)
	if err != nil {
		t.Fatalf("GenerateOrchestrator: %v", err)
	}
	if got.Outputs["test"] != "ok" {
		t.Errorf("Outputs[test] = %q, want ok (hook mutation did not round-trip)", got.Outputs["test"])
	}
}

func TestGenerateOrchestratorPropagatesHookError(t *testing.T) {
	gctx := &config.GothicContext{Stage: "dev", ProjectName: "orchapp"}
	_, err := GenerateOrchestrator("testdata/orchestrator_err", "BeforeDeploy", gctx)
	if err == nil {
		t.Fatal("expected error from a failing BeforeDeploy hook")
	}
	// The orchestrator surfaces the subprocess failure; the hook's "boom" is
	// printed to stderr and the run exits non-zero.
	if !strings.Contains(err.Error(), "BeforeDeploy") {
		t.Errorf("error = %q, want it to reference the BeforeDeploy hook", err.Error())
	}
}

func TestGenerateOrchestratorMissingConfig(t *testing.T) {
	_, err := GenerateOrchestrator("testdata/does-not-exist", "BeforeDeploy", &config.GothicContext{})
	if err == nil {
		t.Fatal("expected error when gothic.config.go is missing")
	}
}
