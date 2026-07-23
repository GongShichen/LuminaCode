package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/viant/sqlite-vec/engine"
	"github.com/viant/sqlite-vec/vec"
	_ "modernc.org/sqlite"
)

type RemoteProcessingPolicy string

const (
	RemoteProcessingOff      RemoteProcessingPolicy = "off"
	RemoteProcessingRedacted RemoteProcessingPolicy = "redacted"
	RemoteProcessingAllow    RemoteProcessingPolicy = "allow"
)

type FabricOptions struct {
	Dir                  string
	LedgerPath           string
	IndexPath            string
	Compiler             SemanticCompiler
	Planner              SemanticPlanner
	Adjudicator          ConflictAdjudicator
	Vectorizer           Vectorizer
	RetrievalEncoder     RetrievalEncoder
	RetrievalSidecarPath string
	RemoteProcessing     RemoteProcessingPolicy
	// CompileBatchTokens is the hard estimated token budget for the complete
	// compiler request, including prompt, schema, JSON, neighborhood and events.
	CompileBatchTokens       int
	CompileOutputTokens      int
	CompileMaxNodes          int
	CompileMaxCalls          int
	CompileMaxSources        int
	CompileSourcesPerCall    int
	CompileSourcesPerSession int
	CompileSourceRunes       int
	CompileCallTimeout       time.Duration
	CompileConcurrency       int
	WorkerCount              int
	EmbeddingBatchSize       int
	CandidateLimit           int
	MaxEvidence              int
	TargetContextTokens      int
	MaxContextTokens         int
	SearchLatencyBudget      time.Duration
	WorkerPollInterval       time.Duration
	StartWorkers             bool
	Clock                    func() time.Time
	UsageObserver            APIUsageObserver
}

func (f *Fabric) observeAPIUsage(ctx context.Context, event APIUsageEvent) error {
	if f.options.UsageObserver == nil {
		return nil
	}
	if event.RecordedAt.IsZero() {
		event.RecordedAt = f.now()
	}
	return f.options.UsageObserver(ctx, event)
}

func DefaultFabricOptions(dir string) FabricOptions {
	return FabricOptions{
		Dir:                      dir,
		RemoteProcessing:         RemoteProcessingRedacted,
		CompileBatchTokens:       10_000,
		CompileOutputTokens:      2_560,
		CompileMaxNodes:          16,
		CompileMaxCalls:          4,
		CompileMaxSources:        32,
		CompileSourcesPerCall:    16,
		CompileSourcesPerSession: 2,
		CompileSourceRunes:       900,
		CompileCallTimeout:       55 * time.Second,
		CompileConcurrency:       2,
		WorkerCount:              3,
		EmbeddingBatchSize:       32,
		CandidateLimit:           64,
		MaxEvidence:              24,
		TargetContextTokens:      2_500,
		MaxContextTokens:         6_000,
		SearchLatencyBudget:      2 * time.Second,
		WorkerPollInterval:       250 * time.Millisecond,
		StartWorkers:             true,
		Clock:                    func() time.Time { return time.Now().UTC() },
	}
}

type Fabric struct {
	options FabricOptions
	ledger  *sql.DB
	index   *sql.DB
	sidecar *sql.DB

	workerCtx     context.Context
	workerCancel  context.CancelFunc
	workerWG      sync.WaitGroup
	closeOnce     sync.Once
	jobWake       chan struct{}
	writeMu       sync.Mutex
	outboxMu      sync.Mutex
	conflictLocks sync.Map
	sidecarSyncMu sync.Mutex
	sidecarMu     sync.RWMutex
	remoteMu      sync.RWMutex
	remoteErr     error
}

var _ Engine = (*Fabric)(nil)

func (f *Fabric) RetrievalSidecarEnabled() bool {
	return f != nil && f.sidecar != nil && f.options.RetrievalEncoder != nil
}

var (
	vectorFunctionsOnce sync.Once
	vectorFunctionsErr  error
	vectorModuleOnce    sync.Once
	vectorModuleErr     error
)

func registerFabricVectorFunctions() error {
	vectorFunctionsOnce.Do(func() {
		vectorFunctionsErr = engine.RegisterVectorFunctions(nil)
	})
	return vectorFunctionsErr
}

