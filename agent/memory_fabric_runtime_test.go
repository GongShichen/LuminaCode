package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/memory"
)

type fabricEngineStub struct {
	appendCalls   int
	rememberCalls int
	searchCalls   int
	sealCalls     int
	lastPolicy    memory.SemanticPolicy
	lastEvents    []memory.RawEvent
	lastRemember  memory.MemoryRequest
	searchResult  memory.SearchResult
	searchErr     error
}

func (s *fabricEngineStub) AppendEvents(_ context.Context, events []memory.RawEvent,
	options memory.IngestOptions) (memory.IngestResult, error) {
	s.appendCalls++
	s.lastPolicy = options.SemanticPolicy
	s.lastEvents = append([]memory.RawEvent(nil), events...)
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	return memory.IngestResult{Durable: true, EventIDs: ids, SemanticStatus: memory.SemanticEventDurable}, nil
}

func (s *fabricEngineStub) Remember(_ context.Context, request memory.MemoryRequest) (memory.MemoryCommitResult, error) {
	s.rememberCalls++
	s.lastRemember = request
	return memory.MemoryCommitResult{Durable: true, SemanticStatus: memory.SemanticActive,
		EventIDs: request.SourceEventIDs, MemoryIDs: []string{"memory-1"}}, nil
}

func (s *fabricEngineStub) SealContext(context.Context, memory.ContextRef) (memory.JobRef, error) {
	s.sealCalls++
	return memory.JobRef{ID: "job-1", Kind: "compile_events", Status: "pending"}, nil
}

func (s *fabricEngineStub) Search(context.Context, memory.SearchRequest) (memory.SearchResult, error) {
	s.searchCalls++
	return s.searchResult, s.searchErr
}

func (*fabricEngineStub) PrioritizeConflicts(context.Context, memory.ConflictSelector) (memory.JobRef, error) {
	return memory.JobRef{}, nil
}

func (*fabricEngineStub) Forget(context.Context, memory.Selector, memory.ForgetMode) error {
	return nil
}

func (*fabricEngineStub) Doctor(context.Context) (memory.HealthReport, error) {
	return memory.HealthReport{Healthy: true}, nil
}

func (*fabricEngineStub) Close() error { return nil }

func fabricAgentTestConfig() config.Config {
	return config.Config{LongTermMemoryEnabled: true, MemoryBackend: "fabric", CWD: "/tmp/project",
		MemoryRecallMaxItems: 8, MemoryContextMaxTokens: 2500, MemoryContextTargetTokens: 1500}
}

func TestFabricRecallUsesLocalSearchOnce(t *testing.T) {
	engine := &fabricEngineStub{searchResult: memory.SearchResult{
		Route: []string{"lexical"}, Evidence: []memory.Evidence{{ID: "doc-1", ResourceID: "event-1",
			ResourceKind: "event", Content: "The workspace uses Vim.", ContextID: "session-1", Actor: "user",
			SourceEventIDs: []string{"event-1"}}}}}
	recalled := RunMemoryRecallWithEngine(context.Background(), fabricAgentTestConfig(),
		&AgentState{MemoryQueryTime: time.Now().UTC()}, "Which editor?", engine)
	if engine.searchCalls != 1 {
		t.Fatalf("Fabric recall performed %d searches", engine.searchCalls)
	}
	if len(recalled) != 1 || !strings.Contains(recalled[0].Content, "workspace uses Vim") {
		t.Fatalf("unexpected Fabric recall: %#v", recalled)
	}
	if !strings.Contains(recalled[0].Content, "context=session-1 actor=user") {
		t.Fatalf("Fabric recall omitted evidence provenance: %#v", recalled)
	}
}

func TestFabricExtractionOrdinaryTurnOnlyAppendsRawEvents(t *testing.T) {
	engine := &fabricEngineStub{}
	cfg := fabricAgentTestConfig()
	controller := NewExtractionController(cfg)
	controller.Engine = engine
	state := NewAgentState()
	state.MemorySessionID = "session-1"
	state.Messages = []map[string]any{
		{"role": "user", "content": "We finished the Atlas task."},
		{"role": "assistant", "content": "The tests passed."},
	}
	if _, err := controller.ExtractNow(context.Background(), &state); err != nil {
		t.Fatal(err)
	}
	if engine.appendCalls != 1 || engine.rememberCalls != 0 || len(engine.lastEvents) != 2 {
		t.Fatalf("ordinary turn calls append=%d remember=%d events=%d", engine.appendCalls,
			engine.rememberCalls, len(engine.lastEvents))
	}
	if engine.lastPolicy != memory.SemanticDeferred || state.MemoryExtractionCursor != len(state.Messages) {
		t.Fatalf("ordinary turn policy=%s cursor=%d", engine.lastPolicy, state.MemoryExtractionCursor)
	}
}

