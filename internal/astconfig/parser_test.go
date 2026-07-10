package astconfig

import (
	"strings"
	"testing"

	config "github.com/gothicframework/core/config"
)

func strPtr(s string) *string { return &s }

func TestParseBasic(t *testing.T) {
	cfg, err := Parse("testdata/basic")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if cfg.ProjectName != "basicapp" {
		t.Errorf("ProjectName = %q, want %q", cfg.ProjectName, "basicapp")
	}
	if cfg.GoModName != "example.com/basicapp" {
		t.Errorf("GoModName = %q, want %q", cfg.GoModName, "example.com/basicapp")
	}
	if cfg.Deploy == nil {
		t.Fatal("Deploy is nil")
	}
	if cfg.Deploy.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", cfg.Deploy.Region)
	}
	if cfg.Deploy.Profile != "default" {
		t.Errorf("Profile = %q, want default", cfg.Deploy.Profile)
	}
	if cfg.Deploy.ServerMemory != 512 {
		t.Errorf("ServerMemory = %d, want 512", cfg.Deploy.ServerMemory)
	}
	if cfg.Deploy.ServerTimeout != 30 {
		t.Errorf("ServerTimeout = %d, want 30", cfg.Deploy.ServerTimeout)
	}
	dev, ok := cfg.Deploy.Stages["dev"]
	if !ok {
		t.Fatal("missing dev stage")
	}
	port, ok := dev.ENV["PORT"]
	if !ok {
		t.Fatal("missing PORT env")
	}
	if port.Source != config.RawEnv || port.Value != "8080" {
		t.Errorf("PORT = %+v, want {RawEnv, 8080}", port)
	}
}

func TestParseFull(t *testing.T) {
	cfg, err := Parse("testdata/full")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if cfg.ProjectName != "fullapp" {
		t.Errorf("ProjectName = %q", cfg.ProjectName)
	}
	if cfg.GoModName != "example.com/fullapp" {
		t.Errorf("GoModName = %q", cfg.GoModName)
	}
	if cfg.TofuBinaryPath != "/usr/local/bin/tofu" {
		t.Errorf("TofuBinaryPath = %q", cfg.TofuBinaryPath)
	}
	if cfg.WasmBinary != "/opt/tinygo/bin/tinygo" {
		t.Errorf("WasmBinary = %q", cfg.WasmBinary)
	}
	if cfg.TailwindBinary != "/opt/tailwind/tailwindcss" {
		t.Errorf("TailwindBinary = %q", cfg.TailwindBinary)
	}
	if cfg.OptimizeImages.LowResolutionRate != 20 {
		t.Errorf("LowResolutionRate = %d, want 20", cfg.OptimizeImages.LowResolutionRate)
	}
	if cfg.Deploy == nil {
		t.Fatal("Deploy is nil")
	}
	if cfg.Deploy.ServerMemory != 1024 || cfg.Deploy.ServerTimeout != 60 {
		t.Errorf("server mem/timeout = %d/%d", cfg.Deploy.ServerMemory, cfg.Deploy.ServerTimeout)
	}
	if cfg.Deploy.Region != "us-west-2" || cfg.Deploy.Profile != "prod-profile" {
		t.Errorf("region/profile = %q/%q", cfg.Deploy.Region, cfg.Deploy.Profile)
	}

	if _, ok := cfg.Deploy.Stages["dev"]; !ok {
		t.Error("missing dev stage")
	}
	prod, ok := cfg.Deploy.Stages["prod"]
	if !ok {
		t.Fatal("missing prod stage")
	}

	// Optional source-aware domain fields: each parses to an *EnvValue carrying the
	// builder's source (raw / SSM / Secrets Manager) and argument.
	checkVal := func(name string, got *config.EnvValue, want config.EnvValue) {
		if got == nil {
			t.Errorf("%s is nil, want %+v", name, want)
		} else if *got != want {
			t.Errorf("%s = %+v, want %+v", name, *got, want)
		}
	}
	checkVal("HostedZoneId", prod.HostedZoneId, config.EnvValue{Source: config.SSMParamEnv, Value: "/fullapp/prod/hosted-zone"})
	checkVal("CustomDomain", prod.CustomDomain, config.EnvValue{Source: config.RawEnv, Value: "app.example.com"})
	checkVal("CertificateArn", prod.CertificateArn, config.EnvValue{Source: config.SecretsManagerEnv, Value: "/fullapp/prod/cert-arn"})
	checkVal("WafArn", prod.WafArn, config.EnvValue{Source: config.RawEnv, Value: "arn:aws:wafv2:us-west-2:111122223333:global/webacl/xyz"})

	// ENV source types.
	want := map[string]config.EnvValue{
		"PORT":        {Source: config.RawEnv, Value: "443"},
		"DB_URL":      {Source: config.SSMParamEnv, Value: "/fullapp/prod/db-url"},
		"API_KEY":     {Source: config.SecretsManagerEnv, Value: "/fullapp/prod/api-key"},
		"JSON_SECRET": {Source: config.SecretsManagerEnv, Value: "/fullapp/prod/creds", JSONKey: "password"},
		"JSON_PARAM":  {Source: config.SSMParamEnv, Value: "/fullapp/prod/config", JSONKey: "host"},
	}
	for k, w := range want {
		got, ok := prod.ENV[k]
		if !ok {
			t.Errorf("missing env %q", k)
			continue
		}
		if got != w {
			t.Errorf("env %q = %+v, want %+v", k, got, w)
		}
	}
}

func TestParseUnknownEnvBuilder(t *testing.T) {
	_, err := Parse("testdata/badenv")
	if err == nil {
		t.Fatal("expected error for unknown ENV builder, got nil")
	}
	if !strings.Contains(err.Error(), "unknown builder") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "unknown builder")
	}
}

func TestParseDynamicEnvDropped(t *testing.T) {
	cfg, err := Parse("testdata/dynamic")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	dev := cfg.Deploy.Stages["dev"]
	if _, ok := dev.ENV["PORT"]; !ok {
		t.Error("static PORT should be present")
	}
	if _, ok := dev.ENV["DYNAMIC"]; ok {
		t.Error("dynamic os.Getenv value should be silently dropped")
	}
}

func TestHasHook(t *testing.T) {
	tests := []struct {
		dir  string
		hook string
		want bool
	}{
		{"testdata/full", "BeforeDeploy", true},
		{"testdata/full", "AfterDeploy", false},
		{"testdata/nohook", "BeforeDeploy", false},
	}
	for _, tt := range tests {
		got, err := HasHook(tt.dir, tt.hook)
		if err != nil {
			t.Fatalf("HasHook(%s, %s) error: %v", tt.dir, tt.hook, err)
		}
		if got != tt.want {
			t.Errorf("HasHook(%s, %s) = %v, want %v", tt.dir, tt.hook, got, tt.want)
		}
	}
}

func TestParseMissingFile(t *testing.T) {
	_, err := Parse("testdata/does-not-exist")
	if err == nil {
		t.Fatal("expected error for missing gothic.config.go")
	}
}
