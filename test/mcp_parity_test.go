package test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/mcp"
	coretools "LuminaCode/tools"
)

func TestMCPProtocolParseSerializeAndContent(t *testing.T) {
	req := mcp.MakeRequest("tools/list", map[string]any{"cursor": "å<tag>"}, 7)
	serialized := mcp.SerializeMessage(req)
	wantSerialized := `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{"cursor":"å<tag>"}}`
	if serialized != wantSerialized {
		t.Fatalf("serialized request=%s want %s", serialized, wantSerialized)
	}
	parsed := mcp.ParseMessage(serialized)
	if parsedReq, ok := parsed.(mcp.JSONRPCRequest); !ok || parsedReq.ID != 7 || parsedReq.Method != "tools/list" {
		t.Fatalf("unexpected parsed request: %#v", parsed)
	}
	errMsg := mcp.ParseMessage(`{"jsonrpc":"2.0","id":3,"error":{"code":-32602,"message":"bad","data":{"x":1}}}`)
	if parsedErr, ok := errMsg.(mcp.JSONRPCError); !ok || parsedErr.Error["message"] != "bad" {
		t.Fatalf("unexpected parsed error: %#v", errMsg)
	}
	text := mcp.ExtractContent([]map[string]any{
		{"type": "text", "text": "hello"},
		{"type": "image", "mimeType": "image/jpeg", "data": "abcd"},
		{"type": "resource", "resource": map[string]any{"uri": "file://a"}},
	})
	if text != "hello\n[Image: image/jpeg, 4 bytes base64]\n[Resource: file://a]" {
		t.Fatalf("unexpected content extraction: %q", text)
	}
}

func TestMCPProtocolParsesNumericStringsLikePython(t *testing.T) {
	parsed := mcp.ParseMessage(`{"jsonrpc":"2.0","id":"7","method":123,"params":{"ok":true}}`)
	req, ok := parsed.(mcp.JSONRPCRequest)
	if !ok || req.ID != 7 || req.Method != "123" {
		t.Fatalf("Python int/str coercion mismatch for request: %#v", parsed)
	}

	parsedErr := mcp.ParseMessage(`{"jsonrpc":"2.0","id":"8","error":{"code":"-32602","message":404}}`)
	errResp, ok := parsedErr.(mcp.JSONRPCError)
	if !ok || errResp.ID != 8 || errResp.Error["code"] != -32602 || errResp.Error["message"] != "404" {
		t.Fatalf("Python int/str coercion mismatch for error: %#v", parsedErr)
	}
}

func TestMCPDynamicToolCoercesStringScalarsBeforeValidation(t *testing.T) {
	tool := coretools.NewMCPDynamicTool(
		"mcp__arxiv__search_arxiv",
		"arxiv",
		"search_arxiv",
		"search arxiv",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":           map[string]any{"type": "string"},
				"limit":           map[string]any{"type": "integer"},
				"include_content": map[string]any{"type": "boolean"},
			},
			"required": []any{"query"},
		},
		"",
	)
	decoded, err := tool.DecodeInput(map[string]any{
		"query":           "medical multimodal model",
		"limit":           "3",
		"include_content": "False",
	})
	if err != nil {
		t.Fatalf("DecodeInput should coerce string scalar values before validation: %v", err)
	}
	got, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("unexpected decoded type: %#v", decoded)
	}
	if got["limit"] != int64(3) {
		t.Fatalf("expected string integer to decode as int64(3), got %#v", got["limit"])
	}
	if got["include_content"] != false {
		t.Fatalf("expected string bool to decode as false, got %#v", got["include_content"])
	}
}

