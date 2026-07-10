{{.MainServerPackageName}}

import (
	"log"
	"log/slog"
	"net/http"
	"os"

	"{{.GoModName}}/src/routes"
	"github.com/gothicframework/middlewares"

	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/joho/godotenv"
)

func {{.MainServerFunctionName}} {
	godotenv.Load()

	router := chi.NewMux()
	router.Use(middleware.Logger)

	// Gothic's runtime as one chi middleware: caching, /public/* static serving, the
	// OptimizedImage endpoint and every built-in route feature — all driven by the
	// Runtime block in gothic.config.go. New features arrive here automatically; you
	// never edit main.go. Tune the behavior (cache backend, static serving) there.
	router.Use(middlewares.Middleware(Config.Runtime))

	// Your file-based pages.
	routes.RegisterFileBasedRoutes(router)

	port := os.Getenv("HTTP_LISTEN_ADDR")
	slog.Info("application running", "port", port)
	log.Fatal(http.ListenAndServe(port, router))
}
