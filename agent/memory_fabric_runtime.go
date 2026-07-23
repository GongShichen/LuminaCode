package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/memory"
	"LuminaCode/memory/localmodel"
)

const (
	fabricCompilerToolName         = "CompileSemanticMemory"
	fabricAdjudicatorToolName      = "AdjudicateMemoryConflict"
	fabricBatchAdjudicatorToolName = "AdjudicateMemoryConflicts"
	fabricCompilerMaxOutput        = 4096
	fabricCompilerMaxNodes         = 16
)

func isFabricMemoryBackend(cfg config.Config) bool {
	return cfg.UsesMemoryFabric()
}

func fabricMemorySpace(cfg config.Config) string {
	return memory.ProjectSpace(cfg.ProjectRoot())
}

func MemoryFabricSpace(cfg config.Config) string {
	return fabricMemorySpace(cfg)
}

// ConfiguredMemorySemanticPlanner exposes the same local planner used by the
// runtime so import orchestration can precompute decisions without opening a
// ledger or invoking a remote model.
func ConfiguredMemorySemanticPlanner(cfg config.Config) memory.SemanticPlanner {
	var vectorizer memory.Vectorizer
	if encoder, err := configuredMemoryBGEEncoder(cfg); err == nil {
		vectorizer = newFabricVectorizer(bgeDenseVectorEncoder{encoder: encoder}, fabricEmbeddingArtifactDir(cfg))
	}
	return memory.NewLocalSemanticPlanner(vectorizer)
}

// ConfiguredMemorySemanticCompiler returns the compiler used by Fabric with
// the configured shared artifact directory. Callers remain responsible for
// applying grounding and committing nodes to a ledger.
func ConfiguredMemorySemanticCompiler(cfg config.Config) memory.SemanticCompiler {
	if strings.EqualFold(strings.TrimSpace(cfg.MemoryRemoteProcessing), string(memory.RemoteProcessingOff)) {
		return nil
	}
	return newFabricSemanticCompiler(cfg, configuredFabricModel(cfg.MemoryCompilerModel, cfg.APIModel))
}

func openConfiguredMemoryFabric(ctx context.Context, cfg config.Config) (*memory.Fabric, error) {
	return OpenConfiguredMemoryFabric(ctx, cfg, true)
}

func OpenConfiguredMemoryFabric(ctx context.Context, cfg config.Config, startWorkers bool) (*memory.Fabric, error) {
	return OpenConfiguredMemoryFabricWithUsageObserver(ctx, cfg, startWorkers, nil)
}

func OpenConfiguredMemoryFabricWithUsageObserver(ctx context.Context, cfg config.Config, startWorkers bool,
	observer memory.APIUsageObserver) (*memory.Fabric, error) {
	if !cfg.LongTermMemoryEnabled || !isFabricMemoryBackend(cfg) {
		return nil, nil
	}
	options := memory.DefaultFabricOptions(cfg.MemoryPath)
	options.StartWorkers = startWorkers
	options.UsageObserver = observer
	options.RemoteProcessing = memory.RemoteProcessingPolicy(strings.ToLower(strings.TrimSpace(cfg.MemoryRemoteProcessing)))
	options.CompileBatchTokens = effectiveFabricCompileBatchTokens(cfg.MemoryCompileBatchTokens)
	options.SearchLatencyBudget = time.Duration(cfg.MemorySearchLatencyMS) * time.Millisecond
	options.CandidateLimit = cfg.MemoryCandidateLimit
	options.MaxEvidence = cfg.MemoryRecallMaxItems
	options.TargetContextTokens = cfg.MemoryContextTargetTokens
	options.MaxContextTokens = cfg.MemoryContextMaxTokens

	encoder, encoderErr := configuredMemoryBGEEncoder(cfg)
	if encoderErr != nil {
		return nil, fmt.Errorf("BGE-M3 is required for Memory Fabric: %w", encoderErr)
	}
	options.Vectorizer = newFabricVectorizer(bgeDenseVectorEncoder{encoder: encoder}, fabricEmbeddingArtifactDir(cfg))
	options.RetrievalEncoder = fabricRetrievalEncoder{encoder: encoder}
	if options.RemoteProcessing != memory.RemoteProcessingOff {
		options.Compiler = newFabricSemanticCompiler(cfg, configuredFabricModel(cfg.MemoryCompilerModel, cfg.APIModel))
		options.Adjudicator = newFabricConflictAdjudicator(cfg, configuredFabricModel(cfg.MemoryConflictModel, cfg.APIModel))
	}
	return memory.OpenFabric(ctx, options)
}

func fabricMemoryRuntimeKey(cfg config.Config) string {
	if !cfg.LongTermMemoryEnabled || !isFabricMemoryBackend(cfg) {
		return ""
	}
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(cfg.MemoryBackend)),
		strings.TrimSpace(cfg.MemoryPath),
		strings.ToLower(strings.TrimSpace(cfg.MemoryRemoteProcessing)),
		configuredFabricModel(cfg.MemoryCompilerModel, cfg.APIModel),
		configuredFabricModel(cfg.MemoryConflictModel, cfg.APIModel),
		fmt.Sprintf("%d", effectiveFabricCompileBatchTokens(cfg.MemoryCompileBatchTokens)),
		fmt.Sprintf("%d", cfg.MemorySearchLatencyMS),
		fmt.Sprintf("%d", cfg.MemoryCandidateLimit),
		strings.TrimSpace(cfg.MemoryBGEModelDir),
	}, "\x1f")
}

func effectiveFabricCompileBatchTokens(configured int) int {
	if configured <= 0 || configured > 10_000 {
		return 10_000
	}
	return configured
}

func (e *CoreExecutionEngine) configureMemoryEngine(ctx context.Context, cfg config.Config) {
	if e == nil {
		return
	}
	desiredKey := fabricMemoryRuntimeKey(cfg)
	e.memoryMu.RLock()
	unchanged := desiredKey != "" && desiredKey == e.memoryEngineRuntimeKey && e.memoryEngine != nil
	e.memoryMu.RUnlock()
	if unchanged {
		if e.extraction != nil {
			e.extraction.Engine = e.memoryEngineSnapshot()
		}
		return
	}

	var next memory.Engine
	var openErr error
	if desiredKey != "" {
		var fabric *memory.Fabric
		fabric, openErr = openConfiguredMemoryFabric(ctx, cfg)
		if fabric != nil {
			next = fabric
		}
	}
	e.memoryMu.Lock()
	previous := e.memoryEngine
	e.memoryEngine = next
	e.memoryEngineErr = openErr
	if openErr == nil {
		e.memoryEngineRuntimeKey = desiredKey
	} else {
		e.memoryEngineRuntimeKey = ""
	}
	e.memoryMu.Unlock()
	if e.extraction != nil {
		e.extraction.Engine = next
	}
	if previous != nil && previous != next {
		_ = previous.Close()
	}
	if openErr != nil {
		slogMemoryFabricError("initialize", openErr)
	}
}

func (e *CoreExecutionEngine) memoryEngineSnapshot() memory.Engine {
	if e == nil {
		return nil
	}
	e.memoryMu.RLock()
	defer e.memoryMu.RUnlock()
	return e.memoryEngine
}

