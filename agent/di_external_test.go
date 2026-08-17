package agent_test

import (
	"LuminaCode/agent"
	"LuminaCode/config"
)

func newTestQueryEngine(cfg config.Config) *agent.QueryEngine {
	factory := agent.NewQueryEngineFactory(agent.NewConfiguredMemoryFabricFactory())
	return factory.Create(cfg)
}
