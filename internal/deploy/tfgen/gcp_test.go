package tfgen

// GCP stack generation tests. The pure layer — file materialization, env
// resolution, domain-mapping gating, and the raw-only source restriction —
// runs against a temp working dir with no tofu and no gcloud involved.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gothicframework/core/config"
)

func gcpParams(workDir string) TfGenParams {
	return TfGenParams{
		Provider:    config.GCP,
		GcpProject:  "my-project-id",
		ProjectName: "app",
		Stage:       "dev",
		Suffix:      "abc123",
		Region:      "southamerica-east1",
		BucketName:  "app-dev-abc123",
		LambdaName:  "app-dev-abc123",
		StateBucket: "app-state-abc123",
		EnvVars: map[string]config.EnvValue{
			"MY_VAR": config.EnvValue{Source: config.RawEnv, Value: "hello"},
		},
		InfraDir: filepath.Join(workDir, "no-infra"),
	}
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return doc
}

// TestGcpPrepareGeneratesStack verifies the full GCP working-dir contract: the
// google provider + gcs backend, the GCP variables, the dynamic resources with
// the stage's ENV entries as container env, and the gothic_* output contract.
func TestGcpPrepareGeneratesStack(t *testing.T) {
	g := NewGenerator()
	workDir := t.TempDir()

	params := gcpParams(workDir)
	if err := g.Prepare(workDir, params); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Provider/backend come from the embedded gcp main.tf.json — no aws
	// provider anywhere in the generated stack.
	main := readJSONFile(t, filepath.Join(workDir, "main.tf.json"))
	if _, ok := main["provider"].(map[string]any)["google"]; !ok {
		t.Fatalf("main.tf.json should declare a google provider, got: %v", main["provider"])
	}
	backend := main["terraform"].(map[string]any)["backend"].(map[string]any)["gcs"].(map[string]any)
	if backend["bucket"] != "${var.state_bucket}" {
		t.Errorf("gcs backend bucket = %v, want ${var.state_bucket}", backend["bucket"])
	}

	// resources.tf.json carries the stage's ENV as container env entries.
	res := readJSONFile(t, filepath.Join(workDir, "resources.tf.json"))
	crun := res["resource"].(map[string]any)["google_cloud_run_service"].(map[string]any)["main"].(map[string]any)
	containers := crun["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]interface{})
	env := containers[0].(map[string]interface{})["env"].([]interface{})
	if len(env) != 2 {
		t.Fatalf("container env count = %d, want 2 (GOTHIC_PROVIDER + MY_VAR)", len(env))
	}
	gotVar := false
	for _, e := range env {
		pair := e.(map[string]interface{})
		if pair["name"] == "MY_VAR" {
			gotVar = true
			if pair["value"] != `local.env_vars["MY_VAR"]` {
				t.Errorf("MY_VAR env value = %v, want the local.env_vars reference", pair["value"])
			}
		}
	}
	if !gotVar {
		t.Error("container env missing MY_VAR entry")
	}

	// env_resolved.tf.json resolves the raw env value into locals.
	envDoc := readJSONFile(t, filepath.Join(workDir, "env_resolved.tf.json"))
	locals := envDoc["locals"].(map[string]any)
	if got := locals["env_vars"].(map[string]any)["MY_VAR"]; got != "hello" {
		t.Errorf("locals.env_vars.MY_VAR = %v, want hello", got)
	}
	if _, ok := envDoc["data"]; ok {
		t.Error("GCP stack must not emit aws data-source blocks")
	}

	// gothic_outputs.tf.json is the user-infra locals contract.
	contract := readJSONFile(t, filepath.Join(workDir, "gothic_outputs.tf.json"))
	gLocals := contract["locals"].(map[string]any)
	for _, name := range []string{"gothic_service_name", "gothic_service_url", "gothic_bucket_name"} {
		if _, ok := gLocals[name]; !ok {
			t.Errorf("gothic_outputs.tf.json missing local %s", name)
		}
	}

	// No AWS-only files may leak into a GCP stack.
	for _, name := range []string{"gothic_cache_policy.tf.json"} {
		if _, err := os.Stat(filepath.Join(workDir, name)); err == nil {
			t.Errorf("%s must not be generated for GCP", name)
		}
	}
}

// TestGcpDomainMappingGatedOnCustomDomain verifies the domain mapping resource
// is generated when (and only when) the stage declares a CustomDomain, and that
// the enable_domain_mapping tfvar follows.
func TestGcpDomainMappingGatedOnCustomDomain(t *testing.T) {
	g := NewGenerator()

	domain := config.Env("app.example.com")

	// With a CustomDomain: mapping present + gate true.
	workDir := t.TempDir()
	params := gcpParams(workDir)
	params.CustomDomain = &domain
	if err := g.Prepare(workDir, params); err != nil {
		t.Fatalf("Prepare with domain: %v", err)
	}
	res := readJSONFile(t, filepath.Join(workDir, "resources.tf.json"))
	if _, ok := res["resource"].(map[string]any)["google_cloud_run_domain_mapping"]; !ok {
		t.Error("domain mapping resource missing when CustomDomain is set")
	}
	vars := readJSONFile(t, filepath.Join(workDir, "gothic_vars.auto.tfvars.json"))
	if vars["enable_domain_mapping"] != true {
		t.Errorf("enable_domain_mapping = %v, want true", vars["enable_domain_mapping"])
	}
	envDoc := readJSONFile(t, filepath.Join(workDir, "env_resolved.tf.json"))
	if got := envDoc["locals"].(map[string]any)["custom_domain"]; got != "app.example.com" {
		t.Errorf("local.custom_domain = %v, want app.example.com", got)
	}

	// Without a CustomDomain: no mapping resource + gate false.
	workDir = t.TempDir()
	if err := g.Prepare(workDir, gcpParams(workDir)); err != nil {
		t.Fatalf("Prepare without domain: %v", err)
	}
	res = readJSONFile(t, filepath.Join(workDir, "resources.tf.json"))
	if _, ok := res["resource"].(map[string]any)["google_cloud_run_domain_mapping"]; ok {
		t.Error("domain mapping resource must be absent without CustomDomain")
	}
	vars = readJSONFile(t, filepath.Join(workDir, "gothic_vars.auto.tfvars.json"))
	if vars["enable_domain_mapping"] != false {
		t.Errorf("enable_domain_mapping = %v, want false", vars["enable_domain_mapping"])
	}
}

// TestGcpRejectsSSMSecretsSources verifies that GCP env/domain values sourced
// from SSM or Secrets Manager fail generation loudly instead of emitting AWS
// data sources into a google-only stack.
func TestGcpRejectsSSMSecretsSources(t *testing.T) {
	g := NewGenerator()

	ssmVar := config.EnvValue{Source: config.SSMParamEnv, Value: "/app/secret"}
	workDir := t.TempDir()
	params := gcpParams(workDir)
	params.EnvVars["SECRET"] = ssmVar
	err := g.Prepare(workDir, params)
	if err == nil {
		t.Fatal("expected an error for an SSM-sourced ENV on GCP")
	}
	if !strings.Contains(err.Error(), "ENV") {
		t.Errorf("error should name the offending field, got: %v", err)
	}

	// Same restriction for the CustomDomain.
	secretDomain := config.EnvValue{Source: config.SecretsManagerEnv, Value: "app/domain"}
	workDir = t.TempDir()
	params = gcpParams(workDir)
	params.CustomDomain = &secretDomain
	err = g.Prepare(workDir, params)
	if err == nil {
		t.Fatal("expected an error for a Secrets Manager-sourced CustomDomain on GCP")
	}
	if !strings.Contains(err.Error(), "CustomDomain") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

// TestGcpPrepareRequiresRegion verifies the clear failure when Region is unset.
func TestGcpPrepareRequiresRegion(t *testing.T) {
	g := NewGenerator()
	workDir := t.TempDir()
	params := gcpParams(workDir)
	params.Region = ""
	err := g.Prepare(workDir, params)
	if err == nil || !strings.Contains(err.Error(), "region") {
		t.Fatalf("Prepare without region = %v, want a region error", err)
	}
}