func (e *CoreExecutionEngine) memoryEngineFailure() error {
	if e == nil {
		return nil
	}
	e.memoryMu.RLock()
	defer e.memoryMu.RUnlock()
	return e.memoryEngineErr
}

func (e *CoreExecutionEngine) closeMemoryEngine() {
	if e == nil {
		return
	}
	e.memoryMu.Lock()
	engine := e.memoryEngine
	e.memoryEngine = nil
	e.memoryEngineRuntimeKey = ""
	e.memoryMu.Unlock()
	if e.extraction != nil {
		e.extraction.Engine = nil
	}
	if engine != nil {
		_ = engine.Close()
	}
}

func slogMemoryFabricError(operation string, err error) {
	if err != nil {
		// Keep background failures visible without turning them into evidence.
		slog.Error("memory fabric operation failed", "operation", operation, "error", err)
	}
}

func (e *CoreExecutionEngine) flushAndSealMemory(ctx context.Context) {
	if e == nil || !isFabricMemoryBackend(e.Config) {
		return
	}
	engine := e.memoryEngineSnapshot()
	if engine == nil {
		return
	}
	flushCtx := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		flushCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
	}
	defer cancel()
	if e.extraction != nil && e.LastState != nil {
		e.extraction.Engine = engine
		for batch := 0; batch < 256; batch++ {
			count, err := e.extraction.IngestMessages(flushCtx, e.LastState)
			if err != nil {
				var lag memory.IndexLagError
				if !errors.As(err, &lag) {
					slogMemoryFabricError("flush raw events", err)
					break
				}
			}
			if count == 0 {
				break
			}
		}
	}
	sessionID := strings.TrimSpace(e.SessionID)
	if sessionID == "" && e.LastState != nil {
		sessionID = strings.TrimSpace(e.LastState.MemorySessionID)
	}
	if sessionID == "" {
		return
	}
	if _, err := engine.SealContext(flushCtx, memory.ContextRef{
		ID: sessionID, Space: fabricMemorySpace(e.Config), Type: "conversation", ClosedAt: time.Now().UTC(),
	}); err != nil {
		slogMemoryFabricError("seal context", err)
	}
}

func (e *CoreExecutionEngine) recordFabricToolEvents(ctx context.Context, state *AgentState,
	toolResults []map[string]any, executor *StreamingToolExecutor) {
	if e == nil || state == nil || !isFabricMemoryBackend(e.Config) || len(toolResults) == 0 {
		return
	}
	engine := e.memoryEngineSnapshot()
	if engine == nil {
		return
	}
	space := fabricMemorySpace(e.Config)
	sessionID := firstNonEmptyString(state.MemorySessionID, e.SessionID)
	now := time.Now().UTC()
	events := make([]memory.RawEvent, 0, len(toolResults))
	for _, result := range toolResults {
		toolUseID := strings.TrimSpace(stringFromAny(result["tool_use_id"]))
		if toolUseID == "" {
			continue
		}
		name := "tool"
		input := map[string]any{}
		if executor != nil {
			if slot := executor.GetSlot(toolUseID); slot != nil {
				name = strings.TrimSpace(slot.TC.Name)
				input = slot.TC.Input
			}
		}
		encodedInput, _ := json.Marshal(input)
		status := "success"
		if value, ok := result["is_error"].(bool); ok && value {
			status = "error"
		}
		content := fmt.Sprintf("tool=%s\ninput=%s\nstatus=%s\nresult=%s", name,
			encodedInput, status, strings.TrimSpace(stringFromAny(result["content"])))
		content = truncateExtractionRunes(content, 20_000)
		kind := fabricToolSourceKind(name, input)
		metadata := map[string]string{
			"tool": name, "status": status, "project": e.Config.ProjectRoot(),
			"environment": e.Config.CWD,
		}
		for _, key := range []string{"path", "file_path", "command", "cmd"} {
			if value := strings.TrimSpace(stringFromAny(input[key])); value != "" {
				metadata[key] = value
			}
		}
		events = append(events, memory.RawEvent{
			ID: fabricEventID(space, sessionID, "tool:"+toolUseID), Space: space,
			ContextID: sessionID, SessionID: sessionID, Actor: "tool", SourceKind: kind,
			Content: content, OccurredAt: now, SourceRef: toolUseID, Metadata: metadata,
		})
	}
	if len(events) == 0 {
		return
	}
	if _, err := engine.AppendEvents(ctx, events,
		memory.IngestOptions{SemanticPolicy: memory.SemanticDeterministic}); err != nil {
		var lag memory.IndexLagError
		if !errors.As(err, &lag) {
			slogMemoryFabricError("record tool evidence", err)
		}
	}
}

func fabricToolSourceKind(name string, input map[string]any) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	if strings.Contains(lower, "bash") || strings.Contains(lower, "shell") ||
		strings.TrimSpace(stringFromAny(input["command"])) != "" || strings.TrimSpace(stringFromAny(input["cmd"])) != "" {
		return "command"
	}
	if strings.Contains(lower, "file") || strings.Contains(lower, "edit") ||
		strings.TrimSpace(stringFromAny(input["path"])) != "" || strings.TrimSpace(stringFromAny(input["file_path"])) != "" {
		return "file"
	}
	return "tool"
}

func configuredFabricModel(configured, fallback string) string {
	configured = strings.TrimSpace(configured)
	if configured == "" || strings.EqualFold(configured, "inherit") {
		return strings.TrimSpace(fallback)
	}
	return configured
}

type fabricDenseEncoder interface {
	Model() string
	Dimensions() int
	EmbedDense(context.Context, []string, memory.VectorPurpose) ([][]float32, error)
}

type bgeDenseVectorEncoder struct {
	encoder localmodel.BGEEncoder
}

func (e bgeDenseVectorEncoder) Model() string {
	return e.encoder.Model() + "@" + e.encoder.Revision()
}

func (e bgeDenseVectorEncoder) Dimensions() int { return localmodel.BGEEmbeddingDimensions }

func (e bgeDenseVectorEncoder) EmbedDense(ctx context.Context, texts []string,
	purpose memory.VectorPurpose) ([][]float32, error) {
	kind := localmodel.BGEDocument
	if purpose == memory.VectorQuery {
		kind = localmodel.BGEQuery
	}
	encoded, err := e.encoder.EncodeChannels(ctx, texts, kind)
	if err != nil {
		return nil, err
	}
	result := make([][]float32, len(encoded))
	for index := range encoded {
		result[index] = append([]float32(nil), encoded[index].Dense...)
	}
	return result, nil
}

type fabricVectorizer struct {
	encoder     fabricDenseEncoder
	artifactDir string
}

func newFabricVectorizer(encoder fabricDenseEncoder, artifactDir string) fabricVectorizer {
	return fabricVectorizer{encoder: encoder, artifactDir: strings.TrimSpace(artifactDir)}
}

func fabricEmbeddingArtifactDir(cfg config.Config) string {
	semanticDir := strings.TrimSpace(cfg.MemoryArtifactPath)
	if semanticDir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(filepath.Clean(semanticDir)), "embedding-artifacts")
}

