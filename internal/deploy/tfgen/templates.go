// Package tfgen generates a fully-formed OpenTofu working directory for the
// Gothic AWS stack. The base .tf.json files are embedded in the CLI binary and
// are NEVER seeded onto the user's project disk. They use OpenTofu's own
// "${var.*}" interpolation syntax (NOT Go text/template "{{...}}" markers) so
// they remain valid JSON consumable directly by OpenTofu. Per-deployment values
// are supplied at runtime via a generated vars.tf.json, and environment-variable
// sources (raw / SSM / Secrets Manager) are resolved via a generated
// env_resolved.tf.json. This mirrors the embed pattern used by
// pkg/helpers/wasm/wasm_templates.go.
package tfgen

import "embed"

//go:embed embedded
var TofuTemplateFS embed.FS
