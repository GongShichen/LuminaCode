package backend

import (
	"context"
	"strings"
	"time"

	"LuminaCode/agent"
	"LuminaCode/apppaths"
	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/session"
)

// SessionRuntimeFactory is the runtime boundary for dependencies whose
// identity and working directory are only known after an RPC request.
type SessionRuntimeFactory struct {
	baseConfig    config.Config
	repository    session.SessionRepository
	engineFactory agent.QueryEngineFactory
	emit          EventEmitter
	assemble      func(string, session.RuntimeStore, *agent.QueryEngine) (*agent.RuntimeAssembly, error)
}

func NewSessionRuntimeFactory(cfg config.Config, repository session.SessionRepository, engineFactory agent.QueryEngineFactory, emit EventEmitter) *SessionRuntimeFactory {
	return &SessionRuntimeFactory{
		baseConfig: cfg, repository: repository, engineFactory: engineFactory, emit: emit,
		assemble: func(sessionID string, journal session.RuntimeStore, engine *agent.QueryEngine) (*agent.RuntimeAssembly, error) {
			return agent.NewRuntimeAssembly(sessionID, journal, engine.CoreEngine.Registry)
		},
	}
}

func (f *SessionRuntimeFactory) Create(ctx context.Context, identity cluster.RuntimeIdentity, sessionID, cwd string, state *agent.AgentState,
	createIfMissing bool) (*SessionController, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := f.baseConfig
	if strings.TrimSpace(cwd) != "" && cwd != cfg.CWD {
		cfg = config.NewConfigForCWD(cwd)
		applyPinnedDaemonConfig(&cfg, f.baseConfig)
	}
	if err := apppaths.EnsureProjectManifest(cfg.ProjectPaths, time.Now()); err != nil {
		return nil, err
	}
	tenantID := strings.TrimSpace(identity.TenantID)
	if tenantID == "" {
		tenantID = session.LocalTenantID
	}
	runtimeLoad, err := f.repository.OpenRuntime(ctx, tenantID, sessionID,
		session.RuntimeOpenOptions{CreateIfMissing: createIfMissing, FenceToken: identity.FenceToken, CWD: cfg.CWD})
	if err != nil {
		return nil, err
	}
	journal := runtimeLoad.Journal
	engine := f.engineFactory.Create(cfg, identity)
	cleanup := func() {
		if engine != nil {
			if engine.CoreEngine != nil && engine.CoreEngine.Runtime != nil {
				_ = engine.CoreEngine.Runtime.Close()
			}
			engine.Shutdown()
		}
		if journal != nil {
			_ = journal.Close()
		}
	}
	if state == nil {
		state = runtimeLoad.State
	}
	if recovery := runtimeLoad.SkillRecovery; recovery != nil && engine.CoreEngine != nil {
		engine.CoreEngine.ImportSkillRecoverySnapshot(recovery)
		engine.CoreEngine.MarkSkillHistoryCompacted("main")
	}
	if tasks := runtimeLoad.Tasks; len(tasks) > 0 && engine.CoreEngine != nil && engine.CoreEngine.TaskRuntime != nil {
		engine.CoreEngine.TaskRuntime.ImportSnapshot(tasks)
	}
	if engine.CoreEngine != nil {
		assembly, assemblyErr := f.assemble(sessionID, journal, engine)
		if assemblyErr != nil {
			cleanup()
			return nil, assemblyErr
		}
		if attachErr := engine.CoreEngine.AttachRuntime(assembly); attachErr != nil {
			_ = assembly.Close()
			cleanup()
			return nil, attachErr
		}
	}
	controller := NewSessionController(identity, sessionID, cfg, engine, state, f.repository, journal, f.emit)
	messageCount, turnCount := 0, 0
	if state != nil {
		messageCount = len(state.Messages)
		turnCount = state.TurnCount
	}
	if err := f.repository.UpdateMetaProjection(ctx, tenantID, sessionID, identity.FenceToken, messageCount, turnCount); err != nil {
		cleanup()
		return nil, err
	}
	controller.Mount()
	return controller, nil
}
