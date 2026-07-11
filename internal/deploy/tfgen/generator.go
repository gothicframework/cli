package tfgen

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gothicframework/core/config"
)

// safeKeyPattern sanitises an env var key into a valid OpenTofu data-source
// resource name: anything outside [a-zA-Z0-9_] becomes "_".
var safeKeyPattern = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// TfGenParams carries every concrete per-deployment value the generator needs
// to materialise vars.tf.json and env_resolved.tf.json. The base .tf.json files
// reference these via "${var.*}" / "${local.env_vars}".
type TfGenParams struct {
	ProjectName   string
	Stage         string
	Suffix        string
	Region        string
	Profile       string
	ServerMemory  int
	ServerTimeout int
	ECRImageURI   string
	BucketName    string
	LambdaName    string
	StateBucket   string
	LockTable     string
	EnvVars       map[string]config.EnvValue
	// The custom-domain fields are source-aware EnvValues (raw / SSM / Secrets
	// Manager), resolved into locals by writeEnvResolved exactly like EnvVars. A nil
	// pointer means the stage did not declare that field.
	WafArn         *config.EnvValue
	CustomDomain   *config.EnvValue
	HostedZoneId   *config.EnvValue
	CertificateArn *config.EnvValue

	// CDN drives the generated aws_cloudfront_cache_policy.server (which query params,
	// cookies, and headers the dynamic/Lambda behavior folds into the cache key and
	// forwards to the origin). Its zero value = all query params, no cookies, no
	// headers. See writeCachePolicy.
	CDN config.CDNConfig

	// InfraDir is the user's custom-infrastructure directory (cwd-relative, like
	// the "public/" asset dir). Every *.tf / *.tf.json file directly inside it is
	// merged flat into workDir so the user's resources land in the SAME module +
	// state as the Gothic stack, and can reference the stable local.gothic_*
	// contract. Empty or absent → no-op. Non-.tf files are ignored.
	InfraDir string
}

// gothicReservedNames are the base names Gothic itself materialises into workDir.
// A user infra/ file matching one of these would clobber Gothic's own config, so
// it is rejected rather than silently overwritten. gothic_image.auto.tfvars.json
// is written later (at Deploy time) but is still reserved here so a colliding
// user file can never shadow it.
var gothicReservedNames = map[string]bool{
	"main.tf.json":                  true,
	"variables.tf.json":             true,
	"resources.tf.json":             true,
	"outputs.tf.json":               true,
	"gothic_outputs.tf.json":        true,
	"gothic_vars.auto.tfvars.json":  true,
	"env_resolved.tf.json":          true,
	"gothic_image.auto.tfvars.json": true,
	"gothic_cache_policy.tf.json":   true,
}

// Generator materialises an OpenTofu working directory for the AWS Gothic stack.
type Generator struct{}

// NewGenerator returns a ready-to-use Generator.
func NewGenerator() *Generator { return &Generator{} }

// Prepare writes a complete OpenTofu working directory into workDir:
//   - the embedded base .tf.json files (provider/backend, variables, resources, outputs)
//   - a generated vars.tf.json supplying concrete defaults for every variable
//   - a generated env_resolved.tf.json resolving env-var sources (raw/SSM/Secrets Manager)
//   - a gothic_outputs.tf.json placeholder
//   - a generated gothic_cache_policy.tf.json (the CloudFront server cache policy)
//
// It NEVER writes to the user's project directory; only workDir is touched.
func (g *Generator) Prepare(workDir string, params TfGenParams) error {
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return fmt.Errorf("create work dir %q: %w", workDir, err)
	}

	if err := g.writeEmbeddedBase(workDir); err != nil {
		return err
	}
	if err := g.writeVars(workDir, params); err != nil {
		return err
	}
	if err := g.writeEnvResolved(workDir, params); err != nil {
		return err
	}
	if err := g.writeGothicOutputs(workDir); err != nil {
		return err
	}
	if err := g.writeCachePolicy(workDir, params); err != nil {
		return err
	}
	// User infra is merged LAST so the collision guard sees the full set of
	// Gothic-generated files already in place (and so nothing a user drops can be
	// overwritten by a later Gothic write).
	if err := g.mergeUserInfra(workDir, params.InfraDir); err != nil {
		return err
	}
	return nil
}

