package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/harness"
	"LuminaCode/session"
	luminateam "LuminaCode/team"

	"github.com/gorilla/websocket"
)

type NormalizedDaemonOptions struct {
	DaemonOptions
}

type AuthToken string

type daemonMemoryPreflight struct{}

func NormalizeDaemonOptions(opts DaemonOptions) NormalizedDaemonOptions {
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
	return NormalizedDaemonOptions{DaemonOptions: opts}
}

func ProvideDaemonConfig(opts NormalizedDaemonOptions) config.Config {
	return opts.Config
}

func ValidateDaemonMemory(ctx context.Context, cfg config.Config, factory agent.MemoryFabricFactory) (daemonMemoryPreflight, error) {
	if !cfg.LongTermMemoryEnabled {
		return daemonMemoryPreflight{}, nil
	}
	fabric, err := factory.Open(ctx, cfg, false)
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
	return EndpointInfo{
		PID:       os.Getpid(),
		Host:      opts.Host,
		Port:      actualPort,
		AuthToken: string(token),
		StartedAt: nowRFC3339(),
		URL:       fmt.Sprintf("ws://%s:%d/v1/ws", opts.Host, actualPort),
	}
}

func ProvideEventHub() (*EventHub, func()) {
	hub := NewEventHub()
	return hub, hub.Close
}

func ProvideEventEmitter(hub *EventHub) EventEmitter {
	return hub.Publish
}

func ProvideSessionStore(cfg config.Config) *session.Store {
	return session.NewStore(cfg.SessionDir)
}

func ProvideSessionManager(cfg config.Config, store *session.Store, factory *SessionRuntimeFactory) (*SessionManager, func()) {
	manager := NewSessionManager(cfg, store, factory)
	return manager, manager.Shutdown
}

func ProvideTeamManager(cfg config.Config, engineFactory agent.QueryEngineFactory, sessions *SessionManager,
	emit EventEmitter) (*luminateam.Manager, func()) {
	var manager *luminateam.Manager
	manager = luminateam.NewManager(cfg, engineFactory, func(parentSessionID, eventType string, payload any) {
		handleTeamRuntimeEvent(sessions, manager, emit, parentSessionID, eventType, payload)
	}, nil)
	manager.UseJournalPersistence(true)
	return manager, manager.Shutdown
}

func handleTeamRuntimeEvent(sessions *SessionManager, teams *luminateam.Manager, emit EventEmitter,
	parentSessionID, eventType string, payload any) {
	if controller, err := sessions.Get(parentSessionID); err == nil {
		if teamSessionID := teamSessionIDFromPayload(payload); teamSessionID != "" {
			if err := controller.AppendTeamEvent(context.Background(), teamSessionID, durableTeamEventType(eventType), payload); err != nil {
				slog.Warn("append team runtime event", "session_id", parentSessionID, "team_session_id", teamSessionID, "error", err)
			}
			if shouldCheckpointTeamEvent(eventType) {
				if teamSession, getErr := teams.Get(teamSessionID); getErr == nil {
					checkpoint := teamSession.ExportRuntimeCheckpoint()
					if appendErr := controller.AppendTeamEvent(context.Background(), teamSessionID, harness.EventTeamRuntimeCheckpointed, checkpoint); appendErr != nil {
						slog.Warn("append team runtime checkpoint", "session_id", parentSessionID, "team_session_id", teamSessionID, "error", appendErr)
					}
				}
			}
		}
	}
	emit(PushEvent{Type: "event", SessionID: parentSessionID, Seq: time.Now().UnixNano(), Event: map[string]any{
		"type": eventType, "payload": payload,
	}})
}

func ProvideWebSocketUpgrader() websocket.Upgrader {
	return websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
		return r.Host == r.URL.Host || strings.HasPrefix(r.RemoteAddr, "127.0.0.1:") || strings.HasPrefix(r.RemoteAddr, "[::1]:")
	}}
}

func NewDaemonServer(opts NormalizedDaemonOptions, token AuthToken, sessions *SessionManager, teams *luminateam.Manager,
	hub *EventHub, shutdown *ShutdownSignal, memoryFactory agent.MemoryFabricFactory, upgrader websocket.Upgrader) *DaemonServer {
	return &DaemonServer{opts: opts.DaemonOptions, token: string(token), manager: sessions, teamManager: teams,
		eventHub: hub, shutdown: shutdown, memoryFactory: memoryFactory, upgrader: upgrader}
}

func ProvideHTTPHandler(server *DaemonServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ws", server.handleWS)
	mux.HandleFunc("/v1/a2a/ws", server.handleA2AWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	return mux
}

func ProvideHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler}
}
