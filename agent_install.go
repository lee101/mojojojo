package main

import (
	"os"
	"path/filepath"

	"github.com/valyala/fasthttp"
)

const agentReleaseDownloadBase = "https://github.com/lee101/mojojojo-agent/releases/latest/download"

// The website owns the memorable installer URLs. Serve the small scripts from
// the adjacent source checkout so a missing release-side script can never turn
// the documented command into a 404. The scripts themselves still download
// immutable, checksummed release artifacts.
func handleAgentInstaller(ctx *fasthttp.RequestCtx, name string) {
	if name != "install.sh" && name != "install.ps1" {
		sendError(ctx, fasthttp.StatusNotFound, "installer not found")
		return
	}
	body, err := os.ReadFile(filepath.Join("..", "mojojojo-agent", "install", name))
	if err != nil {
		sendError(ctx, fasthttp.StatusServiceUnavailable, "installer is unavailable")
		return
	}
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Content-Disposition", `inline; filename="`+name+`"`)
	if name == "install.ps1" {
		ctx.SetContentType("text/plain; charset=utf-8")
	} else {
		ctx.SetContentType("text/x-shellscript; charset=utf-8")
	}
	ctx.SetBody(body)
}
