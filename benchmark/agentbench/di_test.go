package agentbench

import (
	"context"

	"LuminaCode/config"
	"LuminaCode/memory"
)

type memoryFabricFactoryFunc func(context.Context, config.Config, bool, memory.APIUsageObserver) (*memory.Fabric, error)

func (f memoryFabricFactoryFunc) Open(ctx context.Context, cfg config.Config, startWorkers bool) (*memory.Fabric, error) {
	return f(ctx, cfg, startWorkers, nil)
}

func (f memoryFabricFactoryFunc) OpenWithUsageObserver(ctx context.Context, cfg config.Config, startWorkers bool,
	observer memory.APIUsageObserver) (*memory.Fabric, error) {
	return f(ctx, cfg, startWorkers, observer)
}
