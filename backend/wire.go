//go:build wireinject

package backend

import (
	"context"

	"LuminaCode/agent"
	"LuminaCode/config"

	"github.com/google/wire"
)

func initializeDaemonMemoryPreflight(ctx context.Context, cfg config.Config) (daemonMemoryPreflight, error) {
	wire.Build(agent.NewConfiguredMemoryFabricFactory,
		wire.Bind(new(agent.MemoryFabricFactory), new(*agent.ConfiguredMemoryFabricFactory)), ValidateDaemonMemory)
	return daemonMemoryPreflight{}, nil
}

func InitializeDaemonApp(ctx context.Context, opts DaemonOptions) (*DaemonApp, func(), error) {
	wire.Build(
		agent.NewConfiguredMemoryFabricFactory,
		wire.Bind(new(agent.MemoryFabricFactory), new(*agent.ConfiguredMemoryFabricFactory)),
		agent.NewQueryEngineFactory,
		wire.Bind(new(agent.QueryEngineFactory), new(*agent.DefaultQueryEngineFactory)),
		NormalizeDaemonOptions,
		ProvideDaemonConfig,
		ValidateDaemonMemory,
		ProvideAuthToken,
		ProvideListener,
		ProvideEndpointInfo,
		ProvideEventHub,
		ProvideRedisRuntime,
		NewEventRelay,
		ProvideEventEmitter,
		ProvideSessionStore,
		ProvideSessionRepository,
		NewSessionRuntimeFactory,
		ProvideSessionManager,
		ProvideTeamManager,
		NewClusterRouter,
		NewJWTAuthenticator,
		NewShutdownSignal,
		ProvideWebSocketUpgrader,
		NewDaemonServer,
		ProvideHTTPHandler,
		ProvideHertzEngine,
		NewDaemonApp,
	)
	return nil, nil, nil
}
