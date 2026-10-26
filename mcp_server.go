package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

const mcpMaxLineBytes = 16 * 1024 * 1024

// mcpServer is transport agnostic. Handle takes one JSON-RPC message and
// returns one response, which is what the HTTP route needs; Serve speaks the
// line-delimited stdio framing MCP clients use when they spawn a process.
type mcpServer struct {
	name    string
	version string

	mu    sync.RWMutex
	tools map[string]mcpTool
	order []string

	writeMu sync.Mutex
}

func newMCPServer(name, version string) *mcpServer {
	if name == "" {
		name = "mojojojo"
	}
	if version == "" {
		version = "dev"
	}
	return &mcpServer{name: name, version: version, tools: map[string]mcpTool{}}
}

func (s *mcpServer) Register(t mcpTool) error {
	if !mcpToolNamePattern.MatchString(t.Name) {
		return fmt.Errorf("mcp: invalid tool name %q", t.Name)
	}
	if t.Handler == nil {
		return fmt.Errorf("mcp: tool %q has no handler", t.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tools[t.Name]; !exists {
		s.order = append(s.order, t.Name)
	}
	s.tools[t.Name] = t
	return nil
}

func (s *mcpServer) RegisterAll(tools []mcpTool) error {
	for _, t := range tools {
		if err := s.Register(t); err != nil {
			return err
		}
	}
	return nil
}

func (s *mcpServer) ToolInfos() []mcpToolInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]mcpToolInfo, 0, len(s.order))
	for _, name := range s.order {
		out = append(out, s.tools[name].info())
	}
	return out
}

func (s *mcpServer) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), mcpMaxLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if resp := s.Handle(ctx, line); resp != nil {
			if err := s.writeLine(w, resp); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return scanner.Err()
}

func (s *mcpServer) writeLine(w io.Writer, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := w.Write(payload); err != nil {
		return err
	}
	_, err := w.Write([]byte("\n"))
	return err
}

// Handle processes one message. A notification (no id) returns nil, and a
// malformed message returns a JSON-RPC parse error, never a panic.
func (s *mcpServer) Handle(ctx context.Context, raw []byte) []byte {
	var msg mcpMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return mcpMarshalResponse(mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &mcpRPCError{Code: -32700, Message: "parse error", Data: err.Error()}})
	}
	if msg.Method == "" {
		return nil
	}
	if !msg.hasID() {
		return nil
	}
	result, rpcErr := s.dispatch(ctx, &msg)
	resp := mcpResponse{JSONRPC: "2.0", ID: msg.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}
	return mcpMarshalResponse(resp)
}

func (s *mcpServer) dispatch(ctx context.Context, msg *mcpMessage) (any, *mcpRPCError) {
	switch msg.Method {
	case "initialize":
		return s.initializeResult(msg.Params), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.ToolInfos()}, nil
	case "tools/call":
		return s.callTool(ctx, msg.Params)
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	default:
		return nil, &mcpRPCError{Code: -32601, Message: "method not found", Data: msg.Method}
	}
}

func (s *mcpServer) initializeResult(params json.RawMessage) map[string]any {
	version := mcpProtocolVersion
	var req struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		if json.Unmarshal(params, &req) == nil && req.ProtocolVersion != "" {
			version = req.ProtocolVersion
		}
	}
	if !mcpSupportedProtocolVersion(version) {
		version = mcpProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": s.name, "version": s.version},
	}
}

func mcpSupportedProtocolVersion(v string) bool {
	switch v {
	case "2024-11-05", "2025-03-26", "2025-06-18":
		return true
	}
	return false
}

func (s *mcpServer) callTool(ctx context.Context, params json.RawMessage) (any, *mcpRPCError) {
	var req struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, &mcpRPCError{Code: -32602, Message: "invalid params", Data: err.Error()}
	}
	s.mu.RLock()
	tool, ok := s.tools[req.Name]
	s.mu.RUnlock()
	if !ok {
		return nil, &mcpRPCError{Code: -32602, Message: "unknown tool", Data: req.Name}
	}
	if req.Arguments == nil {
		req.Arguments = map[string]any{}
	}
	result, err := tool.Handler(ctx, req.Arguments)
	if err != nil {
		return map[string]any{"content": []mcpContent{mcpTextContent(err.Error())}, "isError": true}, nil
	}
	return map[string]any{"content": mcpToContents(result), "isError": false}, nil
}

func mcpToContents(result any) []mcpContent {
	switch v := result.(type) {
	case nil:
		return []mcpContent{mcpTextContent("ok")}
	case []mcpContent:
		return v
	case mcpContent:
		return []mcpContent{v}
	case string:
		return []mcpContent{mcpTextContent(v)}
	case json.RawMessage:
		return []mcpContent{mcpTextContent(string(v))}
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return []mcpContent{mcpTextContent(fmt.Sprintf("%v", v))}
		}
		return []mcpContent{mcpTextContent(string(data))}
	}
}

func mcpMarshalResponse(resp mcpResponse) []byte {
	data, err := json.Marshal(resp)
	if err != nil {
		fallback := mcpResponse{JSONRPC: "2.0", ID: resp.ID, Error: &mcpRPCError{Code: -32603, Message: "internal error", Data: err.Error()}}
		data, _ = json.Marshal(fallback)
	}
	return data
}
