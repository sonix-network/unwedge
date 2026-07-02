package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func runServer(t *testing.T, s *Server, requests []string) []map[string]interface{} {
	t.Helper()
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	var out bytes.Buffer
	s.r = bufio.NewReaderSize(in, 1<<20)
	s.w = &out
	if err := s.Serve(context.Background()); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var msgs []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad response line %q: %v", line, err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func newServerForTest() *Server {
	return NewServer("test", "1.0", strings.NewReader(""), &bytes.Buffer{})
}

func TestInitializeAndToolsList(t *testing.T) {
	s := newServerForTest()
	s.AddTool(Tool{
		Name:        "echo",
		Description: "echoes",
		InputSchema: map[string]interface{}{"type": "object"},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return "hi", nil
		},
	})
	msgs := runServer(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 responses (init + list), got %d: %+v", len(msgs), msgs)
	}
	initResult := msgs[0]["result"].(map[string]interface{})
	if initResult["protocolVersion"] != "2024-11-05" {
		t.Fatalf("protocolVersion = %v", initResult["protocolVersion"])
	}
	listResult := msgs[1]["result"].(map[string]interface{})
	tools := listResult["tools"].([]interface{})
	if len(tools) != 1 || tools[0].(map[string]interface{})["name"] != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestToolCallSuccessAndError(t *testing.T) {
	s := newServerForTest()
	s.AddTool(Tool{
		Name: "greet",
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Name string `json:"name"`
			}
			json.Unmarshal(args, &a)
			if a.Name == "" {
				return "", context.Canceled
			}
			return "hello " + a.Name, nil
		},
	})
	msgs := runServer(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"greet","arguments":{"name":"world"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"greet","arguments":{}}}`,
	})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(msgs))
	}
	// Success.
	r0 := msgs[0]["result"].(map[string]interface{})
	if r0["isError"].(bool) {
		t.Fatalf("expected success, got %+v", r0)
	}
	content := r0["content"].([]interface{})[0].(map[string]interface{})
	if content["text"] != "hello world" {
		t.Fatalf("text = %v", content["text"])
	}
	// Error path reported as isError.
	r1 := msgs[1]["result"].(map[string]interface{})
	if !r1["isError"].(bool) {
		t.Fatalf("expected isError, got %+v", r1)
	}
}

func TestUnknownMethod(t *testing.T) {
	s := newServerForTest()
	msgs := runServer(t, s, []string{
		`{"jsonrpc":"2.0","id":9,"method":"bogus/method"}`,
	})
	if len(msgs) != 1 || msgs[0]["error"] == nil {
		t.Fatalf("expected error response, got %+v", msgs)
	}
}