func TestFabricExtractionExplicitTurnCompilesExactlyOnce(t *testing.T) {
	engine := &fabricEngineStub{}
	cfg := fabricAgentTestConfig()
	controller := NewExtractionController(cfg)
	controller.Engine = engine
	state := NewAgentState()
	state.MemorySessionID = "session-1"
	state.Messages = []map[string]any{{"role": "user", "content": "记住我的编辑器偏好是 Vim。"}}
	if _, err := controller.ExtractNow(context.Background(), &state); err != nil {
		t.Fatal(err)
	}
	if engine.appendCalls != 1 || engine.rememberCalls != 1 {
		t.Fatalf("explicit turn calls append=%d remember=%d", engine.appendCalls, engine.rememberCalls)
	}
	if engine.lastPolicy != memory.SemanticDurableOnly || !engine.lastRemember.RequireSemantic ||
		engine.lastRemember.Mode != memory.WritePreference || len(engine.lastRemember.SourceEventIDs) != 1 {
		t.Fatalf("unexpected explicit request: policy=%s request=%+v", engine.lastPolicy, engine.lastRemember)
	}
}

type fabricStructuredStub struct {
	response  api.Response
	err       error
	responses []api.Response
	errors    []error
	calls     int
	opts      api.StructuredCompletionOptions
	messages  []map[string]any
}

func (s *fabricStructuredStub) CompleteStructured(_ context.Context, _ string, messages []map[string]any,
	opts api.StructuredCompletionOptions) (api.Response, error) {
	s.calls++
	s.opts = opts
	s.messages = messages
	index := s.calls - 1
	if index < len(s.responses) {
		var err error
		if index < len(s.errors) {
			err = s.errors[index]
		}
		return s.responses[index], err
	}
	return s.response, s.err
}

