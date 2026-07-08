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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"LuminaCode/config"
	luminateam "LuminaCode/team"

	"github.com/gorilla/websocket"
)

type DaemonOptions struct {
	Host              string
	Port              int
	Config            config.Config
	EndpointPath      string
	IdleCheckInterval time.Duration
	IdleEmptyChecks   int
}

type DaemonServer struct {
	opts        DaemonOptions
	token       string
	manager     *SessionManager
	teamManager *luminateam.Manager
	httpSrv     *http.Server
	upgrader    websocket.Upgrader

	mu      sync.Mutex
	clients map[*wsClient]struct{}

	activeConnections int
	emptyIdleChecks   int
}

type wsClient struct {
	conn          *websocket.Conn
	mu            sync.Mutex
	sessionID     string
	exitRequested bool
}

func RunDaemonCLI(args []string) error {
	if len(args) > 0 && args[0] == "shutdown" {
		return RunShutdownCLI(args[1:])
	}
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

func RunShutdownCLI(args []string) error {
	flags := flag.NewFlagSet("shutdown", flag.ContinueOnError)
	endpointPath := flags.String("endpoint", DefaultEndpointPath(), "daemon endpoint file")
	timeout := flags.Duration("timeout", 10*time.Second, "shutdown wait timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	data, err := os.ReadFile(*endpointPath)
	if os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "lumina-backend is not running: %s not found\n", *endpointPath)
		return nil
	}
	if err != nil {
		return err
	}
	var endpoint EndpointInfo
	if err := json.Unmarshal(data, &endpoint); err != nil {
		return err
	}
	host := strings.TrimSpace(endpoint.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	if endpoint.Port <= 0 || endpoint.AuthToken == "" {
		return fmt.Errorf("invalid backend endpoint file: %s", *endpointPath)
	}
	url := fmt.Sprintf("ws://%s:%d/v1/ws?token=%s", host, endpoint.Port, endpoint.AuthToken)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
		_ = conn.SetWriteDeadline(deadline)
	}
	req := RPCRequest{ID: "shutdown", Method: "backend.shutdown"}
	if err := conn.WriteJSON(req); err != nil {
		return err
	}
	for {
		var resp RPCResponse
		if err := conn.ReadJSON(&resp); err != nil {
			return err
		}
		if resp.ID != req.ID {
			continue
		}
		if !resp.OK {
			if resp.Error != nil {
				return fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
			}
			return fmt.Errorf("backend shutdown failed")
		}
		break
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := os.Stat(*endpointPath); os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "lumina-backend stopped")
				return nil
			}
		}
	}
}