func TestMCPConfigMergesLocalOverProjectAndTrust(t *testing.T) {
	dir := t.TempDir()
	project := `{"mcpServers":{"demo":{"command":"node","args":["server.js"],"env":{"A":"1"}},"http":{"url":"https://example.com/mcp"}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(project), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".Lumina", "CONFIG"), 0o755); err != nil {
		t.Fatal(err)
	}
	local := `{"mcpServers":{"demo":{"command":"python","args":["server.py"],"cwd":"."}}}`
	if err := os.WriteFile(filepath.Join(dir, ".Lumina", "CONFIG", "mcp.json"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	configs := mcp.LoadMCPConfig(dir)
	if len(configs) != 2 {
		t.Fatalf("expected two configs, got %#v", configs)
	}
	if configs[0].Name != "demo" || configs[0].Command == nil || *configs[0].Command != "python" {
		t.Fatalf("expected local override for demo, got %#v", configs[0])
	}
	trusted := map[string]string{"demo": configs[0].Fingerprint()}
	if err := mcp.SaveTrustedMCP(dir, trusted); err != nil {
		t.Fatal(err)
	}
	loaded := mcp.LoadTrustedMCP(dir)
	if loaded["demo"] != trusted["demo"] {
		t.Fatalf("unexpected trusted payload: %#v", loaded)
	}
}

func TestMCPConfigLoadsLuminaConfigDirectory(t *testing.T) {
	dir := t.TempDir()
	luminaConfig := filepath.Join(dir, ".Lumina", "CONFIG")
	if err := os.MkdirAll(luminaConfig, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"mcpServers":{"lumina":{"url":"https://example.com/lumina-mcp"}}}`
	if err := os.WriteFile(filepath.Join(luminaConfig, "mcp.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	configs := mcp.LoadMCPConfig(dir)
	if len(configs) != 1 || configs[0].Name != "lumina" || configs[0].URL == nil || *configs[0].URL != "https://example.com/lumina-mcp" {
		t.Fatalf("expected Lumina MCP config, got %#v", configs)
	}
}

func TestMCPConfigOrderOverrideAndFingerprintDefaultsMatchPython(t *testing.T) {
	dir := t.TempDir()
	project := `{"mcpServers":{"zeta":{"command":"node","args":["old.js"]},"alpha":{"url":"https://example.com/alpha"},"bad":"skip me"}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(project), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".Lumina", "CONFIG"), 0o755); err != nil {
		t.Fatal(err)
	}
	local := `{"mcpServers":{"zeta":{"command":"python","args":["new.py"]},"middle":{"url":"https://example.com/middle"}}}`
	if err := os.WriteFile(filepath.Join(dir, ".Lumina", "CONFIG", "mcp.json"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	configs := mcp.LoadMCPConfig(dir)
	if len(configs) != 3 {
		t.Fatalf("expected three valid configs, got %#v", configs)
	}
	names := []string{configs[0].Name, configs[1].Name, configs[2].Name}
	if strings.Join(names, ",") != "zeta,alpha,middle" {
		t.Fatalf("Python preserves merge insertion order, got %v", names)
	}
	if configs[0].Command == nil || *configs[0].Command != "python" {
		t.Fatalf("expected zeta local override without reordering, got %#v", configs[0])
	}
	if configs[1].Fingerprint() != "3b7ef5f5c5d04ed75b3f1a2548e3445ef6ef5776e390c139db6ce01974da1b01" {
		t.Fatalf("fingerprint defaults differ from Python: %s", configs[1].Fingerprint())
	}
}

func TestMCPFingerprintUsesPythonJSONDumpsEscaping(t *testing.T) {
	command := "node"
	cfg := mcp.McpServerConfig{
		Name:    "unicode",
		Command: &command,
		Args:    []string{"项目<tag>"},
		Env:     map[string]string{"A": "值<tag>"},
		Headers: map[string]string{},
	}
	want := "5ad4054bda95178f9dd918ce603e28c0a25d33068f4f8ae853e75b6c4209762b"
	if got := cfg.Fingerprint(); got != want {
		t.Fatalf("fingerprint should match Python json.dumps(sort_keys=True, separators=(',', ':')), got %s want %s", got, want)
	}
}

func TestMCPHTTPClientLifecycleDiscoveryCallAndResource(t *testing.T) {
	var initialized atomic.Bool
	var sawClientInfo atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		msg := mcp.ParseMessage(string(body))
		switch req := msg.(type) {
		case mcp.JSONRPCNotification:
			if req.Method == "notifications/initialized" {
				initialized.Store(true)
			}
			w.WriteHeader(http.StatusNoContent)
		case mcp.JSONRPCRequest:
			result := map[string]any{}
			switch req.Method {
			case "initialize":
				params := req.Params
				clientInfo, _ := params["clientInfo"].(map[string]any)
				if clientInfo["name"] == mcp.ClientName && clientInfo["version"] == mcp.ClientVersion {
					sawClientInfo.Store(true)
				}
				result = map[string]any{
					"serverInfo":   map[string]any{"name": "demo", "version": "1.2.3"},
					"capabilities": map[string]any{"tools": map[string]any{}},
				}
			case "tools/list":
				result = map[string]any{"tools": []map[string]any{{
					"name":        "echo",
					"description": "Echo text",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
				}}}
			case "tools/call":
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": "called:" + req.Params["name"].(string)}}}
			case "resources/list":
				result = map[string]any{"resources": []map[string]any{{"uri": "file://demo", "name": "Demo", "mimeType": "text/plain"}}}
			case "resources/read":
				result = map[string]any{"contents": []map[string]any{{"type": "text", "text": "resource body"}}}
			case "ping":
				result = map[string]any{}
			default:
				t.Fatalf("unexpected MCP method: %s", req.Method)
			}
			_, _ = w.Write([]byte(mcp.SerializeMessage(mcp.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result})))
		default:
			t.Fatalf("unexpected message: %#v", msg)
		}
	}))
	defer server.Close()

	url := server.URL
	client := mcp.NewMcpClient(mcp.McpServerConfig{Name: "http-demo", URL: &url})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !client.Connect(ctx) {
		t.Fatalf("connect failed: %s", client.LastError())
	}
	defer client.Disconnect(ctx)
	if client.State() != mcp.ConnectionConnected {
		t.Fatalf("expected connected state, got %s", client.State())
	}
	if !initialized.Load() || !sawClientInfo.Load() {
		t.Fatalf("initialize handshake did not mirror Python client behavior")
	}
	tools, err := client.DiscoverTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0]["name"] != "echo" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	output, err := client.CallTool(ctx, "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if output != "called:echo" {
		t.Fatalf("unexpected tool output: %q", output)
	}
	resources, err := client.DiscoverResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0]["uri"] != "file://demo" {
		t.Fatalf("unexpected resources: %#v", resources)
	}
	body, err := client.ReadResource(ctx, "file://demo")
	if err != nil {
		t.Fatal(err)
	}
	if body != "resource body" {
		t.Fatalf("unexpected resource body: %q", body)
	}
}

func TestMCPStdioTransportJSONL(t *testing.T) {
	command, args := shellCommandArgv(`read line; echo '{"jsonrpc":"2.0","id":42,"result":{"ok":true}}'`)
	transport := mcp.NewStdioTransport(command, args, nil, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := transport.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer transport.Close(ctx)
	if !transport.IsConnected() {
		t.Fatalf("expected transport to be connected")
	}
	if err := transport.Send(ctx, mcp.MakeRequest("ping", map[string]any{}, 42)); err != nil {
		t.Fatal(err)
	}
	msg, err := transport.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp, ok := msg.(mcp.JSONRPCResponse)
	if !ok || resp.ID != 42 {
		t.Fatalf("unexpected response: %#v", msg)
	}
}

func TestMCPStdioEnvCWDAndProcessExitMatchPython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	dir := t.TempDir()
	script := `read line; cwd=$(pwd); printf '{"jsonrpc":"2.0","id":1,"result":{"env_val":"%s","cwd":"%s"}}\n' "$MCP_TEST_VAR" "$cwd"`
	transport := mcp.NewStdioTransport("/bin/sh", []string{"-c", script}, map[string]string{"MCP_TEST_VAR": "hello-mcp"}, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := transport.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if !transport.IsConnected() {
		t.Fatal("expected stdio transport to be connected")
	}
	if err := transport.Send(ctx, mcp.MakeRequest("ping", nil, 1)); err != nil {
		t.Fatal(err)
	}
	msg, err := transport.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp, ok := msg.(mcp.JSONRPCResponse)
	if !ok {
		t.Fatalf("unexpected response: %#v", msg)
	}
	result, _ := resp.Result.(map[string]any)
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		wantDir = dir
	}
	if result["env_val"] != "hello-mcp" || result["cwd"] != wantDir {
		t.Fatalf("stdio env/cwd should match Python transport, got %#v", result)
	}
	msg, err = transport.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if msg != nil || transport.IsConnected() {
		t.Fatalf("process exit should return nil and mark disconnected like Python, msg=%#v connected=%v", msg, transport.IsConnected())
	}
	if err := transport.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMCPStdioStderrDoesNotAffectReceiveLikePython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	script := `printf 'diagnostic message\n' >&2; read line; printf '{"jsonrpc":"2.0","id":2,"result":"ok"}\n'`
	transport := mcp.NewStdioTransport("/bin/sh", []string{"-c", script}, nil, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := transport.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer transport.Close(ctx)
	if err := transport.Send(ctx, mcp.MakeRequest("ping", nil, 2)); err != nil {
		t.Fatal(err)
	}
	msg, err := transport.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp, ok := msg.(mcp.JSONRPCResponse)
	if !ok || resp.ID != 2 || resp.Result != "ok" {
		t.Fatalf("stderr should not affect JSON-RPC stdout receive, got %#v", msg)
	}
}

func TestMCPStdioCloseTerminatesLikePython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Python asyncio terminate maps differently on Windows")
	}
	dir := t.TempDir()
	signalFile := filepath.Join(dir, "signal.txt")
	readyFile := filepath.Join(dir, "ready.txt")
	script := `trap 'echo TERM > "$1"; exit 0' TERM; trap 'echo INT > "$1"; exit 0' INT; echo ready > "$2"; while true; do sleep 1; done`
	transport := mcp.NewStdioTransport("/bin/sh", []string{"-c", script, "sh", signalFile, readyFile}, nil, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := transport.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(readyFile); err != nil {
		t.Fatalf("child process did not install traps: %v", err)
	}
	if err := transport.Close(ctx); err != nil {
		t.Fatalf("Python close swallows normal terminate wait errors, got %v", err)
	}
	data, err := os.ReadFile(signalFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "TERM" {
		t.Fatalf("stdio close should use terminate/SIGTERM like Python, got %q", data)
	}
}

func TestMCPRequestTimeoutDisconnectsHungStdioServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	script := `
read init
printf '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{"listChanged":false}},"serverInfo":{"name":"hung","version":"0"}}}\n'
read initialized
read request
sleep 60
`
	command := "/bin/sh"
	args := []string{"-c", script}
	cfg := mcp.McpServerConfig{Name: "hung"}
	cfg.Command = &command
	cfg.Args = args
	client := mcp.NewMcpClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !client.Connect(ctx) {
		t.Fatalf("client should connect before hung request: %s", client.LastError())
	}
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer reqCancel()
	if _, err := client.Request(reqCtx, "tools/list", map[string]any{}); err == nil {
		t.Fatal("expected timeout from hung MCP request")
	}
	if client.State() != mcp.ConnectionDisconnected {
		t.Fatalf("hung MCP timeout should disconnect transport, state=%s lastError=%q", client.State(), client.LastError())
	}
}

func TestMCPHTTPTransportIgnoresNonResponseBodiesLikePython(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"x":1}}`))
	}))
	defer server.Close()

	transport := mcp.NewHTTPTransport(server.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := transport.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer transport.Close(ctx)
	if err := transport.Send(ctx, mcp.MakeRequest("ping", map[string]any{}, 9)); err != nil {
		t.Fatal(err)
	}
	msg, err := transport.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if msg != nil {
		t.Fatalf("Python HTTP transport only buffers response/error messages, got %#v", msg)
	}
}

