package cmd

import (
	"os"
	"strings"
	"testing"
)

// customMainGo mirrors a hand-written v2 main.go (like the portfolio's): a chi
// router, a logger, a bespoke LOCAL_SERVE static block, and RegisterFileBasedRoutes
// — but NO gothic runtime middleware, and no gothicRoutes.Setup call.
const customMainGo = `package main

import (
	"log"
	"net/http"
	"os"

	"example.com/demo/src/routes"

	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/chi/v5"
)

func main() {
	router := chi.NewMux()
	router.Use(middleware.Logger)

	if os.Getenv("LOCAL_SERVE") == "true" {
		router.Handle("/public/*", http.StripPrefix("/public/", http.FileServer(http.Dir("./public/"))))
	}

	router.Group(routes.RegisterFileBasedRoutes)

	log.Fatal(http.ListenAndServe(os.Getenv("HTTP_LISTEN_ADDR"), router))
}
`

func TestRewriteMainGoV3InjectsMiddleware(t *testing.T) {
	p := writeMain(t, customMainGo)

	rewritten, runtimeLit, err := rewriteMainGoV3(p)
	if err != nil {
		t.Fatalf("rewriteMainGoV3: %v", err)
	}
	if !rewritten {
		t.Fatal("expected rewritten=true for a custom Gothic main without the middleware")
	}
	if runtimeLit != "" {
		t.Errorf("inject path should not produce a runtime literal, got %q", runtimeLit)
	}

	got, _ := os.ReadFile(p)
	s := string(got)
	if !strings.Contains(s, ".Use(gothicServer.Middleware(Config.Runtime))") {
		t.Errorf("middleware not injected:\n%s", s)
	}
	if !strings.Contains(s, `gothicframework/middlewares"`) {
		t.Errorf("middlewares import not added:\n%s", s)
	}
	// Additive: the user's custom block and route registration must survive.
	if !strings.Contains(s, "LOCAL_SERVE") || !strings.Contains(s, "RegisterFileBasedRoutes") {
		t.Errorf("custom code was not preserved:\n%s", s)
	}
	// The Gothic middleware must land AFTER the user's logger Use (scaffold order).
	if idxLogger, idxMw := strings.Index(s, "middleware.Logger"), strings.Index(s, "gothicServer.Middleware"); idxLogger < 0 || idxMw < idxLogger {
		t.Errorf("Gothic middleware not placed after the logger:\n%s", s)
	}
}

func TestRewriteMainGoV3InjectIdempotent(t *testing.T) {
	p := writeMain(t, customMainGo)
	if _, _, err := rewriteMainGoV3(p); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	rewritten, _, err := rewriteMainGoV3(p)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if rewritten {
		t.Error("second pass should be a no-op (middleware already mounted)")
	}
}

func TestRewriteMainGoV3InjectSkipsNonGothicMain(t *testing.T) {
	// A plain main with a chi router but no RegisterFileBasedRoutes is not a Gothic
	// project main — leave it alone.
	const plain = `package main

import "github.com/go-chi/chi/v5"

func main() {
	router := chi.NewMux()
	_ = router
}
`
	p := writeMain(t, plain)
	rewritten, _, err := rewriteMainGoV3(p)
	if err != nil {
		t.Fatalf("rewriteMainGoV3: %v", err)
	}
	if rewritten {
		t.Error("expected no rewrite for a non-Gothic main")
	}
}
