// Package mcp is a minimal Model Context Protocol server over stdio. It speaks
// newline-delimited JSON-RPC 2.0 and supports the tools capability, which is
// all the unwedge MCP bridge needs. Keeping it dependency-free avoids churn
// from evolving third-party SDKs.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
)

// ProtocolVersion is advertised if the client does not request one.
const ProtocolVersion = "2024-11-05"

// ToolHandler executes a tool call. args is the raw JSON of the "arguments"
// object. It returns text output, or an error which is reported to the client
// as an tool error (isError=true) rather than a protocol error.
type ToolHandler func(ctx context.Context, args json.RawMessage) (string, error)

// Tool is a registered tool.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]interface{}
	Handler     ToolHandler
}

// Server is a stdio MCP server.
type Server struct {
	name    string
	version string
	r       *bufio.Reader
	w       io.Writer
	wmu     sync.Mutex

	tools map[string]Tool
	order []string
}

// NewServer creates a server reading from in and writing to out.
func NewServer(name, version string, in io.Reader, out io.Writer) *Server {
	return &Server{
		name:    name,
		version: version,
		r:       bufio.NewReaderSize(in, 1<<20),
		w:       out,
		tools:   map[string]Tool{},
	}
}

// AddTool registers a tool.
func (s *Server) AddTool(t Tool) {
	if _, exists := s.tools[t.Name]; !exists {
		s.order = append(s.order, t.Name)
	}
	s.tools[t.Name] = t
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve runs the read/dispatch loop until stdin closes or ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := s.r.ReadBytes('\n')
		if len(line) > 0 {
			s.handleLine(ctx, line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Server) handleLine(ctx context.Context, line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		// Only respond if it looks like a request; otherwise ignore blank lines.
		if len(trimSpace(line)) > 0 {
			s.writeError(nil, -32700, "parse error")
		}
		return
	}
	// Notifications have no id; do not respond.
	isNotification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		s.reply(req.ID, s.initializeResult(req.Params))
	case "notifications/initialized", "initialized":
		// no response
	case "ping":
		s.reply(req.ID, map[string]interface{}{})
	case "tools/list":
		s.reply(req.ID, s.toolsListResult())
	case "tools/call":
		s.handleToolCall(ctx, req)
	default:
		if !isNotification {
			s.writeError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (s *Server) initializeResult(params json.RawMessage) map[string]interface{} {
	protocol := ProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 && json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		protocol = p.ProtocolVersion
	}
	return map[string]interface{}{
		"protocolVersion": protocol,
		"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
		"serverInfo":      map[string]interface{}{"name": s.name, "version": s.version},
	}
}

type toolSpec struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

func (s *Server) toolsListResult() map[string]interface{} {
	list := make([]toolSpec, 0, len(s.order))
	for _, name := range s.order {
		t := s.tools[name]
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]interface{}{"type": "object"}
		}
		list = append(list, toolSpec{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return map[string]interface{}{"tools": list}
}

func (s *Server) handleToolCall(ctx context.Context, req rpcRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	tool, ok := s.tools[params.Name]
	if !ok {
		s.writeError(req.ID, -32602, "unknown tool: "+params.Name)
		return
	}
	text, err := tool.Handler(ctx, params.Arguments)
	if err != nil {
		s.reply(req.ID, toolResult("error: "+err.Error(), true))
		return
	}
	s.reply(req.ID, toolResult(text, false))
}

func toolResult(text string, isError bool) map[string]interface{} {
	return map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": text}},
		"isError": isError,
	}
}

func (s *Server) reply(id json.RawMessage, result interface{}) {
	s.writeMessage(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) writeError(id json.RawMessage, code int, msg string) {
	s.writeMessage(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) writeMessage(resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.w.Write(b)
	s.w.Write([]byte("\n"))
}

func trimSpace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	j := len(b)
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\r' || b[j-1] == '\n') {
		j--
	}
	return b[i:j]
}
