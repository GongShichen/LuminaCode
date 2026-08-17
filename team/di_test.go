package team

import "LuminaCode/agent"

func newTestQueryEngineFactory() agent.QueryEngineFactory {
	return agent.NewQueryEngineFactory(agent.NewConfiguredMemoryFabricFactory())
}
