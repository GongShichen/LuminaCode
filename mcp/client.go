package mcp

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	MCPProtocolVersion    = "2024-11-05"
	ClientName            = "LUMINA"
	ClientVersion         = "0.1.0"
	DefaultPingInterval   = 30 * time.Second
	DefaultRequestTimeout = 30 * time.Second
	DefaultConnectTimeout = 10 * time.Second
)

type ConnectionState int

const (
	ConnectionDisconnected ConnectionState = iota
	ConnectionConnecting
	ConnectionConnected
	ConnectionError
)

func (s ConnectionState) String() string {
	switch s {
	case ConnectionDisconnected:
		return "DISCONNECTED"
	case ConnectionConnecting:
		return "CONNECTING"
	case ConnectionConnected:
		return "CONNECTED"
	case ConnectionError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

type McpError struct {
	Code    int
	Message string
	Data    any
}

func (e McpError) Error() string {
	if e.Data != nil {
		return fmt.Sprintf("[%d] %s (data: %v)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

type McpClient struct {
	config McpServerConfig

	transport          McpTransport
	state              ConnectionState
	requestID          int
	serverInfo         map[string]any
	serverCapabilities map[string]any
	tools              []map[string]any
	resources          []map[string]any
	lastError          string
	pingCancel         context.CancelFunc

	mu        sync.Mutex
	requestMu sync.Mutex
}

func NewMcpClient(cfg McpServerConfig) *McpClient {
	return &McpClient{
		config:             cfg,
		state:              ConnectionDisconnected,
		serverInfo:         map[string]any{},
		serverCapabilities: map[string]any{},
	}
}

func (c *McpClient) State() ConnectionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *McpClient) Tools() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copyMapSlice(c.tools)
}

func (c *McpClient) ServerName() string { return c.config.Name }

func (c *McpClient) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastError
}

func (c *McpClient) ServerInfo() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copyAnyMap(c.serverInfo)
}

func (c *McpClient) ServerCapabilities() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copyAnyMap(c.serverCapabilities)
}

func (c *McpClient) Connect(ctx context.Context) bool {
	c.mu.Lock()
	if c.state == ConnectionConnected {
		c.mu.Unlock()
		return true
	}
	c.state = ConnectionConnecting
	c.lastError = ""
	c.mu.Unlock()

	transport, err := c.createTransport()
	if err != nil {
		c.setError("Failed to create transport: " + err.Error())
		return false
	}
	c.mu.Lock()
	c.transport = transport
	c.mu.Unlock()

	if !c.connectTransport(ctx) {
		return false
	}
	if !c.initialize(ctx) {
		return false
	}

	c.mu.Lock()
	c.state = ConnectionConnected
	c.mu.Unlock()
	c.startPing()
	return true
}

func (c *McpClient) Disconnect(ctx context.Context) {
	c.mu.Lock()
	c.stopPingLocked()
	transport := c.transport
	c.transport = nil
	c.state = ConnectionDisconnected
	c.mu.Unlock()
	if transport != nil {
		_ = transport.Close(ctx)
	}
}

func (c *McpClient) Reconnect(ctx context.Context) bool {
	c.Disconnect(ctx)
	return c.Connect(ctx)
}