// writeGothicOutputs writes gothic_outputs.tf.json: a `locals` block re-exporting
// the Gothic stack's key resource attributes under STABLE `gothic_*` names. This
// is the public contract user infra/ files build against (e.g. attaching an IAM
// policy to the Gothic Lambda role, or granting it access to a new table) — it
// decouples them from Gothic's internal resource addresses, which may change.
// Every reference is verified against embedded/aws/resources.tf.json.
func (g *Generator) writeGothicOutputs(workDir string) error {
	locals := map[string]any{
		"gothic_lambda_role_name":           "${aws_iam_role.lambda.name}",
		"gothic_lambda_role_arn":            "${aws_iam_role.lambda.arn}",
		"gothic_lambda_function_name":       "${aws_lambda_function.main.function_name}",
		"gothic_lambda_function_arn":        "${aws_lambda_function.main.arn}",
		"gothic_s3_bucket_name":             "${aws_s3_bucket.main.bucket}",
		"gothic_s3_bucket_arn":              "${aws_s3_bucket.main.arn}",
		"gothic_cloudfront_distribution_id": "${aws_cloudfront_distribution.main.id}",
		"gothic_cloudfront_domain_name":     "${aws_cloudfront_distribution.main.domain_name}",
	}
	return writeJSON(filepath.Join(workDir, "gothic_outputs.tf.json"), map[string]any{"locals": locals})
}

// writeCachePolicy generates gothic_cache_policy.tf.json: the
// aws_cloudfront_cache_policy.server resource for the dynamic (Lambda) behavior.
// It is generated here rather than embedded statically so the query-param, cookie,
// and header knobs from Deploy.Providers.AWS.CDN drive the cache key (and what
// CloudFront forwards to the origin). The distribution in the embedded
// resources.tf.json references it by ${aws_cloudfront_cache_policy.server.id};
// OpenTofu resolves that across files in the same working dir.
//
// Defaults (zero-value CDN): all query params in the key, no cookies, no headers —
// identical to the previous static policy. min/max/default TTL honor origin
// Cache-Control so ISR keeps working. Accept-Encoding is left out of the key
// (brotli/gzip both false) to avoid fragmenting the SSR cache.
func (g *Generator) writeCachePolicy(workDir string, params TfGenParams) error {
	cdn := params.CDN

	queryCfg, err := cacheKeyBlock("query_string", "QueryParams", ruleBehavior(cdn.QueryParams, "all"), cdn.QueryParams.Items())
	if err != nil {
		return err
	}
	cookieCfg, err := cacheKeyBlock("cookie", "Cookies", ruleBehavior(cdn.Cookies, "none"), cdn.Cookies.Items())
	if err != nil {
		return err
	}
	// CloudFront cache policies accept only "none" or "whitelist" for headers — "all"
	// and "allExcept" are rejected by the AWS API. Guard here with a clear message
	// rather than surfacing an opaque apply-time error.
	headerBehavior := ruleBehavior(cdn.Headers, "none")
	if headerBehavior == "all" || headerBehavior == "allExcept" {
		return fmt.Errorf("CDN.Headers cannot be AllowAll()/AllowAllExcept() — CloudFront cache policies only allow headers via AllowNone() or Allow(...); got %q behavior", headerBehavior)
	}
	headerCfg, err := cacheKeyBlock("header", "Headers", headerBehavior, cdn.Headers.Items())
	if err != nil {
		return err
	}

	policy := map[string]any{
		"name": "${var.project_name}-${var.stage}-server-cache-policy",
		// AWS caps CloudFront cache-policy Comment at 128 chars — keep this short.
		"comment":     "Gothic server behavior: honor origin Cache-Control (ISR); cache key/forwarding per Deploy.Providers.AWS.CDN",
		"default_ttl": 0,
		"min_ttl":     0,
		"max_ttl":     31536000,
		"parameters_in_cache_key_and_forwarded_to_origin": map[string]any{
			"enable_accept_encoding_brotli": false,
			"enable_accept_encoding_gzip":   false,
			"cookies_config":                cookieCfg,
			"headers_config":                headerCfg,
			"query_strings_config":          queryCfg,
		},
	}
	doc := map[string]any{
		"resource": map[string]any{
			"aws_cloudfront_cache_policy": map[string]any{
				"server": policy,
			},
		},
	}
	return writeJSON(filepath.Join(workDir, "gothic_cache_policy.tf.json"), doc)
}

// ruleBehavior returns an AllowRule's CloudFront behavior string, substituting the
// per-field default ("all" for query params, "none" for cookies/headers) when the
// rule is unset (zero value).
func ruleBehavior(r config.AllowRule, dflt string) string {
	if b := r.Behavior(); b != "" {
		return b
	}
	return dflt
}

// cacheKeyBlock builds one *_config block (cookies_config / headers_config /
// query_strings_config) for the cache policy. kind is the singular noun used for
// both the behavior key ("<kind>_behavior") and the nested items block
// ("<kind>s" — query_strings/cookies/headers); field is the user-facing CDN field
// name for error messages. The items block is included ONLY for whitelist /
// allExcept, and those behaviors require a non-empty name list.
func cacheKeyBlock(kind, field, behavior string, items []string) (map[string]any, error) {
	block := map[string]any{kind + "_behavior": behavior}
	if behavior == "whitelist" || behavior == "allExcept" {
		if len(items) == 0 {
			return nil, fmt.Errorf("CDN.%s uses Allow(...)/AllowAllExcept(...) but names no values: pass at least one name, e.g. gothic.Allow(\"lang\")", field)
		}
		// Pluralize the item-block key: query_string→query_strings, cookie→cookies,
		// header→headers.
		block[kind+"s"] = map[string]any{"items": items}
	}
	return block, nil
}