func (v fabricVectorizer) Model() string { return v.encoder.Model() }

func (v fabricVectorizer) Dimensions() int { return v.encoder.Dimensions() }

func (v fabricVectorizer) Embed(ctx context.Context, texts []string, purpose memory.VectorPurpose) ([][]float32, error) {
	if purpose == memory.VectorContent && v.artifactDir != "" {
		return v.embedContentWithArtifacts(ctx, texts, purpose)
	}
	return v.encoder.EmbedDense(ctx, texts, purpose)
}

func configuredMemoryBGEEncoder(cfg config.Config) (localmodel.BGEEncoder, error) {
	return localmodel.SharedLocalBGEEncoder(cfg.MemoryBGEModelDir)
}

type fabricRetrievalEncoder struct {
	encoder localmodel.BGEEncoder
}

func (e fabricRetrievalEncoder) Model() string { return e.encoder.Model() }

func (e fabricRetrievalEncoder) Revision() string { return e.encoder.Revision() }

func (e fabricRetrievalEncoder) TokenizerHash() string { return e.encoder.TokenizerHash() }

func (e fabricRetrievalEncoder) CompatibleRevision(other string) bool {
	compatible, ok := e.encoder.(interface {
		CompatibleRevision(string) bool
	})
	return ok && compatible.CompatibleRevision(other)
}

func (e fabricRetrievalEncoder) Split(text string, maxTokens, overlap int) ([]string, error) {
	return e.encoder.Split(text, maxTokens, overlap)
}

func (e fabricRetrievalEncoder) SplitMany(text string, specs []memory.RetrievalSplitSpec) ([][]string, error) {
	localSpecs := make([]localmodel.BGESplitSpec, len(specs))
	for index, spec := range specs {
		localSpecs[index] = localmodel.BGESplitSpec{MaxTokens: spec.MaxTokens, Overlap: spec.Overlap}
	}
	return e.encoder.SplitMany(text, localSpecs)
}

func (e fabricRetrievalEncoder) Encode(ctx context.Context, texts []string,
	kind memory.RetrievalEncodingKind) ([]memory.RetrievalEncoding, error) {
	bgeKind := localmodel.BGEDocument
	if kind == memory.RetrievalQuery {
		bgeKind = localmodel.BGEQuery
	}
	encoded, err := e.encoder.Encode(ctx, texts, bgeKind)
	if err != nil {
		return nil, err
	}
	result := make([]memory.RetrievalEncoding, len(encoded))
	for index, item := range encoded {
		multi := make([]memory.RetrievalTokenVector, len(item.Multi))
		for tokenIndex, token := range item.Multi {
			multi[tokenIndex] = memory.RetrievalTokenVector{TokenID: token.TokenID, Position: token.Position,
				Weight: token.Weight, Values: append([]float32(nil), token.Values...)}
		}
		result[index] = memory.RetrievalEncoding{Dense: append([]float32(nil), item.Dense...),
			Sparse: item.Sparse, Multi: multi}
	}
	return result, nil
}

func (e fabricRetrievalEncoder) EncodeChannels(ctx context.Context, texts []string,
	kind memory.RetrievalEncodingKind) ([]memory.RetrievalEncoding, error) {
	bgeKind := localmodel.BGEDocument
	if kind == memory.RetrievalQuery {
		bgeKind = localmodel.BGEQuery
	}
	encoded, err := e.encoder.EncodeChannels(ctx, texts, bgeKind)
	if err != nil {
		return nil, err
	}
	result := make([]memory.RetrievalEncoding, len(encoded))
	for index, item := range encoded {
		result[index] = memory.RetrievalEncoding{Dense: append([]float32(nil), item.Dense...), Sparse: item.Sparse}
	}
	return result, nil
}

type fabricStructuredClientFactory func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error)

func defaultFabricStructuredClientFactory(cfg config.Config, model string, maxTokens int, retry *api.RetryConfig) (api.StructuredCompletionClient, error) {
	client, err := CreateConfiguredLLMClient(cfg, model, maxTokens, nil, retry)
	if err != nil {
		return nil, err
	}
	structured, ok := client.(api.StructuredCompletionClient)
	if !ok {
		return nil, errors.New("configured memory model does not support structured completion")
	}
	return structured, nil
}

type fabricSemanticCompiler struct {
	cfg      config.Config
	model    string
	factory  fabricStructuredClientFactory
	cacheDir string

	mu     sync.Mutex
	client api.StructuredCompletionClient
	err    error
}

const fabricCompilerContractRepairRetries = 3

func newFabricSemanticCompiler(cfg config.Config, model string) *fabricSemanticCompiler {
	cacheDir := strings.TrimSpace(cfg.MemoryArtifactPath)
	if cacheDir == "" {
		if path := strings.TrimSpace(cfg.MemoryPath); path != "" {
			cacheDir = filepath.Join(filepath.Dir(filepath.Clean(path)), "semantic-cache")
		}
	}
	return &fabricSemanticCompiler{cfg: cfg, model: model, cacheDir: cacheDir,
		factory: defaultFabricStructuredClientFactory}
}

