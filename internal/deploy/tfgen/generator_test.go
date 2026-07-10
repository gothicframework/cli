package tfgen

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gothicframework/core/config"
)

func minimalParams() TfGenParams {
	return TfGenParams{
		ProjectName:   "demo",
		Stage:         "dev",
		Suffix:        "abc123",
		Region:        "us-east-1",
		Profile:       "default",
		ServerMemory:  512,
		ServerTimeout: 30,
		BucketName:    "demo-dev-abc123",
		LambdaName:    "demo-dev-abc123",
		StateBucket:   "demo-state-abc123",
		LockTable:     "demo-lock-abc123",
		EnvVars:       map[string]config.EnvValue{},
	}
}

func prepareInto(t *testing.T, params TfGenParams) string {
	t.Helper()
	dir := t.TempDir()
	g := NewGenerator()
	if err := g.Prepare(dir, params); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return dir
}

func TestPrepareWritesEveryEmbeddedFile(t *testing.T) {
	dir := prepareInto(t, minimalParams())

	const root = "embedded/aws"
	err := fs.WalkDir(TofuTemplateFS, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if _, statErr := os.Stat(filepath.Join(dir, base)); statErr != nil {
			t.Errorf("embedded file %q not written to workDir: %v", base, statErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded: %v", err)
	}
}

// readTfJSONFiles returns every *.tf.json file written into dir.
func readTfJSONFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf.json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = b
	}
	return out
}

func TestPrepareNoGoTemplateMarkers(t *testing.T) {
	dir := prepareInto(t, minimalParams())
	for name, content := range readTfJSONFiles(t, dir) {
		if bytes.Contains(content, []byte("{{")) {
			t.Errorf("%s contains a Go template marker {{", name)
		}
	}
}

func TestPrepareTfJSONIsValidJSON(t *testing.T) {
	dir := prepareInto(t, minimalParams())
	files := readTfJSONFiles(t, dir)
	if len(files) == 0 {
		t.Fatal("no .tf.json files produced")
	}
	for name, content := range files {
		var any any
		if err := json.Unmarshal(content, &any); err != nil {
			t.Errorf("%s is not valid JSON: %v", name, err)
		}
	}
}

func TestPrepareVarsDefaults(t *testing.T) {
	params := minimalParams()
	dir := prepareInto(t, params)

	// vars are now supplied as VALUES via an auto-loaded tfvars file, not a second
	// "variable" block (which would duplicate variables.tf.json and break init).
	b, err := os.ReadFile(filepath.Join(dir, "gothic_vars.auto.tfvars.json"))
	if err != nil {
		t.Fatalf("read gothic_vars.auto.tfvars.json: %v", err)
	}
	var vals map[string]any
	if err := json.Unmarshal(b, &vals); err != nil {
		t.Fatalf("unmarshal vars: %v", err)
	}
	if got := vals["project_name"]; got != params.ProjectName {
		t.Errorf("project_name = %v, want %q", got, params.ProjectName)
	}
	// The domain VALUE no longer lives in the vars file (it is resolved into a
	// local); only the plan-time gate boolean does, defaulting to false.
	if _, ok := vals["custom_domain"]; ok {
		t.Error("custom_domain must not be a tfvar anymore (resolved into local.custom_domain)")
	}
	if got, ok := vals["enable_custom_domain"]; !ok || got != false {
		t.Errorf("enable_custom_domain = %v, want false", got)
	}
	if _, isDecl := vals["variable"]; isDecl {
		t.Error("vars file must carry VALUES, not a variable{} declaration block")
	}
}

// envResolvedDoc is the shape of env_resolved.tf.json for assertions.
type envResolvedDoc struct {
	Data   map[string]map[string]any `json:"data"`
	Locals struct {
		EnvVars map[string]string `json:"env_vars"`
	} `json:"locals"`
}

func readEnvResolved(t *testing.T, dir string) envResolvedDoc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "env_resolved.tf.json"))
	if err != nil {
		t.Fatalf("read env_resolved.tf.json: %v", err)
	}
	var doc envResolvedDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal env_resolved: %v", err)
	}
	return doc
}

func TestEnvSplittingRaw(t *testing.T) {
	params := minimalParams()
	params.EnvVars = map[string]config.EnvValue{
		"PORT": {Source: config.RawEnv, Value: "8080"},
	}
	doc := readEnvResolved(t, prepareInto(t, params))

	if got := doc.Locals.EnvVars["PORT"]; got != "8080" {
		t.Errorf("env_vars.PORT = %q, want 8080", got)
	}
	if len(doc.Data) != 0 {
		t.Errorf("expected no data blocks for a raw env var, got %v", doc.Data)
	}
}