// mergeUserInfra copies every *.tf / *.tf.json file directly inside infraDir into

// mergeUserInfra copies every *.tf / *.tf.json file directly inside infraDir into
// workDir using a flat layout (base name only), so the user's resources join the
// SAME OpenTofu module + state as the Gothic stack. Non-.tf files are ignored;
// subdirectories are not descended into. A file whose base name collides with a
// Gothic-generated file is rejected (never silently overwritten). An empty or
// absent infraDir is a no-op.
func (g *Generator) mergeUserInfra(workDir, infraDir string) error {
	if infraDir == "" {
		return nil
	}
	entries, err := os.ReadDir(infraDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no custom infra — nothing to merge
		}
		return fmt.Errorf("reading infra dir %q: %w", infraDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !isTofuFile(name) {
			continue // ignore non-.tf files (READMEs, .gitkeep, etc.)
		}
		if gothicReservedNames[name] {
			return fmt.Errorf("infra/%s collides with a Gothic-generated file; rename it", name)
		}
		data, err := os.ReadFile(filepath.Join(infraDir, name))
		if err != nil {
			return fmt.Errorf("reading infra file %q: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(workDir, name), data, 0644); err != nil {
			return fmt.Errorf("writing infra file %q into work dir: %w", name, err)
		}
	}
	return nil
}

// isTofuFile reports whether name is an OpenTofu config file (.tf or .tf.json).
// The .tf.json suffix is checked before .tf so it is not misclassified.
func isTofuFile(name string) bool {
	return strings.HasSuffix(name, ".tf.json") || strings.HasSuffix(name, ".tf")
}

// writeEmbeddedBase copies every embedded base file under embedded/aws/ into
// workDir using a flat layout (base name only).
func (g *Generator) writeEmbeddedBase(workDir string) error {
	const root = "embedded/aws"
	return fs.WalkDir(TofuTemplateFS, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := TofuTemplateFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded %q: %w", path, err)
		}
		dst := filepath.Join(workDir, filepath.Base(path))
		if err := os.WriteFile(dst, data, 0644); err != nil {
			return fmt.Errorf("write %q: %w", dst, err)
		}
		return nil
	})
}

// writeVars builds gothic_vars.auto.tfvars.json: concrete VALUES for the
// variables declared in variables.tf.json. It must NOT re-declare them (a second
// "variable" block would collide with variables.tf.json — OpenTofu rejects
// duplicate variable declarations). OpenTofu auto-loads *.auto.tfvars.json.
func (g *Generator) writeVars(workDir string, params TfGenParams) error {
	// Serving a custom domain over CloudFront needs an ACM certificate in us-east-1.
	// Gothic can create + DNS-validate one itself (via the aws.us_east_1 provider
	// alias, so it works from any stack region) when a HostedZoneId is given, or use
	// a BYO CertificateArn. A CustomDomain with neither has no way to obtain a
	// certificate — fail loudly rather than silently dropping the domain.
	if params.CustomDomain != nil && params.CertificateArn == nil && params.HostedZoneId == nil {
		return fmt.Errorf("stage %q sets CustomDomain but neither HostedZoneId nor CertificateArn: set HostedZoneId to have Gothic create + DNS-validate the ACM certificate for you (in us-east-1, from any stack region), or set CertificateArn to reuse an existing us-east-1 certificate", params.Stage)
	}

	// Gate booleans (plan-time known). enableManagedCert: Gothic creates the cert
	// itself (domain + hosted zone, no BYO arn). enableCustomDomain: the domain is
	// served because we have a certificate one way or the other. enableRoute53: also
	// create the alias A record (needs a hosted zone; DNS may be managed elsewhere).
	enableManagedCert := params.CustomDomain != nil && params.CertificateArn == nil && params.HostedZoneId != nil
	enableCustomDomain := params.CustomDomain != nil && (params.CertificateArn != nil || enableManagedCert)
	enableRoute53 := enableCustomDomain && params.HostedZoneId != nil

	values := map[string]any{
		"project_name":   params.ProjectName,
		"stage":          params.Stage,
		"suffix":         params.Suffix,
		"region":         params.Region,
		"profile":        params.Profile,
		"server_memory":  params.ServerMemory,
		"server_timeout": params.ServerTimeout,
		// ecr_image_uri is intentionally NOT here — it is only known after the
		// image is built+pushed, so Deploy writes gothic_image.auto.tfvars.json
		// with it after Build. Its variable defaults to "" for init/validate.
		"bucket_name":  params.BucketName,
		"lambda_name":  params.LambdaName,
		"state_bucket": params.StateBucket,
		"lock_table":   params.LockTable,
		// The actual domain / zone / cert / WAF VALUES are resolved into locals by
		// writeEnvResolved (they may come from a data source, so they cannot be
		// literal tfvars). Only these plan-time-known booleans live here: they gate
		// the count of the optional resources, which cannot depend on a data-source
		// value that is unknown until apply.
		"enable_custom_domain": enableCustomDomain,
		"enable_managed_cert":  enableManagedCert,
		"enable_route53":       enableRoute53,
		"enable_waf":           params.WafArn != nil,
	}
	return writeJSON(filepath.Join(workDir, "gothic_vars.auto.tfvars.json"), values)
}

