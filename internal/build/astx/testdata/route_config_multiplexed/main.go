package main

import "example.com/helpers/helpers/routes"

var Page = routes.RouteConfig{
	ClientSideState: func() {
		x := 1
		_ = x
	},
	Multiplexed: true,
}

func main() {}