func TestFabricCompilerUsesDedicatedStructuredContract(t *testing.T) {
	input := map[string]any{"nodes": []any{map[string]any{
		"kind": "claim", "claim_type": "preference", "statement": "The user prefers Vim.",
		"subject": "user", "facet": "preference", "attribute_key": "editor",
		"value": map[string]any{"kind": "text", "text": "Vim"}, "evidence_mode": "user_declared",
		"sources": []any{map[string]any{"source_ref": "source-1", "text": "The user prefers Vim."}},
	}}}
	stub := &fabricStructuredStub{response: api.Response{InputTokens: 80, OutputTokens: 20,
		ToolCalls: []map[string]any{{"name": fabricCompilerToolName, "input": input}}}}
	cfg := fabricAgentTestConfig()
	cfg.APIModel = "primary"
	compiler := newFabricSemanticCompiler(cfg, "compiler-model")
	var gotConfig config.Config
	var gotRetry *api.RetryConfig
	compiler.factory = func(cfg config.Config, _ string, _ int, retry *api.RetryConfig) (api.StructuredCompletionClient, error) {
		gotConfig, gotRetry = cfg, retry
		return stub, nil
	}
	result, err := compiler.Compile(context.Background(), memory.CompileRequest{
		Sources: []memory.CompileSource{{SourceRef: "source-1", SessionRef: "session-1", Actor: "user",
			Text: "The user prefers Vim."}}, MaxOutputTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 || stub.opts.RequiredTool != fabricCompilerToolName || !stub.opts.DisableThinking ||
		len(result.Nodes) != 1 || result.Usage.Calls != 1 || result.Usage.Model != "compiler-model" {
		t.Fatalf("unexpected compiler call: opts=%+v result=%+v", stub.opts, result)
	}
	if gotConfig.FallbackAPIEnabled || gotRetry == nil || gotRetry.MaxRetries != 1 {
		t.Fatalf("compiler failure policy is unsafe: fallback=%t retry=%+v", gotConfig.FallbackAPIEnabled, gotRetry)
	}
}

func TestFabricCompilerRepairsInvalidGeneratedContractOnce(t *testing.T) {
	valid := map[string]any{"nodes": []any{map[string]any{
		"kind": "claim", "claim_type": "preference", "statement": "The user prefers Vim.",
		"subject": "user", "facet": "preference", "attribute_key": "editor", "scope": map[string]any{},
		"value": map[string]any{"kind": "text", "text": "Vim"}, "evidence_mode": "user_declared",
		"sources": []any{map[string]any{"source_ref": "source-repair", "text": "prefer Vim"}},
	}}}
	stub := &fabricStructuredStub{responses: []api.Response{
		{InputTokens: 70, OutputTokens: 18, ToolCalls: []map[string]any{{"name": fabricCompilerToolName,
			"input": map[string]any{"nodes": `not valid JSON`}}}},
		{InputTokens: 92, CacheReadInputTokens: 40, OutputTokens: 20,
			ToolCalls: []map[string]any{{"name": fabricCompilerToolName, "input": valid}}},
	}}
	compiler := newFabricSemanticCompiler(fabricAgentTestConfig(), "compiler-model")
	compiler.cacheDir = t.TempDir()
	compiler.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return stub, nil
	}
	result, err := compiler.Compile(context.Background(), memory.CompileRequest{
		Sources: []memory.CompileSource{{SourceRef: "source-repair", SessionRef: "session-repair",
			Actor: "user", Text: "I prefer Vim."}}, MaxInputTokens: 20_000, MaxOutputTokens: 2048, MaxNodes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 2 || len(result.Nodes) != 1 || result.Usage.Calls != 2 ||
		result.Usage.InputTokens != 162 || result.Usage.CacheReadInputTokens != 40 || result.Usage.OutputTokens != 38 {
		t.Fatalf("unexpected repaired compile: calls=%d result=%+v", stub.calls, result)
	}
	if len(stub.messages) != 2 || !strings.Contains(fmt.Sprint(stub.messages[1]["content"]),
		"failed local contract validation") {
		t.Fatalf("repair feedback was not sent to the model: %#v", stub.messages)
	}
}

func TestFabricCompilerBoundsGeneratedContractRepairsAtThree(t *testing.T) {
	stub := &fabricStructuredStub{response: api.Response{InputTokens: 10, OutputTokens: 5,
		ToolCalls: []map[string]any{{"name": fabricCompilerToolName,
			"input": map[string]any{"nodes": `still not valid JSON`}}}}}
	compiler := newFabricSemanticCompiler(fabricAgentTestConfig(), "compiler-model")
	compiler.cacheDir = t.TempDir()
	compiler.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return stub, nil
	}
	result, err := compiler.Compile(context.Background(), memory.CompileRequest{
		Sources: []memory.CompileSource{{SourceRef: "source-invalid", SessionRef: "session-invalid",
			Actor: "user", Text: "I prefer Vim."}}, MaxInputTokens: 20_000, MaxOutputTokens: 2048, MaxNodes: 8,
	})
	var contractErr *memory.CompileContractError
	if !errors.As(err, &contractErr) {
		t.Fatalf("error=%v, want CompileContractError", err)
	}
	wantCalls := 1 + fabricCompilerContractRepairRetries
	if stub.calls != wantCalls || result.Usage.Calls != wantCalls ||
		result.Usage.InputTokens != wantCalls*10 || result.Usage.OutputTokens != wantCalls*5 {
		t.Fatalf("unbounded or unrecorded repair calls: calls=%d usage=%+v", stub.calls, result.Usage)
	}
}

func TestFabricCompilerRepairsLocallyWithoutGenerationRetry(t *testing.T) {
	node := `{"kind":"claim","claim_type":"preference","statement":"The user prefers Vim.",` +
		`"subject":"user","facet":"preference","attribute_key":"editor","scope":{},` +
		`"value":{"kind":"text","text":"Vim"},"evidence_mode":"user_declared",` +
		`"sources":[{"source_ref":"source-local","text":"prefer Vim"}]}`
	stub := &fabricStructuredStub{response: api.Response{InputTokens: 70, OutputTokens: 20,
		ToolCalls: []map[string]any{{"name": fabricCompilerToolName,
			"input": map[string]any{"nodes": `[` + node + ` : ` + node + `]`}}}}}
	compiler := newFabricSemanticCompiler(fabricAgentTestConfig(), "compiler-model")
	compiler.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return stub, nil
	}
	result, err := compiler.Compile(context.Background(), memory.CompileRequest{
		Sources: []memory.CompileSource{{SourceRef: "source-local", SessionRef: "session-local",
			Actor: "user", Text: "I prefer Vim."}}, MaxInputTokens: 20_000, MaxOutputTokens: 2048, MaxNodes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 || len(result.Nodes) != 2 || result.Usage.Calls != 1 {
		t.Fatalf("local parser repair used another generation: calls=%d result=%+v", stub.calls, result)
	}
}

func TestFabricCompilerRejectsOversizedInputBeforeAPI(t *testing.T) {
	stub := &fabricStructuredStub{}
	compiler := newFabricSemanticCompiler(fabricAgentTestConfig(), "compiler-model")
	compiler.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return stub, nil
	}
	_, err := compiler.Compile(context.Background(), memory.CompileRequest{
		Sources:        []memory.CompileSource{{SourceRef: "source-1", Actor: "user", Text: strings.Repeat("durable fact ", 200)}},
		MaxInputTokens: 64, MaxOutputTokens: 2048, MaxNodes: 8})
	var budgetErr *memory.CompileInputBudgetError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("expected preflight budget error, got %v", err)
	}
	if stub.calls != 0 {
		t.Fatalf("oversized input made %d API calls", stub.calls)
	}
}

func TestFabricCompilerNormalizesDateOnlyTimesLocally(t *testing.T) {
	cfg := fabricAgentTestConfig()
	cfg.MemoryArtifactPath = filepath.Join(t.TempDir(), "shared-artifacts")
	stub := &fabricStructuredStub{response: api.Response{InputTokens: 80, OutputTokens: 40,
		ToolCalls: []map[string]any{{"name": fabricCompilerToolName, "input": map[string]any{
			"nodes": []any{map[string]any{
				"kind": "claim", "claim_type": "state", "statement": "The appointment is on May 28.",
				"subject": "appointment", "facet": "state", "attribute_key": "date",
				"scope": map[string]any{}, "value": map[string]any{"kind": "time", "time": "2023-05-28"},
				"valid_from": "2023-05-28", "valid_until": "", "evidence_mode": "user_declared",
				"sources": []any{map[string]any{"source_ref": "source-date", "text": "May 28"}},
			}},
		}}}}}
	compiler := newFabricSemanticCompiler(cfg, "compiler-model")
	compiler.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return stub, nil
	}
	result, err := compiler.Compile(context.Background(), memory.CompileRequest{
		Sources: []memory.CompileSource{{SourceRef: "source-date", SessionRef: "session-date",
			Actor: "user", Text: "The appointment is on May 28."}},
		MaxInputTokens: 20_000, MaxOutputTokens: 2048, MaxNodes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(result.Nodes))
	}
	want := time.Date(2023, time.May, 28, 0, 0, 0, 0, time.UTC)
	if !result.Nodes[0].ValidFrom.Equal(want) || !result.Nodes[0].Value.Time.Equal(want) {
		t.Fatalf("normalized times = %s / %s, want %s", result.Nodes[0].ValidFrom,
			result.Nodes[0].Value.Time, want)
	}
	if !result.Nodes[0].ValidUntil.IsZero() {
		t.Fatalf("empty valid_until = %s, want zero", result.Nodes[0].ValidUntil)
	}
}