func registerFabricVectorModule(index *sql.DB) error {
	vectorModuleOnce.Do(func() {
		vectorModuleErr = vec.Register(index)
	})
	return vectorModuleErr
}

func OpenFabric(ctx context.Context, options FabricOptions) (*Fabric, error) {
	options = normalizeFabricOptions(options)
	if err := os.MkdirAll(options.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create memory fabric directory: %w", err)
	}
	_ = os.Chmod(options.Dir, 0o700)

	ledger, err := openFabricDB(options.LedgerPath, false)
	if err != nil {
		return nil, fmt.Errorf("open memory ledger: %w", err)
	}
	if err := registerFabricVectorFunctions(); err != nil {
		_ = ledger.Close()
		return nil, fmt.Errorf("register memory vector functions: %w", err)
	}
	index, err := openFabricDB(options.IndexPath, true)
	if err != nil {
		_ = ledger.Close()
		return nil, fmt.Errorf("open memory index: %w", err)
	}
	if err := registerFabricVectorModule(index); err != nil {
		_ = index.Close()
		_ = ledger.Close()
		return nil, fmt.Errorf("register memory vector table: %w", err)
	}

	fabric := &Fabric{options: options, ledger: ledger, index: index, jobWake: make(chan struct{}, 1)}
	if options.RetrievalEncoder != nil {
		sidecar, sidecarErr := openFabricDB(options.RetrievalSidecarPath, false)
		if sidecarErr != nil {
			_ = index.Close()
			_ = ledger.Close()
			return nil, fmt.Errorf("open BGE retrieval sidecar: %w", sidecarErr)
		}
		fabric.sidecar = sidecar
	}
	if err := fabric.migrate(ctx); err != nil {
		_ = fabric.Close()
		return nil, err
	}
	if options.Vectorizer != nil {
		// Vector rows are derived data and must never cross model generations.
		// Existing ledgers and lexical indexes remain reusable; the BGE sidecar
		// is rebuilt from durable raw events when retrieval first synchronizes.
		if err := fabric.ensureVectorModel(ctx); err != nil {
			_ = fabric.Close()
			return nil, err
		}
	}
	if err := fabric.recoverInterruptedWork(ctx); err != nil {
		_ = fabric.Close()
		return nil, err
	}
	_ = os.Chmod(options.LedgerPath, 0o600)
	_ = os.Chmod(options.IndexPath, 0o600)
	if fabric.sidecar != nil {
		if err := fabric.migrateRetrievalSidecar(ctx); err != nil {
			_ = fabric.Close()
			return nil, err
		}
		_ = os.Chmod(options.RetrievalSidecarPath, 0o600)
	}
	if options.StartWorkers {
		fabric.startWorkers()
	}
	return fabric, nil
}