func TestEnvSplittingSSM(t *testing.T) {
	params := minimalParams()
	params.EnvVars = map[string]config.EnvValue{
		"DB": {Source: config.SSMParamEnv, Value: "/app/db"},
	}
	doc := readEnvResolved(t, prepareInto(t, params))

	ssm, ok := doc.Data["aws_ssm_parameter"]
	if !ok {
		t.Fatal("expected aws_ssm_parameter data block")
	}
	if _, ok := ssm["DB"]; !ok {
		t.Errorf("expected data.aws_ssm_parameter.DB, got keys %v", ssm)
	}
	if got := doc.Locals.EnvVars["DB"]; !strings.Contains(got, "${data.aws_ssm_parameter.DB.value}") {
		t.Errorf("env_vars.DB = %q, want it to reference the SSM data source", got)
	}
}

func TestEnvSplittingSecretsManager(t *testing.T) {
	params := minimalParams()
	params.EnvVars = map[string]config.EnvValue{
		"API_KEY": {Source: config.SecretsManagerEnv, Value: "/app/key"},
	}
	doc := readEnvResolved(t, prepareInto(t, params))

	sm, ok := doc.Data["aws_secretsmanager_secret_version"]
	if !ok {
		t.Fatal("expected aws_secretsmanager_secret_version data block")
	}
	if _, ok := sm["API_KEY"]; !ok {
		t.Errorf("expected data.aws_secretsmanager_secret_version.API_KEY, got keys %v", sm)
	}
	if got := doc.Locals.EnvVars["API_KEY"]; !strings.Contains(got, "${data.aws_secretsmanager_secret_version.API_KEY.secret_string}") {
		t.Errorf("env_vars.API_KEY = %q, want it to reference the Secrets Manager data source", got)
	}
}

func TestEnvSplittingSecretsManagerJSONKey(t *testing.T) {
	// A JSON secret with .Get("password") must resolve through jsondecode + index,
	// not inject the whole secret_string blob.
	params := minimalParams()
	params.EnvVars = map[string]config.EnvValue{
		"DB_PASS": {Source: config.SecretsManagerEnv, Value: "/app/creds", JSONKey: "password"},
	}
	doc := readEnvResolved(t, prepareInto(t, params))

	if _, ok := doc.Data["aws_secretsmanager_secret_version"]["DB_PASS"]; !ok {
		t.Fatalf("expected secret data block DB_PASS, got %v", doc.Data)
	}
	got := doc.Locals.EnvVars["DB_PASS"]
	want := `${jsondecode(data.aws_secretsmanager_secret_version.DB_PASS.secret_string)["password"]}`
	if got != want {
		t.Errorf("env_vars.DB_PASS = %q, want %q", got, want)
	}
}

func TestEnvSplittingSSMJSONKey(t *testing.T) {
	params := minimalParams()
	params.EnvVars = map[string]config.EnvValue{
		"HOST": {Source: config.SSMParamEnv, Value: "/app/config", JSONKey: "host"},
	}
	doc := readEnvResolved(t, prepareInto(t, params))
	got := doc.Locals.EnvVars["HOST"]
	want := `${jsondecode(data.aws_ssm_parameter.HOST.value)["host"]}`
	if got != want {
		t.Errorf("env_vars.HOST = %q, want %q", got, want)
	}
}

func TestEnvSplittingSanitizesKey(t *testing.T) {
	// A key with a dot must be sanitised to a valid data-source resource name.
	params := minimalParams()
	params.EnvVars = map[string]config.EnvValue{
		"app.db": {Source: config.SSMParamEnv, Value: "/app/db"},
	}
	doc := readEnvResolved(t, prepareInto(t, params))
	ssm := doc.Data["aws_ssm_parameter"]
	if _, ok := ssm["app_db"]; !ok {
		t.Errorf("expected sanitised key app_db, got keys %v", ssm)
	}
	// The locals map preserves the original env var name as the key.
	if got := doc.Locals.EnvVars["app.db"]; !strings.Contains(got, "app_db") {
		t.Errorf("env_vars[app.db] = %q, want it to reference app_db data source", got)
	}
}

func TestResourcesUseConditionalCount(t *testing.T) {
	// Confirm the embedded resources file gates ACM/Route53/WAF on the
	// count = var.X != "" ? 1 : 0 pattern (custom domain conditional).
	b, err := TofuTemplateFS.ReadFile("embedded/aws/resources.tf.json")
	if err != nil {
		t.Fatalf("read resources: %v", err)
	}
	if !bytes.Contains(b, []byte(`var.enable_route53 ? 1 : 0`)) {
		t.Error(`resources.tf.json should gate the Route53 record with count = var.enable_route53 ? 1 : 0`)
	}
	if !bytes.Contains(b, []byte(`var.enable_custom_domain ?`)) {
		t.Error(`resources.tf.json should switch CloudFront aliases/viewer_certificate on var.enable_custom_domain`)
	}
}

