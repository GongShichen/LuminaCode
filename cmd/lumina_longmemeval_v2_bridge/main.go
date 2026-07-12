package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"LuminaCode/agent"
	"LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/longmemory"
)

type request struct {
	ID         string         `json:"id"`
	Op         string         `json:"op"`
	Trajectory map[string]any `json:"trajectory,omitempty"`
	Query      string         `json:"query,omitempty"`
	QueryImage string         `json:"query_image,omitempty"`
}

type response struct {
	ID      string         `json:"id"`
	OK      bool           `json:"ok"`
	Context string         `json:"context,omitempty"`
	Error   string         `json:"error,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"`
}

func main() {
	storePath := flag.String("store", "", "SQLite memory store path")
	projectRoot := flag.String("project-root", "", "stable project scope root")
	flag.Parse()
	if strings.TrimSpace(*storePath) == "" || strings.TrimSpace(*projectRoot) == "" {
		fmt.Fprintln(os.Stderr, "--store and --project-root are required")
		os.Exit(2)
	}
	if err := os.MkdirAll(filepath.Dir(*storePath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg := bridgeConfig(*projectRoot, *storePath)
	ctx := context.Background()
	backfill := newEmbeddingBackfillWorker(ctx, cfg, flushEmbeddingBacklog)
	defer backfill.Close()
	ingestion := newTrajectoryIngestionPool(ctx, cfg, backfill, configuredIngestionWorkers(), ingestTrajectory)
	defer ingestion.Close()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			_ = encoder.Encode(response{OK: false, Error: err.Error()})
			continue
		}
		result := handle(ctx, cfg, ingestion, backfill, req)
		if err := encoder.Encode(result); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if req.Op == "close" {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func bridgeConfig(projectRoot, storePath string) config.Config {
	cfg := config.NewConfigForCWD(projectRoot)
	cfg.CWD = projectRoot
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = storePath
	cfg.SessionMemoryEnabled = false
	cfg.MemoryBackgroundExtractionEnabled = false
	cfg.HarnessMode = "longmemeval-v2"
	if value := strings.TrimSpace(os.Getenv("LUMINA_MEMORY_API_KEY")); value != "" {
		cfg.APIKey = value
	}
	if value := strings.TrimSpace(os.Getenv("LUMINA_MEMORY_API_BASE_URL")); value != "" {
		cfg.APIBaseURL = value
	}
	if value := strings.TrimSpace(os.Getenv("LUMINA_MEMORY_API_MODEL")); value != "" {
		cfg.APIModel = value
	}
	if value := strings.TrimSpace(os.Getenv("LUMINA_MEMORY_API_TYPE")); value != "" {
		cfg.APIType = value
	}
	return cfg
}

func handle(ctx context.Context, cfg config.Config, ingestion *trajectoryIngestionPool, backfill *embeddingBackfillWorker, req request) response {
	started := time.Now()
	result := response{ID: req.ID, OK: true, Meta: map[string]any{}}
	switch req.Op {
	case "insert":
		trajectoryID := strings.TrimSpace(stringField(req.Trajectory, "id"))
		if trajectoryID == "" {
			return failed(req.ID, "trajectory id is required")
		}
		if err := ingestion.Enqueue(ctx, req.Trajectory); err != nil {
			return failed(req.ID, err.Error())
		}
		result.Meta["trajectory_id"] = trajectoryID
		result.Meta["queued"] = true
	case "query":
		query := strings.TrimSpace(req.Query)
		if query == "" {
			return failed(req.ID, "query is required")
		}
		ingestionStarted := time.Now()
		ingestionStats, err := ingestion.Drain(ctx)
		if err != nil {
			return failed(req.ID, "flush raw memory ingestion: "+err.Error())
		}
		result.Meta["ingestion_completed"] = ingestionStats.Completed
		result.Meta["ingestion_messages"] = ingestionStats.Messages
		result.Meta["ingestion_items"] = ingestionStats.Ingested
		result.Meta["ingestion_flush_ms"] = time.Since(ingestionStarted).Milliseconds()
		maintenanceStarted := time.Now()
		embedded, err := backfill.Drain(ctx)
		if err != nil {
			return failed(req.ID, "flush memory embeddings: "+err.Error())
		}
		result.Meta["embedding_backfill_items"] = embedded
		result.Meta["embedding_backfill_ms"] = time.Since(maintenanceStarted).Milliseconds()
		state := agent.NewAgentState()
		state.MemorySessionID = "longmemeval-v2-query"
		state.MemoryAgentID = "main"
		state.MemoryAgentType = "main"
		state.MemoryQueryTime = time.Now().UTC()
		factory := func(ctx context.Context, model string) (api.LLMClient, error) {
			return agent.CreateConfiguredLLMClient(cfg, model, 1024, nil, api.DefaultRetryConfigPtr())
		}
		recalls := agent.RunMemoryRecallWithRuntime(ctx, cfg, &state, query, factory)
		parts := make([]string, 0, len(recalls))
		for _, recall := range recalls {
			if text := strings.TrimSpace(recall.Content); text != "" {
				parts = append(parts, text)
			}
		}
		result.Context = strings.Join(parts, "\n\n")
		result.Meta["recalls"] = len(recalls)
		result.Meta["query_image_supplied"] = strings.TrimSpace(req.QueryImage) != ""
	case "ping":
		result.Meta["status"] = "ready"
	case "close":
		if _, err := ingestion.Drain(ctx); err != nil {
			return failed(req.ID, "flush raw memory ingestion: "+err.Error())
		}
		result.Meta["status"] = "closed"
	default:
		return failed(req.ID, "unsupported operation: "+req.Op)
	}
	result.Meta["duration_ms"] = time.Since(started).Milliseconds()
	return result
}

func flushEmbeddingBacklog(ctx context.Context, cfg config.Config) (int, error) {
	if !cfg.MemoryEmbeddingEnabled {
		return 0, nil
	}
	local, err := longmemory.SharedLocalEmbedder(cfg.MemoryEmbeddingModel, cfg.MemoryEmbeddingModelDir)
	if err != nil {
		return 0, err
	}
	embedder := longmemory.SharedEmbeddingScheduler(local, longmemory.EmbeddingSchedulerOptions{
		BatchSize: cfg.MemoryEmbeddingBatchSize, BatchWait: time.Duration(cfg.MemoryEmbeddingBatchWaitMS) * time.Millisecond,
		QueryCacheEntries: cfg.MemoryEmbeddingQueryCacheEntries,
		ExecutionTimeout:  time.Duration(cfg.MemoryEmbeddingExecutionTimeout * float64(time.Second)),
	})
	store, err := longmemory.OpenWithBusyTimeout(ctx, cfg.LongTermMemoryStore, 15*time.Minute)
	if err != nil {
		return 0, err
	}
	defer store.Close()
	total := 0
	for {
		maintenance, runErr := store.RunMaintenance(ctx, embedder, max(cfg.MemoryEmbeddingBatchSize*4, 128))
		if runErr != nil {
			return total, runErr
		}
		processed := maintenance.Embedded + maintenance.ChunkEmbedded + maintenance.AtomEmbedded + maintenance.SessionEmbedded
		total += processed
		if processed == 0 {
			return total, nil
		}
	}
}

func failed(id, message string) response {
	return response{ID: id, OK: false, Error: message}
}

func stringField(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return text
}