func Serve(ctx context.Context, opts DaemonOptions) error {
	if strings.TrimSpace(opts.Host) == "" {
		opts.Host = "127.0.0.1"
	}
	if opts.EndpointPath == "" {
		opts.EndpointPath = DefaultEndpointPath()
	}
	if opts.IdleCheckInterval <= 0 {
		opts.IdleCheckInterval = 10 * time.Minute
	}
	if opts.IdleEmptyChecks <= 0 {
		opts.IdleEmptyChecks = 2
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
	server.teamManager = luminateam.NewManager(opts.Config, func(parentSessionID, eventType string, payload any) {
		server.broadcast(PushEvent{
			Type:      "event",
			SessionID: parentSessionID,
			Seq:       time.Now().UnixNano(),
			Event: map[string]any{
				"type":    eventType,
				"payload": payload,
			},
		})
	}, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ws", server.handleWS)
	mux.HandleFunc("/v1/a2a/ws", server.handleA2AWS)
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
	server.startManagedServices()
	defer server.shutdownManagedResources()
	go func() {
		<-ctx.Done()
		_ = server.httpSrv.Shutdown(context.Background())
	}()
	go server.startIdleHeartbeat(ctx)
	fmt.Fprintf(os.Stderr, "lumina-backend daemon listening on %s:%d\n", opts.Host, actualPort)
	err = server.httpSrv.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *DaemonServer) startManagedServices() {
	if path := s.searxNGScriptPath(); path != "" {
		if output, err := runManagedScript(path, "start", s.opts.Config); err != nil {
			fmt.Fprintf(os.Stderr, "lumina-backend warning: failed to start managed SearxNG: %v\n%s\n", err, output)
		}
	}
}

func (s *DaemonServer) shutdownManagedResources() {
	_ = os.Remove(s.opts.EndpointPath)
	if s.manager != nil {
		s.manager.Shutdown()
	}
	if s.teamManager != nil {
		s.teamManager.Shutdown()
	}
	if path := s.searxNGScriptPath(); path != "" {
		if output, err := runManagedScript(path, "stop", s.opts.Config); err != nil {
			fmt.Fprintf(os.Stderr, "lumina-backend warning: failed to stop managed SearxNG: %v\n%s\n", err, output)
		}
	}
}

func (s *DaemonServer) searxNGScriptPath() string {
	if strings.HasSuffix(os.Args[0], ".test") {
		return ""
	}
	candidates := []string{}
	if root := strings.TrimSpace(os.Getenv("LUMINA_RESOURCE_ROOT")); root != "" {
		candidates = append(candidates, filepath.Join(root, "setup-searxng.sh"))
	}
	if s.opts.Config.TeamDir != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(s.opts.Config.TeamDir), "setup-searxng.sh"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

func runManagedScript(path, action string, cfg config.Config) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, action)
	appRoot := filepath.Dir(cfg.TeamDir)
	if appRoot == "." || appRoot == "" {
		appRoot = filepath.Join(os.Getenv("HOME"), ".lumina")
	}
	cmd.Env = append(os.Environ(),
		"LUMINA_APP_ROOT="+appRoot,
		"LUMINA_WEB_SEARCH_BASE_URL="+strings.TrimRight(cfg.WebSearchBaseURL, "/"),
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(output), fmt.Errorf("%s timed out", filepath.Base(path))
	}
	return string(output), err
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
	unregister := s.registerConnection()
	defer unregister()
	client := &wsClient{conn: conn}
	s.mu.Lock()
	s.clients[client] = struct{}{}
	s.mu.Unlock()
	defer func() {
		exitRequested := client.exitWasRequested()
		s.mu.Lock()
		delete(s.clients, client)
		remainingClients := len(s.clients)
		s.mu.Unlock()
		_ = conn.Close()
		if exitRequested && remainingClients == 0 {
			go func() {
				time.Sleep(50 * time.Millisecond)
				if s.clientCountExcluding(nil) == 0 {
					_ = s.httpSrv.Shutdown(context.Background())
				}
			}()
		}
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

func (s *DaemonServer) handleA2AWS(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("token") != s.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	teamSessionID := r.URL.Query().Get("team_session_id")
	agentID := r.URL.Query().Get("agent_id")
	if strings.TrimSpace(teamSessionID) == "" || strings.TrimSpace(agentID) == "" {
		http.Error(w, "team_session_id and agent_id are required", http.StatusBadRequest)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	unregister := s.registerConnection()
	defer unregister()
	defer conn.Close()
	for {
		var req RPCRequest
		if err := conn.ReadJSON(&req); err != nil {
			return
		}
		result, err := s.teamManager.HandleA2A(r.Context(), teamSessionID, agentID, req.Method, req.Params)
		if err != nil {
			_ = conn.WriteJSON(RPCResponse{ID: req.ID, OK: false, Error: &RPCError{Code: "a2a_error", Message: err.Error()}})
			continue
		}
		_ = conn.WriteJSON(RPCResponse{ID: req.ID, OK: true, Result: result})
	}
}

func (c *wsClient) setSessionID(sessionID string) {
	c.mu.Lock()
	c.sessionID = sessionID
	c.mu.Unlock()
}

func (c *wsClient) requestExit() {
	c.mu.Lock()
	c.exitRequested = true
	c.mu.Unlock()
}

func (c *wsClient) exitWasRequested() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitRequested
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

func (s *DaemonServer) registerConnection() func() {
	s.mu.Lock()
	s.activeConnections++
	s.emptyIdleChecks = 0
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.activeConnections > 0 {
				s.activeConnections--
			}
			s.mu.Unlock()
		})
	}
}

func (s *DaemonServer) activeConnectionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeConnections
}