// writeEnvResolved builds env_resolved.tf.json: data-source blocks for SSM /
// Secrets Manager backed values, plus a locals.env_vars map merging every source
// into the single map the Lambda resource consumes.
func (g *Generator) writeEnvResolved(workDir string, params TfGenParams) error {
	ssmBlocks := map[string]any{}
	secretBlocks := map[string]any{}

	// resolve turns an EnvValue into the tf expression the config should use — a
	// raw literal for RawEnv, or a "${data.*}" interpolation for SSM / Secrets
	// Manager — registering the corresponding data-source block as a side effect.
	// dataName is the (sanitized) data-source resource name to register under.
	resolve := func(dataName string, v config.EnvValue) string {
		safe := safeKeyPattern.ReplaceAllString(dataName, "_")
		switch v.Source {
		case config.SSMParamEnv:
			ssmBlocks[safe] = map[string]any{
				"name":            v.Value,
				"with_decryption": true,
			}
			return wrapJSONKey(fmt.Sprintf("data.aws_ssm_parameter.%s.value", safe), v.JSONKey)
		case config.SecretsManagerEnv:
			secretBlocks[safe] = map[string]any{
				"secret_id": v.Value,
			}
			return wrapJSONKey(fmt.Sprintf("data.aws_secretsmanager_secret_version.%s.secret_string", safe), v.JSONKey)
		default: // RawEnv
			return v.Value
		}
	}

	localEnv := map[string]string{}
	for k, v := range params.EnvVars {
		localEnv[k] = resolve(k, v)
	}

	locals := map[string]any{"env_vars": localEnv}

	// The four domain locals are ALWAYS defined (default "") so resources that read
	// local.custom_domain / local.hosted_zone_id / local.certificate_arn /
	// local.waf_arn still resolve when the stage declares no custom domain (their
	// resources are then gated off by the enable_* booleans). Data-source names are
	// prefixed with "gothic_" so they can never collide with a user ENV key.
	domainLocal := func(localName, dataName string, v *config.EnvValue) {
		if v == nil {
			locals[localName] = ""
			return
		}
		locals[localName] = resolve(dataName, *v)
	}
	domainLocal("custom_domain", "gothic_custom_domain", params.CustomDomain)
	domainLocal("hosted_zone_id", "gothic_hosted_zone_id", params.HostedZoneId)
	domainLocal("certificate_arn", "gothic_certificate_arn", params.CertificateArn)
	domainLocal("waf_arn", "gothic_waf_arn", params.WafArn)

	dataBlocks := map[string]any{}
	if len(ssmBlocks) > 0 {
		dataBlocks["aws_ssm_parameter"] = ssmBlocks
	}
	if len(secretBlocks) > 0 {
		dataBlocks["aws_secretsmanager_secret_version"] = secretBlocks
	}

	doc := map[string]any{"locals": locals}
	// A `"data": {}` block is invalid config (a data block needs a type label).
	// Only emit `data` when there are real SSM / Secrets Manager sources.
	if len(dataBlocks) > 0 {
		doc["data"] = dataBlocks
	}
	return writeJSON(filepath.Join(workDir, "env_resolved.tf.json"), doc)
}

// wrapJSONKey turns a bare Terraform data-source expression into the "${...}"
// interpolation the config consumes. When jsonKey is set (via EnvValue.Get), the
// expression is decoded and indexed — jsondecode(expr)["jsonKey"] — so a JSON
// Secrets Manager / SSM value yields just that field instead of the whole blob.
func wrapJSONKey(expr, jsonKey string) string {
	if jsonKey == "" {
		return "${" + expr + "}"
	}
	return fmt.Sprintf("${jsondecode(%s)[%q]}", expr, jsonKey)
}

// def wraps a concrete value in a {"default": v} variable override.
func def(v any) map[string]any { return map[string]any{"default": v} }

// writeJSON marshals v with indentation and writes it to path.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %q: %w", path, err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}
