package main

import (
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

func TestAgentInstallerServesChecksummedReleaseScripts(t *testing.T) {
	for _, name := range []string{"install.sh", "install.ps1"} {
		ctx := &fasthttp.RequestCtx{}
		handleAgentInstaller(ctx, name)
		if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
			t.Errorf("%s status = %d", name, got)
		}
		body := string(ctx.Response.Body())
		for _, required := range []string{"SHA256SUMS", "MJJ_INSTALL_DIR", "checksum"} {
			if !strings.Contains(body, required) {
				t.Errorf("%s missing %q", name, required)
			}
		}
		if got := string(ctx.Response.Header.Peek("Cache-Control")); got != "no-cache" {
			t.Errorf("%s cache control = %q", name, got)
		}
	}
}

func TestAgentInstallerRejectsUnknownNames(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	handleAgentInstaller(ctx, "../../secret")
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusNotFound {
		t.Fatalf("status = %d", got)
	}
}