func (c *McpClient) DiscoverTools(ctx context.Context) ([]map[string]any, error) {
	resp, err := c.Request(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	tools := mapSliceFromAny(asMap(resp)["tools"])
	c.mu.Lock()
	c.tools = copyMapSlice(tools)
	c.mu.Unlock()
	return tools, nil
}

func (c *McpClient) DiscoverResources(ctx context.Context) ([]map[string]any, error) {
	resp, err := c.Request(ctx, "resources/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	resources := mapSliceFromAny(asMap(resp)["resources"])
	c.mu.Lock()
	c.resources = copyMapSlice(resources)
	c.mu.Unlock()
	return resources, nil
}

func (c *McpClient) CallTool(ctx context.Context, toolName string, arguments map[string]any) (string, error) {
	resp, err := c.Request(ctx, "tools/call", map[string]any{
		"name":      toolName,
		"arguments": arguments,
	})
	if err != nil {
		return "", err
	}
	return ExtractContent(mapSliceFromAny(asMap(resp)["content"])), nil
}

func (c *McpClient) ReadResource(ctx context.Context, uri string) (string, error) {
	resp, err := c.Request(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return "", err
	}
	respMap := asMap(resp)
	contents := mapSliceFromAny(respMap["contents"])
	if len(contents) > 0 {
		return ExtractContent(contents), nil
	}
	if text, ok := respMap["text"].(string); ok {
		return text, nil
	}
	return fmt.Sprint(resp), nil
}

func (c *McpClient) Request(ctx context.Context, method string, params map[string]any) (any, error) {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()

	c.mu.Lock()
	if c.state != ConnectionConnected {
		state := c.state
		name := c.config.Name
		c.mu.Unlock()
		return nil, McpError{Code: MCPServerNotInitialized, Message: fmt.Sprintf("Server '%s' is not connected (state: %s)", name, state)}
	}
	transport := c.transport
	c.mu.Unlock()
	if transport == nil {
		return nil, McpError{Code: MCPServerNotInitialized, Message: "Transport not available"}
	}

	reqID := c.nextID()
	req := MakeRequest(method, params, reqID)
	if err := transport.Send(ctx, req); err != nil {
		c.setError("Send failed: " + err.Error())
		return nil, McpError{Code: MCPServerNotInitialized, Message: "Failed to send request: " + err.Error()}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, DefaultRequestTimeout)
	defer cancel()
	for {
		msg, err := transport.Receive(timeoutCtx)
		if err != nil {
			if timeoutCtx.Err() != nil {
				msg := fmt.Sprintf("Request '%s' timed out after %.0fs", method, DefaultRequestTimeout.Seconds())
				c.setError(msg)
				c.disconnectTimedOutTransport()
				return nil, McpError{Code: MCPServerNotInitialized, Message: msg}
			}
			c.setError("Receive failed: " + err.Error())
			return nil, McpError{Code: MCPServerNotInitialized, Message: "Failed to receive response: " + err.Error()}
		}
		if msg == nil {
			c.setError("Server closed connection")
			return nil, McpError{Code: MCPServerNotInitialized, Message: "Server closed connection unexpectedly"}
		}
		switch typed := msg.(type) {
		case JSONRPCNotification:
			continue
		case JSONRPCResponse:
			if typed.ID != reqID {
				continue
			}
			return typed.Result, nil
		case JSONRPCError:
			if typed.ID != reqID {
				continue
			}
			return nil, McpError{
				Code:    intFromAny(typed.Error["code"], MCPUnknownError),
				Message: stringFromAny(typed.Error["message"], "Unknown error"),
				Data:    typed.Error["data"],
			}
		}
	}
}

func (c *McpClient) createTransport() (McpTransport, error) {
	if c.config.IsStdio() {
		command := ""
		if c.config.Command != nil {
			command = *c.config.Command
		}
		cwd := ""
		if c.config.CWD != nil {
			cwd = *c.config.CWD
		}
		return NewStdioTransport(command, c.config.Args, c.config.Env, cwd), nil
	}
	if c.config.IsHTTP() {
		url := ""
		if c.config.URL != nil {
			url = *c.config.URL
		}
		return NewHTTPTransport(url, c.config.Headers), nil
	}
	return nil, fmt.Errorf("No transport configured (need command or url)")
}

func (c *McpClient) connectTransport(ctx context.Context) bool {
	c.mu.Lock()
	transport := c.transport
	c.mu.Unlock()
	if transport == nil {
		c.setError("Transport not available")
		return false
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, DefaultConnectTimeout)
	defer cancel()
	if err := transport.Connect(timeoutCtx); err != nil {
		if timeoutCtx.Err() != nil {
			c.setError("Transport connect timed out")
		} else {
			c.setError("Transport connect failed: " + err.Error())
		}
		return false
	}
	return true
}

func (c *McpClient) initialize(ctx context.Context) bool {
	c.mu.Lock()
	transport := c.transport
	c.mu.Unlock()
	if transport == nil {
		c.setError("Transport not available")
		return false
	}
	initReq := MakeRequest("initialize", map[string]any{
		"protocolVersion": MCPProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": ClientName, "version": ClientVersion},
	}, c.nextID())

	if err := transport.Send(ctx, initReq); err != nil {
		c.setError("Initialize handshake failed: " + err.Error())
		return false
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, DefaultRequestTimeout)
	defer cancel()
	resp, err := transport.Receive(timeoutCtx)
	if err != nil {
		if timeoutCtx.Err() != nil {
			c.setError("Initialize handshake timed out")
			c.disconnectTimedOutTransport()
		} else {
			c.setError("Initialize handshake failed: " + err.Error())
		}
		return false
	}
	if resp == nil {
		c.setError("Server closed connection during initialize")
		return false
	}
	if errResp, ok := resp.(JSONRPCError); ok {
		c.setError(fmt.Sprintf("Initialize error: [%v] %v", errResp.Error["code"], errResp.Error["message"]))
		return false
	}
	response, ok := resp.(JSONRPCResponse)
	if !ok {
		c.setError(fmt.Sprintf("Unexpected response type during initialize: %T", resp))
		return false
	}
	result := asMap(response.Result)
	c.mu.Lock()
	c.serverInfo = copyAnyMap(asMap(result["serverInfo"]))
	c.serverCapabilities = copyAnyMap(asMap(result["capabilities"]))
	c.mu.Unlock()
	_ = transport.Send(ctx, MakeNotification("notifications/initialized", nil))
	return true
}

func (c *McpClient) disconnectTimedOutTransport() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.Disconnect(ctx)
}

func (c *McpClient) nextID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requestID++
	return c.requestID
}

func (c *McpClient) setError(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastError = msg
	c.state = ConnectionError
}

func (c *McpClient) startPing() {
	c.mu.Lock()
	if c.pingCancel != nil {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.pingCancel = cancel
	c.mu.Unlock()
	go c.pingLoop(ctx, DefaultPingInterval)
}

func (c *McpClient) stopPingLocked() {
	if c.pingCancel != nil {
		c.pingCancel()
		c.pingCancel = nil
	}
}

func (c *McpClient) pingLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if c.State() != ConnectionConnected {
				return
			}
			if _, err := c.Request(ctx, "ping", map[string]any{}); err != nil {
				c.setError("Ping failed - connection lost")
				return
			}
		}
	}
}

func asMap(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func mapSliceFromAny(value any) []map[string]any {
	values, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]map[string]any); ok {
			return copyMapSlice(typed)
		}
		return nil
	}
	out := make([]map[string]any, 0, len(values))
	for _, item := range values {
		out = append(out, asMap(item))
	}
	return out
}

func copyMapSlice(values []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(values))
	for _, value := range values {
		out = append(out, copyAnyMap(value))
	}
	return out
}

func copyAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func intFromAny(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	default:
		return fallback
	}
}

func stringFromAny(value any, fallback string) string {
	if s, ok := value.(string); ok {
		return s
	}
	return fallback
}
