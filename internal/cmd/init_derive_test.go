package cmd

import "testing"

// TestDeriveProjectName exercises the pure module-path -> kebab-name derivation
// used by `gothic init` to default the project name from the Go module path.
func TestDeriveProjectName(t *testing.T) {
	cases := []struct {
		name       string
		modulePath string
		wantName   string
		wantOK     bool
	}{
		{"simple last segment", "github.com/x/my-app", "my-app", true},
		{"skips major version suffix", "github.com/x/my-app/v3", "my-app", true},
		{"skips v2 suffix", "github.com/example/gothicframework/v3", "gothicframework", true},
		{"underscores become dashes", "github.com/x/My_App", "my-app", true},
		{"dots become dashes", "example.com/Foo.Bar", "foo-bar", true},
		{"single-word module", "myapp", "myapp", true},
		{"uppercase single word", "MyApp", "myapp", true},
		{"leading and trailing junk trimmed", "github.com/x/-my-app-", "my-app", true},
		{"collapses dash runs", "github.com/x/my___app", "my-app", true},
		{"numbers preserved", "github.com/x/app2", "app2", true},
		{"trailing slash ignored", "github.com/x/my-app/", "my-app", true},
		{"all-invalid segment fails", "github.com/x/___", "", false},
		{"empty string fails", "", "", false},
		{"vN only keeps vN when no prior segment", "v3", "v3", true},
		{"scoped segment sanitized", "example.com/@scope", "scope", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotOK := deriveProjectName(tc.modulePath)
			if gotOK != tc.wantOK {
				t.Fatalf("deriveProjectName(%q) ok = %v, want %v (name=%q)", tc.modulePath, gotOK, tc.wantOK, gotName)
			}
			if gotOK && gotName != tc.wantName {
				t.Errorf("deriveProjectName(%q) = %q, want %q", tc.modulePath, gotName, tc.wantName)
			}
		})
	}
}

// TestDeriveProjectNameProducesValidKebab guards the invariant that any name
// deriveProjectName returns with ok=true also passes the interactive prompt's
// validation, so the two entry points can never disagree.
func TestDeriveProjectNameProducesValidKebab(t *testing.T) {
	modules := []string{
		"github.com/x/my-app",
		"github.com/x/my-app/v3",
		"github.com/x/My_App",
		"example.com/Foo.Bar",
		"myapp",
		"github.com/x/-my-app-",
	}
	for _, m := range modules {
		name, ok := deriveProjectName(m)
		if !ok {
			t.Fatalf("expected %q to derive a valid name", m)
		}
		if !isValidKebabName(name) {
			t.Errorf("derived name %q from %q is not valid kebab", name, m)
		}
	}
}
