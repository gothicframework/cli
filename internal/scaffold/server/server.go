{{.MainServerPackageName}}

import (
	"log"
	"log/slog"
	"net/http"
	"os"

	"{{.GoModName}}/src/routes"
	"github.com/gothicframework/middlewares"

	"github.com/go-chi/chi/v5"
	"github.com/joho/godotenv"
)

func {{.MainServerFunctionName}} {
	godotenv.Load()

	router := chi.NewMux()
	router.Use(middlewares.Logger())

	// Gothic's runtime as one chi middleware: caching, /public/* static serving, the
	// OptimizedImage endpoint and every built-in route feature — all driven by the
	// Runtime block in gothic.config.go. New features arrive here automatically; you
	// never edit main.go. Tune the behavior (cache backend, static serving) there.
	router.Use(middlewares.Middleware(Config.Runtime))

	// Your file-based pages.
	routes.RegisterFileBasedRoutes(router)

	port := os.Getenv("HTTP_LISTEN_ADDR")
	// Hot reload restarts the app on every rebuild and the CLI already printed the address.
	if os.Getenv("GOTHIC_MODE") != "dev" {
		slog.Info("application running", "port", port)
	}
	log.Fatal(http.ListenAndServe(port, router))
}