// TestDomainFieldSplitting verifies the 4 custom-domain fields resolve exactly like
// ENV: SSM/Secrets sources become data-source-backed locals, raw stays literal, an
// unset field still yields an (empty) local, and the enable_* gate booleans track
// whether each field was declared.
func TestDomainFieldSplitting(t *testing.T) {
	params := minimalParams()
	params.CustomDomain = &config.EnvValue{Source: config.SSMParamEnv, Value: "/app/domain"}
	params.CertificateArn = &config.EnvValue{Source: config.SecretsManagerEnv, Value: "/app/cert"}
	params.HostedZoneId = &config.EnvValue{Source: config.RawEnv, Value: "Z123"}
	// WafArn intentionally left nil.
	dir := prepareInto(t, params)

	var doc struct {
		Data   map[string]map[string]any `json:"data"`
		Locals map[string]any            `json:"locals"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "env_resolved.tf.json"))
	if err != nil {
		t.Fatalf("read env_resolved: %v", err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal env_resolved: %v", err)
	}

	if got := doc.Locals["custom_domain"]; got != "${data.aws_ssm_parameter.gothic_custom_domain.value}" {
		t.Errorf("local.custom_domain = %v, want SSM data-source reference", got)
	}
	if got := doc.Locals["certificate_arn"]; got != "${data.aws_secretsmanager_secret_version.gothic_certificate_arn.secret_string}" {
		t.Errorf("local.certificate_arn = %v, want Secrets Manager reference", got)
	}
	if got := doc.Locals["hosted_zone_id"]; got != "Z123" {
		t.Errorf("local.hosted_zone_id = %v, want raw literal Z123", got)
	}
	if got, ok := doc.Locals["waf_arn"]; !ok || got != "" {
		t.Errorf("local.waf_arn = %v (present=%v), want an empty string for an unset field", got, ok)
	}
	if _, ok := doc.Data["aws_ssm_parameter"]["gothic_custom_domain"]; !ok {
		t.Error("expected an SSM data source for custom_domain")
	}
	if _, ok := doc.Data["aws_secretsmanager_secret_version"]["gothic_certificate_arn"]; !ok {
		t.Error("expected a Secrets Manager data source for certificate_arn")
	}

	var vals map[string]any
	vb, err := os.ReadFile(filepath.Join(dir, "gothic_vars.auto.tfvars.json"))
	if err != nil {
		t.Fatalf("read vars: %v", err)
	}
	if err := json.Unmarshal(vb, &vals); err != nil {
		t.Fatalf("unmarshal vars: %v", err)
	}
	if vals["enable_custom_domain"] != true {
		t.Errorf("enable_custom_domain = %v, want true", vals["enable_custom_domain"])
	}
	// A BYO CertificateArn is set here, so Gothic does NOT manage the cert.
	if vals["enable_managed_cert"] != false {
		t.Errorf("enable_managed_cert = %v, want false (BYO cert)", vals["enable_managed_cert"])
	}
	if vals["enable_waf"] != false {
		t.Errorf("enable_waf = %v, want false", vals["enable_waf"])
	}
}

// TestManagedCertMode verifies that a CustomDomain + HostedZoneId with NO BYO
// CertificateArn switches on the managed-certificate path (Gothic creates + DNS-
// validates the ACM cert itself) and still serves the domain + Route53 record.
func TestManagedCertMode(t *testing.T) {
	params := minimalParams()
	params.CustomDomain = &config.EnvValue{Source: config.RawEnv, Value: "app.example.com"}
	params.HostedZoneId = &config.EnvValue{Source: config.SSMParamEnv, Value: "/app/zone"}
	// CertificateArn intentionally nil.
	dir := prepareInto(t, params)

	var vals map[string]any
	b, err := os.ReadFile(filepath.Join(dir, "gothic_vars.auto.tfvars.json"))
	if err != nil {
		t.Fatalf("read vars: %v", err)
	}
	if err := json.Unmarshal(b, &vals); err != nil {
		t.Fatalf("unmarshal vars: %v", err)
	}
	for k, want := range map[string]bool{
		"enable_managed_cert":  true,
		"enable_custom_domain": true,
		"enable_route53":       true,
	} {
		if vals[k] != want {
			t.Errorf("%s = %v, want %v", k, vals[k], want)
		}
	}
}

// TestCustomDomainWithoutCertOrZone fails the generation with a clear error when a
// CustomDomain is set but there is no way to obtain a certificate (neither a hosted
// zone to auto-create one, nor a BYO arn).
func TestCustomDomainWithoutCertOrZone(t *testing.T) {
	params := minimalParams()
	params.CustomDomain = &config.EnvValue{Source: config.RawEnv, Value: "app.example.com"}
	// HostedZoneId and CertificateArn both nil.
	dir := t.TempDir()
	err := NewGenerator().Prepare(dir, params)
	if err == nil {
		t.Fatal("expected an error when CustomDomain has neither HostedZoneId nor CertificateArn")
	}
	if !strings.Contains(err.Error(), "HostedZoneId") || !strings.Contains(err.Error(), "CertificateArn") {
		t.Errorf("error should mention both HostedZoneId and CertificateArn, got: %v", err)
	}
}
