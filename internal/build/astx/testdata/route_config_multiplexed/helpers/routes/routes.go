package routes

// RouteConfig mirrors the production routes.RouteConfig shape closely enough
// that its type string ends in "helpers/routes.RouteConfig". The Multiplexed
// field mirrors the additive Phase 14 opt-in flag.
type RouteConfig struct {
	ClientSideState func()
	WasmCompression string
	WasmCompiler    string
	Multiplexed     bool
}
