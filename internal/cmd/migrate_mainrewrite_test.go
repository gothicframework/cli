package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMain(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "main.go")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

const stockMain = `package main

import (
	"log"
	"net/http"
	"os"

	"demo/src/routes"
	gothicComponents "github.com/gothicframework/components"
	gothicRoutes "github.com/gothicframework/core/router"

	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()

	router := chi.NewMux()
	router.Use(middleware.Logger)

	// App configuration comment.
	gothicRoutes.Setup(router, gothicRoutes.AppConfig{
		CacheStrategy:         gothicRoutes.CACHE_CONTROL_HEADERS,
		LocalDevelopmentCache: gothicRoutes.IN_MEMORY,
		ServeStaticFiles:      gothicRoutes.HOT_RELOAD_ONLY,
	}, routes.RegisterFileBasedRoutes)

	// OptimizedImage comment.
	gothicComponents.OptimizedImageConfig.RegisterRoute(router, "/optimizedImage/{name}/{extension}", gothicComponents.OptimizedImage)

	port := os.Getenv("HTTP_LISTEN_ADDR")
	log.Fatal(http.ListenAndServe(port, router))
}
`

func TestRewriteMainStock(t *testing.T) {
	p := writeMain(t, stockMain)
	rw, rt, err := rewriteMainGoV3(p)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !rw {
		t.Fatal("expected the stock main.go to be rewritten")
	}
	if !strings.Contains(rt, "gothic.RuntimeConfig") || !strings.Contains(rt, "gothic.CACHE_CONTROL_HEADERS") {
		t.Errorf("runtime literal should carry the default config, got: %q", rt)
	}
	// The old StaticFilesMode name must be migrated to the current one (the ordinal
	// is unchanged; the identifier was renamed HOT_RELOAD_ONLY→CDN).
	if !strings.Contains(rt, "gothic.CDN") {
		t.Errorf("runtime literal should rename ServeStaticFiles to gothic.CDN, got: %q", rt)
	}
	if strings.Contains(rt, "HOT_RELOAD_ONLY") {
		t.Errorf("runtime literal should NOT keep the legacy HOT_RELOAD_ONLY name, got: %q", rt)
	}
	got, _ := os.ReadFile(p)
	s := string(got)
	for _, want := range []string{
		"router.Use(gothicServer.Middleware(Config.Runtime))",
		"routes.RegisterFileBasedRoutes(router)",
		"gothicframework/middlewares",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n---\n%s", want, s)
		}
	}
	for _, bad := range []string{
		"gothicRoutes.Setup",
		"OptimizedImageConfig.RegisterRoute",
		"core/router",  // framework routes import removed (unused)
		"components\"", // bare components import removed (middlewares import survives)
	} {
		if strings.Contains(s, bad) {
			t.Errorf("output should not contain %q\n---\n%s", bad, s)
		}
	}
	// Preserved boilerplate.
	if !strings.Contains(s, "router.Use(middleware.Logger)") {
		t.Error("logger middleware should be preserved")
	}
}

const customMain = `package main

import (
	"log"
	"net/http"
	"os"

	"demo/src/routes"
	gothicComponents "github.com/gothicframework/components"
	gothicRoutes "github.com/gothicframework/core/router"

	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()

	router := chi.NewMux()
	router.Use(middleware.Logger)
	router.Use(myCORS)
	router.Get("/health", healthHandler)

	gothicRoutes.Setup(router, gothicRoutes.AppConfig{
		CacheStrategy: gothicRoutes.REDIS,
	}, routes.RegisterFileBasedRoutes)

	gothicComponents.OptimizedImageConfig.RegisterRoute(router, "/optimizedImage/{name}/{extension}", gothicComponents.OptimizedImage)

	port := os.Getenv("HTTP_LISTEN_ADDR")
	log.Fatal(http.ListenAndServe(port, router))
}
`

func TestRewriteMainPreservesCustomCode(t *testing.T) {
	p := writeMain(t, customMain)
	rw, rt, err := rewriteMainGoV3(p)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !rw {
		t.Fatal("expected rewrite")
	}
	got, _ := os.ReadFile(p)
	s := string(got)
	// Custom code preserved verbatim.
	for _, want := range []string{
		"router.Use(myCORS)",
		`router.Get("/health", healthHandler)`,
		"router.Use(gothicServer.Middleware(Config.Runtime))",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, "gothicRoutes.Setup") {
		t.Errorf("Setup should be gone\n---\n%s", s)
	}
	// The non-default cache (REDIS) is carried into the RuntimeConfig literal for
	// the caller to inject into gothic.config.go.
	if !strings.Contains(rt, "gothic.REDIS") || !strings.Contains(rt, "gothic.RuntimeConfig") {
		t.Errorf("runtime literal should carry gothic.REDIS, got: %q", rt)
	}
}

func TestInjectRuntimeConfig(t *testing.T) {
	cfg := `package main

import gothic "github.com/gothicframework/core/config"

var Config = gothic.Config{
	ProjectName: "demo",
	// Runtime doc comment stays put.
	Runtime: gothic.RuntimeConfig{
		CacheStrategy: gothic.CACHE_CONTROL_HEADERS,
	},
	Deploy: &gothic.DeployConfig{Provider: gothic.AWS},
}
`
	dir := t.TempDir()
	p := filepath.Join(dir, "gothic.config.go")
	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	lit := "gothic.RuntimeConfig{\n\t\tCacheStrategy: gothic.REDIS,\n\t}"
	if err := injectRuntimeConfig(p, lit); err != nil {
		t.Fatalf("inject: %v", err)
	}
	got, _ := os.ReadFile(p)
	s := string(got)
	if !strings.Contains(s, "gothic.REDIS") {
		t.Errorf("Runtime value not replaced\n---\n%s", s)
	}
	if strings.Contains(s, "CACHE_CONTROL_HEADERS") {
		t.Errorf("old Runtime value should be gone\n---\n%s", s)
	}
	// Doc comment + other fields preserved.
	for _, want := range []string{"Runtime doc comment stays put", `ProjectName: "demo"`, "Deploy:"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing preserved %q\n---\n%s", want, s)
		}
	}
}

func TestRewriteMainSkipsWhenNotRecognized(t *testing.T) {
	alreadyV3 := `package main

import (
	"demo/src/routes"
	gothicServer "github.com/gothicframework/middlewares"
	"github.com/go-chi/chi/v5"
)

func main() {
	router := chi.NewMux()
	router.Use(gothicServer.Middleware(Config.Runtime))
	routes.RegisterFileBasedRoutes(router)
}
`
	p := writeMain(t, alreadyV3)
	rw, _, err := rewriteMainGoV3(p)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if rw {
		t.Fatal("an already-v3 main.go must be left untouched")
	}
	got, _ := os.ReadFile(p)
	if string(got) != alreadyV3 {
		t.Errorf("file was modified but should not have been\n---\n%s", got)
	}
}
