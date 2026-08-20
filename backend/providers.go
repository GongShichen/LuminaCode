package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"LuminaCode/agent"
	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/harness"
	"LuminaCode/session"
	luminateam "LuminaCode/team"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/adaptor"
	"github.com/cloudwego/hertz/pkg/network/standard"
	"github.com/gorilla/websocket"
)

type NormalizedDaemonOptions struct {
	DaemonOptions
}

type AuthToken string

type daemonMemoryPreflight struct{}

func NormalizeDaemonOptions(opts DaemonOptions) NormalizedDaemonOptions {
	if opts.Config.UsesClusterRuntime() && strings.TrimSpace(opts.Config.ClusterListenAddr) != "" {
		if host, portText, err := net.SplitHostPort(opts.Config.ClusterListenAddr); err == nil {
			if port, parseErr := strconv.Atoi(portText); parseErr == nil {
				opts.Host, opts.Port = host, port
			}
		}
	}
	if strings.TrimSpace(opts.Host) == "" {
		opts.Host = "127.0.0.1"
	}
	if opts.EndpointPath == "" {
		opts.EndpointPath = DefaultEndpointPath()
	}
	if opts.Config.UsesClusterRuntime() && opts.Config.InstanceID != "" {
		defaultEndpoint := DefaultEndpointPath()
		if opts.EndpointPath == defaultEndpoint {
			extension := filepath.Ext(defaultEndpoint)
			base := strings.TrimSuffix(filepath.Base(defaultEndpoint), extension)
			opts.EndpointPath = filepath.Join(filepath.Dir(defaultEndpoint),
				base+"-"+safeEndpointComponent(opts.Config.InstanceID)+extension)
		}
	}
	if opts.IdleCheckInterval <= 0 {
		opts.IdleCheckInterval = 10 * time.Minute
	}
	if opts.IdleEmptyChecks <= 0 {
		opts.IdleEmptyChecks = 2
	}
	return NormalizedDaemonOptions{DaemonOptions: opts}
}

func ProvideDaemonConfig(opts NormalizedDaemonOptions) config.Config {
	return opts.Config
}

func ValidateDaemonMemory(ctx context.Context, cfg config.Config, factory agent.MemoryFabricFactory) (daemonMemoryPreflight, error) {
	if !cfg.LongTermMemoryEnabled {
		return daemonMemoryPreflight{}, nil
	}
	fabric, err := factory.Open(ctx, cfg, agent.MemoryOpenOptions{Identity: cluster.RuntimeIdentity{TenantID: session.LocalTenantID}})
	if err != nil {
		return daemonMemoryPreflight{}, fmt.Errorf("open Memory Fabric: %w", err)
	}
	if fabric == nil {
		return daemonMemoryPreflight{}, errors.New("Memory Fabric is required when long-term memory is enabled")
	}
	defer fabric.Close()
	if _, err := fabric.Doctor(ctx); err != nil {
		return daemonMemoryPreflight{}, fmt.Errorf("check Memory Fabric: %w", err)
	}
	return daemonMemoryPreflight{}, nil
}

func ProvideAuthToken() (AuthToken, error) {
	token, err := randomToken()
	return AuthToken(token), err
}

func ProvideListener(opts NormalizedDaemonOptions) (net.Listener, func(), error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", opts.Host, opts.Port))
	if err != nil {
		return nil, nil, err
	}
	return listener, func() { _ = listener.Close() }, nil
}

func ProvideEndpointInfo(opts NormalizedDaemonOptions, listener net.Listener, token AuthToken) EndpointInfo {
	actualPort := listener.Addr().(*net.TCPAddr).Port
	authToken := string(token)
	if opts.Config.UsesClusterRuntime() {
		authToken = ""
	}
	return EndpointInfo{
		PID:       os.Getpid(),
		Host:      opts.Host,
		Port:      actualPort,
		AuthToken: authToken,
		StartedAt: nowRFC3339(),
		URL:       fmt.Sprintf("ws://%s:%d/v1/ws", opts.Host, actualPort),
	}
}

func safeEndpointComponent(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "instance"
	}
	return builder.String()
}

func ProvideEventHub() (*EventHub, func()) {
	hub := NewEventHub()
	return hub, hub.Close
}

func ProvideEventEmitter(relay *EventRelay) EventEmitter {
	return relay.Publish
}

func ProvideRedisRuntime(ctx context.Context, cfg config.Config) (*cluster.RedisRuntime, func(), error) {
	if !cfg.UsesClusterRuntime() {
		return nil, func() {}, nil
	}
	if err := cfg.ValidateClusterConfig(); err != nil {
		return nil, nil, err
	}
	runtime, err := cluster.NewRedisRuntime(ctx, cfg.RedisURL, cfg.ClusterID)
	if err != nil {
		return nil, nil, err
	}
	runtime.SetCommandClaimIdle(time.Duration(cfg.ClusterLeaseTTLSeconds) * time.Second)
	return runtime, func() { _ = runtime.Close() }, nil
}

