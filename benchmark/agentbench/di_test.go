package agentbench

import (
	"context"

	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/memory"
)

type memoryFabricFactoryFunc func(context.Context, config.Config, agent.MemoryOpenOptions) (memory.FabricEngine, error)

func (f memoryFabricFactoryFunc) Open(ctx context.Context, cfg config.Config,
	options agent.MemoryOpenOptions) (memory.FabricEngine, error) {
	return f(ctx, cfg, options)
}
