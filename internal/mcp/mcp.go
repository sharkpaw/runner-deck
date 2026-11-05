// Package mcp serves the Manager's API as Model Context Protocol tools over Streamable HTTP, in the
// plain request and response form: POST carries JSON-RPC and gets JSON back, with no server stream.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Newest first; the first is offered to a client asking for one this server does not know.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Caller performs an API request inside the Manager and returns its status and body.
type Caller func(ctx context.Context, method, path string, body []byte) (int, []byte, error)

type param struct {
	Name, Type, Description string
	Required                bool
}

type tool struct {
	Name, Description string
	ReadOnly          bool
	Destructive       bool
	Params            []param
	Method            string
	// Path may hold {name}, filled from the argument of that name. Other GET arguments become the query.
	Path string
}

var runnerName = param{Name: "name", Type: "string", Description: "Runner name", Required: true}

var tools = []tool{
	{Name: "list_runners", Description: "List all runners with their target and status.", ReadOnly: true, Method: http.MethodGet, Path: "/api/runners"},
	{Name: "get_runner", Description: "Get one runner's configuration and status.", ReadOnly: true, Params: []param{runnerName}, Method: http.MethodGet, Path: "/api/runners/{name}"},
	{Name: "runner_logs", Description: "Read the end of a runner's output.", ReadOnly: true, Params: []param{runnerName, {Name: "tail", Type: "integer", Description: "Number of lines"}}, Method: http.MethodGet, Path: "/api/runners/{name}/logs"},
	{Name: "list_events", Description: "Read the activity history: starts, stops, job results, memory kills and errors.", ReadOnly: true, Params: []param{{Name: "runner", Type: "string", Description: "Only this runner"}, {Name: "level", Type: "string", Description: "info, warn or error"}, {Name: "limit", Type: "integer", Description: "Maximum events"}}, Method: http.MethodGet, Path: "/api/events"},
	{Name: "job_stats", Description: "Job totals, success rate and average duration, overall and per runner.", ReadOnly: true, Params: []param{{Name: "days", Type: "integer", Description: "Days to cover"}}, Method: http.MethodGet, Path: "/api/stats"},
	{Name: "system_resources", Description: "CPU, memory and disk use of the machine and of each runner.", ReadOnly: true, Method: http.MethodGet, Path: "/api/system"},
	{Name: "add_runner", Description: "Add and register a runner. Needs a fresh registration token from GitHub.", Params: []param{runnerName, {Name: "target_type", Type: "string", Description: "repo or org", Required: true}, {Name: "target", Type: "string", Description: "owner/repo or organization", Required: true}, {Name: "registration_token", Type: "string", Description: "GitHub registration token", Required: true}, {Name: "labels", Type: "array", Description: "Runner labels"}}, Method: http.MethodPost, Path: "/api/runners"},
	{Name: "start_runner", Description: "Start a stopped runner.", Params: []param{runnerName}, Method: http.MethodPost, Path: "/api/runners/{name}/start"},
	{Name: "stop_runner", Description: "Stop a runner. A running job is canceled.", Destructive: true, Params: []param{runnerName}, Method: http.MethodPost, Path: "/api/runners/{name}/stop"},
	{Name: "recreate_runner", Description: "Rebuild a runner's container from the current settings. A running job is canceled.", Destructive: true, Params: []param{runnerName}, Method: http.MethodPost, Path: "/api/runners/{name}/recreate"},
	{Name: "clean_runner_work", Description: "Empty a runner's work directory. Refused while a job runs.", Destructive: true, Params: []param{runnerName}, Method: http.MethodPost, Path: "/api/runners/{name}/clean"},
	{Name: "remove_runner", Description: "Remove a runner and its files.", Destructive: true, Params: []param{runnerName}, Method: http.MethodDelete, Path: "/api/runners/{name}"},
}

// Server answers MCP requests.
type Server struct {
	Name, Version string
	Call          Caller
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func failure(id json.RawMessage, code int, message string) *response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

// Handle answers one HTTP body. It returns nil when no reply is due, as for a notification. Without
// canWrite, only read-only tools are listed or run.
func (s *Server) Handle(ctx context.Context, body []byte, canWrite bool) *response {
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return failure(nil, codeParse, "invalid JSON")
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return failure(req.ID, codeInvalidRequest, "not a JSON-RPC 2.0 request")
	}
	if len(req.ID) == 0 {
		return nil
	}
	result, rpcErr := s.dispatch(ctx, req, canWrite)
	if rpcErr != nil {
		return failure(req.ID, rpcErr.Code, rpcErr.Message)
	}
	return &response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func (s *Server) dispatch(ctx context.Context, req request, canWrite bool) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := protocolVersions[0]
		for _, v := range protocolVersions {
			if v == params.ProtocolVersion {
				version = v
			}
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
			"instructions":    "Manages self-hosted GitHub Actions runners. Check list_runners and list_events before changing anything; stopping a runner cancels its job.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		list := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			if canWrite || t.ReadOnly {
				list = append(list, t.describe())
			}
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		return s.callTool(ctx, req.Params, canWrite)
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown method: " + req.Method}
}

func (t tool) describe() map[string]any {
	properties := map[string]any{}
	required := []string{}
	for _, p := range t.Params {
		schema := map[string]any{"type": p.Type, "description": p.Description}
		if p.Type == "array" {
			schema["items"] = map[string]any{"type": "string"}
		}
		properties[p.Name] = schema
		if p.Required {
			required = append(required, p.Name)
		}
	}
	return map[string]any{
		"name":        t.Name,
		"description": t.Description,
		"inputSchema": map[string]any{"type": "object", "properties": properties, "required": required},
		"annotations": map[string]any{"readOnlyHint": t.ReadOnly, "destructiveHint": t.Destructive},
	}
}

func textResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

func argString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func (t tool) request(args map[string]any) (path string, body []byte, err error) {
	path = t.Path
	rest := map[string]any{}
	for _, p := range t.Params {
		v, ok := args[p.Name]
		if !ok || v == nil {
			if p.Required {
				return "", nil, fmt.Errorf("missing argument: %s", p.Name)
			}
			continue
		}
		if placeholder := "{" + p.Name + "}"; strings.Contains(path, placeholder) {
			path = strings.ReplaceAll(path, placeholder, url.PathEscape(argString(v)))
			continue
		}
		rest[p.Name] = v
	}
	if len(rest) == 0 {
		return path, nil, nil
	}
	if t.Method != http.MethodGet {
		body, err = json.Marshal(rest)
		return path, body, err
	}
	query := url.Values{}
	for k, v := range rest {
		query.Set(k, argString(v))
	}
	return path + "?" + query.Encode(), nil, nil
}

func (s *Server) callTool(ctx context.Context, raw json.RawMessage, canWrite bool) (any, *rpcError) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params"}
	}
	for _, t := range tools {
		if t.Name != params.Name {
			continue
		}
		if !canWrite && !t.ReadOnly {
			return textResult("This connection has read-only access, so "+t.Name+" is not allowed. Sign in again with full access to use it.", true), nil
		}
		path, body, err := t.request(params.Arguments)
		if err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		status, out, err := s.Call(ctx, t.Method, path, body)
		if err != nil {
			return textResult(err.Error(), true), nil
		}
		var pretty bytes.Buffer
		text := string(out)
		if json.Indent(&pretty, out, "", "  ") == nil {
			text = pretty.String()
		}
		return textResult(text, status >= http.StatusBadRequest), nil
	}
	return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + params.Name}
}
