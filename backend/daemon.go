package backend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"LuminaCode/config"

	"github.com/gorilla/websocket"
)

type DaemonOptions struct {
	Host         string
	Port         int
	Config       config.Config
	EndpointPath string
}

type DaemonServer struct {
	opts     DaemonOptions
	token    string
	manager  *SessionManager
	httpSrv  *http.Server
	upgrader websocket.Upgrader

	mu      sync.Mutex
	clients map[*wsClient]struct{}
}

type wsClient struct {
	conn      *websocket.Conn
	mu        sync.Mutex
	sessionID string
}

func RunDaemonCLI(args []string) error {
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	host := flags.String("host", "127.0.0.1", "daemon host")
	port := flags.Int("port", 0, "daemon port")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg := config.GetConfig()
	return Serve(context.Background(), DaemonOptions{
		Host:         *host,
		Port:         *port,
		Config:       cfg,
		EndpointPath: DefaultEndpointPath(),
	})
}

func Serve(ctx context.Context, opts DaemonOptions) error {
	if strings.TrimSpace(opts.Host) == "" {
		opts.Host = "127.0.0.1"
	}
	if opts.EndpointPath == "" {
		opts.EndpointPath = DefaultEndpointPath()
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", opts.Host, opts.Port))
	if err != nil {
		return err
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	server := &DaemonServer{
		opts:    opts,
		token:   token,
		clients: map[*wsClient]struct{}{},
		upgrader: websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
			return r.Host == r.URL.Host || strings.HasPrefix(r.RemoteAddr, "127.0.0.1:") || strings.HasPrefix(r.RemoteAddr, "[::1]:")
		}},
	}
	server.manager = NewSessionManager(opts.Config, server.broadcast)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ws", server.handleWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	server.httpSrv = &http.Server{Handler: mux}
	endpoint := EndpointInfo{
		PID:       os.Getpid(),
		Host:      opts.Host,
		Port:      actualPort,
		AuthToken: token,
		StartedAt: nowRFC3339(),
		URL:       fmt.Sprintf("ws://%s:%d/v1/ws", opts.Host, actualPort),
	}
	if err := writeEndpoint(opts.EndpointPath, endpoint); err != nil {
		_ = listener.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		_ = server.httpSrv.Shutdown(context.Background())
	}()
	fmt.Fprintf(os.Stderr, "lumina-backend daemon listening on %s:%d\n", opts.Host, actualPort)
	err = server.httpSrv.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func DefaultEndpointPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lumina", "run", "backend.json")
}

func writeEndpoint(path string, endpoint EndpointInfo) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(endpoint, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (s *DaemonServer) handleWS(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("token") != s.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := &wsClient{conn: conn}
	s.mu.Lock()
	s.clients[client] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, client)
		s.mu.Unlock()
		_ = conn.Close()
	}()
	for {
		var req RPCRequest
		if err := conn.ReadJSON(&req); err != nil {
			return
		}
		resp := s.dispatch(r.Context(), client, req)
		client.write(resp)
	}
}

func (c *wsClient) setSessionID(sessionID string) {
	c.mu.Lock()
	c.sessionID = sessionID
	c.mu.Unlock()
}

func (s *DaemonServer) clientCountExcluding(excluded *wsClient) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for client := range s.clients {
		if client == excluded {
			continue
		}
		count++
	}
	return count
}

func (s *DaemonServer) dispatch(ctx context.Context, client *wsClient, req RPCRequest) RPCResponse {
	result, rpcErr := s.dispatchResult(ctx, client, req)
	if rpcErr != nil {
		return RPCResponse{ID: req.ID, OK: false, Error: rpcErr}
	}
	return RPCResponse{ID: req.ID, OK: true, Result: result}
}