func TestFabricCompilerTrimsNodesAboveContractLimitWithoutRetry(t *testing.T) {
	cfg := fabricAgentTestConfig()
	cfg.MemoryArtifactPath = filepath.Join(t.TempDir(), "shared-artifacts")
	node := func(statement string) map[string]any {
		return map[string]any{"kind": "claim", "claim_type": "fact", "statement": statement,
			"subject": "user", "facet": "profile", "attribute_key": statement,
			"scope": map[string]any{}, "value": map[string]any{"kind": "text", "text": statement},
			"evidence_mode": "user_declared",
			"sources":       []any{map[string]any{"source_ref": "source-limit", "text": "Remember"}}}
	}
	stub := &fabricStructuredStub{response: api.Response{OutputTokens: 40,
		ToolCalls: []map[string]any{{"name": fabricCompilerToolName, "input": map[string]any{
			"nodes": []any{node("first"), node("second"), node("third")},
		}}}}}
	compiler := newFabricSemanticCompiler(cfg, "compiler-model")
	compiler.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return stub, nil
	}
	result, err := compiler.Compile(context.Background(), memory.CompileRequest{
		Sources: []memory.CompileSource{{SourceRef: "source-limit", SessionRef: "session-limit",
			Actor: "user", Text: "Remember these facts."}},
		MaxInputTokens: 20_000, MaxOutputTokens: 2048, MaxNodes: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 || len(result.Nodes) != 2 {
		t.Fatalf("calls=%d nodes=%d, want one call and two nodes", stub.calls, len(result.Nodes))
	}
}

func TestTrimFabricCompileResponseBoundsAliasesSeparately(t *testing.T) {
	response := memory.CompileResponse{Aliases: make([]memory.IdentityAliasProposal, 7)}
	trimmed := trimFabricCompileResponse(response, 10)
	if len(trimmed.Aliases) != 3 {
		t.Fatalf("aliases = %d, want 3", len(trimmed.Aliases))
	}
}

func TestNormalizeFabricAdjudicationTimesDropsEmptyAndNormalizesDate(t *testing.T) {
	input := map[string]any{"results": []any{
		map[string]any{"valid_from": "", "valid_until": nil},
		map[string]any{"valid_from": "2026-07-20", "valid_until": "2026-07-21T09:30:00"},
	}}
	if err := normalizeFabricAdjudicationTimes(input); err != nil {
		t.Fatal(err)
	}
	results := input["results"].([]any)
	first := results[0].(map[string]any)
	if _, ok := first["valid_from"]; ok {
		t.Fatalf("empty valid_from was retained: %#v", first)
	}
	if _, ok := first["valid_until"]; ok {
		t.Fatalf("null valid_until was retained: %#v", first)
	}
	second := results[1].(map[string]any)
	if second["valid_from"] != "2026-07-20T00:00:00Z" || second["valid_until"] != "2026-07-21T09:30:00Z" {
		t.Fatalf("times were not normalized: %#v", second)
	}
}

func TestNormalizeFabricCompilerTimesAnchorsTimeOfDayToSourceDate(t *testing.T) {
	input := map[string]any{"nodes": []any{map[string]any{
		"sources": []any{map[string]any{"source_ref": "session-1:turn-2", "text": "The alarm is at 06:30."}},
		"value":   map[string]any{"kind": "time", "time": "06:30"},
	}}}
	sources := []memory.CompileSource{{SourceRef: "session-1:turn-2",
		OccurredAt: time.Date(2026, time.July, 20, 18, 0, 0, 0, time.UTC)}}
	if err := normalizeFabricCompilerTimes(input, sources); err != nil {
		t.Fatal(err)
	}
	node := input["nodes"].([]any)[0].(map[string]any)
	value := node["value"].(map[string]any)
	if got, want := value["time"], "2026-07-20T06:30:00Z"; got != want {
		t.Fatalf("anchored time=%v, want %s", got, want)
	}
}

func TestFabricCompilerAcceptsCompleteToolPayloadAtLengthStop(t *testing.T) {
	cfg := fabricAgentTestConfig()
	cfg.MemoryArtifactPath = filepath.Join(t.TempDir(), "shared-artifacts")
	stub := &fabricStructuredStub{response: api.Response{StopReason: "max_tokens", OutputTokens: 2048,
		ToolCalls: []map[string]any{{"name": fabricCompilerToolName, "input": map[string]any{
			"nodes": []any{map[string]any{
				"kind": "claim", "claim_type": "fact", "statement": "The user prefers Vim.",
				"subject": "user", "facet": "preference", "attribute_key": "editor",
				"scope": map[string]any{}, "value": map[string]any{"kind": "text", "text": "Vim"},
				"evidence_mode": "user_declared",
				"sources":       []any{map[string]any{"source_ref": "source-length", "text": "prefer Vim"}},
			}},
		}}}}}
	compiler := newFabricSemanticCompiler(cfg, "compiler-model")
	compiler.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return stub, nil
	}
	result, err := compiler.Compile(context.Background(), memory.CompileRequest{
		Sources: []memory.CompileSource{{SourceRef: "source-length", SessionRef: "session-length",
			Actor: "user", Text: "I prefer Vim."}},
		MaxInputTokens: 20_000, MaxOutputTokens: 2048, MaxNodes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || stub.calls != 1 {
		t.Fatalf("nodes=%d calls=%d, want one usable response", len(result.Nodes), stub.calls)
	}
}

func TestFabricCompilerReusesFilesystemCacheAcrossInstances(t *testing.T) {
	input := map[string]any{"nodes": []any{}}
	request := memory.CompileRequest{Sources: []memory.CompileSource{{
		SourceRef: "source-cache", SessionRef: "session-cache", Actor: "user", Text: "Remember the quartz profile.",
	}}, MaxInputTokens: 20_000, MaxOutputTokens: 2048, MaxNodes: 8}
	cfg := fabricAgentTestConfig()
	cfg.MemoryPath = filepath.Join(t.TempDir(), "case-store")
	firstStub := &fabricStructuredStub{response: api.Response{InputTokens: 80, OutputTokens: 4,
		ToolCalls: []map[string]any{{"name": fabricCompilerToolName, "input": input}}}}
	first := newFabricSemanticCompiler(cfg, "compiler-model")
	first.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return firstStub, nil
	}
	firstResult, err := first.Compile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if firstStub.calls != 1 || firstResult.Usage.Calls != 1 {
		t.Fatalf("unexpected first compile: calls=%d usage=%+v", firstStub.calls, firstResult.Usage)
	}

	secondStub := &fabricStructuredStub{}
	second := newFabricSemanticCompiler(cfg, "compiler-model")
	second.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return secondStub, nil
	}
	// Artifact identity is session-content based, not coupled to the batch's
	// output and node budgets.
	request.MaxOutputTokens = 768
	request.MaxNodes = 3
	secondResult, err := second.Compile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if secondStub.calls != 0 || secondResult.Usage.Calls != 0 || !secondResult.CacheHit {
		t.Fatalf("cached compile used API: calls=%d usage=%+v", secondStub.calls, secondResult.Usage)
	}
}

