module example.com/pinnedapp

go 1.23

require (
	github.com/go-chi/chi/v5 v5.2.0
	github.com/gothicframework/components v1.3.0
	github.com/gothicframework/core v1.6.0
	github.com/gothicframework/middlewares v1.3.0
)

replace github.com/gothicframework/core => ../../../../core
