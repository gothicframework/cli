package tfgen

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/gothicframework/core/config"
)

// safeKeyPattern sanitises an env var key into a valid OpenTofu data-source
// resource name: anything outside [a-zA-Z0-9_] becomes "_".
var safeKeyPattern = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// TfGenParams carries every concrete per-deployment value the generator needs
// to materialise vars.tf.json and env_resolved.tf.json. The base .tf.json files
// reference these via "${var.*}" / "${local.env_vars}".
type TfGenParams struct {
	ProjectName    string
	Stage          string
	Suffix         string
	Region         string
	Profile        string
	ServerMemory   int
	ServerTimeout  int
	ECRImageURI    string
	BucketName     string
	LambdaName     string
	StateBucket string
	LockTable   string
	EnvVars     map[string]config.EnvValue
	// The custom-domain fields are source-aware EnvValues (raw / SSM / Secrets
	// Manager), resolved into locals by writeEnvResolved exactly like EnvVars. A nil
	// pointer means the stage did not declare that field.
	WafArn         *config.EnvValue
	CustomDomain   *config.EnvValue
	HostedZoneId   *config.EnvValue
	CertificateArn *config.EnvValue
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
	if err := writeJSON(filepath.Join(workDir, "gothic_outputs.tf.json"), map[string]any{}); err != nil {
		return err
	}
	return nil
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