func TestFabricCompilerDoesNotCacheTruncatedEmptyResponse(t *testing.T) {
	cfg := fabricAgentTestConfig()
	cfg.MemoryArtifactPath = filepath.Join(t.TempDir(), "shared-artifacts")
	request := memory.CompileRequest{Sources: []memory.CompileSource{{
		SourceRef: "source-truncated", SessionRef: "session-truncated", Actor: "user",
		Text: "I prefer the quartz profile.",
	}}, MaxInputTokens: 20_000, MaxOutputTokens: 768, MaxNodes: 3}

	truncatedStub := &fabricStructuredStub{response: api.Response{StopReason: "length", OutputTokens: 768,
		ToolCalls: []map[string]any{{"name": fabricCompilerToolName, "input": map[string]any{"nodes": []any{}}}}}}
	first := newFabricSemanticCompiler(cfg, "compiler-model")
	first.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return truncatedStub, nil
	}
	_, err := first.Compile(context.Background(), request)
	var contractErr *memory.CompileContractError
	if !errors.As(err, &contractErr) {
		t.Fatalf("truncated output error = %v, want CompileContractError", err)
	}

	validStub := &fabricStructuredStub{response: api.Response{OutputTokens: 4,
		ToolCalls: []map[string]any{{"name": fabricCompilerToolName, "input": map[string]any{"nodes": []any{}}}}}}
	second := newFabricSemanticCompiler(cfg, "compiler-model")
	second.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return validStub, nil
	}
	if _, err := second.Compile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if validStub.calls != 1 {
		t.Fatalf("truncated empty result poisoned cache; subsequent calls=%d", validStub.calls)
	}
}

