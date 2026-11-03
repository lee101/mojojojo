package main

// Agent surface for model businesses: every <slug>.<domain> serves an MCP
// endpoint, an OpenAPI document and llms.txt generated from the catalog's tool
// list, so the same metered API is reachable from an agent without a wrapper.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/valyala/fasthttp"
)

type bizTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Method      string         `json:"method"`
	Path        string         `json:"path"`
	InputSchema map[string]any `json:"inputSchema"`
}

const mcpProtocol = "2025-03-26"

// businessAgentDocs serves the discovery documents and MCP. It reports whether
// it handled the request.
func (s *Server) businessAgentDocs(ctx *fasthttp.RequestCtx, b business, path string) bool {
	if len(b.Tools) == 0 {
		return false
	}
	host := b.Slug + "." + s.cfg.BusinessDomain
	switch {
	case path == "/openapi.json" && string(ctx.Method()) == "GET":
		sendJSON(ctx, s.businessOpenAPI(b, host))
	case path == "/llms.txt" && string(ctx.Method()) == "GET":
		ctx.SetContentType("text/plain; charset=utf-8")
		ctx.SetBodyString(s.businessLLMS(b, host))
	case path == "/.well-known/mcp.json" && string(ctx.Method()) == "GET":
		sendJSON(ctx, map[string]any{"name": b.Slug, "title": b.Title, "description": b.Pitch,
			"transport": map[string]any{"type": "streamable-http", "url": "https://" + host + "/mcp"},
			"auth":      map[string]any{"type": "bearer", "keys": s.cfg.SiteBaseURL + "/keys"},
			"openapi":   "https://" + host + "/openapi.json", "llms": "https://" + host + "/llms.txt", "tools": len(b.Tools)})
	case path == "/mcp":
		s.businessMCP(ctx, b)
	default:
		return false
	}
	return true
}