func (c *fabricSemanticCompiler) Compile(ctx context.Context, request memory.CompileRequest) (memory.CompileResponse, error) {
	var result memory.CompileResponse
	request.MaxNodes = normalizeFabricCompilerMaxNodes(request.MaxNodes)
	request.MaxOutputTokens = normalizeFabricCompilerOutputTokens(request.MaxOutputTokens)
	estimated, err := c.EstimateInputTokens(request)
	if err != nil {
		return result, err
	}
	if request.MaxInputTokens > 0 && estimated > request.MaxInputTokens {
		return result, &memory.CompileInputBudgetError{EstimatedTokens: estimated,
			LimitTokens: request.MaxInputTokens, EventCount: len(request.Sources)}
	}
	schema := fabricCompilerToolSchemaFor(request.MaxNodes)
	sessionRequests := splitFabricCompileRequestBySession(request)
	missing := make([]memory.CompileSource, 0, len(request.Sources))
	missingKeys := make(map[string]string, len(sessionRequests))
	sessions := make([]string, 0, len(sessionRequests))
	for session := range sessionRequests {
		sessions = append(sessions, session)
	}
	sort.Strings(sessions)
	for _, session := range sessions {
		sessionRequest := sessionRequests[session]
		key, err := fabricCompilerArtifactCacheKey(c.model, sessionRequest.Sources)
		if err != nil {
			return result, err
		}
		if cached, ok := c.loadCachedCompileResponse(key); ok {
			result.Nodes = append(result.Nodes, cached.Nodes...)
			result.Aliases = append(result.Aliases, cached.Aliases...)
			continue
		}
		missingKeys[session] = key
		missing = append(missing, sessionRequest.Sources...)
	}
	if len(missing) == 0 {
		result.CacheHit = true
		return trimFabricCompileResponse(result, request.MaxNodes), nil
	}
	request.Sources = missing
	payload, err := json.Marshal(request)
	if err != nil {
		return result, fmt.Errorf("encode semantic compiler request: %w", err)
	}
	rawKey := fabricCompilerRawArtifactCacheKey(c.model, payload)
	input, rawHit := c.loadCachedCompilerInput(rawKey)
	messages := []map[string]any{{"role": "user", "content": string(payload)}}
	var response api.Response
	var fresh memory.CompileResponse
	var contractErr error
	if rawHit {
		input, fresh, contractErr = decodeFabricCompilerInput(input, request, false, 0)
		if contractErr == nil {
			result.CacheHit = true
		}
	} else {
		client, clientErr := c.structuredClient(fabricCompilerMaxOutput)
		if clientErr != nil {
			return result, clientErr
		}
		response, err = client.CompleteStructured(fabricAPIContext(ctx, c.cfg), fabricCompilerSystemPrompt,
			messages, fabricCompilerCompletionOptions(request, schema))
		mergeFabricCompilerUsage(&result.Usage, response, c.model)
		if err != nil {
			return result, err
		}
		input, contractErr = structuredMemoryInput(response, fabricCompilerToolName)
		if contractErr == nil {
			// Keep the provider's structured payload even when the current local
			// parser rejects it. A later parser fix can reprocess it without
			// spending another generation call.
			if !fabricStructuredResponseTruncated(response.StopReason) {
				c.saveCachedCompilerInput(rawKey, input)
			}
			input, fresh, contractErr = decodeFabricCompilerInput(input, request,
				fabricStructuredResponseTruncated(response.StopReason), response.OutputTokens)
		} else if fabricStructuredResponseTruncated(response.StopReason) {
			contractErr = fmt.Errorf("model output was truncated and incomplete: %s", response.StopReason)
		}
	}

	if contractErr != nil {
		client, clientErr := c.structuredClient(fabricCompilerMaxOutput)
		if clientErr != nil {
			return result, clientErr
		}
		for repairAttempt := 0; repairAttempt < fabricCompilerContractRepairRetries && contractErr != nil; repairAttempt++ {
			repairMessages := append([]map[string]any(nil), messages...)
			repairMessages = append(repairMessages, map[string]any{"role": "user", "content": fabricCompilerRepairInstruction(contractErr, input, response)})
			repaired, repairErr := client.CompleteStructured(fabricAPIContext(ctx, c.cfg), fabricCompilerSystemPrompt,
				repairMessages, fabricCompilerCompletionOptions(request, schema))
			mergeFabricCompilerUsage(&result.Usage, repaired, c.model)
			if repairErr != nil {
				return result, repairErr
			}
			response = repaired
			repairedInput, extractErr := structuredMemoryInput(repaired, fabricCompilerToolName)
			if extractErr != nil {
				input = nil
				if fabricStructuredResponseTruncated(repaired.StopReason) {
					extractErr = fmt.Errorf("model output was truncated and incomplete: %s", repaired.StopReason)
				}
				contractErr = extractErr
				continue
			}
			input, fresh, contractErr = decodeFabricCompilerInput(repairedInput, request,
				fabricStructuredResponseTruncated(repaired.StopReason), repaired.OutputTokens)
		}
		if contractErr != nil {
			return result, &memory.CompileContractError{Reason: contractErr.Error()}
		}
		c.saveCachedCompilerInput(rawKey, input)
	}
	if contractErr == nil && !result.CacheHit {
		c.saveCachedCompilerInput(rawKey, input)
	}

	fresh.Usage = result.Usage
	fresh = trimFabricCompileResponse(fresh, request.MaxNodes)
	for session, key := range missingKeys {
		c.saveCachedCompileResponse(key, filterFabricCompileResponseForSession(fresh, session, request.Sources))
	}
	result.Nodes = append(result.Nodes, fresh.Nodes...)
	result.Aliases = append(result.Aliases, fresh.Aliases...)
	result.Usage = fresh.Usage
	return trimFabricCompileResponse(result, request.MaxNodes), nil
}

func fabricCompilerCompletionOptions(request memory.CompileRequest, schema map[string]any) api.StructuredCompletionOptions {
	return api.StructuredCompletionOptions{MaxTokens: request.MaxOutputTokens, Tools: []map[string]any{schema},
		RequiredTool: fabricCompilerToolName, DisableThinking: true}
}

func decodeFabricCompilerInput(input map[string]any, request memory.CompileRequest, truncated bool,
	outputTokens int) (map[string]any, memory.CompileResponse, error) {
	normalized, err := normalizeStructuredJSONArrayFields(input, "nodes", "aliases")
	if err != nil {
		return input, memory.CompileResponse{}, err
	}
	if err := normalizeFabricCompilerTimes(normalized, request.Sources); err != nil {
		return input, memory.CompileResponse{}, err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return input, memory.CompileResponse{}, fmt.Errorf("encode semantic compiler response: %w", err)
	}
	var response memory.CompileResponse
	if err := json.Unmarshal(encoded, &response); err != nil {
		return input, memory.CompileResponse{}, fmt.Errorf("decode response: %w", err)
	}
	response = trimFabricCompileResponse(response, request.MaxNodes)
	if truncated && len(response.Nodes) == 0 && len(response.Aliases) == 0 {
		return input, memory.CompileResponse{}, errors.New("model output was truncated and incomplete")
	}
	if len(response.Nodes) == 0 && len(response.Aliases) == 0 && request.MaxOutputTokens > 0 &&
		outputTokens >= request.MaxOutputTokens {
		return input, memory.CompileResponse{}, errors.New("model exhausted the output budget without a semantic result")
	}
	return normalized, response, nil
}

func fabricCompilerRepairInstruction(contractErr error, input map[string]any, response api.Response) string {
	previous := any(input)
	if previous == nil {
		previous = map[string]any{"text": response.Text, "tool_calls": response.ToolCalls,
			"stop_reason": response.StopReason}
	}
	encoded, _ := json.Marshal(previous)
	return fmt.Sprintf(`Your previous CompileSemanticMemory tool input failed local contract validation:
%s

Correct the generated structure. Re-read the original sources, preserve only grounded claims, obey all field types and limits, and call CompileSemanticMemory exactly once. Do not explain the correction.

Previous invalid tool input:
%s`, contractErr.Error(), string(encoded))
}

func mergeFabricCompilerUsage(total *memory.APIUsage, response api.Response, model string) {
	if total == nil {
		return
	}
	total.Calls++
	total.InputTokens += response.InputTokens
	total.CacheReadInputTokens += response.CacheReadInputTokens
	total.CacheCreationInputTokens += response.CacheCreationInputTokens
	total.OutputTokens += response.OutputTokens
	total.Model = model
}