func ProvideSessionStore(cfg config.Config) *session.Store {
	return session.NewStore(cfg.SessionDir)
}

func ProvideSessionRepository(ctx context.Context, cfg config.Config, store *session.Store) (session.SessionRepository, func(), error) {
	if !cfg.UsesClusterRuntime() {
		return session.NewLocalRepository(store), func() {}, nil
	}
	if err := cfg.ValidateClusterConfig(); err != nil {
		return nil, nil, err
	}
	repository, err := session.NewPostgresRepository(ctx, cfg.PostgresURL, cfg.ClusterID)
	if err != nil {
		return nil, nil, err
	}
	return repository, func() { _ = repository.Close() }, nil
}

func ProvideSessionManager(cfg config.Config, repository session.SessionRepository, factory *SessionRuntimeFactory) (*SessionManager, func()) {
	manager := NewSessionManager(cfg, repository, factory)
	return manager, manager.Shutdown
}

func ProvideTeamManager(cfg config.Config, engineFactory agent.QueryEngineFactory, sessions *SessionManager,
	emit EventEmitter) (*luminateam.Manager, func()) {
	var manager *luminateam.Manager
	manager = luminateam.NewTenantManager(cfg, engineFactory, func(tenantID, parentSessionID, eventType string, payload any) {
		handleTeamRuntimeEvent(sessions, manager, emit, tenantID, parentSessionID, eventType, payload)
	}, nil)
	manager.UseJournalPersistence(true)
	return manager, manager.Shutdown
}

func handleTeamRuntimeEvent(sessions *SessionManager, teams *luminateam.Manager, emit EventEmitter,
	tenantID, parentSessionID, eventType string, payload any) {
	principal := cluster.Principal{TenantID: tenantID}
	if controller, err := sessions.GetFor(principal, parentSessionID); err == nil {
		if teamSessionID := teamSessionIDFromPayload(payload); teamSessionID != "" {
			if err := controller.AppendTeamEvent(context.Background(), teamSessionID, durableTeamEventType(eventType), payload); err != nil {
				slog.Warn("append team runtime event", "session_id", parentSessionID, "team_session_id", teamSessionID, "error", err)
			}
			if shouldCheckpointTeamEvent(eventType) {
				if teamSession, getErr := teams.GetFor(tenantID, teamSessionID); getErr == nil {
					checkpoint := teamSession.ExportRuntimeCheckpoint()
					if appendErr := controller.AppendTeamEvent(context.Background(), teamSessionID, harness.EventTeamRuntimeCheckpointed, checkpoint); appendErr != nil {
						slog.Warn("append team runtime checkpoint", "session_id", parentSessionID, "team_session_id", teamSessionID, "error", appendErr)
					}
				}
			}
		}
	}
	emit(PushEvent{Type: "event", TenantID: tenantID, SessionID: parentSessionID, Seq: time.Now().UnixNano(), Event: map[string]any{
		"type": eventType, "payload": payload,
	}})
}

func ProvideWebSocketUpgrader() websocket.Upgrader {
	return websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
		return r.Host == r.URL.Host || strings.HasPrefix(r.RemoteAddr, "127.0.0.1:") || strings.HasPrefix(r.RemoteAddr, "[::1]:")
	}}
}

func NewDaemonServer(opts NormalizedDaemonOptions, token AuthToken, sessions *SessionManager, teams *luminateam.Manager,
	hub *EventHub, shutdown *ShutdownSignal, memoryFactory agent.MemoryFabricFactory, upgrader websocket.Upgrader,
	router *ClusterRouter) *DaemonServer {
	server := &DaemonServer{opts: opts.DaemonOptions, token: string(token), manager: sessions, teamManager: teams,
		eventHub: hub, shutdown: shutdown, memoryFactory: memoryFactory, upgrader: upgrader, clusterRouter: router}
	if router != nil {
		router.SetHandler(server.dispatchLocal)
	}
	return server
}

func ProvideHTTPHandler(cfg config.Config, server *DaemonServer, authenticator *JWTAuthenticator,
	router *ClusterRouter) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ws", server.handleWS)
	mux.HandleFunc("/v1/a2a/ws", server.handleA2AWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, request *http.Request) {
		if router != nil {
			ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
			defer cancel()
			if err := router.Ready(ctx); err != nil {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("ready"))
	})
	if cfg.UsesClusterRuntime() {
		return authenticator.Middleware(mux)
	}
	return mux
}

func ProvideHertzEngine(cfg config.Config, listener net.Listener, handler http.Handler) *server.Hertz {
	exitWait := 30 * time.Second
	if cfg.ClusterShutdownGraceSeconds > 0 {
		exitWait = time.Duration(cfg.ClusterShutdownGraceSeconds) * time.Second
	}
	engine := server.New(server.WithListener(listener), server.WithTransport(standard.NewTransporter),
		server.WithExitWaitTime(exitWait), server.WithDisablePrintRoute(true))
	engine.Any("/*path", adaptor.HertzHandler(handler))
	return engine
}