func (s *Server) businessOpenAPI(b business, host string) map[string]any {
	paths := map[string]any{}
	for _, t := range b.Tools {
		item, _ := paths[t.Path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[t.Path] = item
		}
		op := map[string]any{"operationId": t.Name, "summary": t.Description,
			"security":  []any{map[string]any{"bearerAuth": []string{}}},
			"responses": map[string]any{"200": map[string]any{"description": "OK"}}}
		var params []any
		for _, p := range pathParams(t.Path) {
			params = append(params, map[string]any{"name": p, "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		if t.Method == "POST" || t.Method == "PUT" {
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": t.InputSchema}}}
		}
		item[strings.ToLower(t.Method)] = op
	}
	return map[string]any{"openapi": "3.1.0",
		"info":       map[string]any{"title": b.Title, "version": "1.0.0", "description": b.Pitch},
		"servers":    []any{map[string]any{"url": "https://" + host}},
		"paths":      paths,
		"components": map[string]any{"securitySchemes": map[string]any{"bearerAuth": map[string]any{"type": "http", "scheme": "bearer", "description": "mojojojo API key (mj_live_...)"}}}}
}

func (s *Server) businessLLMS(b business, host string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n> %s\n\n", b.Title, b.Pitch)
	fmt.Fprintf(&sb, "MCP: https://%s/mcp (JSON-RPC over POST). OpenAPI: https://%s/openapi.json.\n", host, host)
	fmt.Fprintf(&sb, "Auth: `Authorization: Bearer mj_live_...`; create a key at %s/keys. Usage is billed from mojojojo credits.\n\n## Pricing\n\n", s.cfg.SiteBaseURL)
	for _, m := range metersFor(b.Parts) {
		fmt.Fprintf(&sb, "- %s: $%s per %s (%s free per month)\n", m.Label, trimFloat(m.USD()), m.Unit, trimFloat(m.Free))
	}
	sb.WriteString("\n## Endpoints\n\n")
	for _, t := range b.Tools {
		fmt.Fprintf(&sb, "- `%s %s` (MCP tool `%s`): %s\n", t.Method, t.Path, t.Name, t.Description)
	}
	if len(b.Notes) > 0 {
		sb.WriteString("\n## Limits and measured results\n\n")
		for _, n := range b.Notes {
			sb.WriteString("- " + n + "\n")
		}
	}
	return sb.String()
}

func trimFloat(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", v), "0"), ".")
}

func pathParams(p string) []string {
	var out []string
	for {
		i := strings.IndexByte(p, '{')
		j := strings.IndexByte(p, '}')
		if i < 0 || j < i {
			return out
		}
		out = append(out, p[i+1:j])
		p = p[j+1:]
	}
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func rpcResult(id json.RawMessage, v any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": v}
}

func rpcError(id json.RawMessage, code int, msg string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
}

func toolText(isErr bool, text string) map[string]any {
	return map[string]any{"isError": isErr, "content": []any{map[string]any{"type": "text", "text": text}}}
}

func (s *Server) businessMCP(ctx *fasthttp.RequestCtx, b business) {
	ctx.Response.Header.Set("Cache-Control", "no-store")
	ctx.Response.Header.Set("Access-Control-Allow-Origin", "*")
	ctx.Response.Header.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id")
	switch string(ctx.Method()) {
	case "OPTIONS":
		ctx.SetStatusCode(204)
		return
	case "POST":
	default:
		sendError(ctx, 405, "use POST with JSON-RPC")
		return
	}
	var r rpcRequest
	if json.Unmarshal(ctx.PostBody(), &r) != nil {
		sendJSON(ctx, rpcError(nil, -32700, "parse error"))
		return
	}
	if len(r.ID) == 0 {
		ctx.SetStatusCode(202)
		return
	}
	switch r.Method {
	case "initialize":
		sendJSON(ctx, rpcResult(r.ID, map[string]any{"protocolVersion": mcpProtocol,
			"capabilities": map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":   map[string]any{"name": b.Slug, "title": b.Title, "version": "1.0.0"},
			"instructions": b.Pitch + " Authenticate with Authorization: Bearer <mojojojo API key> from " + s.cfg.SiteBaseURL + "/keys."}))
	case "ping":
		sendJSON(ctx, rpcResult(r.ID, map[string]any{}))
	case "tools/list":
		list := make([]map[string]any, 0, len(b.Tools))
		for _, t := range b.Tools {
			list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
		}
		sendJSON(ctx, rpcResult(r.ID, map[string]any{"tools": list}))
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if json.Unmarshal(r.Params, &p) != nil {
			sendJSON(ctx, rpcError(r.ID, -32602, "invalid params"))
			return
		}
		var tool *bizTool
		for i := range b.Tools {
			if b.Tools[i].Name == p.Name {
				tool = &b.Tools[i]
			}
		}
		if tool == nil {
			sendJSON(ctx, rpcError(r.ID, -32602, "unknown tool "+p.Name))
			return
		}
		sendJSON(ctx, rpcResult(r.ID, s.mcpCall(ctx, b, *tool, p.Arguments)))
	default:
		sendJSON(ctx, rpcError(r.ID, -32601, "method not found"))
	}
}

// mcpCall runs a tool through the same gate as a REST call: key, free
// allowance, upstream headers and reported-unit metering.
func (s *Server) mcpCall(ctx *fasthttp.RequestCtx, b business, t bizTool, args map[string]any) map[string]any {
	if !s.businessLive(b) {
		return toolText(true, b.Title+" is not live yet")
	}
	u := s.auth.userFromRequest(ctx)
	if u == nil {
		return toolText(true, "API key required: create one at "+s.cfg.SiteBaseURL+"/keys and send it as Authorization: Bearer")
	}
	if m := modelMeter(b.Parts); m != "" && s.meterBlocked(u, m) {
		return toolText(true, "free allowance used - add credits at "+s.cfg.TopUpURL)
	}
	path := t.Path
	body := map[string]any{}
	for k, v := range args {
		body[k] = v
	}
	for _, name := range pathParams(t.Path) {
		v, _ := args[name].(string)
		if v == "" {
			return toolText(true, "missing "+name)
		}
		path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(v))
		delete(body, name)
	}
	up := s.upstreamFor(b)
	base, err := url.Parse(up.base)
	if err != nil || up.base == "" {
		return toolText(true, "upstream unavailable")
	}
	req, resp := fasthttp.AcquireRequest(), fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.Header.SetMethod(t.Method)
	req.SetRequestURI(base.Scheme + "://" + base.Host + path)
	req.Header.SetHost(base.Host)
	req.Header.Set("X-Mojo-Business", b.Slug)
	req.Header.Set("X-Mojo-Tenant", u.ID)
	req.Header.Set("X-Mojo-Token", up.token)
	if t.Method == "POST" || t.Method == "PUT" {
		enc, _ := json.Marshal(body)
		req.Header.SetContentType("application/json")
		req.SetBody(enc)
	}
	if err := streamClient.Do(req, resp); err != nil {
		return toolText(true, "upstream unavailable")
	}
	ok := resp.StatusCode() < 400
	if ok {
		s.meterUnits(u.ID, b, string(resp.Header.Peek("X-Mojo-Units")))
	}
	out := resp.Body()
	if len(out) > 1<<20 {
		out = append(out[:1<<20:1<<20], []byte("\n[truncated]")...)
	}
	return toolText(!ok, string(out))
}
