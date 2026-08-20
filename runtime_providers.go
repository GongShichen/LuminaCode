package main

import (
	"context"
	"errors"
	"fmt"

	"LuminaCode/agent"
	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/memory"
	"LuminaCode/session"
)

type memoryPreflight struct{}

func provideMemoryPreflight(ctx context.Context, cfg config.Config, factory agent.MemoryFabricFactory) (memoryPreflight, error) {
	if !cfg.LongTermMemoryEnabled {
		return memoryPreflight{}, nil
	}
	fabric, err := factory.Open(ctx, cfg, agent.MemoryOpenOptions{Identity: cluster.RuntimeIdentity{TenantID: session.LocalTenantID}})
	if err != nil {
		return memoryPreflight{}, fmt.Errorf("open Memory Fabric: %w", err)
	}
	if fabric == nil {
		return memoryPreflight{}, errors.New("Memory Fabric is required")
	}
	defer fabric.Close()
	if _, err := fabric.Doctor(ctx); err != nil {
		return memoryPreflight{}, fmt.Errorf("check Memory Fabric: %w", err)
	}
	return memoryPreflight{}, nil
}

type PromptRuntime struct {
	Engine *agent.QueryEngine
	Store  *session.Store
}

func providePromptEngine(cfg config.Config, memoryFactory agent.MemoryFabricFactory) (*agent.QueryEngine, func()) {
	core := agent.NewCoreExecutionEngine(cfg, cluster.RuntimeIdentity{TenantID: session.LocalTenantID}, memoryFactory)
	engine := agent.NewQueryEngine(cfg, core)
	return engine, engine.Shutdown
}

func provideSessionStore(cfg config.Config) *session.Store {
	return session.NewStore(cfg.SessionDir)
}

func newPromptRuntime(engine *agent.QueryEngine, store *session.Store) *PromptRuntime {
	return &PromptRuntime{Engine: engine, Store: store}
}

type MemoryCommandRuntime struct {
	Fabric memory.FabricEngine
}

func provideMemoryCommandFabric(ctx context.Context, cfg config.Config, factory agent.MemoryFabricFactory) (memory.FabricEngine, func(), error) {
	fabric, err := factory.Open(ctx, cfg, agent.MemoryOpenOptions{Identity: cluster.RuntimeIdentity{TenantID: session.LocalTenantID}})
	if err != nil {
		return nil, nil, err
	}
	if fabric == nil {
		return nil, nil, errors.New("Memory Fabric is unavailable")
	}
	return fabric, func() { _ = fabric.Close() }, nil
}

func newMemoryCommandRuntime(fabric memory.FabricEngine) *MemoryCommandRuntime {
	return &MemoryCommandRuntime{Fabric: fabric}
}

type RuntimeInspectionOptions struct {
	Config    config.Config
	SessionID string
}

type runtimeSessionID string

type RuntimeInspectionRuntime struct {
	Journal  session.RuntimeStore
	Engine   *agent.QueryEngine
	Assembly *agent.RuntimeAssembly
}

func provideRuntimeInspectionConfig(opts RuntimeInspectionOptions) config.Config { return opts.Config }
func provideRuntimeSessionID(opts RuntimeInspectionOptions) runtimeSessionID {
	return runtimeSessionID(opts.SessionID)
}

func provideRuntimeJournal(ctx context.Context, cfg config.Config, sessionID runtimeSessionID) (session.RuntimeStore, func(), error) {
	journal, err := session.OpenRuntimeJournal(ctx, cfg.SessionDir, string(sessionID))
	if err != nil {
		return nil, nil, err
	}
	return journal, func() { _ = journal.Close() }, nil
}

func provideRuntimeAssembly(sessionID runtimeSessionID, journal session.RuntimeStore,
	engine *agent.QueryEngine) (*agent.RuntimeAssembly, error) {
	assembly, err := agent.NewRuntimeAssembly(string(sessionID), journal, engine.CoreEngine.Registry)
	if err != nil {
		return nil, err
	}
	if err := engine.CoreEngine.AttachRuntime(assembly); err != nil {
		_ = assembly.Close()
		return nil, err
	}
	return assembly, nil
}

func newRuntimeInspectionRuntime(journal session.RuntimeStore, engine *agent.QueryEngine,
	assembly *agent.RuntimeAssembly) *RuntimeInspectionRuntime {
	return &RuntimeInspectionRuntime{Journal: journal, Engine: engine, Assembly: assembly}
}

type SessionStoreOptions struct {
	Config  config.Config
	Mutable bool
}

type SessionStoreRuntime struct {
	Store *session.Store
}

func provideConfiguredSessionStore(opts SessionStoreOptions) *session.Store {
	if opts.Mutable {
		return session.NewStore(opts.Config.SessionDir)
	}
	return session.NewInspectionStore(opts.Config.SessionDir)
}

func newSessionStoreRuntime(store *session.Store) *SessionStoreRuntime {
	return &SessionStoreRuntime{Store: store}
}
