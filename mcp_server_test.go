package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

func testMCPServer(t *testing.T) *mcpServer {
	t.Helper()
	s := newToolMCP(testToolIndex())
	if err := s.Register(mcpTool{
		Name:        "echo",
		Description: "Echo back the msg argument.",
		InputSchema: mcpObjectSchema(map[string]any{"msg": mcpStringSchema("Message to echo.")}, []string{"msg"}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			msg, _ := args["msg"].(string)
			if msg == "" {
				return nil, errors.New("msg is required")
			}
			return map[string]any{"echoed": msg}, nil
		},
	}); err != nil {
		t.Fatalf("register echo: %v", err)
	}
	return s
}

func mcpRoundtrip(t *testing.T, s *mcpServer, req string) map[string]any {
	t.Helper()
	raw := s.Handle(context.Background(), []byte(req))
	if raw == nil {
		t.Fatalf("no response for %s", req)
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("bad response %s: %v", raw, err)
	}
	return resp
}

func mcpToolPayload(t *testing.T, resp map[string]any) string {
	t.Helper()
	if resp["error"] != nil {
		t.Fatalf("error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("isError: %v", result)
	}
	content := result["content"].([]any)
	first := content[0].(map[string]any)
	if first["type"] != "text" {
		t.Fatalf("content = %v", content)
	}
	return first["text"].(string)
}

func TestMCPInitializeHandshake(t *testing.T) {
	s := testMCPServer(t)
	resp := mcpRoundtrip(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if resp["error"] != nil {
		t.Fatalf("initialize error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", result["protocolVersion"])
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "mojojojo-toolsearch" {
		t.Fatalf("serverInfo = %v", info)
	}
	caps := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("capabilities missing tools: %v", caps)
	}
	if got := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); got != nil {
		t.Fatalf("notification produced a response: %s", got)
	}
}

func TestMCPInitializeUnknownVersionFallsBack(t *testing.T) {
	s := testMCPServer(t)
	resp := mcpRoundtrip(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] != mcpProtocolVersion {
		t.Fatalf("protocolVersion = %v", result["protocolVersion"])
	}
}

func TestMCPToolsList(t *testing.T) {
	s := testMCPServer(t)
	resp := mcpRoundtrip(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	result := resp["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools = %v", tools)
	}
	names := map[string]bool{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		names[tool["name"].(string)] = true
		schema := tool["inputSchema"].(map[string]any)
		if schema["type"] != "object" {
			t.Fatalf("inputSchema = %v", schema)
		}
	}
	for _, want := range []string{"tool_search", "tool_describe", "echo"} {
		if !names[want] {
			t.Fatalf("missing tool %q in %v", want, names)
		}
	}
}

func TestMCPToolsCallSearch(t *testing.T) {
	s := testMCPServer(t)
	text := mcpToolPayload(t, mcpRoundtrip(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":"github","kind":"plugin","limit":5}}}`))
	var payload struct {
		Count   int             `json:"count"`
		Results []toolSearchHit `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("payload %q: %v", text, err)
	}
	if payload.Count == 0 || len(payload.Results) == 0 {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Results[0].ID != "plugin:github-app" {
		t.Fatalf("first hit = %+v", payload.Results[0])
	}
	if len(payload.Results) > 5 {
		t.Fatalf("limit ignored: %d", len(payload.Results))
	}
}

func TestMCPToolsCallDescribe(t *testing.T) {
	s := testMCPServer(t)
	text := mcpToolPayload(t, mcpRoundtrip(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"tool_describe","arguments":{"id":"mcp:postgres"}}}`))
	var entry toolEntry
	if err := json.Unmarshal([]byte(text), &entry); err != nil {
		t.Fatalf("payload %q: %v", text, err)
	}
	if entry.ID != "mcp:postgres" || entry.Install != "hermes mcp install postgres" {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestMCPToolsCallErrors(t *testing.T) {
	s := testMCPServer(t)
	resp := mcpRoundtrip(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected isError: %v", result)
	}

	resp = mcpRoundtrip(t, s, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"tool_search","arguments":{}}}`)
	result = resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("missing query should be a tool error: %v", result)
	}
	if !strings.Contains(result["content"].([]any)[0].(map[string]any)["text"].(string), "query is required") {
		t.Fatalf("error text = %v", result)
	}

	resp = mcpRoundtrip(t, s, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"tool_describe","arguments":{"id":"mcp:nope"}}}`)
	result = resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("unknown id should be a tool error: %v", result)
	}

	resp = mcpRoundtrip(t, s, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"nope"}}`)
	errObj := resp["error"].(map[string]any)
	if errObj["code"].(float64) != -32602 {
		t.Fatalf("error = %v", errObj)
	}

	resp = mcpRoundtrip(t, s, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":"git","kind":"bogus"}}}`)
	result = resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("bad kind should be a tool error: %v", result)
	}
}

func TestMCPUnknownMethod(t *testing.T) {
	s := testMCPServer(t)
	resp := mcpRoundtrip(t, s, `{"jsonrpc":"2.0","id":10,"method":"bogus/method"}`)
	errObj := resp["error"].(map[string]any)
	if errObj["code"].(float64) != -32601 {
		t.Fatalf("error = %v", errObj)
	}
}

func TestMCPPingAndParseError(t *testing.T) {
	s := testMCPServer(t)
	resp := mcpRoundtrip(t, s, `{"jsonrpc":"2.0","id":11,"method":"ping"}`)
	if resp["error"] != nil {
		t.Fatalf("ping error = %v", resp["error"])
	}
	resp = mcpRoundtrip(t, s, `{not json`)
	errObj := resp["error"].(map[string]any)
	if errObj["code"].(float64) != -32700 {
		t.Fatalf("parse error = %v", errObj)
	}
}

func TestMCPRegisterRejectsBadNames(t *testing.T) {
	s := newMCPServer("x", "1")
	handler := func(ctx context.Context, args map[string]any) (any, error) { return nil, nil }
	if err := s.Register(mcpTool{Name: "bad name", Handler: handler}); err == nil {
		t.Fatal("expected error for invalid name")
	}
	if err := s.Register(mcpTool{Name: "ok_tool-1", Handler: handler}); err != nil {
		t.Fatalf("valid name rejected: %v", err)
	}
	if err := s.Register(mcpTool{Name: "no_handler"}); err == nil {
		t.Fatal("expected error for nil handler")
	}
}

func TestMCPServeStdioLines(t *testing.T) {
	s := testMCPServer(t)
	in := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n")
	var out bytes.Buffer
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %q", len(lines), out.String())
	}
	for _, line := range lines {
		var resp map[string]any
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		if resp["jsonrpc"] != "2.0" {
			t.Fatalf("resp = %v", resp)
		}
	}
}

func TestMCPHTTPRequiresAuth(t *testing.T) {
	s := &Server{}
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetRequestURI("/mcp")
	ctx.Request.SetBodyString(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	s.handleMCP(&ctx)
	if ctx.Response.StatusCode() != fasthttp.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", ctx.Response.StatusCode())
	}

	var searchCtx fasthttp.RequestCtx
	searchCtx.Request.SetRequestURI("/api/tools/search?q=github")
	s.handleToolSearch(&searchCtx)
	if searchCtx.Response.StatusCode() != fasthttp.StatusUnauthorized {
		t.Fatalf("search status = %d, want 401", searchCtx.Response.StatusCode())
	}
}