func TestFabricCompilerExplicitSkippedArtifactAvoidsAPICall(t *testing.T) {
	cfg := fabricAgentTestConfig()
	cfg.MemoryArtifactPath = filepath.Join(t.TempDir(), "shared-artifacts")
	sources := []memory.CompileSource{{SourceRef: "source-skipped", SessionRef: "session-skipped",
		Actor: "user", Text: "A durable source that was intentionally left raw-only."}}
	compiler := newFabricSemanticCompiler(cfg, "compiler-model")
	if err := compiler.MarkCompileArtifactSkipped(sources); err != nil {
		t.Fatal(err)
	}
	if cached, err := compiler.CompileArtifactCached(sources); err != nil || !cached {
		t.Fatalf("skipped artifact cached=%v err=%v", cached, err)
	}
	stub := &fabricStructuredStub{}
	compiler.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return stub, nil
	}
	result, err := compiler.Compile(context.Background(), memory.CompileRequest{Sources: sources,
		MaxInputTokens: 20_000, MaxOutputTokens: 768, MaxNodes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 0 || !result.CacheHit || len(result.Nodes)+len(result.Aliases) != 0 {
		t.Fatalf("skipped artifact called API or returned data: calls=%d result=%+v", stub.calls, result)
	}
}

func TestFabricCompilerCompilesOnlyMissingSessionArtifacts(t *testing.T) {
	node := func(sourceRef, statement string) map[string]any {
		return map[string]any{"kind": "claim", "claim_type": "preference", "statement": statement,
			"subject": "user", "facet": "preference", "attribute_key": sourceRef,
			"value": map[string]any{"kind": "text", "text": statement}, "evidence_mode": "user_declared",
			"sources": []any{map[string]any{"source_ref": sourceRef, "text": statement}}}
	}
	cfg := fabricAgentTestConfig()
	cfg.MemoryArtifactPath = filepath.Join(t.TempDir(), "shared-artifacts")
	firstStub := &fabricStructuredStub{response: api.Response{InputTokens: 100, OutputTokens: 20,
		ToolCalls: []map[string]any{{"name": fabricCompilerToolName, "input": map[string]any{
			"nodes": []any{node("source-a", "I prefer Vim."), node("source-b", "I prefer Go.")},
		}}}}}
	first := newFabricSemanticCompiler(cfg, "compiler-model")
	first.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return firstStub, nil
	}
	request := memory.CompileRequest{Sources: []memory.CompileSource{
		{SourceRef: "source-a", SessionRef: "session-a", Actor: "user", Text: "I prefer Vim."},
		{SourceRef: "source-b", SessionRef: "session-b", Actor: "user", Text: "I prefer Go."},
	}, MaxInputTokens: 20_000, MaxOutputTokens: 2048, MaxNodes: 8}
	if _, err := first.Compile(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	secondStub := &fabricStructuredStub{response: api.Response{InputTokens: 60, OutputTokens: 10,
		ToolCalls: []map[string]any{{"name": fabricCompilerToolName, "input": map[string]any{
			"nodes": []any{node("source-c", "I prefer Rust.")},
		}}}}}
	second := newFabricSemanticCompiler(cfg, "compiler-model")
	second.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return secondStub, nil
	}
	request.Sources = []memory.CompileSource{
		{SourceRef: "source-a", SessionRef: "session-a", Actor: "user", Text: "I prefer Vim."},
		{SourceRef: "source-c", SessionRef: "session-c", Actor: "user", Text: "I prefer Rust."},
	}
	result, err := second.Compile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if secondStub.calls != 1 || len(result.Nodes) != 2 || result.Usage.Calls != 1 {
		t.Fatalf("partial cache result: calls=%d result=%+v", secondStub.calls, result)
	}
	if len(secondStub.messages) != 1 {
		t.Fatalf("missing API request payload: %+v", secondStub.messages)
	}
	var transmitted memory.CompileRequest
	if err := json.Unmarshal([]byte(secondStub.messages[0]["content"].(string)), &transmitted); err != nil {
		t.Fatal(err)
	}
	if len(transmitted.Sources) != 1 || transmitted.Sources[0].SessionRef != "session-c" {
		t.Fatalf("cached session was retransmitted: %+v", transmitted.Sources)
	}
}

