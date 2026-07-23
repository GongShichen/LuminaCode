package test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"LuminaCode/config"
	"LuminaCode/memory"
)

func enableTestMemoryFabric(cfg *config.Config, root string) {
	cfg.LongTermMemoryEnabled = true
	cfg.MemoryBackend = "fabric"
	cfg.MemoryPath = filepath.Join(root, "memory-fabric")
	cfg.MemoryRemoteProcessing = "off"
	cfg.MemoryEmbeddingEnabled = false
}

func openSeededTestMemoryFabric(t *testing.T, cfg config.Config, events []memory.RawEvent) *memory.Fabric {
	t.Helper()
	options := memory.DefaultFabricOptions(cfg.MemoryPath)
	options.StartWorkers = false
	options.RemoteProcessing = memory.RemoteProcessingOff
	fabric, err := memory.OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	for index := range events {
		if events[index].Space == "" {
			events[index].Space = memory.ProjectSpace(cfg.ProjectRoot())
		}
		if events[index].Actor == "" {
			events[index].Actor = "user"
		}
		if events[index].OccurredAt.IsZero() {
			events[index].OccurredAt = time.Now().UTC().Add(time.Duration(index) * time.Millisecond)
		}
	}
	if _, err := fabric.AppendEvents(context.Background(), events, memory.IngestOptions{
		SemanticPolicy: memory.SemanticDurableOnly,
	}); err != nil {
		_ = fabric.Close()
		t.Fatal(err)
	}
	return fabric
}
