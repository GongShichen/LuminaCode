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

	"LuminaCode/agent"
	"LuminaCode/api"
	"LuminaCode/apppaths"
	"LuminaCode/config"
	"LuminaCode/llmclient"
	"LuminaCode/longmemory"
	luminateam "LuminaCode/team"
	coretools "LuminaCode/tools"

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
	if len(cfg.PathErrors) > 0 {
		return fmt.Errorf("invalid AppRoot configuration: %s", strings.Join(cfg.PathErrors, "; "))
	}
	if err := apppaths.PrepareRuntime(cfg.Paths, "dev"); err != nil {
		return err
	}
	if cfg.LongTermMemoryEnabled {
		if err := cfg.ValidateMemoryConfig(); err != nil {
			return err
		}
	}
	if cfg.LongTermMemoryEnabled {
		store, err := longmemory.Open(context.Background(), cfg.LongTermMemoryStore)
		if err != nil {
			return fmt.Errorf("open long-term memory store: %w", err)
		}
		_ = store.Close()
	}
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
	go server.startMemoryMaintenance(ctx)
	fmt.Fprintf(os.Stderr, "lumina-backend daemon listening on %s:%d\n", opts.Host, actualPort)
	err = server.httpSrv.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *DaemonServer) startMemoryMaintenance(ctx context.Context) {
	run := func() {
		cfg := config.GetConfig()
		if !cfg.LongTermMemoryEnabled {
			return
		}
		if len(cfg.MemoryConfigErrors) > 0 {
			fmt.Fprintf(os.Stderr, "lumina-backend memory configuration invalid: %s\n", strings.Join(cfg.MemoryConfigErrors, "; "))
			return
		}
		store, err := longmemory.Open(ctx, cfg.LongTermMemoryStore)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lumina-backend memory maintenance store: %v\n", err)
			return
		}
		defer store.Close()
		if cfg.MemoryLifecycleEnabled {
			policy := memoryLifecyclePolicy(cfg)
			if _, err := store.BackfillLifecycle(ctx, policy, time.Now().UTC()); err != nil {
				fmt.Fprintf(os.Stderr, "lumina-backend memory lifecycle migration: %v\n", err)
				return
			}
			decisions, err := store.PreviewMaintenance(ctx, policy, time.Now().UTC())
			if err != nil {
				fmt.Fprintf(os.Stderr, "lumina-backend memory lifecycle preview: %v\n", err)
				return
			}
			if applied, err := store.ApplyMaintenance(ctx, decisions); err != nil {
				fmt.Fprintf(os.Stderr, "lumina-backend memory lifecycle apply: %v\n", err)
				return
			} else if applied > 0 {
				fmt.Fprintf(os.Stderr, "lumina-backend memory lifecycle: applied=%d\n", applied)
			}
		}
		extractionJobs, _ := store.ClaimJobs(ctx, []string{"extraction"}, 8)
		if len(extractionJobs) > 0 {
			controller := agent.NewExtractionController(cfg, coretools.NewToolRegistry())
			for _, job := range extractionJobs {
				if err := controller.ProcessExtractionJob(ctx, job); err != nil {
					_ = store.RetryJob(context.WithoutCancel(ctx), job.JobID, err, time.Minute)
					fmt.Fprintf(os.Stderr, "lumina-backend memory enrichment: %v\n", err)
				} else {
					_ = store.CompleteJob(context.WithoutCancel(ctx), job.JobID)
				}
			}
		}
		backfillJobs, _ := store.ClaimJobs(ctx, []string{"canonical_entity_backfill", "canonical_event_backfill",
			"session_chunk_index_backfill", "evidence_atom_backfill", "atom_structure_backfill",
			"atom_structure_embedding_backfill", "atom_overlap_repair_backfill", "atom_speech_act_repair_backfill"}, 8)
		for _, job := range backfillJobs {
			if err := store.RunBackfillJob(ctx, job); err != nil {
				_ = store.RetryJob(context.WithoutCancel(ctx), job.JobID, err, time.Minute)
				fmt.Fprintf(os.Stderr, "lumina-backend memory backfill: %v\n", err)
			} else {
				_ = store.CompleteJob(context.WithoutCancel(ctx), job.JobID)
			}
		}
		if !cfg.MemoryEmbeddingEnabled {
			return
		}
		embedder, err := longmemory.SharedLocalEmbedder(cfg.MemoryEmbeddingModel, cfg.MemoryEmbeddingModelDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lumina-backend memory maintenance: %v\n", err)
			return
		}
		jobs, _ := store.ClaimJobs(ctx, []string{"embedding_backfill", "chunk_embedding_backfill", "atom_embedding_backfill",
			"consolidation", "migration_backfill"}, 32)
		scheduled := longmemory.SharedEmbeddingScheduler(embedder, longmemory.EmbeddingSchedulerOptions{
			BatchSize: cfg.MemoryEmbeddingBatchSize, BatchWait: time.Duration(cfg.MemoryEmbeddingBatchWaitMS) * time.Millisecond,
			QueryCacheEntries: cfg.MemoryEmbeddingQueryCacheEntries,
			ExecutionTimeout:  time.Duration(cfg.MemoryEmbeddingExecutionTimeout * float64(time.Second))})
		if result, err := store.RunMaintenance(ctx, scheduled, 32); err != nil {
			for _, job := range jobs {
				_ = store.RetryJob(context.WithoutCancel(ctx), job.JobID, err, time.Minute)
			}
			fmt.Fprintf(os.Stderr, "lumina-backend memory maintenance failed: %v\n", err)
		} else if result.Embedded+result.ChunkEmbedded+result.AtomEmbedded+result.SessionEmbedded+result.Enriched+result.Consolidated+
			result.Linked+result.Promoted+result.Archived > 0 {
			for _, job := range jobs {
				_ = store.CompleteJob(context.WithoutCancel(ctx), job.JobID)
			}
			fmt.Fprintf(os.Stderr, "lumina-backend memory maintenance: %s\n", result.String())
		} else {
			for _, job := range jobs {
				_ = store.CompleteJob(context.WithoutCancel(ctx), job.JobID)
			}
		}
	}
	run()
	for {
		interval := config.GetConfig().MemoryMaintenanceIntervalSeconds
		if interval <= 0 {
			interval = 300
		}
		timer := time.NewTimer(time.Duration(interval) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			run()
		}
	}
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
	if s.opts.Config.Paths.ScriptsDir != "" {
		candidates = append(candidates, filepath.Join(s.opts.Config.Paths.ScriptsDir, "setup-searxng.sh"))
	}
	if s.opts.Config.Paths.ResourcesDir != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(s.opts.Config.Paths.ResourcesDir), "setup-searxng.sh"))
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
	cmd.Env = append(os.Environ(),
		"LUMINA_APP_ROOT="+cfg.Paths.Root,
		"LUMINA_WEB_SEARCH_BASE_URL="+strings.TrimRight(cfg.WebSearchBaseURL, "/"),
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(output), fmt.Errorf("%s timed out", filepath.Base(path))
	}
	return string(output), err
}