func TestFabricCompilerSchemaRequiresClaimProjection(t *testing.T) {
	schema := fabricCompilerToolSchema()
	input := schema["input_schema"].(map[string]any)
	properties := input["properties"].(map[string]any)
	nodes := properties["nodes"].(map[string]any)
	node := nodes["items"].(map[string]any)
	allOf := node["allOf"].([]any)
	then := allOf[0].(map[string]any)["then"].(map[string]any)
	required := then["required"].([]string)
	for _, field := range []string{"claim_type", "subject", "facet", "attribute_key", "scope", "value", "evidence_mode"} {
		found := false
		for _, candidate := range required {
			found = found || candidate == field
		}
		if !found {
			t.Fatalf("claim projection field %q is not required: %v", field, required)
		}
	}
}

func TestFabricCompilerSchemaHonorsNodeAndFieldBounds(t *testing.T) {
	schema := fabricCompilerToolSchemaFor(5)
	input := schema["input_schema"].(map[string]any)
	properties := input["properties"].(map[string]any)
	nodes := properties["nodes"].(map[string]any)
	if nodes["maxItems"] != 5 {
		t.Fatalf("node bound = %v, want 5", nodes["maxItems"])
	}
	draft := nodes["items"].(map[string]any)
	statement := draft["properties"].(map[string]any)["statement"].(map[string]any)
	if statement["maxLength"] == nil {
		t.Fatal("statement has no hard length bound")
	}
	if _, exists := properties["audit"]; exists {
		t.Fatal("unbounded compiler audit field is still exposed")
	}
	sources := draft["properties"].(map[string]any)["sources"].(map[string]any)
	span := sources["items"].(map[string]any)
	spanProperties := span["properties"].(map[string]any)
	if _, exists := spanProperties["start_rune"]; exists {
		t.Fatal("compiler is still asked to calculate start_rune")
	}
	if _, exists := spanProperties["end_rune"]; exists {
		t.Fatal("compiler is still asked to calculate end_rune")
	}
	if spanProperties["text"] == nil {
		t.Fatal("compiler source quote is not required by the schema")
	}
	required := span["required"].([]string)
	if len(required) != 2 || required[0] != "source_ref" || required[1] != "text" {
		t.Fatalf("compiler source contract = %v, want source_ref+text", required)
	}
}