func (s *DaemonServer) startIdleHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(s.opts.IdleCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			count := s.activeConnectionCount()
			if count > 0 {
				s.mu.Lock()
				s.emptyIdleChecks = 0
				s.mu.Unlock()
				continue
			}
			s.mu.Lock()
			s.emptyIdleChecks++
			emptyChecks := s.emptyIdleChecks
			limit := s.opts.IdleEmptyChecks
			s.mu.Unlock()
			if emptyChecks >= limit {
				fmt.Fprintf(os.Stderr, "lumina-backend idle heartbeat: no websocket connections for %d consecutive checks; shutting down\n", emptyChecks)
				_ = s.httpSrv.Shutdown(context.Background())
				return
			}
		}
	}
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
			"pid":                 os.Getpid(),
			"model":               s.opts.Config.APIModel,
			"cwd":                 s.opts.Config.CWD,
			"sessions":            s.manager.Count(),
			"websocket_clients":   s.clientCountExcluding(nil),
			"active_connections":  s.activeConnectionCount(),
			"idle_check_interval": s.opts.IdleCheckInterval.String(),
			"idle_empty_checks":   s.opts.IdleEmptyChecks,
			"started":             true,
		}, nil
	case "backend.shutdown":
		go func() {
			time.Sleep(50 * time.Millisecond)
			_ = s.httpSrv.Shutdown(context.Background())
		}()
		return map[string]any{"shutting_down": true}, nil
	case "session.create":
		var p struct {
			CWD  string `json:"cwd"`
			Yolo bool   `json:"yolo"`
		}
		decodeParams(req.Params, &p)
		controller, err := s.manager.Create(p.CWD)
		if err != nil {
			return nil, toRPCError("session_create_failed", err)
		}
		if p.Yolo {
			controller.SetYolo(true)
			s.teamManager.ApplyParentRuntimeConfig(controller.ID(), controller.RuntimeConfig())
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
		snapshot := controller.Snapshot()
		snapshot.Teams = s.teamManager.RestorePersistedForParent(controller.ID(), p.CWD)
		return snapshot, nil
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
		client.requestExit()
		otherClients := s.clientCountExcluding(client)
		return map[string]any{
			"exited":                    true,
			"session_id":                p.SessionID,
			"other_clients":             otherClients,
			"shutting_down":             false,
			"shutdown_after_disconnect": otherClients == 0,
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
		result := controller.ToggleYolo()
		s.teamManager.ApplyParentRuntimeConfig(controller.ID(), controller.RuntimeConfig())
		return result, nil
	case "team.list":
		return s.teamManager.List(), nil
	case "team.create_template":
		var p struct {
			Name string `json:"name"`
		}
		decodeParams(req.Params, &p)
		result, err := s.teamManager.CreateTemplate(p.Name)
		if err != nil {
			return nil, toRPCError("team_create_template_failed", err)
		}
		return result, nil
	case "team.start":
		var p struct {
			SessionID string `json:"session_id"`
			TeamName  string `json:"team_name"`
			CWD       string `json:"cwd"`
		}
		decodeParams(req.Params, &p)
		base := s.opts.Config
		if parent, err := s.manager.Get(p.SessionID); err == nil {
			base = parent.RuntimeConfig()
		}
		controller, err := s.teamManager.StartWithConfig(p.SessionID, p.TeamName, p.CWD, base)
		if err != nil {
			return nil, toRPCError("team_start_failed", err)
		}
		return controller.Snapshot(), nil
	case "team.submit":
		var p struct {
			TeamSessionID string `json:"team_session_id"`
			Input         string `json:"input"`
		}
		decodeParams(req.Params, &p)
		controller, err := s.teamManager.Get(p.TeamSessionID)
		if err != nil {
			return nil, toRPCError("team_session_not_found", err)
		}
		if err := controller.Submit(ctx, p.Input); err != nil {
			code := "team_submit_failed"
			if err.Error() == "team_session_busy" {
				code = "team_session_busy"
			}
			return nil, toRPCError(code, err)
		}
		return map[string]any{"accepted": true}, nil
	case "team.out":
		var p struct {
			TeamSessionID string `json:"team_session_id"`
			Abort         bool   `json:"abort"`
		}
		decodeParams(req.Params, &p)
		if p.Abort {
			s.teamManager.Abort(p.TeamSessionID)
		}
		return map[string]any{"team_mode": false, "team_session_id": p.TeamSessionID}, nil
	case "team.abort":
		var p struct {
			TeamSessionID string `json:"team_session_id"`
		}
		decodeParams(req.Params, &p)
		ok := s.teamManager.Abort(p.TeamSessionID)
		return map[string]any{"aborted": ok}, nil
	case "team.snapshot", "team.status":
		var p struct {
			TeamSessionID string `json:"team_session_id"`
		}
		decodeParams(req.Params, &p)
		controller, err := s.teamManager.Get(p.TeamSessionID)
		if err != nil {
			return nil, toRPCError("team_session_not_found", err)
		}
		return controller.Snapshot(), nil
	case "team.artifacts":
		var p struct {
			TeamSessionID string `json:"team_session_id"`
		}
		decodeParams(req.Params, &p)
		controller, err := s.teamManager.Get(p.TeamSessionID)
		if err != nil {
			return nil, toRPCError("team_session_not_found", err)
		}
		return controller.Artifacts(), nil
	case "team.timeline":
		var p struct {
			TeamSessionID string `json:"team_session_id"`
		}
		decodeParams(req.Params, &p)
		controller, err := s.teamManager.Get(p.TeamSessionID)
		if err != nil {
			return nil, toRPCError("team_session_not_found", err)
		}
		return controller.Timeline(), nil
	case "team.dialogue":
		var p struct {
			TeamSessionID string `json:"team_session_id"`
		}
		decodeParams(req.Params, &p)
		controller, err := s.teamManager.Get(p.TeamSessionID)
		if err != nil {
			return nil, toRPCError("team_session_not_found", err)
		}
		return controller.Dialogue(), nil
	case "team.summary":
		var p struct {
			TeamSessionID string `json:"team_session_id"`
		}
		decodeParams(req.Params, &p)
		controller, err := s.teamManager.Get(p.TeamSessionID)
		if err != nil {
			return nil, toRPCError("team_session_not_found", err)
		}
		return controller.Summary(), nil
	case "team.detail":
		var p struct {
			TeamSessionID string `json:"team_session_id"`
			Kind          string `json:"kind"`
			ID            string `json:"id"`
			Name          string `json:"name"`
		}
		decodeParams(req.Params, &p)
		controller, err := s.teamManager.Get(p.TeamSessionID)
		if err != nil {
			return nil, toRPCError("team_session_not_found", err)
		}
		detail, err := controller.Detail(p.Kind, p.ID, p.Name)
		if err != nil {
			return nil, toRPCError("team_detail_not_found", err)
		}
		return detail, nil
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
			SessionID     string `json:"session_id"`
			TeamSessionID string `json:"team_session_id"`
			RequestID     string `json:"request_id"`
			Decision      string `json:"decision"`
		}
		decodeParams(req.Params, &p)
		if strings.TrimSpace(p.TeamSessionID) != "" {
			ok := s.teamManager.ResolvePermission(p.RequestID, p.Decision)
			return map[string]any{"resolved": ok}, nil
		}
		controller, err := s.manager.Get(p.SessionID)
		if err != nil {
			return nil, toRPCError("session_not_found", err)
		}
		ok := controller.ResolvePermission(p.RequestID, p.Decision)
		if !ok {
			ok = s.teamManager.ResolvePermission(p.RequestID, p.Decision)
		}
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
