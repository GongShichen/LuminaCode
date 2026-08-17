//go:build wireinject

package main

import (
	"LuminaCode/agent"
	"LuminaCode/benchmark/agentbench"

	"github.com/google/wire"
)

func initializeBenchmarkRuntime() *BenchmarkRuntime {
	wire.Build(
		agent.NewConfiguredMemoryFabricFactory,
		wire.Bind(new(agent.MemoryFabricFactory), new(*agent.ConfiguredMemoryFabricFactory)),
		agent.NewQueryEngineFactory,
		wire.Bind(new(agent.QueryEngineFactory), new(*agent.DefaultQueryEngineFactory)),
		agentbench.NewHeadlessAgentRunner,
		wire.Bind(new(agentbench.AgentRunner), new(*agentbench.HeadlessAgentRunner)),
		agentbench.NewLongMemEvalAnswerRunner,
		newBenchmarkRuntime,
	)
	return nil
}