func normalizeFabricOptions(options FabricOptions) FabricOptions {
	defaults := DefaultFabricOptions(options.Dir)
	if strings.TrimSpace(options.Dir) == "" {
		options.Dir = filepath.Join(".", "memory-fabric")
	}
	options.Dir = filepath.Clean(options.Dir)
	if strings.TrimSpace(options.LedgerPath) == "" {
		options.LedgerPath = filepath.Join(options.Dir, "ledger.sqlite")
	}
	if strings.TrimSpace(options.IndexPath) == "" {
		options.IndexPath = filepath.Join(options.Dir, "index.sqlite")
	}
	if strings.TrimSpace(options.RetrievalSidecarPath) == "" {
		options.RetrievalSidecarPath = filepath.Join(options.Dir, "retrieval-bge-m3.sqlite")
	}
	if options.RemoteProcessing == "" {
		options.RemoteProcessing = defaults.RemoteProcessing
	}
	if options.CompileBatchTokens <= 0 {
		options.CompileBatchTokens = defaults.CompileBatchTokens
	}
	if options.CompileOutputTokens <= 0 {
		options.CompileOutputTokens = defaults.CompileOutputTokens
	}
	if options.CompileMaxNodes <= 0 {
		options.CompileMaxNodes = defaults.CompileMaxNodes
	}
	if options.CompileMaxCalls <= 0 {
		options.CompileMaxCalls = defaults.CompileMaxCalls
	}
	if options.CompileMaxSources <= 0 {
		options.CompileMaxSources = defaults.CompileMaxSources
	}
	if options.CompileSourcesPerCall <= 0 {
		options.CompileSourcesPerCall = defaults.CompileSourcesPerCall
	}
	if options.CompileSourcesPerSession <= 0 {
		options.CompileSourcesPerSession = defaults.CompileSourcesPerSession
	}
	if options.CompileSourceRunes <= 0 {
		options.CompileSourceRunes = defaults.CompileSourceRunes
	}
	if options.CompileCallTimeout <= 0 {
		options.CompileCallTimeout = defaults.CompileCallTimeout
	}
	if options.CompileConcurrency <= 0 {
		options.CompileConcurrency = defaults.CompileConcurrency
	}
	if options.WorkerCount <= 0 {
		options.WorkerCount = defaults.WorkerCount
	}
	if options.EmbeddingBatchSize <= 0 {
		options.EmbeddingBatchSize = defaults.EmbeddingBatchSize
	}
	if options.CandidateLimit <= 0 {
		options.CandidateLimit = defaults.CandidateLimit
	}
	if options.MaxEvidence <= 0 {
		options.MaxEvidence = defaults.MaxEvidence
	}
	if options.TargetContextTokens <= 0 {
		options.TargetContextTokens = defaults.TargetContextTokens
	}
	if options.MaxContextTokens <= 0 {
		options.MaxContextTokens = defaults.MaxContextTokens
	}
	if options.SearchLatencyBudget <= 0 {
		options.SearchLatencyBudget = defaults.SearchLatencyBudget
	}
	if options.WorkerPollInterval <= 0 {
		options.WorkerPollInterval = defaults.WorkerPollInterval
	}
	if options.Clock == nil {
		options.Clock = defaults.Clock
	}
	if options.Planner == nil {
		options.Planner = NewLocalSemanticPlanner(options.Vectorizer)
	}
	return options
}

func openFabricDB(path string, singleConnection bool) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := path
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	dsn += separator + "_pragma=busy_timeout%3D5000&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if singleConnection {
		// The vec virtual table performs shadow-table reads through the same
		// pool while MATCH owns its connection, so it needs one spare.
		db.SetMaxOpenConns(2)
		db.SetMaxIdleConns(2)
	} else {
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(2)
	}
	return db, nil
}

func (f *Fabric) Close() error {
	if f == nil {
		return nil
	}
	var closeErr error
	f.closeOnce.Do(func() {
		if f.workerCancel != nil {
			f.workerCancel()
		}
		f.workerWG.Wait()
		var errs []error
		if f.index != nil {
			f.invalidateVectorCaches()
			errs = append(errs, f.index.Close())
		}
		if f.ledger != nil {
			errs = append(errs, f.ledger.Close())
		}
		if f.sidecar != nil {
			f.sidecarMu.Lock()
			errs = append(errs, f.sidecar.Close())
			f.sidecar = nil
			f.sidecarMu.Unlock()
		}
		closeErr = errors.Join(errs...)
	})
	return closeErr
}

func (f *Fabric) invalidateVectorCaches() {
	rows, err := f.index.Query(`SELECT DISTINCT dataset_id FROM _vec_memory_vectors`)
	if err != nil {
		return
	}
	var datasets []string
	for rows.Next() {
		var dataset string
		if rows.Scan(&dataset) == nil && strings.TrimSpace(dataset) != "" {
			datasets = append(datasets, dataset)
		}
	}
	_ = rows.Close()
	for _, dataset := range datasets {
		vec.InvalidateCache("_vec_memory_vectors", dataset)
	}
}

func (f *Fabric) now() time.Time {
	return f.options.Clock().UTC()
}

func (f *Fabric) startWorkers() {
	f.workerCtx, f.workerCancel = context.WithCancel(context.Background())
	for range f.options.WorkerCount {
		f.workerWG.Add(1)
		go func() {
			defer f.workerWG.Done()
			f.workerLoop(f.workerCtx)
		}()
	}
}

func (f *Fabric) wakeWorker() {
	select {
	case f.jobWake <- struct{}{}:
	default:
	}
}

func (f *Fabric) workerLoop(ctx context.Context) {
	ticker := time.NewTicker(f.options.WorkerPollInterval)
	defer ticker.Stop()
	for {
		for {
			worked, err := f.processNextWork(ctx)
			if err != nil || !worked {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-f.jobWake:
		case <-ticker.C:
		}
	}
}