func TestMCPHTTPConnectSendsPythonDefaultJSONHeaders(t *testing.T) {
	var sawOptions atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodOptions {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		sawOptions.Store(true)
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("OPTIONS Content-Type=%q want application/json", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("OPTIONS Accept=%q want application/json", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	transport := mcp.NewHTTPTransport(server.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := transport.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer transport.Close(ctx)
	if !sawOptions.Load() {
		t.Fatalf("expected Python-style non-fatal OPTIONS connectivity check")
	}
}

func TestMCPHTTPTransportCustomHeadersAndCloseMatchPython(t *testing.T) {
	var sawPost atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodOptions:
			if got := r.Header.Get("Authorization"); got != "Bearer xyz" {
				t.Fatalf("OPTIONS custom header=%q want Bearer xyz", got)
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			sawPost.Store(true)
			if got := r.Header.Get("Authorization"); got != "Bearer xyz" {
				t.Fatalf("POST custom header=%q want Bearer xyz", got)
			}
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("POST Content-Type=%q want application/json", got)
			}
			if got := r.Header.Get("Accept"); got != "application/json" {
				t.Fatalf("POST Accept=%q want application/json", got)
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"ok"}`))
		default:
			t.Fatalf("unexpected method: %s", r.Method)
		}
	}))
	defer server.Close()

	transport := mcp.NewHTTPTransport(server.URL, map[string]string{"Authorization": "Bearer xyz"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := transport.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if !transport.IsConnected() {
		t.Fatal("expected HTTP transport to be connected")
	}
	if err := transport.Send(ctx, mcp.MakeRequest("ping", nil, 1)); err != nil {
		t.Fatal(err)
	}
	msg, err := transport.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp, ok := msg.(mcp.JSONRPCResponse)
	if !ok || resp.ID != 1 || resp.Result != "ok" || !sawPost.Load() {
		t.Fatalf("unexpected HTTP response/header flow: msg=%#v sawPost=%v", msg, sawPost.Load())
	}
	if err := transport.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if transport.IsConnected() {
		t.Fatal("HTTP transport close should clear connected state like Python")
	}
}

func TestMCPRegisterToolsDeferredExecutionAndResources(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	var initialized atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		msg := mcp.ParseMessage(string(body))
		switch req := msg.(type) {
		case mcp.JSONRPCNotification:
			if req.Method == "notifications/initialized" {
				initialized.Store(true)
			}
			w.WriteHeader(http.StatusNoContent)
		case mcp.JSONRPCRequest:
			result := map[string]any{}
			switch req.Method {
			case "initialize":
				result = map[string]any{"serverInfo": map[string]any{"name": "demo"}, "capabilities": map[string]any{}}
			case "tools/list":
				result = map[string]any{"tools": []map[string]any{{
					"name":        "Echo.Tool",
					"description": "Echo text",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"text": map[string]any{"type": "string", "description": "Text to echo"}},
						"required":   []string{"text"},
					},
				}}}
			case "tools/call":
				args, _ := req.Params["arguments"].(map[string]any)
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": "echo:" + args["text"].(string)}}}
			case "resources/list":
				result = map[string]any{"resources": []map[string]any{{"uri": "mem://demo", "name": "Demo memory", "mimeType": "text/plain"}}}
			case "resources/read":
				result = map[string]any{"contents": []map[string]any{{"type": "text", "text": "resource:" + req.Params["uri"].(string)}}}
			default:
				t.Fatalf("unexpected method: %s", req.Method)
			}
			_, _ = w.Write([]byte(mcp.SerializeMessage(mcp.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result})))
		default:
			t.Fatalf("unexpected message: %#v", msg)
		}
	}))
	defer server.Close()

	config := `{"mcpServers":{"Demo Server":{"url":"` + server.URL + `"}}}`
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded := mcp.LoadProjectMCPConfig(root)
	if len(loaded) != 1 {
		t.Fatalf("expected one project MCP config, got %#v", loaded)
	}
	if err := mcp.SaveTrustedMCP(root, map[string]string{"Demo Server": loaded[0].Fingerprint()}); err != nil {
		t.Fatal(err)
	}

	registry := coretools.NewToolRegistry()
	execCtx := coretools.ExecutionContext{}
	if err := coretools.RegisterMCPTools(registry, root, execCtx); err != nil {
		t.Fatal(err)
	}
	if !initialized.Load() {
		t.Fatalf("expected initialized notification")
	}
	deferred := registry.GetDeferredTools()
	publicName := coretools.SanitizeMCPToolName("Demo Server", "Echo.Tool")
	if deferred[publicName] == nil || deferred["mcp_list_resources"] == nil || deferred["mcp_read_resource"] == nil {
		t.Fatalf("expected MCP tools in deferred index, got keys %#v", deferred)
	}
	dynamic := registry.ActivateTool(publicName)
	if dynamic == nil {
		t.Fatalf("failed to activate dynamic MCP tool")
	}
	if dynamic.IsReadOnly(nil) || !dynamic.IsDestructive(nil) {
		t.Fatalf("MCP dynamic tools should fail closed for permissions")
	}
	schema := dynamic.ToAPISchema()["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("expected raw MCP JSON schema, got %#v", schema)
	}
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID:    "mcp-1",
		Name:  publicName,
		Input: map[string]any{"text": "hello"},
	}, execCtx)
	if result.IsError || result.Content != "echo:hello" {
		t.Fatalf("unexpected MCP tool result: error=%v content=%q", result.IsError, result.Content)
	}

	registry.ActivateTool("mcp_list_resources")
	listResult := registry.Execute(context.Background(), coretools.ToolCall{ID: "mcp-2", Name: "mcp_list_resources"}, execCtx)
	if listResult.IsError || !strings.Contains(listResult.Content, "[Demo Server] Demo memory (text/plain) - mem://demo") {
		t.Fatalf("unexpected resources list: error=%v content=%q", listResult.IsError, listResult.Content)
	}
	registry.ActivateTool("mcp_read_resource")
	readResult := registry.Execute(context.Background(), coretools.ToolCall{
		ID:    "mcp-3",
		Name:  "mcp_read_resource",
		Input: map[string]any{"server_name": "Demo Server", "uri": "mem://demo"},
	}, execCtx)
	if readResult.IsError || readResult.Content != "resource:mem://demo" {
		t.Fatalf("unexpected resource read: error=%v content=%q", readResult.IsError, readResult.Content)
	}
	clients := execCtx["mcp_clients"].(map[string]*mcp.McpClient)
	for _, client := range clients {
		client.Disconnect(context.Background())
	}
}

func TestMCPRegisterToolsConnectFailureAndNoConfigDoNotPolluteRegistryLikePython(t *testing.T) {
	emptyRoot := t.TempDir()
	t.Setenv("HOME", filepath.Join(emptyRoot, "home"))
	registry := coretools.NewToolRegistry()
	execCtx := coretools.ExecutionContext{}
	if err := coretools.RegisterMCPTools(registry, emptyRoot, execCtx); err != nil {
		t.Fatal(err)
	}
	if len(registry.GetDeferredTools()) != 0 || execCtx["mcp_clients"] != nil || execCtx["pending_mcp_trust"] != nil {
		t.Fatalf("no MCP config should be a clean no-op like Python, deferred=%#v ctx=%#v", registry.GetDeferredTools(), execCtx)
	}

	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home2"))
	raw := `{"mcpServers":{"bad-server":{"command":"does-not-exist-lumina-mcp"}}}`
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded := mcp.LoadProjectMCPConfig(root)
	if len(loaded) != 1 {
		t.Fatalf("expected bad project config, got %#v", loaded)
	}
	if err := mcp.SaveTrustedMCP(root, map[string]string{"bad-server": loaded[0].Fingerprint()}); err != nil {
		t.Fatal(err)
	}
	registry = coretools.NewToolRegistry()
	execCtx = coretools.ExecutionContext{}
	if err := coretools.RegisterMCPTools(registry, root, execCtx); err != nil {
		t.Fatal(err)
	}
	clients, _ := execCtx["mcp_clients"].(map[string]*mcp.McpClient)
	if len(registry.GetDeferredTools()) != 0 || len(clients) != 0 {
		t.Fatalf("failed MCP connect should not register tools or connected clients like Python, deferred=%#v clients=%#v", registry.GetDeferredTools(), clients)
	}
}

func TestMCPListResourcesPreservesRegisteredServerOrder(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	newResourceServer := func(resourceName, resourceURI string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			msg := mcp.ParseMessage(string(body))
			switch req := msg.(type) {
			case mcp.JSONRPCNotification:
				w.WriteHeader(http.StatusNoContent)
			case mcp.JSONRPCRequest:
				result := map[string]any{}
				switch req.Method {
				case "initialize":
					result = map[string]any{"serverInfo": map[string]any{"name": resourceName}, "capabilities": map[string]any{}}
				case "tools/list":
					result = map[string]any{"tools": []map[string]any{}}
				case "resources/list":
					result = map[string]any{"resources": []map[string]any{{"uri": resourceURI, "name": resourceName}}}
				default:
					t.Fatalf("unexpected method: %s", req.Method)
				}
				_, _ = w.Write([]byte(mcp.SerializeMessage(mcp.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result})))
			default:
				t.Fatalf("unexpected message: %#v", msg)
			}
		}))
	}
	zebra := newResourceServer("Zebra resource", "mem://zebra")
	defer zebra.Close()
	alpha := newResourceServer("Alpha resource", "mem://alpha")
	defer alpha.Close()

	config := `{"mcpServers":{"Zebra Server":{"url":"` + zebra.URL + `"},"Alpha Server":{"url":"` + alpha.URL + `"}}}`
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded := mcp.LoadProjectMCPConfig(root)
	trusted := map[string]string{}
	for _, cfg := range loaded {
		trusted[cfg.Name] = cfg.Fingerprint()
	}
	if err := mcp.SaveTrustedMCP(root, trusted); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry()
	execCtx := coretools.ExecutionContext{}
	if err := coretools.RegisterMCPTools(registry, root, execCtx); err != nil {
		t.Fatal(err)
	}
	registry.ActivateTool("mcp_list_resources")
	result := registry.Execute(context.Background(), coretools.ToolCall{ID: "mcp-order", Name: "mcp_list_resources"}, execCtx)
	if result.IsError {
		t.Fatalf("list resources failed: %s", result.Content)
	}
	zebraIdx := strings.Index(result.Content, "[Zebra Server]")
	alphaIdx := strings.Index(result.Content, "[Alpha Server]")
	if zebraIdx < 0 || alphaIdx < 0 || zebraIdx > alphaIdx {
		t.Fatalf("expected Python insertion-order resource listing, got:\n%s", result.Content)
	}
	clients := execCtx["mcp_clients"].(map[string]*mcp.McpClient)
	for _, client := range clients {
		client.Disconnect(context.Background())
	}
}

func TestMCPInputSchemaConversionMatchesPythonShape(t *testing.T) {
	schema := coretools.BuildMCPInputSchema("Demo.Tool", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text":   map[string]any{"type": "string", "description": "Text", "minLength": 2},
			"mode":   map[string]any{"enum": []any{"a", "b"}},
			"count":  map[string]any{"type": []any{"integer", "null"}, "minimum": 1},
			"nested": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "number"}}, "required": []any{"x"}},
			"items":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required":             []any{"text", "nested"},
		"additionalProperties": false,
	})
	if schema["title"] != "Demo_Tool" || schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("unexpected top-level MCP schema: %#v", schema)
	}
	props := schema["properties"].(map[string]any)
	text := props["text"].(map[string]any)
	if text["description"] != "Text" || text["minLength"] != 2 || text["title"] != "Text" || text["type"] != "string" {
		t.Fatalf("text field mismatch: %#v", text)
	}
	mode := props["mode"].(map[string]any)
	if mode["default"] != nil || mode["title"] != "Mode" {
		t.Fatalf("optional enum should carry Python default/title: %#v", mode)
	}
	count := props["count"].(map[string]any)
	if count["default"] != nil {
		t.Fatalf("optional nullable count should default to null: %#v", count)
	}
	if _, ok := count["anyOf"].([]any); !ok {
		t.Fatalf("nullable count should use anyOf like Pydantic: %#v", count)
	}
	defs := schema["$defs"].(map[string]any)
	nested := defs["Demo_Tool_nested"].(map[string]any)
	if nested["title"] != "Demo_Tool_nested" || nested["type"] != "object" {
		t.Fatalf("nested schema mismatch: %#v", nested)
	}
	ref := props["nested"].(map[string]any)
	if ref["$ref"] != "#/$defs/Demo_Tool_nested" {
		t.Fatalf("nested field should be a $ref, got %#v", ref)
	}
}

func TestMCPDynamicToolAPISchemaUsesConvertedPythonShape(t *testing.T) {
	tool := coretools.NewMCPDynamicTool("mcp__demo__echo", "demo", "Echo.Tool", "Echo text", map[string]any{
		"type":       "object",
		"properties": map[string]any{"text": map[string]any{"type": "string", "description": "Text to echo"}},
		"required":   []any{"text"},
	}, "")
	inputSchema := tool.ToAPISchema()["input_schema"].(map[string]any)
	if inputSchema["title"] != "Echo_Tool" {
		t.Fatalf("dynamic MCP schema should use original tool-name model title, got %#v", inputSchema)
	}
	props := inputSchema["properties"].(map[string]any)
	if props["text"].(map[string]any)["title"] != "Text" {
		t.Fatalf("dynamic MCP schema should be converted through Python shape, got %#v", props)
	}
}

func TestMCPDynamicToolDecodeInputValidatesLikePythonPydantic(t *testing.T) {
	tool := coretools.NewMCPDynamicTool("mcp__demo__strict", "demo", "strict", "Strict input", map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"name", "status"},
		"properties": map[string]any{
			"name":   map[string]any{"type": "string", "minLength": 2},
			"status": map[string]any{"type": "string", "enum": []any{"open", "closed"}},
			"count":  map[string]any{"type": []any{"integer", "null"}, "minimum": 1},
		},
	}, "")
	registry := coretools.NewToolRegistry(tool)
	registry.ActivateTool("mcp__demo__strict")

	cases := []struct {
		name  string
		input map[string]any
	}{
		{name: "missing required", input: map[string]any{"name": "Alice"}},
		{name: "invalid enum", input: map[string]any{"name": "Alice", "status": "merged"}},
		{name: "extra forbidden", input: map[string]any{"name": "Alice", "status": "open", "extra": "bad"}},
		{name: "constraint", input: map[string]any{"name": "A", "status": "open"}},
	}
	for _, tc := range cases {
		result := registry.Execute(context.Background(), coretools.ToolCall{
			ID: "mcp-invalid", Name: "mcp__demo__strict", Input: tc.input,
		}, coretools.ExecutionContext{})
		if !result.IsError || !strings.Contains(result.Content, "Invalid input for tool 'mcp__demo__strict'") {
			t.Fatalf("%s: expected Python-style invalid input error, got error=%v content=%q", tc.name, result.IsError, result.Content)
		}
	}
}

func TestMCPDynamicToolDecodeInputFiltersLikePythonModelDump(t *testing.T) {
	tool := coretools.NewMCPDynamicTool("mcp__demo__filter", "demo", "filter", "Filter input", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":  map[string]any{"type": "string"},
			"count": map[string]any{"type": []any{"integer", "null"}},
			"nested": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"keep": map[string]any{"type": "string"},
				},
			},
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":       "object",
					"properties": map[string]any{"id": map[string]any{"type": "string"}},
				},
			},
		},
	}, "")
	decoded, err := tool.DecodeInput(map[string]any{
		"name":  "Alice",
		"count": nil,
		"extra": "ignored",
		"nested": map[string]any{
			"keep":  "yes",
			"extra": "ignored",
		},
		"items": []any{
			map[string]any{"id": "1", "extra": "ignored"},
		},
	})
	if err != nil {
		t.Fatalf("valid MCP input failed decode: %v", err)
	}
	got := decoded.(map[string]any)
	if _, ok := got["extra"]; ok {
		t.Fatalf("top-level extras should be ignored like Pydantic, got %#v", got)
	}
	if _, ok := got["count"]; ok {
		t.Fatalf("nil optional fields should be excluded like model_dump(exclude_none=True), got %#v", got)
	}
	if got["name"] != "Alice" {
		t.Fatalf("known field missing after decode: %#v", got)
	}
	nested := got["nested"].(map[string]any)
	if nested["keep"] != "yes" || nested["extra"] != nil {
		t.Fatalf("nested extras should be ignored like Pydantic, got %#v", nested)
	}
	item := got["items"].([]any)[0].(map[string]any)
	if item["id"] != "1" || item["extra"] != nil {
		t.Fatalf("array item extras should be ignored like Pydantic, got %#v", item)
	}
}

func TestMCPProjectConfigRequiresTrust(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	config := `{"mcpServers":{"untrusted":{"url":"https://example.invalid/mcp"}}}`
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := coretools.NewToolRegistry()
	execCtx := coretools.ExecutionContext{}
	if err := coretools.RegisterMCPTools(registry, root, execCtx); err != nil {
		t.Fatal(err)
	}
	if len(registry.GetDeferredTools()) != 0 {
		t.Fatalf("untrusted project MCP should not be registered")
	}
	pending, _ := execCtx["pending_mcp_trust"].([]map[string]any)
	if len(pending) != 1 || pending[0]["name"] != "untrusted" || pending[0]["fingerprint"] == "" {
		t.Fatalf("expected pending trust payload, got %#v", pending)
	}
}

func TestCoreExecutionEngineClearMCPDisconnectsClientsLikePython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	signalFile := filepath.Join(root, "mcp-term.txt")
	serverScript := filepath.Join(root, "mcp-server.sh")
	script := `#!/bin/sh
trap 'echo TERM > "$1"; exit 0' TERM
while IFS= read -r line; do
  if printf '%s' "$line" | grep -q '"method":"initialize"'; then
    printf '{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"fake","version":"1.0"},"capabilities":{}}}\n'
  elif printf '%s' "$line" | grep -q '"method":"tools/list"'; then
    printf '{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}\n'
    while true; do sleep 1; done
  elif printf '%s' "$line" | grep -q '"method":"ping"'; then
    printf '{"jsonrpc":"2.0","id":3,"result":{}}\n'
  fi
done
`
	if err := os.WriteFile(serverScript, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	command := "/bin/sh"
	rawConfig, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"srv": map[string]any{"command": command, "args": []string{serverScript, signalFile}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), rawConfig, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded := mcp.LoadProjectMCPConfig(root)
	if len(loaded) != 1 {
		t.Fatalf("expected one MCP config, got %#v", loaded)
	}
	if err := mcp.SaveTrustedMCP(root, map[string]string{"srv": loaded[0].Fingerprint()}); err != nil {
		t.Fatal(err)
	}

	cfg := config.NewConfig()
	cfg.CWD = root
	cfg.MCPEnabled = true
	cfg.SkillsEnabled = false
	cfg.APIKey = ""
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.Messages = []map[string]any{{"role": "user", "content": "hello"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range engine.QueryLoop(ctx, &state) {
	}
	if engine.Registry.GetDeferredTools()["mcp_list_resources"] == nil {
		t.Fatalf("expected MCP resource tools to be registered before ClearMCP")
	}

	engine.ClearMCP()
	for i := 0; i < 50; i++ {
		if data, err := os.ReadFile(signalFile); err == nil && strings.TrimSpace(string(data)) == "TERM" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("ClearMCP should disconnect stdio MCP clients and terminate the server process")
}

func TestCoreExecutionEnginePromptsAndPersistsMCPTrustLikePython(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"docs":{"command":"fake-mcp","args":["--stdio"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfig()
	cfg.CWD = root
	cfg.MCPEnabled = true
	cfg.APIKey = ""
	state := agent.NewAgentState()
	state.SystemPrompt = "system"
	state.Messages = []map[string]any{{"role": "user", "content": "hello"}}

	engine := agent.NewCoreExecutionEngine(&cfg)
	stream := engine.QueryLoop(context.Background(), &state)

	first, ok := <-stream
	if !ok {
		t.Fatal("expected MCP trust permission event")
	}
	if first.Type != "permission_needed" || first.Content != "mcp_project_trust" {
		t.Fatalf("expected MCP trust permission event, got %#v", first)
	}
	pending, _ := first.Metadata["mcp_trust_request"].([]map[string]any)
	if len(pending) != 1 || pending[0]["name"] != "docs" || pending[0]["fingerprint"] == "" {
		t.Fatalf("unexpected MCP trust payload: %#v", first.Metadata)
	}

	engine.ResolveMCPTrust(true)
	for range stream {
	}

	data, err := os.ReadFile(mcp.TrustedMCPPath(root))
	if err != nil {
		t.Fatalf("expected trusted MCP file after approval: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("invalid trusted MCP JSON: %v", err)
	}
	servers, _ := payload["servers"].(map[string]any)
	if servers["docs"] != pending[0]["fingerprint"] {
		t.Fatalf("trusted MCP fingerprint mismatch: payload=%#v pending=%#v", payload, pending)
	}
}
