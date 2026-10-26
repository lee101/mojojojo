package main

// A small, generic MCP server: JSON-RPC 2.0 over stdio or one HTTP POST.
// Ported from app-site's internal/mcp so both products speak the same
// protocol. Symbols carry an mcp prefix because this repository is one flat
// package; the wire format is unchanged.
//
// Nothing here knows what a tool is. The catalog server (toolsearch.go)
// registers tool_search and tool_describe on top of it.

import (
	"context"
	"encoding/json"
	"regexp"
)

const mcpProtocolVersion = "2025-06-18"

type mcpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *mcpRPCError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

func (m *mcpMessage) hasID() bool {
	return len(m.ID) > 0 && string(m.ID) != "null"
}

type mcpContent map[string]any

func mcpTextContent(text string) mcpContent {
	return mcpContent{"type": "text", "text": text}
}

type mcpTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, args map[string]any) (any, error)
}

type mcpToolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

var mcpToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func (t mcpTool) info() mcpToolInfo {
	schema := t.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	return mcpToolInfo{Name: t.Name, Description: t.Description, InputSchema: schema}
}

func mcpObjectSchema(properties map[string]any, required []string) map[string]any {
	out := map[string]any{"type": "object"}
	if len(properties) > 0 {
		out["properties"] = properties
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func mcpStringSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func mcpNumberSchema(description string) map[string]any {
	return map[string]any{"type": "number", "description": description}
}