func (s *DaemonServer) dispatchResult(ctx context.Context, client *wsClient, req RPCRequest) (any, *RPCError) {
	switch req.Method {
	case "backend.status":
		return map[string]any{
			"pid":      os.Getpid(),
			"model":    s.opts.Config.APIModel,
			"cwd":      s.opts.Config.CWD,
			"sessions": s.manager.Count(),
			"started":  true,
		}, nil
	case "backend.shutdown":
		go func() {
			time.Sleep(50 * time.Millisecond)
			_ = s.httpSrv.Shutdown(context.Background())
		}()
		return map[string]any{"shutting_down": true}, nil
	case "session.create":
		var p struct {
			CWD string `json:"cwd"`
		}
		decodeParams(req.Params, &p)
		controller, err := s.manager.Create(p.CWD)
		if err != nil {
			return nil, toRPCError("session_create_failed", err)
		}
		client.setSessionID(controller.ID())
		return controller.Snapshot(), nil
	case "session.resume":
		var p struct {
			SessionID string `json:"session_id"`
			CWD       string `json:"cwd"`
		}
		decodeParams(req.Params, &p)
		controller, err := s.manager.Resume(p.SessionID, p.CWD)
		if err != nil {
			return nil, toRPCError("session_resume_failed", err)
		}
		client.setSessionID(controller.ID())
		return controller.Snapshot(), nil
	case "session.list":
		return s.manager.List(), nil
	case "session.snapshot":
		controller, rpcErr := s.controllerFromParams(req.Params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		client.setSessionID(controller.ID())
		return controller.Snapshot(), nil
	case "session.submit":
		var p struct {
			SessionID string `json:"session_id"`
			Input     string `json:"input"`
		}
		decodeParams(req.Params, &p)
		controller, err := s.manager.Get(p.SessionID)
		if err != nil {
			return nil, toRPCError("session_not_found", err)
		}
		client.setSessionID(controller.ID())
		if err := controller.Submit(ctx, p.Input); err != nil {
			code := "session_submit_failed"
			if err.Error() == "session_busy" {
				code = "session_busy"
			}
			return nil, toRPCError(code, err)
		}
		return map[string]any{"accepted": true}, nil
	case "session.exit":
		var p struct {
			SessionID string `json:"session_id"`
		}
		decodeParams(req.Params, &p)
		client.setSessionID("")
		otherClients := s.clientCountExcluding(client)
		if otherClients == 0 {
			go func() {
				time.Sleep(75 * time.Millisecond)
				if s.clientCountExcluding(client) == 0 {
					_ = s.httpSrv.Shutdown(context.Background())
				}
			}()
		}
		return map[string]any{
			"exited":        true,
			"session_id":    p.SessionID,
			"other_clients": otherClients,
			"shutting_down": otherClients == 0,
		}, nil
	case "session.abort":
		controller, rpcErr := s.controllerFromParams(req.Params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		controller.Abort()
		return map[string]any{"aborted": true}, nil
	case "session.save":
		controller, rpcErr := s.controllerFromParams(req.Params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		if err := controller.Save(); err != nil {
			return nil, toRPCError("session_save_failed", err)
		}
		return map[string]any{"saved": true}, nil
	case "session.clear":
		controller, rpcErr := s.controllerFromParams(req.Params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		controller.Clear()
		return controller.Snapshot(), nil
	case "session.compact":
		controller, rpcErr := s.controllerFromParams(req.Params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return controller.Compact(), nil
	case "session.tokens":
		controller, rpcErr := s.controllerFromParams(req.Params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return controller.Tokens(), nil
	case "session.yolo":
		controller, rpcErr := s.controllerFromParams(req.Params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return controller.ToggleYolo(), nil
	case "slash.list":
		controller, rpcErr := s.optionalController(req.Params)
		if rpcErr != nil || controller == nil {
			return map[string]any{"items": []any{}, "rows": []any{}}, nil
		}
		return map[string]any{"items": controller.SlashItems(), "rows": controller.SlashRows()}, nil
	case "skills.list":
		controller, rpcErr := s.controllerFromParams(req.Params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return controller.Skills(), nil
	case "skills.pick":
		var p struct {
			SessionID string `json:"session_id"`
			Name      string `json:"name"`
		}
		decodeParams(req.Params, &p)
		return map[string]any{"text": "/" + strings.TrimPrefix(p.Name, "/") + " "}, nil
	case "mcp.list":
		controller, rpcErr := s.controllerFromParams(req.Params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return controller.MCPTools(), nil
	case "permission.resolve":
		var p struct {
			SessionID string `json:"session_id"`
			RequestID string `json:"request_id"`
			Decision  string `json:"decision"`
		}
		decodeParams(req.Params, &p)
		controller, err := s.manager.Get(p.SessionID)
		if err != nil {
			return nil, toRPCError("session_not_found", err)
		}
		ok := controller.ResolvePermission(p.RequestID, p.Decision)
		return map[string]any{"resolved": ok}, nil
	default:
		return nil, &RPCError{Code: "method_not_found", Message: "unknown method: " + req.Method}
	}
}

func (s *DaemonServer) controllerFromParams(raw json.RawMessage) (*SessionController, *RPCError) {
	controller, rpcErr := s.optionalController(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if controller == nil {
		return nil, &RPCError{Code: "session_id_required", Message: "session_id is required"}
	}
	return controller, nil
}

func (s *DaemonServer) optionalController(raw json.RawMessage) (*SessionController, *RPCError) {
	var p struct {
		SessionID string `json:"session_id"`
	}
	decodeParams(raw, &p)
	if p.SessionID == "" {
		return nil, nil
	}
	controller, err := s.manager.Get(p.SessionID)
	if err != nil {
		return nil, toRPCError("session_not_found", err)
	}
	return controller, nil
}

func decodeParams(raw json.RawMessage, out any) {
	if len(raw) == 0 {
		return
	}
	_ = json.Unmarshal(raw, out)
}

func toRPCError(code string, err error) *RPCError {
	if err == nil {
		return nil
	}
	return &RPCError{Code: code, Message: err.Error()}
}

func (s *DaemonServer) broadcast(event PushEvent) {
	s.mu.Lock()
	clients := make([]*wsClient, 0, len(s.clients))
	for client := range s.clients {
		clients = append(clients, client)
	}
	s.mu.Unlock()
	for _, client := range clients {
		client.write(event)
	}
}

func (c *wsClient) write(value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.WriteJSON(value)
}
