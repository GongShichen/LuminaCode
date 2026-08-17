package main

import (
	"LuminaCode/agent"
	"LuminaCode/benchmark/agentbench"
)

type BenchmarkRuntime struct {
	AgentRunner             agentbench.AgentRunner
	LongMemEvalAnswerRunner agentbench.LongMemEvalAnswerRunner
	MemoryFactory           agent.MemoryFabricFactory
}

func newBenchmarkRuntime(agentRunner agentbench.AgentRunner, answerRunner agentbench.LongMemEvalAnswerRunner,
	memoryFactory agent.MemoryFabricFactory) *BenchmarkRuntime {
	return &BenchmarkRuntime{AgentRunner: agentRunner, LongMemEvalAnswerRunner: answerRunner, MemoryFactory: memoryFactory}
}