func normalizeFabricCompilerTimes(input map[string]any, sources []memory.CompileSource) error {
	sourceTimes := make(map[string]time.Time, len(sources))
	for _, source := range sources {
		if sourceRef := strings.TrimSpace(source.SourceRef); sourceRef != "" && !source.OccurredAt.IsZero() {
			sourceTimes[sourceRef] = source.OccurredAt
		}
	}
	nodes, _ := input["nodes"].([]any)
	for _, item := range nodes {
		node, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"valid_from", "valid_until"} {
			if err := normalizeFabricTimeField(node, field, "semantic compiler"); err != nil {
				return err
			}
		}
		if value, ok := node["value"].(map[string]any); ok {
			if err := normalizeFabricCompilerValueTime(value, node, sourceTimes); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeFabricCompilerValueTime(value, node map[string]any, sourceTimes map[string]time.Time) error {
	raw, exists := value["time"]
	if !exists || raw == nil {
		return normalizeFabricTimeField(value, "time", "semantic compiler")
	}
	text, ok := raw.(string)
	if !ok {
		return normalizeFabricTimeField(value, "time", "semantic compiler")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		delete(value, "time")
		return nil
	}
	clock, ok := parseFabricClockTime(text)
	if !ok {
		return normalizeFabricTimeField(value, "time", "semantic compiler")
	}
	base := normalizedFabricNodeTime(node, "valid_from")
	if base.IsZero() {
		if spans, ok := node["sources"].([]any); ok {
			for _, item := range spans {
				span, ok := item.(map[string]any)
				if !ok {
					continue
				}
				sourceRef, _ := span["source_ref"].(string)
				if candidate := sourceTimes[strings.TrimSpace(sourceRef)]; !candidate.IsZero() {
					base = candidate
					break
				}
			}
		}
	}
	if base.IsZero() {
		return fmt.Errorf("semantic compiler time-of-day %q has no source date", text)
	}
	anchored := time.Date(base.Year(), base.Month(), base.Day(), clock.Hour(), clock.Minute(), clock.Second(), 0,
		base.Location())
	value["time"] = anchored.UTC().Format(time.RFC3339)
	return nil
}

func parseFabricClockTime(value string) (time.Time, bool) {
	for _, layout := range []string{"15:04", "15:04:05", "3:04 PM", "3:04PM", "3 PM", "3PM"} {
		if parsed, err := time.Parse(layout, strings.ToUpper(strings.TrimSpace(value))); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func normalizedFabricNodeTime(node map[string]any, field string) time.Time {
	value, _ := node[field].(string)
	parsed, _ := time.Parse(time.RFC3339, strings.TrimSpace(value))
	return parsed
}

func normalizeFabricTimeField(object map[string]any, field, component string) error {
	raw, exists := object[field]
	if !exists {
		return nil
	}
	if raw == nil {
		delete(object, field)
		return nil
	}
	value, ok := raw.(string)
	if !ok {
		return fmt.Errorf("%s %s must be a string", component, field)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		delete(object, field)
		return nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		object[field] = parsed.UTC().Format(time.RFC3339)
		return nil
	}
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		object[field] = parsed.UTC().Format(time.RFC3339)
		return nil
	}
	if parsed, err := time.Parse("2006-01-02T15:04:05", value); err == nil {
		object[field] = parsed.UTC().Format(time.RFC3339)
		return nil
	}
	return fmt.Errorf("%s %s has unsupported time %q", component, field, value)
}

func normalizeFabricAdjudicationTimes(input map[string]any) error {
	objects := []map[string]any{input}
	if results, ok := input["results"].([]any); ok {
		for _, item := range results {
			if object, ok := item.(map[string]any); ok {
				objects = append(objects, object)
			}
		}
	}
	for _, object := range objects {
		for _, field := range []string{"valid_from", "valid_until"} {
			if err := normalizeFabricTimeField(object, field, "conflict adjudicator"); err != nil {
				return err
			}
		}
	}
	return nil
}

func splitFabricCompileRequestBySession(request memory.CompileRequest) map[string]memory.CompileRequest {
	grouped := map[string][]memory.CompileSource{}
	for _, source := range request.Sources {
		session := strings.TrimSpace(source.SessionRef)
		if session == "" {
			session = strings.TrimSpace(source.SourceRef)
		}
		grouped[session] = append(grouped[session], source)
	}
	result := make(map[string]memory.CompileRequest, len(grouped))
	for session, sources := range grouped {
		sort.SliceStable(sources, func(i, j int) bool { return sources[i].SourceRef < sources[j].SourceRef })
		item := request
		item.Sources = sources
		result[session] = item
	}
	return result
}

func filterFabricCompileResponseForSession(response memory.CompileResponse, session string,
	sources []memory.CompileSource) memory.CompileResponse {
	sourceSessions := make(map[string]string, len(sources))
	for _, source := range sources {
		key := strings.TrimSpace(source.SessionRef)
		if key == "" {
			key = strings.TrimSpace(source.SourceRef)
		}
		sourceSessions[source.SourceRef] = key
	}
	belongs := func(spans []memory.SourceSpan) bool {
		if len(spans) == 0 {
			return false
		}
		for _, span := range spans {
			if sourceSessions[span.SourceRef] != session {
				return false
			}
		}
		return true
	}
	result := memory.CompileResponse{}
	for _, node := range response.Nodes {
		if belongs(node.Sources) {
			result.Nodes = append(result.Nodes, node)
		}
	}
	for _, alias := range response.Aliases {
		if belongs(alias.Sources) {
			result.Aliases = append(result.Aliases, alias)
		}
	}
	return result
}

func trimFabricCompileResponse(response memory.CompileResponse, maxNodes int) memory.CompileResponse {
	if len(response.Nodes) > maxNodes {
		response.Nodes = response.Nodes[:maxNodes]
	}
	maxAliases := minIntAgent(3, maxNodes)
	if len(response.Aliases) > maxAliases {
		response.Aliases = response.Aliases[:maxAliases]
	}
	return response
}

func (c *fabricSemanticCompiler) EstimateInputTokens(request memory.CompileRequest) (int, error) {
	request.MaxNodes = normalizeFabricCompilerMaxNodes(request.MaxNodes)
	request.MaxOutputTokens = normalizeFabricCompilerOutputTokens(request.MaxOutputTokens)
	payload, err := json.Marshal(request)
	if err != nil {
		return 0, fmt.Errorf("encode semantic compiler request for sizing: %w", err)
	}
	schema, err := json.Marshal(fabricCompilerToolSchemaFor(request.MaxNodes))
	if err != nil {
		return 0, fmt.Errorf("encode semantic compiler schema for sizing: %w", err)
	}
	// Include all transmitted prompt material plus a conservative envelope for
	// provider message/tool framing. Non-ASCII runes are counted individually.
	return estimateFabricPromptTokens(fabricCompilerSystemPrompt) + estimateFabricPromptTokens(string(payload)) +
		estimateFabricPromptTokens(string(schema)) + 512, nil
}

func normalizeFabricCompilerOutputTokens(value int) int {
	if value <= 0 || value > fabricCompilerMaxOutput {
		return fabricCompilerMaxOutput
	}
	return value
}

func normalizeFabricCompilerMaxNodes(value int) int {
	if value <= 0 || value > fabricCompilerMaxNodes {
		return fabricCompilerMaxNodes
	}
	return value
}

func estimateFabricPromptTokens(value string) int {
	ascii, nonASCII := 0, 0
	for _, r := range value {
		if r <= 127 {
			ascii++
		} else {
			nonASCII++
		}
	}
	return (ascii+2)/3 + nonASCII
}

const (
	fabricCompilerArtifactContract    = "grounded-compact-session-claims"
	fabricCompilerRawArtifactContract = "structured-compiler-input"
)

type fabricCompilerArtifact struct {
	Contract string                         `json:"contract"`
	Complete bool                           `json:"complete"`
	Nodes    []memory.MemoryDraft           `json:"nodes"`
	Aliases  []memory.IdentityAliasProposal `json:"aliases"`
}

type fabricCompilerRawArtifact struct {
	Contract string         `json:"contract"`
	Complete bool           `json:"complete"`
	Input    map[string]any `json:"input"`
}

func fabricCompilerRawArtifactCacheKey(model string, payload []byte) string {
	digest := sha256.New()
	for _, part := range [][]byte{[]byte(model), []byte(fabricCompilerSystemPrompt),
		[]byte(fabricCompilerRawArtifactContract), payload} {
		_, _ = digest.Write(part)
		_, _ = digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func fabricCompilerArtifactCacheKey(model string, sources []memory.CompileSource) (string, error) {
	payload, err := json.Marshal(struct {
		Sources []memory.CompileSource `json:"sources"`
	}{Sources: sources})
	if err != nil {
		return "", fmt.Errorf("encode semantic compiler artifact key: %w", err)
	}
	digest := sha256.New()
	for _, part := range [][]byte{[]byte(model), []byte(fabricCompilerSystemPrompt),
		[]byte(fabricCompilerArtifactContract), payload} {
		_, _ = digest.Write(part)
		_, _ = digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func fabricStructuredResponseTruncated(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	for _, value := range []string{"length", "max_tokens", "max_output_tokens", "token_limit"} {
		if reason == value || strings.Contains(reason, value) {
			return true
		}
	}
	return false
}

func (c *fabricSemanticCompiler) loadCachedCompileResponse(key string) (memory.CompileResponse, bool) {
	if c.cacheDir == "" || len(key) < 2 {
		return memory.CompileResponse{}, false
	}
	encoded, err := os.ReadFile(filepath.Join(c.cacheDir, key[:2], key+".json"))
	if err != nil {
		return memory.CompileResponse{}, false
	}
	var artifact fabricCompilerArtifact
	if json.Unmarshal(encoded, &artifact) != nil {
		return memory.CompileResponse{}, false
	}
	if artifact.Contract != "" || artifact.Complete {
		if artifact.Contract != fabricCompilerArtifactContract || !artifact.Complete {
			return memory.CompileResponse{}, false
		}
		return memory.CompileResponse{Nodes: artifact.Nodes, Aliases: artifact.Aliases, CacheHit: true}, true
	}
	// Non-empty artifacts written before the completion envelope remain safe to
	// reuse. An unmarked empty artifact is ambiguous and must be regenerated.
	var result memory.CompileResponse
	if json.Unmarshal(encoded, &result) != nil || len(result.Nodes)+len(result.Aliases) == 0 {
		return memory.CompileResponse{}, false
	}
	result.Usage = memory.APIUsage{}
	result.CacheHit = true
	return result, true
}

func (c *fabricSemanticCompiler) saveCachedCompileResponse(key string, response memory.CompileResponse) {
	if c.cacheDir == "" || len(key) < 2 {
		return
	}
	nodes := response.Nodes
	if nodes == nil {
		nodes = []memory.MemoryDraft{}
	}
	aliases := response.Aliases
	if aliases == nil {
		aliases = []memory.IdentityAliasProposal{}
	}
	encoded, err := json.Marshal(fabricCompilerArtifact{Contract: fabricCompilerArtifactContract,
		Complete: true, Nodes: nodes, Aliases: aliases})
	if err != nil {
		return
	}
	dir := filepath.Join(c.cacheDir, key[:2])
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	temporary, err := os.CreateTemp(dir, ".semantic-*.tmp")
	if err != nil {
		return
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	_ = temporary.Chmod(0o600)
	if _, err = temporary.Write(encoded); err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		_ = os.Rename(name, filepath.Join(dir, key+".json"))
	}
}

func (c *fabricSemanticCompiler) CompileArtifactCached(sources []memory.CompileSource) (bool, error) {
	requests := splitFabricCompileRequestBySession(memory.CompileRequest{Sources: sources})
	for _, request := range requests {
		key, err := fabricCompilerArtifactCacheKey(c.model, request.Sources)
		if err != nil {
			return false, err
		}
		if _, ok := c.loadCachedCompileResponse(key); !ok {
			return false, nil
		}
	}
	return len(requests) > 0, nil
}

func (c *fabricSemanticCompiler) MarkCompileArtifactSkipped(sources []memory.CompileSource) error {
	requests := splitFabricCompileRequestBySession(memory.CompileRequest{Sources: sources})
	for _, request := range requests {
		key, err := fabricCompilerArtifactCacheKey(c.model, request.Sources)
		if err != nil {
			return err
		}
		c.saveCachedCompileResponse(key, memory.CompileResponse{})
		if _, ok := c.loadCachedCompileResponse(key); !ok {
			return fmt.Errorf("semantic compiler artifact was not persisted")
		}
	}
	return nil
}

func (c *fabricSemanticCompiler) loadCachedCompilerInput(key string) (map[string]any, bool) {
	if c.cacheDir == "" || len(key) < 2 {
		return nil, false
	}
	encoded, err := os.ReadFile(filepath.Join(c.cacheDir, "raw", key[:2], key+".json"))
	if err != nil {
		return nil, false
	}
	var artifact fabricCompilerRawArtifact
	if json.Unmarshal(encoded, &artifact) != nil || artifact.Contract != fabricCompilerRawArtifactContract ||
		!artifact.Complete || artifact.Input == nil {
		return nil, false
	}
	return artifact.Input, true
}

func (c *fabricSemanticCompiler) saveCachedCompilerInput(key string, input map[string]any) {
	if c.cacheDir == "" || len(key) < 2 || input == nil {
		return
	}
	encoded, err := json.Marshal(fabricCompilerRawArtifact{Contract: fabricCompilerRawArtifactContract,
		Complete: true, Input: input})
	if err != nil {
		return
	}
	dir := filepath.Join(c.cacheDir, "raw", key[:2])
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	temporary, err := os.CreateTemp(dir, ".compiler-input-*.tmp")
	if err != nil {
		return
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	_ = temporary.Chmod(0o600)
	if _, err = temporary.Write(encoded); err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		_ = os.Rename(name, filepath.Join(dir, key+".json"))
	}
}

func (c *fabricSemanticCompiler) structuredClient(maxTokens int) (api.StructuredCompletionClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil || c.err != nil {
		return c.client, c.err
	}
	if strings.TrimSpace(c.model) == "" {
		c.err = errors.New("memory semantic compiler model is not configured")
		return nil, c.err
	}
	cfg := c.cfg
	cfg.FallbackAPIEnabled = false
	retry := fabricSemanticRetryConfig()
	c.client, c.err = c.factory(cfg, c.model, maxTokens, retry)
	return c.client, c.err
}

type fabricConflictAdjudicator struct {
	cfg     config.Config
	model   string
	factory fabricStructuredClientFactory

	mu     sync.Mutex
	client api.StructuredCompletionClient
	err    error
}

func newFabricConflictAdjudicator(cfg config.Config, model string) *fabricConflictAdjudicator {
	return &fabricConflictAdjudicator{cfg: cfg, model: model, factory: defaultFabricStructuredClientFactory}
}

func (a *fabricConflictAdjudicator) Adjudicate(ctx context.Context, request memory.AdjudicationRequest) (memory.AdjudicationResponse, error) {
	var result memory.AdjudicationResponse
	client, err := a.structuredClient(4096)
	if err != nil {
		return result, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return result, fmt.Errorf("encode conflict adjudication request: %w", err)
	}
	response, err := client.CompleteStructured(fabricAPIContext(ctx, a.cfg), fabricAdjudicatorSystemPrompt,
		[]map[string]any{{"role": "user", "content": string(payload)}}, api.StructuredCompletionOptions{
			MaxTokens: 4096, Tools: []map[string]any{fabricAdjudicatorToolSchema()},
			RequiredTool: fabricAdjudicatorToolName, DisableThinking: true,
		})
	if err != nil {
		return result, err
	}
	result.Usage = memory.APIUsage{Calls: 1, InputTokens: response.InputTokens,
		CacheReadInputTokens:     response.CacheReadInputTokens,
		CacheCreationInputTokens: response.CacheCreationInputTokens,
		OutputTokens:             response.OutputTokens, Model: a.model}
	input, err := structuredMemoryInput(response, fabricAdjudicatorToolName)
	if err != nil {
		return result, err
	}
	input, err = normalizeStructuredJSONArrayFields(input, "winner_ids", "loser_ids", "support_ids")
	if err != nil {
		return result, err
	}
	if err := normalizeFabricAdjudicationTimes(input); err != nil {
		return result, err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return result, fmt.Errorf("encode conflict adjudication response: %w", err)
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return result, fmt.Errorf("decode conflict adjudication response: %w", err)
	}
	return result, nil
}

func (a *fabricConflictAdjudicator) AdjudicateBatch(ctx context.Context,
	request memory.AdjudicationBatchRequest) (memory.AdjudicationBatchResponse, error) {
	var result memory.AdjudicationBatchResponse
	if len(request.Items) == 0 {
		return result, nil
	}
	if len(request.Items) > 8 {
		return result, fmt.Errorf("conflict adjudication batch has %d items; maximum is 8", len(request.Items))
	}
	client, err := a.structuredClient(4096)
	if err != nil {
		return result, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return result, fmt.Errorf("encode conflict adjudication batch: %w", err)
	}
	response, err := client.CompleteStructured(fabricAPIContext(ctx, a.cfg), fabricBatchAdjudicatorSystemPrompt,
		[]map[string]any{{"role": "user", "content": string(payload)}}, api.StructuredCompletionOptions{
			MaxTokens: 4096, Tools: []map[string]any{fabricBatchAdjudicatorToolSchema()},
			RequiredTool: fabricBatchAdjudicatorToolName, DisableThinking: true,
		})
	result.Usage = memory.APIUsage{Calls: 1, InputTokens: response.InputTokens,
		CacheReadInputTokens: response.CacheReadInputTokens, CacheCreationInputTokens: response.CacheCreationInputTokens,
		OutputTokens: response.OutputTokens, Model: a.model}
	if err != nil {
		return result, err
	}
	input, err := structuredMemoryInput(response, fabricBatchAdjudicatorToolName)
	if err != nil {
		return result, err
	}
	if err := normalizeFabricAdjudicationTimes(input); err != nil {
		return result, err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return result, fmt.Errorf("encode conflict adjudication batch response: %w", err)
	}
	usage := result.Usage
	if err := json.Unmarshal(encoded, &result); err != nil {
		return result, fmt.Errorf("decode conflict adjudication batch response: %w", err)
	}
	result.Usage = usage
	if len(result.Results) > len(request.Items) {
		return result, fmt.Errorf("batch adjudicator returned %d results for %d conflicts", len(result.Results), len(request.Items))
	}
	return result, nil
}

func (a *fabricConflictAdjudicator) structuredClient(maxTokens int) (api.StructuredCompletionClient, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil || a.err != nil {
		return a.client, a.err
	}
	if strings.TrimSpace(a.model) == "" {
		a.err = errors.New("memory conflict adjudicator model is not configured")
		return nil, a.err
	}
	cfg := a.cfg
	cfg.FallbackAPIEnabled = false
	a.client, a.err = a.factory(cfg, a.model, maxTokens, fabricSemanticRetryConfig())
	return a.client, a.err
}

func fabricSemanticRetryConfig() *api.RetryConfig {
	retry := api.DefaultRetryConfig()
	retry.MaxRetries = 1
	return &retry
}

func fabricAPIContext(ctx context.Context, cfg config.Config) context.Context {
	return api.ContextWithStreamIdleTimeout(ctx,
		time.Duration(cfg.APIStreamIdleTimeoutSeconds*float64(time.Second)))
}

const fabricCompilerSystemPrompt = `You are the Semantic Compiler for Lumina Memory Fabric.
The local planner has already selected a small set of high-value source spans. Compile only durable cross-session memory: user profile facts, preferences, constraints, corrections, changing state, goals, personal episodes, confirmed outcomes, and reusable procedures. Do not preserve generic assistant explanations, trivia, tutorials, or unconfirmed suggestions.

Produce grounded fact, state, preference, constraint, episode, or procedure nodes. Every source must contain source_ref and a short exact quote copied verbatim from that source. Never emit event IDs or character, byte, token, or rune offsets; the runtime maps stable source references and computes offsets locally. Do not combine evidence from different sessions into one node. Normalize subject, facet, attribute, scope, typed value, valid time, keys, and prospective retrieval cues. Conflict and revision decisions are deliberately handled by local commit rules and a separate adjudicator.

Treat max_nodes and max_output_tokens as hard limits. Emit at most one compact node per source claim, merge redundant wording, keep statements under 240 characters, exact quotes to the shortest sufficient clause, keys to short canonical terms, and retrieval cues to short likely queries. Return no narration or reasoning and omit empty optional fields. Do not invent evidence, source references, aliases, values, dates, paths, or entities. A claim requires claim_type, subject, facet, attribute_key, scope, value, and evidence_mode. Episodes and procedures use kind=episode or kind=procedure. Use observed for tool evidence, user_declared for explicit user statements, and inferred only when directly entailed. Omit unknown optional dates. The nodes field must always be a JSON array, never null; use an empty array only when none of the supplied sources merits durable memory. Call CompileSemanticMemory exactly once.`

const fabricAdjudicatorSystemPrompt = `You are the independent Conflict Adjudicator for Lumina Memory Fabric.
Use only the supplied claims, exact source excerpts, scope, time, and authority policy. Decide supersedes, coexists, scoped, duplicate, uncertain, or needs_recompile. Select only existing claim IDs and existing typed values. Do not synthesize a third fact. Use uncertain when evidence or intent is insufficient; uncertainty is preferable to a false overwrite. New synthesis must return needs_recompile. For a decisive result, winner_ids and loser_ids must form a valid partition of the relevant conflict members, and support_ids must reference supplied claims. Call AdjudicateMemoryConflict exactly once.`

const fabricBatchAdjudicatorSystemPrompt = `You are the independent Conflict Adjudicator for Lumina Memory Fabric.
Resolve each supplied conflict independently using only its claims, exact source excerpts, scope, time, and authority policy. Return one result per conflict_id. Decide supersedes, coexists, scoped, duplicate, uncertain, or needs_recompile. Select only existing claim IDs and typed values. Never synthesize a third fact. Use uncertain when evidence or intent is insufficient. For decisive supersedes or duplicate results, winner_ids and loser_ids must partition the conflict members. Call AdjudicateMemoryConflicts exactly once.`

func fabricCompilerToolSchema() map[string]any {
	return fabricCompilerToolSchemaFor(fabricCompilerMaxNodes)
}

func fabricCompilerToolSchemaFor(maxNodes int) map[string]any {
	maxNodes = normalizeFabricCompilerMaxNodes(maxNodes)
	maxAliases := minIntAgent(3, maxNodes)
	boundedString := func(maxLength int) map[string]any {
		return map[string]any{"type": "string", "maxLength": maxLength}
	}
	stringArray := func(maxItems, maxLength int) map[string]any {
		return map[string]any{"type": "array", "maxItems": maxItems, "items": boundedString(maxLength)}
	}
	timeField := map[string]any{"type": "string", "format": "date-time"}
	span := map[string]any{"type": "object", "additionalProperties": false,
		"required": []string{"source_ref", "text"}, "properties": map[string]any{
			"source_ref": boundedString(200), "text": map[string]any{"type": "string", "minLength": 1,
				"maxLength": 160, "description": "Shortest exact verbatim clause sufficient to ground the memory."},
			"role": boundedString(32),
		}}
	value := map[string]any{"type": "object", "additionalProperties": false,
		"required": []string{"kind"}, "properties": map[string]any{
			"kind": map[string]any{"type": "string", "enum": []string{"text", "number", "time", "list", "bool"}},
			"text": boundedString(256), "number": map[string]any{"type": "number"},
			"unit": boundedString(48), "time": timeField, "list": stringArray(12, 128),
			"bool": map[string]any{"type": "boolean"},
		}}
	scope := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"project": boundedString(160), "environment": boundedString(160),
		"actor": boundedString(96), "condition": boundedString(160),
	}}
	draft := map[string]any{"type": "object", "additionalProperties": false,
		"required": []string{"kind", "statement", "sources"}, "properties": map[string]any{
			"kind":       map[string]any{"type": "string", "enum": []string{"claim", "episode", "procedure"}},
			"claim_type": map[string]any{"type": "string", "enum": []string{"fact", "state", "preference", "constraint"}},
			"statement":  map[string]any{"type": "string", "minLength": 1, "maxLength": 200}, "subject": boundedString(160),
			"subject_type":  boundedString(96),
			"facet":         map[string]any{"type": "string", "enum": []string{"profile", "state", "preference", "constraint", "configuration", "location", "relationship", "goal", "procedure-state"}},
			"attribute_key": boundedString(128), "scope": scope, "value": value,
			"valid_from": timeField, "valid_until": timeField,
			"evidence_mode": map[string]any{"type": "string", "enum": []string{"observed", "user_declared", "inferred"}},
			"sources":       map[string]any{"type": "array", "minItems": 1, "maxItems": 2, "items": span},
			"keys":          stringArray(6, 64), "retrieval_cues": stringArray(4, 80),
		}, "allOf": []any{map[string]any{
			"if": map[string]any{"required": []string{"kind"}, "properties": map[string]any{
				"kind": map[string]any{"const": "claim"},
			}},
			"then": map[string]any{"required": []string{
				"claim_type", "subject", "facet", "attribute_key", "scope", "value", "evidence_mode",
			}},
		}}}
	alias := map[string]any{"type": "object", "additionalProperties": false,
		"required": []string{"canonical", "aliases", "sources"}, "properties": map[string]any{
			"canonical": boundedString(160), "type": boundedString(64),
			"aliases": stringArray(4, 160), "sources": map[string]any{"type": "array", "minItems": 1, "maxItems": 4, "items": span},
		}}
	return map[string]any{"name": fabricCompilerToolName,
		"description": "Submit grounded semantic memory proposals and identity alias proposals.",
		"input_schema": map[string]any{"type": "object", "additionalProperties": false,
			"required": []string{"nodes"}, "properties": map[string]any{
				"nodes":   map[string]any{"type": "array", "maxItems": maxNodes, "items": draft},
				"aliases": map[string]any{"type": "array", "maxItems": maxAliases, "items": alias},
			}},
	}
}

func fabricAdjudicatorToolSchema() map[string]any {
	stringArray := func(max int) map[string]any {
		return map[string]any{"type": "array", "maxItems": max, "items": map[string]any{"type": "string"}}
	}
	timeField := map[string]any{"type": "string", "format": "date-time"}
	return map[string]any{"name": fabricAdjudicatorToolName,
		"description": "Resolve one supplied memory conflict without creating new facts.",
		"input_schema": map[string]any{"type": "object", "additionalProperties": false,
			"required": []string{"decision", "winner_ids", "loser_ids", "support_ids", "reason"},
			"properties": map[string]any{
				"decision":   map[string]any{"type": "string", "enum": []string{"supersedes", "coexists", "scoped", "duplicate", "uncertain", "needs_recompile"}},
				"winner_ids": stringArray(32), "loser_ids": stringArray(32),
				"conditions": map[string]any{"type": "string"}, "valid_from": timeField, "valid_until": timeField,
				"support_ids": stringArray(32), "reason": map[string]any{"type": "string"},
			}},
	}
}

func fabricBatchAdjudicatorToolSchema() map[string]any {
	stringArray := func(max int) map[string]any {
		return map[string]any{"type": "array", "maxItems": max, "items": map[string]any{"type": "string"}}
	}
	timeField := map[string]any{"type": "string", "format": "date-time"}
	item := map[string]any{"type": "object", "additionalProperties": false,
		"required": []string{"conflict_id", "decision", "winner_ids", "loser_ids", "support_ids", "reason"},
		"properties": map[string]any{
			"conflict_id": map[string]any{"type": "string"},
			"decision": map[string]any{"type": "string", "enum": []string{
				"supersedes", "coexists", "scoped", "duplicate", "uncertain", "needs_recompile"}},
			"winner_ids": stringArray(32), "loser_ids": stringArray(32),
			"conditions": map[string]any{"type": "string"}, "valid_from": timeField, "valid_until": timeField,
			"support_ids": stringArray(32), "reason": map[string]any{"type": "string"},
		}}
	return map[string]any{"name": fabricBatchAdjudicatorToolName,
		"description": "Resolve up to eight supplied memory conflicts in one call.",
		"input_schema": map[string]any{"type": "object", "additionalProperties": false,
			"required": []string{"results"}, "properties": map[string]any{
				"results": map[string]any{"type": "array", "maxItems": 8, "items": item},
			}},
	}
}
