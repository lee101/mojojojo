package main

import (
	_ "embed"
	"encoding/json"

	"github.com/valyala/fasthttp"
)

//go:embed apps.json
var mojoAppsJSON []byte

type mojoApp struct {
	Slug   string `json:"slug"`
	Title  string `json:"title"`
	Pitch  string `json:"pitch"`
	Repo   string `json:"repo"`
	Status string `json:"status"`
}

func loadMojoApps() []mojoApp {
	var apps []mojoApp
	_ = json.Unmarshal(mojoAppsJSON, &apps)
	return apps
}

func (s *Server) handleMojoApps(ctx *fasthttp.RequestCtx) {
	apps := loadMojoApps()
	published := 0
	for _, a := range apps {
		if a.Status == "published" {
			published++
		}
	}
	s.render(ctx, "mojoapps.html", map[string]any{
		"Meta": s.metaAt("Compiled Mojo apps — mojojojo",
			"Real compiled Mojo projects with open source, showcasing mojosub and the mojojojo SDK.",
			"/mojo-apps"),
		"User": s.auth.userFromSession(ctx), "Apps": apps, "Published": published,
	})
}
