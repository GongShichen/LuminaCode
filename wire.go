//go:build wireinject

package main

import (
	"context"

	"LuminaCode/agent"
	"LuminaCode/config"

	"github.com/google/wire"
)

func initializeMemoryPreflight(ctx context.Context, cfg config.Config) (memoryPreflight, error) {
	wire.Build(agent.NewConfiguredMemoryFabricFactory,
		wire.Bind(new(agent.MemoryFabricFactory), new(*agent.ConfiguredMemoryFabricFactory)), provideMemoryPreflight)
	return memoryPreflight{}, nil
}

func initializePromptRuntime(cfg config.Config) (*PromptRuntime, func(), error) {
	wire.Build(agent.NewConfiguredMemoryFabricFactory,
		wire.Bind(new(agent.MemoryFabricFactory), new(*agent.ConfiguredMemoryFabricFactory)),
		providePromptEngine, provideSessionStore, newPromptRuntime)
	return nil, nil, nil
}

func initializeMemoryCommandRuntime(ctx context.Context, cfg config.Config) (*MemoryCommandRuntime, func(), error) {
	wire.Build(agent.NewConfiguredMemoryFabricFactory,
		wire.Bind(new(agent.MemoryFabricFactory), new(*agent.ConfiguredMemoryFabricFactory)),
		provideMemoryCommandFabric, newMemoryCommandRuntime)
	return nil, nil, nil
}

func initializeRuntimeInspectionRuntime(ctx context.Context, opts RuntimeInspectionOptions) (*RuntimeInspectionRuntime, func(), error) {
	wire.Build(agent.NewConfiguredMemoryFabricFactory,
		wire.Bind(new(agent.MemoryFabricFactory), new(*agent.ConfiguredMemoryFabricFactory)),
		provideRuntimeInspectionConfig, provideRuntimeSessionID, provideRuntimeJournal,
		providePromptEngine, provideRuntimeAssembly, newRuntimeInspectionRuntime)
	return nil, nil, nil
}

func initializeSessionStoreRuntime(opts SessionStoreOptions) *SessionStoreRuntime {
	wire.Build(provideConfiguredSessionStore, newSessionStoreRuntime)
	return nil
}
