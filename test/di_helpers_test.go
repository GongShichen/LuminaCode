package test

import (
	"LuminaCode/agent"
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
	return newTestQueryEngineFactory().Create(cfg)
}

func newTestCoreExecutionEngine(cfg config.Config) *agent.CoreExecutionEngine {
	return agent.NewCoreExecutionEngine(cfg, newTestMemoryFactory())
}

func newTestCoreExecutionEngineWithMemoryEngine(cfg config.Config, engine memory.Engine) *agent.CoreExecutionEngine {
	return agent.NewCoreExecutionEngineWithMemoryEngine(cfg, newTestMemoryFactory(), engine)
}