func DefaultEndpointPath() string {
	paths, err := apppaths.ResolveCurrent()
	if err != nil {
		return ""
	}
	return paths.EndpointFile
}

func writeEndpoint(path string, endpoint EndpointInfo) error {
	data, err := json.MarshalIndent(endpoint, "", "  ")
	if err != nil {
		return err
	}
	return apppaths.WriteFileAtomic(path, append(data, '\n'), 0o600)
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
	case "session.pin":
		var p struct {
			SessionID string `json:"session_id"`
			Pinned    bool   `json:"pinned"`
		}
		decodeParams(req.Params, &p)
		if strings.TrimSpace(p.SessionID) == "" {
			return nil, &RPCError{Code: "session_id_required", Message: "session_id is required"}
		}
		meta, err := s.manager.Pin(p.SessionID, p.Pinned)
		if err != nil {
			return nil, toRPCError("session_pin_failed", err)
		}
		return meta, nil
	case "storage.status":
		report, err := s.manager.StorageStatus()
		if err != nil {
			return nil, toRPCError("storage_status_failed", err)
		}
		return report, nil
	case "storage.cleanup":
		var p struct {
			Enforce bool `json:"enforce"`
		}
		decodeParams(req.Params, &p)
		report, err := s.manager.CleanupStorage(p.Enforce)
		if err != nil {
			return nil, toRPCError("storage_cleanup_failed", err)
		}
		return report, nil
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
	case "memory.list":
		var p struct {
			SessionID       string                `json:"session_id"`
			ScopeType       longmemory.ScopeType  `json:"scope_type"`
			ScopeKey        string                `json:"scope_key"`
			MemoryType      longmemory.MemoryType `json:"memory_type"`
			Status          longmemory.Status     `json:"status"`
			Tags            []string              `json:"tags"`
			Limit           int                   `json:"limit"`
			IncludeInactive bool                  `json:"include_inactive"`
			IncludeExpired  bool                  `json:"include_expired"`
			CreatedAfter    string                `json:"created_after"`
			CreatedBefore   string                `json:"created_before"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		opts := longmemory.SearchOptions{Tags: p.Tags, Limit: p.Limit, IncludeInactive: p.IncludeInactive || p.Status != "", IncludeExpired: p.IncludeExpired}
		opts.CreatedAfter = parseMemoryFilterTime(p.CreatedAfter)
		opts.CreatedBefore = parseMemoryFilterTime(p.CreatedBefore)
		if p.ScopeType != "" && strings.TrimSpace(p.ScopeKey) != "" {
			opts.Scopes = []longmemory.Scope{{Type: p.ScopeType, Key: p.ScopeKey}}
		}
		if p.MemoryType != "" {
			opts.Types = []longmemory.MemoryType{p.MemoryType}
		}
		entries, err := store.List(ctx, opts)
		if err != nil {
			return nil, toRPCError("memory_list_failed", err)
		}
		if p.Status != "" {
			entries = filterMemoryStatus(entries, p.Status)
		}
		return map[string]any{"items": entries}, nil
	case "memory.search":
		var p struct {
			SessionID string             `json:"session_id"`
			Query     string             `json:"query"`
			Scopes    []longmemory.Scope `json:"scopes"`
			Limit     int                `json:"limit"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		scopes := p.Scopes
		if len(scopes) == 0 {
			scopes = s.defaultMemoryScopes(p.SessionID)
		}
		cfg := config.GetConfig()
		var embedder longmemory.Embedder
		if cfg.MemoryEmbeddingEnabled {
			if local, embedErr := longmemory.SharedLocalEmbedder(cfg.MemoryEmbeddingModel, cfg.MemoryEmbeddingModelDir); embedErr == nil {
				embedder = longmemory.SharedEmbeddingScheduler(local, longmemory.EmbeddingSchedulerOptions{
					BatchSize: cfg.MemoryEmbeddingBatchSize, BatchWait: time.Duration(cfg.MemoryEmbeddingBatchWaitMS) * time.Millisecond,
					QueryCacheEntries: cfg.MemoryEmbeddingQueryCacheEntries,
					ExecutionTimeout:  time.Duration(cfg.MemoryEmbeddingExecutionTimeout * float64(time.Second))})
			}
		}
		limit := p.Limit
		if limit <= 0 {
			limit = cfg.MemoryAtomMaxSelected
		}
		query := longmemory.MemoryQuery{Text: strings.TrimSpace(p.Query), Timestamp: time.Now().UTC(),
			Scopes: scopes, SessionID: p.SessionID, AgentID: "main"}
		catalog, catalogErr := store.InspectCatalog(ctx, scopes)
		expansion, expansionModel, expansionError := agent.ExpandMemoryQuery(ctx, cfg, query, catalog,
			func(ctx context.Context, model string) (api.LLMClient, error) {
				return llmclient.Create(cfg, model, 1024, nil, api.DefaultRetryConfigPtr())
			})
		if catalogErr != nil {
			if expansionError != "" {
				expansionError += "; "
			}
			expansionError += "inspect memory catalog: " + catalogErr.Error()
		}
		hybrid, err := store.SearchAllChannels(ctx, query, expansion, embedder, longmemory.HybridSearchOptions{
			FTSCandidates: cfg.MemoryFTSCandidates, VectorCandidates: cfg.MemoryVectorCandidates,
			GraphCandidates: cfg.MemoryGraphCandidates, GraphMaxHops: cfg.MemoryGraphMaxHops,
			RRFK: cfg.MemoryRRFK, MaxItems: limit,
			CoreContextTokens: cfg.MemoryCoreContextTokens, TargetContextTokens: cfg.MemoryContextTargetTokens,
			MaxContextTokens: cfg.MemoryContextMaxTokens, LocalTimeout: time.Duration(cfg.MemoryRetrievalLocalTimeoutSeconds * float64(time.Second)),
			SessionID: p.SessionID, AgentID: "main",
			ExpansionModel: expansionModel, ExpansionError: expansionError,
			NeighborChunks:  cfg.MemoryEvidenceNeighborChunks,
			AtomMaxSelected: limit, CoverageMaxFacets: cfg.MemoryCoverageMaxFacets,
			CoverageCompletionRounds:      cfg.MemoryCoverageCompletionRounds,
			CoverageRelevanceWeight:       cfg.MemoryCoverageRelevanceWeight,
			CoverageFacetWeight:           cfg.MemoryCoverageFacetWeight,
			CoverageProvenanceWeight:      cfg.MemoryCoverageProvenanceWeight,
			CoverageSourceWeight:          cfg.MemoryCoverageSourceWeight,
			CoverageCoherenceWeight:       cfg.MemoryCoverageCoherenceWeight,
			CoverageSupportTarget:         cfg.MemoryCoverageSupportTarget,
			CoverageResidualTrigger:       cfg.MemoryCoverageResidualTrigger,
			CoverageMinMarginalGain:       cfg.MemoryCoverageMinMarginalGain,
			StructuralContextEnabled:      cfg.MemoryAtomStructuralContextEnabled,
			StructuralContextTokens:       cfg.MemoryAtomStructuralContextTokens,
			EvidencePrimaryBudgetRatio:    cfg.MemoryEvidencePrimaryBudgetRatio,
			EvidenceCompletionBudgetRatio: cfg.MemoryEvidenceCompletionBudgetRatio,
			EvidenceContextBudgetRatio:    cfg.MemoryEvidenceContextBudgetRatio,
		})
		if err != nil {
			return nil, toRPCError("memory_search_failed", err)
		}
		return map[string]any{"items": hybrid.Packet.Evidence, "evidence_packet": hybrid.Packet, "retrieval_trace": hybrid.Trace}, nil
	case "memory.facts":
		var p struct {
			SessionID string             `json:"session_id"`
			Scopes    []longmemory.Scope `json:"scopes"`
			Entities  []string           `json:"entities"`
			At        string             `json:"at"`
			Limit     int                `json:"limit"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		if len(p.Scopes) == 0 {
			p.Scopes = s.defaultMemoryScopes(p.SessionID)
		}
		facts, err := store.ResolveFactsAt(ctx, p.Scopes, p.Entities, parseMemoryFilterTime(p.At), p.Limit)
		if err != nil {
			return nil, toRPCError("memory_facts_failed", err)
		}
		return map[string]any{"items": facts}, nil
	case "memory.retrieval_traces":
		var p struct {
			Limit int `json:"limit"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		traces, err := store.ListRetrievalTraces(ctx, p.Limit)
		if err != nil {
			return nil, toRPCError("memory_retrieval_trace_failed", err)
		}
		return map[string]any{"items": traces}, nil
	case "memory.get":
		var p struct {
			MemoryID string `json:"memory_id"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		entry, err := store.Get(ctx, p.MemoryID)
		if err != nil {
			return nil, toRPCError("memory_not_found", err)
		}
		return entry, nil
	case "memory.create", "memory.update":
		var candidate longmemory.Candidate
		decodeParams(req.Params, &candidate)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		entry, err := store.Upsert(ctx, candidate)
		if err != nil {
			return nil, toRPCError("memory_upsert_failed", err)
		}
		return entry, nil
	case "memory.delete":
		var p struct {
			MemoryID string `json:"memory_id"`
			Hard     bool   `json:"hard"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		if err := store.Delete(ctx, p.MemoryID, p.Hard); err != nil {
			return nil, toRPCError("memory_delete_failed", err)
		}
		return map[string]any{"deleted": true, "hard": p.Hard}, nil
	case "memory.archive":
		var p struct {
			MemoryID string `json:"memory_id"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		if err := store.Archive(ctx, p.MemoryID, "manual_archive"); err != nil {
			return nil, toRPCError("memory_archive_failed", err)
		}
		return map[string]any{"archived": true}, nil
	case "memory.pin", "memory.unpin":
		var p struct {
			MemoryID string `json:"memory_id"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		pinned := req.Method == "memory.pin"
		if err := store.Pin(ctx, p.MemoryID, pinned); err != nil {
			return nil, toRPCError("memory_pin_failed", err)
		}
		return map[string]any{"pinned": pinned}, nil
	case "memory.lifecycle":
		var p struct {
			MemoryID string `json:"memory_id"`
			Limit    int    `json:"limit"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		decision, err := store.CalculateLifecycle(ctx, p.MemoryID, memoryLifecyclePolicy(config.GetConfig()), time.Now().UTC())
		if err != nil {
			return nil, toRPCError("memory_lifecycle_failed", err)
		}
		events, err := store.ListLifecycleEvents(ctx, p.MemoryID, p.Limit)
		if err != nil {
			return nil, toRPCError("memory_lifecycle_events_failed", err)
		}
		return map[string]any{"decision": decision, "events": events}, nil
	case "memory.maintenance.preview", "memory.maintenance.run":
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		cfg := config.GetConfig()
		if len(cfg.MemoryConfigErrors) > 0 {
			return nil, toRPCError("memory_config_invalid", fmt.Errorf("%s", strings.Join(cfg.MemoryConfigErrors, "; ")))
		}
		policy := memoryLifecyclePolicy(cfg)
		if _, err := store.BackfillLifecycle(ctx, policy, time.Now().UTC()); err != nil {
			return nil, toRPCError("memory_lifecycle_migration_failed", err)
		}
		decisions, err := store.PreviewMaintenance(ctx, policy, time.Now().UTC())
		if err != nil {
			return nil, toRPCError("memory_maintenance_preview_failed", err)
		}
		applied := 0
		if req.Method == "memory.maintenance.run" {
			applied, err = store.ApplyMaintenance(ctx, decisions)
			if err != nil {
				return nil, toRPCError("memory_maintenance_run_failed", err)
			}
		}
		return map[string]any{"decisions": decisions, "applied": applied}, nil
	case "memory.approve":
		var p struct {
			MemoryID string `json:"memory_id"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		if err := store.Approve(ctx, p.MemoryID); err != nil {
			return nil, toRPCError("memory_approve_failed", err)
		}
		return map[string]any{"approved": true}, nil
	case "memory.restore":
		var p struct {
			MemoryID string `json:"memory_id"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		if err := store.Restore(ctx, p.MemoryID); err != nil {
			return nil, toRPCError("memory_restore_failed", err)
		}
		return map[string]any{"restored": true}, nil
	case "memory.prioritize":
		var p struct {
			MemoryID   string  `json:"memory_id"`
			Importance float64 `json:"importance"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		if err := store.UpdateImportance(ctx, p.MemoryID, p.Importance); err != nil {
			return nil, toRPCError("memory_prioritize_failed", err)
		}
		return map[string]any{"prioritized": true, "importance": p.Importance}, nil
	case "memory.deprioritize":
		var p struct {
			MemoryID string `json:"memory_id"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		if err := store.Deprioritize(ctx, p.MemoryID); err != nil {
			return nil, toRPCError("memory_deprioritize_failed", err)
		}
		return map[string]any{"deprioritized": true, "importance": 0}, nil
	case "memory.supersede":
		var p struct {
			OldMemoryID string               `json:"old_memory_id"`
			NewMemoryID string               `json:"new_memory_id"`
			Candidate   longmemory.Candidate `json:"candidate"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		if p.Candidate.Title != "" || p.Candidate.Content != "" || p.Candidate.Summary != "" {
			entry, err := store.SupersedeWith(ctx, p.OldMemoryID, p.Candidate)
			if err != nil {
				return nil, toRPCError("memory_supersede_failed", err)
			}
			return map[string]any{"superseded": true, "new_memory": entry}, nil
		}
		if err := store.Supersede(ctx, p.OldMemoryID, p.NewMemoryID); err != nil {
			return nil, toRPCError("memory_supersede_failed", err)
		}
		return map[string]any{"superseded": true, "new_memory_id": p.NewMemoryID}, nil
	case "memory.export":
		var p struct {
			Format string `json:"format"`
			OutDir string `json:"out_dir"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		dir, err := longmemory.ExportMarkdown(ctx, store, p.OutDir)
		if err != nil {
			return nil, toRPCError("memory_export_failed", err)
		}
		return map[string]any{"format": "markdown", "path": dir}, nil
	case "memory.import":
		var p struct {
			Path       string                 `json:"path"`
			Candidates []longmemory.Candidate `json:"candidates"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		count, err := ImportMemoryCandidates(ctx, store, p.Path, p.Candidates)
		if err != nil {
			return nil, toRPCError("memory_import_failed", err)
		}
		return map[string]any{"imported": count}, nil
	case "memory.used":
		var p struct {
			Limit int `json:"limit"`
		}
		decodeParams(req.Params, &p)
		store, err := s.openMemoryStore(ctx)
		if err != nil {
			return nil, toRPCError("memory_store_open_failed", err)
		}
		defer store.Close()
		records, err := store.ListUsed(ctx, p.Limit)
		if err != nil {
			return nil, toRPCError("memory_used_failed", err)
		}
		return map[string]any{"items": records}, nil
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

func (s *DaemonServer) openMemoryStore(ctx context.Context) (*longmemory.Store, error) {
	if !s.opts.Config.LongTermMemoryEnabled {
		return nil, fmt.Errorf("long-term memory is disabled")
	}
	return longmemory.Open(ctx, s.opts.Config.LongTermMemoryStore)
}

func (s *DaemonServer) defaultMemoryScopes(sessionID string) []longmemory.Scope {
	cfg := s.opts.Config
	if strings.TrimSpace(sessionID) != "" {
		if controller, err := s.manager.Get(sessionID); err == nil && controller != nil {
			cfg = controller.RuntimeConfig()
		}
	}
	return longmemory.RuntimeScopesCanonical(cfg.ProjectRoot(), "main", "", "")
}

func filterMemoryStatus(entries []longmemory.Entry, status longmemory.Status) []longmemory.Entry {
	if status == "" {
		return entries
	}
	filtered := make([]longmemory.Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.Status == status {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func parseMemoryFilterTime(text string) time.Time {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func memoryLifecyclePolicy(cfg config.Config) longmemory.LifecyclePolicy {
	policy := longmemory.DefaultLifecyclePolicy()
	policy.Enabled = cfg.MemoryLifecycleEnabled
	policy.HotAccessDays = cfg.MemoryHotAccessDays
	policy.WarmAccessDays = cfg.MemoryWarmAccessDays
	policy.AccessRecencyHalfLife = cfg.MemoryAccessRecencyHalfLifeDays
	policy.ArchiveGraceDays = cfg.MemoryArchiveGraceDays
	policy.ArchiveValueThreshold = cfg.MemoryArchiveValueThreshold
	policy.RetentionDays = longmemory.RetentionPolicy{}
	for memoryType, days := range cfg.MemoryRetentionDays {
		policy.RetentionDays[longmemory.MemoryType(memoryType)] = days
	}
	weights := cfg.MemoryValueWeights
	policy.Weights = longmemory.ValueWeights{
		Importance: weights["importance"], Confidence: weights["confidence"],
		AccessRecency: weights["access_recency"], AccessFrequency: weights["access_frequency"],
		Reinforcement: weights["reinforcement"], ProvenanceStrength: weights["provenance_strength"],
		DependencyStrength: weights["dependency_strength"],
	}
	return policy
}

func ImportMemoryCandidates(ctx context.Context, store *longmemory.Store, path string, candidates []longmemory.Candidate) (int, error) {
	count := 0
	for _, candidate := range candidates {
		if _, err := store.Upsert(ctx, candidate); err != nil {
			return count, err
		}
		count++
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return count, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return count, err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return count, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := strings.ToLower(entry.Name())
			if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".md") {
				continue
			}
			n, err := ImportMemoryCandidates(ctx, store, filepath.Join(path, entry.Name()), nil)
			if err != nil {
				return count, err
			}
			count += n
		}
		return count, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return count, err
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		var list []longmemory.Candidate
		if err := json.Unmarshal(data, &list); err != nil {
			var wrapper struct {
				Memories   []longmemory.Candidate `json:"memories"`
				Candidates []longmemory.Candidate `json:"candidates"`
			}
			if wrapErr := json.Unmarshal(data, &wrapper); wrapErr != nil {
				return count, err
			}
			list = append(wrapper.Memories, wrapper.Candidates...)
		}
		for _, candidate := range list {
			if _, err := store.Upsert(ctx, candidate); err != nil {
				return count, err
			}
			count++
		}
	case ".jsonl":
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var candidate longmemory.Candidate
			if err := json.Unmarshal([]byte(line), &candidate); err != nil {
				return count, err
			}
			if _, err := store.Upsert(ctx, candidate); err != nil {
				return count, err
			}
			count++
		}
	case ".md":
		candidate := parseMemoryMarkdown(data)
		if _, err := store.Upsert(ctx, candidate); err != nil {
			return count, err
		}
		count++
	default:
		return count, fmt.Errorf("unsupported memory import file: %s", path)
	}
	return count, nil
}

func parseMemoryMarkdown(data []byte) longmemory.Candidate {
	text := string(data)
	frontmatter := map[string]string{}
	body := text
	if strings.HasPrefix(text, "---\n") {
		rest := strings.TrimPrefix(text, "---\n")
		if idx := strings.Index(rest, "\n---"); idx >= 0 {
			raw := rest[:idx]
			body = strings.TrimSpace(rest[idx+4:])
			for _, line := range strings.Split(raw, "\n") {
				key, value, ok := strings.Cut(line, ":")
				if ok {
					frontmatter[strings.TrimSpace(key)] = strings.TrimSpace(value)
				}
			}
		}
	}
	title := frontmatter["title"]
	if title == "" {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			if line != "" {
				title = line
				break
			}
		}
	}
	scopeType := longmemory.ScopeType(frontmatter["scope_type"])
	scopeKey := frontmatter["scope_key"]
	memoryType := longmemory.MemoryType(frontmatter["memory_type"])
	return longmemory.Candidate{
		MemoryID:      frontmatter["memory_id"],
		ScopeType:     scopeType,
		ScopeKey:      scopeKey,
		MemoryType:    memoryType,
		Status:        longmemory.Status(frontmatter["status"]),
		Title:         title,
		Content:       strings.TrimSpace(body),
		Summary:       frontmatter["summary"],
		Tags:          splitMemoryCSV(frontmatter["tags"]),
		Entities:      splitMemoryCSV(frontmatter["entities"]),
		SourceAgentID: frontmatter["source_agent_id"],
	}
}

func splitMemoryCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
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