func TestFabricCompilerAcceptsStringEncodedArrayFields(t *testing.T) {
	nodes := `[{"kind":"claim","claim_type":"preference","statement":"The user prefers Vim.","subject":"user","facet":"preference","attribute_key":"editor","value":{"kind":"text","text":"Vim"},"evidence_mode":"user_declared","sources":[{"source_ref":"source-1","text":"The user prefers Vim."}]}]`
	stub := &fabricStructuredStub{response: api.Response{ToolCalls: []map[string]any{{
		"name":  fabricCompilerToolName,
		"input": map[string]any{"nodes": nodes, "aliases": `[]`},
	}}}}
	compiler := newFabricSemanticCompiler(fabricAgentTestConfig(), "compiler-model")
	compiler.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return stub, nil
	}
	result, err := compiler.Compile(context.Background(), memory.CompileRequest{
		Sources:         []memory.CompileSource{{SourceRef: "source-1", SessionRef: "session-1", Text: "The user prefers Vim."}},
		MaxOutputTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Statement != "The user prefers Vim." {
		t.Fatalf("unexpected decoded compiler response: %+v", result)
	}
}

func TestDecodeStructuredJSONArrayRepairsObjectCollections(t *testing.T) {
	for _, input := range []string{
		`{{"kind":"claim","statement":"remember quartz","sources":[]},{"kind":"episode","statement":"visited Rome","sources":[]}}`,
		"{\"kind\":\"claim\",\"statement\":\"remember quartz\",\"sources\":[]}\n{\"kind\":\"episode\",\"statement\":\"visited Rome\",\"sources\":[]}",
		`{"kind":"claim","statement":"remember quartz","sources":[]}`,
	} {
		decoded, err := decodeStructuredJSONArray(input)
		if err != nil {
			t.Fatalf("decode %q: %v", input, err)
		}
		if _, ok := decoded.([]any); !ok {
			t.Fatalf("decode %q returned %T", input, decoded)
		}
	}
}

func TestDecodeStructuredJSONArrayRepairsColonBetweenArrayElements(t *testing.T) {
	input := `[{"kind":"claim","statement":"remember quartz","sources":[]} : {"kind":"episode","statement":"visited Rome","sources":[]}]`
	decoded, err := decodeStructuredJSONArray(input)
	if err != nil {
		t.Fatal(err)
	}
	items, ok := decoded.([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("unexpected decoded items: %#v", decoded)
	}
}

func TestDecodeStructuredJSONArrayRepairsColonBetweenObjectFields(t *testing.T) {
	input := `[{"kind":"claim","statement":"remember quartz","sources":[] : "keys":["quartz"]}]`
	decoded, err := decodeStructuredJSONArray(input)
	if err != nil {
		t.Fatal(err)
	}
	items, ok := decoded.([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("unexpected decoded items: %#v", decoded)
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["keys"] == nil {
		t.Fatalf("missing repaired keys: %#v", decoded)
	}
}

func TestFabricCompilerRawArtifactRoundTrip(t *testing.T) {
	compiler := newFabricSemanticCompiler(fabricAgentTestConfig(), "compiler-model")
	compiler.cacheDir = t.TempDir()
	key := fabricCompilerRawArtifactCacheKey("compiler-model", []byte(`{"sources":["one"]}`))
	want := map[string]any{"nodes": `[{"kind":"claim","statement":"A","sources":[]}: {"kind":"episode","statement":"B","sources":[]}]`, "aliases": `[]`}
	compiler.saveCachedCompilerInput(key, want)
	got, ok := compiler.loadCachedCompilerInput(key)
	if !ok || got["nodes"] != want["nodes"] || got["aliases"] != want["aliases"] {
		t.Fatalf("raw artifact round trip = %#v, %t", got, ok)
	}
	if _, err := normalizeStructuredJSONArrayFields(got, "nodes", "aliases"); err != nil {
		t.Fatalf("cached input could not be normalized locally: %v", err)
	}
}

func TestNormalizeStructuredJSONArrayExtractsNodesFromDoubleWrappedObjects(t *testing.T) {
	node := `{"kind":"claim","statement":"remember quartz","sources":[]}`
	input := map[string]any{"nodes": `[[{{` + node[1:len(node)-1] + `}}]]`}
	normalized, err := normalizeStructuredJSONArrayFields(input, "nodes")
	if err != nil {
		t.Fatal(err)
	}
	nodes, ok := normalized["nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("unexpected normalized nodes: %#v", normalized["nodes"])
	}
}

func TestFabricAdjudicatorUsesIndependentStructuredContract(t *testing.T) {
	stub := &fabricStructuredStub{response: api.Response{ToolCalls: []map[string]any{{
		"name": fabricAdjudicatorToolName, "input": map[string]any{
			"decision": "uncertain", "winner_ids": []any{}, "loser_ids": []any{},
			"support_ids": []any{}, "reason": "The scopes are ambiguous.",
		},
	}}}}
	judge := newFabricConflictAdjudicator(fabricAgentTestConfig(), "judge-model")
	judge.factory = func(config.Config, string, int, *api.RetryConfig) (api.StructuredCompletionClient, error) {
		return stub, nil
	}
	result, err := judge.Adjudicate(context.Background(), memory.AdjudicationRequest{Conflict: memory.Conflict{
		ID: "conflict-1", Members: []memory.MemoryNode{{ID: "old"}, {ID: "new"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 || stub.opts.RequiredTool != fabricAdjudicatorToolName ||
		result.Decision != memory.DecisionUncertain || result.Usage.Model != "judge-model" {
		t.Fatalf("unexpected adjudicator call: opts=%+v result=%+v", stub.opts, result)
	}
}
