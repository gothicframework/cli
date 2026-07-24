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
