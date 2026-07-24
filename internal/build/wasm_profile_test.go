package helpers

import "testing"

// TestProfileFor_KnownAndSafeDefault pins the capability-profile contract: known
// verified versions return their recorded profile, and every unknown/unverified
// version falls back to the SAFE default (manual-GC shim, StockWasmExec:false).
func TestProfileFor_KnownAndSafeDefault(t *testing.T) {
	cases := []struct {
		version       string
		wantStockExec bool
	}{
		// Verified rows.
		{"0.41.1", false},         // bundled default: no finalizers → manual GC
		{"0.41.1-gothic.1", true}, // patched fork (#5521): finalizers → stock shim
		{"0.42.0-gothic.1", true}, // patched fork (#5521 + idle-point GC) → stock shim
		{"0.42.0-gothic.2", true}, // same build with review feedback applied → stock shim
		// Unknown/unverified → safe default (manual GC), NEVER inferred true.
		{"0.42.0", false},          // a future bare official release: unverified
		{"0.42.0-gothic.3", false}, // an unlisted patched pin: unverified
		{"", false},
		{"garbage", false},
	}
	for _, c := range cases {
		if got := ProfileFor(c.version).StockWasmExec; got != c.wantStockExec {
			t.Errorf("ProfileFor(%q).StockWasmExec = %v, want %v", c.version, got, c.wantStockExec)
		}
	}
}

// TestWasmExecEnviron_MatchesProfile checks the CLI→server env translation: the
// stock signal is emitted only when the profile calls for it.
func TestWasmExecEnviron_MatchesProfile(t *testing.T) {
	stock := WasmHelper{Version: "0.41.1-gothic.1"}
	if got := stock.WasmExecEnviron(); len(got) != 1 || got[0] != WasmExecEnvKey+"=stock" {
		t.Errorf("stock WasmExecEnviron() = %v, want [%s=stock]", got, WasmExecEnvKey)
	}
	manual := WasmHelper{Version: "0.41.1"}
	if got := manual.WasmExecEnviron(); len(got) != 0 {
		t.Errorf("manual WasmExecEnviron() = %v, want empty", got)
	}
	unknown := WasmHelper{Version: "0.99.9"}
	if got := unknown.WasmExecEnviron(); len(got) != 0 {
		t.Errorf("unknown WasmExecEnviron() = %v, want empty (safe default)", got)
	}
}

// TestResolveTinyGoVersion pins the build/deploy consistency contract: a config
// pin passes through, an empty WasmTinyGoVersion resolves to the bundled default
// (not to ""), and that default MUST profile as a finalizer-carrying (stock-shim)
// toolchain. Otherwise a deploy with no pin would key GOTHIC_WASM_EXEC off
// ProfileFor("") (the manual-shim safe default) while the build compiled the
// finalizer-carrying default → the manual shim + finalizers double-free.
func TestResolveTinyGoVersion(t *testing.T) {
	if got := ResolveTinyGoVersion("0.99.9-gothic.7"); got != "0.99.9-gothic.7" {
		t.Errorf("pin should pass through: got %q", got)
	}
	def := ResolveTinyGoVersion("")
	if def == "" {
		t.Fatal("empty config must resolve to the bundled default, got empty")
	}
	if !ProfileFor(def).StockWasmExec {
		t.Errorf("bundled default %q must profile StockWasmExec=true; an empty pin "+
			"must not fall back to the manual shim while the build compiles the "+
			"finalizer-carrying default", def)
	}
}
