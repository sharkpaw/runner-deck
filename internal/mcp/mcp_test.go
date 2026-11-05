package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type call struct {
	method, path, body string
}

func newServer(status int, out string, err error) (*Server, *call) {
	got := &call{}
	return &Server{Name: "runner-deck", Version: "test", Call: func(_ context.Context, method, path string, body []byte) (int, []byte, error) {
		*got = call{method, path, string(body)}
		return status, []byte(out), err
	}}, got
}

func rpc(t *testing.T, s *Server, body string) map[string]any {
	t.Helper()
	resp := s.Handle(context.Background(), []byte(body))
	if resp == nil {
		t.Fatal("no response")
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func errorCode(m map[string]any) float64 {
	e, _ := m["error"].(map[string]any)
	code, _ := e["code"].(float64)
	return code
}

func TestInitialize_NegotiatesTheVersion(t *testing.T) {
	s, _ := newServer(200, "", nil)
	got := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if v := got["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Errorf("version = %v", v)
	}
	got = rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if v := got["result"].(map[string]any)["protocolVersion"]; v != protocolVersions[0] {
		t.Errorf("version = %v", v)
	}
}

func TestHandle_ProtocolErrors(t *testing.T) {
	s, _ := newServer(200, "", nil)
	for body, code := range map[string]float64{
		`{`:                        codeParse,
		`{"id":1,"method":"ping"}`: codeInvalidRequest,
		`{"jsonrpc":"2.0","id":1}`: codeInvalidRequest,
		`{"jsonrpc":"2.0","id":1,"method":"nope"}`:                                      codeMethodNotFound,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`:       codeInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`:                   codeInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_runner"}}`: codeInvalidParams,
	} {
		if got := errorCode(rpc(t, s, body)); got != code {
			t.Errorf("%s: code %v", body, got)
		}
	}
	if s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)) != nil {
		t.Error("a notification gets no reply")
	}
	if rpc(t, s, `{"jsonrpc":"2.0","id":"a","method":"ping"}`)["id"] != "a" {
		t.Error("id not echoed")
	}
}

func TestToolsList_MarksDestructiveTools(t *testing.T) {
	s, _ := newServer(200, "", nil)
	list := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)["result"].(map[string]any)["tools"].([]any)
	if len(list) != len(tools) {
		t.Fatalf("got %d", len(list))
	}
	seen := map[string]map[string]any{}
	for _, item := range list {
		m := item.(map[string]any)
		seen[m["name"].(string)] = m
	}
	if seen["stop_runner"]["annotations"].(map[string]any)["destructiveHint"] != true {
		t.Error("stop_runner must be destructive")
	}
	if seen["list_runners"]["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Error("list_runners must be read only")
	}
	labels := seen["add_runner"]["inputSchema"].(map[string]any)["properties"].(map[string]any)["labels"].(map[string]any)
	if labels["items"] == nil {
		t.Error("an array schema needs items")
	}
}

func TestToolCall_BuildsTheAPIRequest(t *testing.T) {
	cases := []struct {
		body string
		want call
	}{
		{`{"name":"list_runners"}`, call{http.MethodGet, "/api/runners", ""}},
		{`{"name":"get_runner","arguments":{"name":"a/../b"}}`, call{http.MethodGet, "/api/runners/a%2F..%2Fb", ""}},
		{`{"name":"runner_logs","arguments":{"name":"a","tail":50}}`, call{http.MethodGet, "/api/runners/a/logs?tail=50", ""}},
		{`{"name":"list_events","arguments":{"runner":"a b","limit":5}}`, call{http.MethodGet, "/api/events?limit=5&runner=a+b", ""}},
		{`{"name":"stop_runner","arguments":{"name":"a"}}`, call{http.MethodPost, "/api/runners/a/stop", ""}},
		{`{"name":"remove_runner","arguments":{"name":"a"}}`, call{http.MethodDelete, "/api/runners/a", ""}},
	}
	for _, c := range cases {
		s, got := newServer(200, `{"ok":true}`, nil)
		res := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+c.body+`}`)["result"].(map[string]any)
		if *got != c.want {
			t.Errorf("%s: %+v", c.body, *got)
		}
		if res["isError"] != false {
			t.Errorf("%s: %v", c.body, res)
		}
	}
	s, got := newServer(200, `{}`, nil)
	rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_runner","arguments":{"name":"a","target_type":"repo","target":"o/r","registration_token":"t","labels":["x"]}}}`)
	if got.path != "/api/runners" || !strings.Contains(got.body, `"labels":["x"]`) || !strings.Contains(got.body, `"name":"a"`) {
		t.Errorf("%+v", *got)
	}
}

func TestToolCall_ReportsFailuresToTheModel(t *testing.T) {
	for _, s := range []*Server{first(newServer(404, `{"message":"not found"}`, nil)), first(newServer(0, "", errors.New("boom")))} {
		res := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_runners"}}`)["result"].(map[string]any)
		if res["isError"] != true {
			t.Errorf("%v", res)
		}
	}
	s, _ := newServer(200, "plain text", nil)
	res := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_runners"}}`)["result"].(map[string]any)
	if res["content"].([]any)[0].(map[string]any)["text"] != "plain text" {
		t.Errorf("%v", res)
	}
}

func first(s *Server, _ *call) *Server { return s }

func TestArgString(t *testing.T) {
	if argString(true) != "true" || argString(1.5) != "1.5" || argString("x") != "x" {
		t.Error("argString")
	}
}
