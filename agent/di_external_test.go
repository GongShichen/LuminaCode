package agent_test

import (
	"LuminaCode/agent"
	"LuminaCode/cluster"
	"LuminaCode/config"
)

func newTestQueryEngine(cfg config.Config) *agent.QueryEngine {
	factory := agent.NewQueryEngineFactory(agent.NewConfiguredMemoryFabricFactory())
	return factory.Create(cfg, cluster.RuntimeIdentity{TenantID: "local"})
}
