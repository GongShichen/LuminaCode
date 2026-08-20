package test

import (
	"LuminaCode/agent"
	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/memory"
)

func newTestMemoryFactory() agent.MemoryFabricFactory {
	return agent.NewConfiguredMemoryFabricFactory()
}

func newTestQueryEngineFactory() agent.QueryEngineFactory {
	return agent.NewQueryEngineFactory(newTestMemoryFactory())
}

func newTestQueryEngine(cfg config.Config) *agent.QueryEngine {
	return newTestQueryEngineFactory().Create(cfg, cluster.RuntimeIdentity{TenantID: "local"})
}

func newTestCoreExecutionEngine(cfg config.Config) *agent.CoreExecutionEngine {
	return agent.NewCoreExecutionEngine(cfg, cluster.RuntimeIdentity{TenantID: "local"}, newTestMemoryFactory())
}

func newTestCoreExecutionEngineWithMemoryEngine(cfg config.Config, engine memory.Engine) *agent.CoreExecutionEngine {
	return agent.NewCoreExecutionEngineWithMemoryEngine(cfg, cluster.RuntimeIdentity{TenantID: "local"}, newTestMemoryFactory(), engine)
}
