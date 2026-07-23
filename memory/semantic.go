package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type compileJobPayload struct {
	EventIDs  []string        `json:"event_ids"`
	Sources   []CompileSource `json:"sources"`
	ContextID string          `json:"context_id"`
	Mode      MemoryWriteMode `json:"mode"`
}

type ImportPlanningOptions struct {
	MaxCompilerCalls     int
	MaxSources           int
	MaxSourcesPerSession int
	MaxSourceRunes       int
}

type CompileInputBudgetError struct {
	EstimatedTokens int
	LimitTokens     int
	EventCount      int
}

func (e *CompileInputBudgetError) Error() string {
	return fmt.Sprintf("semantic compiler request exceeds input budget: estimated=%d limit=%d events=%d",
		e.EstimatedTokens, e.LimitTokens, e.EventCount)
}

func (f *Fabric) Remember(ctx context.Context, request MemoryRequest) (MemoryCommitResult, error) {
	result := MemoryCommitResult{SemanticStatus: SemanticEventDurable}
	request.Space = normalizeSpace(request.Space)
	if request.Mode == "" {
		request.Mode = WriteExplicit
	}
	if len(request.Events) > 0 {
		policy := SemanticDeferred
		if request.RequireSemantic || isSynchronousWriteMode(request.Mode) || len(request.Drafts) > 0 {
			policy = SemanticDurableOnly
		}
		for index := range request.Events {
			if request.Events[index].Space == "" {
				request.Events[index].Space = request.Space
			}
			if request.Events[index].ContextID == "" {
				request.Events[index].ContextID = request.ContextID
			}
		}
		ingested, err := f.AppendEvents(ctx, request.Events, IngestOptions{SemanticPolicy: policy})
		result.Durable = ingested.Durable
		result.EventIDs = append(result.EventIDs, ingested.EventIDs...)
		if err != nil {
			var lag IndexLagError
			if !errors.As(err, &lag) {
				return result, err
			}
		}
	}
	result.Durable = result.Durable || len(request.SourceEventIDs) > 0
	request.SourceEventIDs = uniqueStrings(append(request.SourceEventIDs, result.EventIDs...))
	if len(request.SourceEventIDs) == 0 && len(request.Drafts) == 0 {
		return result, errors.New("semantic memory requires source events or deterministic drafts")
	}
	if len(request.Drafts) > 0 {
		commit, err := f.commitDrafts(ctx, request, request.Drafts)
		commit.Durable = result.Durable
		commit.EventIDs = uniqueStrings(append(commit.EventIDs, result.EventIDs...))
		return commit, err
	}
	return f.rememberDurableEvents(ctx, request)
}

func (f *Fabric) rememberDurableEvents(ctx context.Context, request MemoryRequest) (MemoryCommitResult, error) {
	result := MemoryCommitResult{Durable: true, EventIDs: uniqueStrings(request.SourceEventIDs), SemanticStatus: SemanticEventDurable}
	synchronous := request.RequireSemantic || isSynchronousWriteMode(request.Mode)
	if !synchronous {
		job, err := f.enqueueCompileEvents(ctx, request.Space, request.ContextID, request.SourceEventIDs, request.Mode)
		result.PendingJobID = job.ID
		result.SemanticStatus = SemanticProposed
		return result, err
	}
	if f.options.Compiler == nil || f.options.RemoteProcessing == RemoteProcessingOff {
		if request.RequireSemantic {
			return result, errors.New("semantic compiler is unavailable while raw events remain durable")
		}
		job, err := f.enqueueCompileEvents(ctx, request.Space, request.ContextID, request.SourceEventIDs, request.Mode)
		result.PendingJobID = job.ID
		return result, err
	}
	return f.compileDurableEvents(ctx, request)
}

func (f *Fabric) compileDurableEvents(ctx context.Context, request MemoryRequest) (MemoryCommitResult, error) {
	result := MemoryCommitResult{Durable: true, EventIDs: uniqueStrings(request.SourceEventIDs), SemanticStatus: SemanticEventDurable}
	events, err := f.loadEvents(ctx, request.SourceEventIDs)
	if err != nil {
		return result, err
	}
	if len(events) == 0 {
		return result, errors.New("semantic memory source events do not exist")
	}
	if len(request.CompileSources) == 0 {
		plan, planErr := f.planSemanticEvents(ctx, events, PlanningOptions{
			Mode: request.Mode, MaxSources: f.options.CompileMaxSources,
			MaxSourcesPerSession: f.options.CompileSourcesPerSession,
			MaxSourceRunes:       f.options.CompileSourceRunes,
		})
		if planErr != nil {
			return result, planErr
		}
		if err := f.markSemanticSkippedEvents(ctx, plan.SkippedEventIDs); err != nil {
			return result, err
		}
		request.SourceEventIDs, request.CompileSources = candidatesToCompileInput(plan.Candidates)
		result.EventIDs = uniqueStrings(request.SourceEventIDs)
	}
	if len(request.CompileSources) == 0 {
		result.SemanticStatus = SemanticSkipped
		return result, nil
	}
	response, err := f.compileEvents(ctx, request)
	result.Usage = response.Usage
	if err != nil {
		return result, err
	}
	boundNodes, boundAliases, dropped, err := bindCompilerSources(response.Nodes, response.Aliases,
		request.CompileSources, request.SourceEventIDs)
	if err != nil {
		return result, &CompileContractError{Reason: err.Error()}
	}
	response.Nodes, response.Aliases = boundNodes, boundAliases
	if dropped > 0 && len(response.Nodes) == 0 && len(response.Aliases) == 0 && request.RequireSemantic {
		return result, &CompileContractError{Reason: "all semantic compiler candidates cited unknown sources"}
	}
	usedEventIDs := compilerResponseEventIDs(response.Nodes, response.Aliases)
	unusedEventIDs := differenceStrings(request.SourceEventIDs, usedEventIDs)
	if err := f.markSemanticSkippedEvents(ctx, unusedEventIDs); err != nil {
		return result, err
	}
	if len(usedEventIDs) == 0 {
		result.SemanticStatus = SemanticSkipped
		return result, nil
	}
	request.SourceEventIDs = usedEventIDs
	if err := f.applyAliasProposals(ctx, request.Space, response.Aliases); err != nil {
		return result, err
	}
	commit, err := f.commitDrafts(ctx, request, response.Nodes)
	commit.Durable = true
	commit.EventIDs = result.EventIDs
	commit.Usage = mergeAPIUsage(response.Usage, commit.Usage)
	if err == nil {
		status := commit.SemanticStatus
		if status == "" {
			status = SemanticActive
		}
		_, _ = f.ledger.ExecContext(ctx, `UPDATE events SET semantic_status=? WHERE event_id IN (`+
			placeholders(len(request.SourceEventIDs))+`)`, appendAny([]any{status}, request.SourceEventIDs)...)
	}
	return commit, err
}

func isSynchronousWriteMode(mode MemoryWriteMode) bool {
	switch mode {
	case WriteExplicit, WriteCorrection, WritePreference, WriteConstraint, WriteCriticalResult:
		return true
	default:
		return false
	}
}

func (f *Fabric) compileEvents(ctx context.Context, request MemoryRequest) (CompileResponse, error) {
	if f.options.Compiler == nil {
		return CompileResponse{}, errors.New("semantic compiler is not configured")
	}
	if len(request.CompileSources) == 0 {
		return CompileResponse{}, nil
	}
	remoteSources := make([]CompileSource, 0, len(request.CompileSources))
	totalTokens := 0
	for _, source := range request.CompileSources {
		remote := source
		if f.options.RemoteProcessing == RemoteProcessingRedacted {
			remote.Text = redactSecrets(remote.Text)
		}
		remoteSources = append(remoteSources, remote)
		totalTokens += estimateTokens(source.Text)
	}
	compileRequest := CompileRequest{
		Mode: request.Mode, Instructions: request.Instructions, Sources: remoteSources,
		MaxInputTokens: f.options.CompileBatchTokens, MaxOutputTokens: f.compileOutputBudget(totalTokens, len(remoteSources)),
		MaxNodes: f.compileNodeBudget(len(remoteSources)),
	}

	estimated, estimateErr := estimateCompileInputTokens(f.options.Compiler, compileRequest)
	if estimateErr != nil {
		return CompileResponse{}, estimateErr
	}
	if estimated > compileRequest.MaxInputTokens {
		return CompileResponse{}, &CompileInputBudgetError{EstimatedTokens: estimated,
			LimitTokens: compileRequest.MaxInputTokens, EventCount: len(remoteSources)}
	}
	callCtx := ctx
	cancel := func() {}
	if f.options.CompileCallTimeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, f.options.CompileCallTimeout)
	}
	defer cancel()
	response, err := f.options.Compiler.Compile(callCtx, compileRequest)
	response.Usage.Calls = maxIntMemory(response.Usage.Calls, boolIntMemory(response.Usage.InputTokens > 0 || response.Usage.OutputTokens > 0))
	usageErr := f.observeAPIUsage(ctx, APIUsageEvent{Stage: APIStageSemanticCompile, Space: request.Space,
		ContextID: request.ContextID, Usage: response.Usage, Error: errorStringMemory(err)})
	if usageErr != nil && err == nil {
		err = usageErr
	}
	if err != nil {
		return response, err
	}
	if !response.CacheHit {
		response.Usage.Calls = maxIntMemory(1, response.Usage.Calls)
	}
	return response, nil
}

func (f *Fabric) compileOutputBudget(eventTokens, eventCount int) int {
	limit := f.options.CompileOutputTokens
	budget := 1024
	if eventTokens > 800 || eventCount > 4 {
		budget = 2304
	}
	if eventTokens > 1_600 || eventCount > 8 {
		budget = 2560
	}
	return minIntMemory(limit, budget)
}

func (f *Fabric) compileNodeBudget(eventCount int) int {
	limit := f.options.CompileMaxNodes
	budget := eventCount
	if budget < 1 {
		budget = 1
	}
	if budget > 8 {
		budget = 8
	}
	return minIntMemory(limit, budget)
}

func estimateCompileInputTokens(compiler SemanticCompiler, request CompileRequest) (int, error) {
	if sizer, ok := compiler.(CompileRequestSizer); ok {
		return sizer.EstimateInputTokens(request)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return 0, fmt.Errorf("estimate semantic compiler request: %w", err)
	}
	// The fallback reserves room for a typical system prompt and structured
	// output schema. Production compilers should implement CompileRequestSizer.
	return estimateTokens(string(encoded)) + 3_000, nil
}

func compactCompilerEvent(event RawEvent) RawEvent {
	metadata := map[string]string{}
	for _, key := range []string{"path", "file_path", "command", "cmd", "tool", "status", "project", "environment"} {
		if value := strings.TrimSpace(event.Metadata[key]); value != "" {
			metadata[key] = value
		}
	}
	if len(metadata) == 0 {
		metadata = nil
	}
	return RawEvent{ID: event.ID, SessionID: event.SessionID, Actor: event.Actor,
		SourceKind: event.SourceKind, Content: event.Content, OccurredAt: event.OccurredAt, Metadata: metadata}
}

func compactCompilerNeighborhood(nodes []MemoryNode) []MemoryNode {
	result := make([]MemoryNode, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, MemoryNode{ID: node.ID, Kind: node.Kind, ClaimType: node.ClaimType,
			Statement: node.Statement, SubjectID: node.SubjectID, Subject: node.Subject, Facet: node.Facet,
			AttributeKey: node.AttributeKey, ScopeKey: node.ScopeKey, SlotID: node.SlotID, Value: node.Value,
			ValidFrom: node.ValidFrom, ValidUntil: node.ValidUntil, EvidenceMode: node.EvidenceMode, Status: node.Status})
	}
	return result
}

func compactCompilerIdentities(identities []Identity) []Identity {
	result := make([]Identity, 0, len(identities))
	for _, identity := range identities {
		result = append(result, Identity{ID: identity.ID, Canonical: identity.Canonical,
			Type: identity.Type, DisplayName: identity.DisplayName})
	}
	return result
}

func semanticCandidateEvents(events []RawEvent) []RawEvent {
	result := make([]RawEvent, 0, len(events))
	seen := map[string]int{}
	for _, event := range events {
		if isLowSignalSemanticEvent(event) {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(event.Actor)) + "\x1f" + normalizeClaim(event.Content)
		if previous, ok := seen[key]; ok {
			// Repeated text carries the same semantic claim. Retain the latest
			// source timestamp without paying to compile it twice.
			result[previous] = event
			continue
		}
		seen[key] = len(result)
		result = append(result, event)
	}
	return result
}

func isLowSignalSemanticEvent(event RawEvent) bool {
	sourceKind := strings.ToLower(strings.TrimSpace(event.SourceKind))
	if sourceKind == "tool" || sourceKind == "command" || sourceKind == "file" || sourceKind == "observation" {
		return false
	}
	value := strings.ToLower(strings.TrimSpace(event.Content))
	value = strings.Trim(value, " \t\r\n.!?,;:，。！？；：~～")
	if value == "" {
		return true
	}
	for _, exact := range []string{"ok", "okay", "sure", "thanks", "thank you", "got it", "great", "cool",
		"hello", "hi", "bye", "you're welcome", "you are welcome", "no problem", "好的", "好", "谢谢",
		"明白了", "知道了", "收到", "没问题", "不用谢", "再见"} {
		if value == exact {
			return true
		}
	}
	if len([]rune(value)) > 160 {
		return false
	}
	for _, boilerplate := range []string{"let me know if you need", "feel free to ask", "is there anything else",
		"how can i help", "如果还需要", "如有其他问题", "还有什么可以帮", "随时告诉我"} {
		if strings.Contains(value, boilerplate) {
			return true
		}
	}
	return false
}

func redactMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	result := make(map[string]string, len(metadata))
	for key, value := range metadata {
		result[key] = redactSecrets(value)
	}
	return result
}

func (f *Fabric) SealContext(ctx context.Context, ref ContextRef) (JobRef, error) {
	ref.Space = normalizeSpace(ref.Space)
	if ref.ClosedAt.IsZero() {
		ref.ClosedAt = f.now()
	}
	if err := f.upsertContext(ctx, ref); err != nil {
		return JobRef{}, err
	}
	ids, err := f.contextEventIDs(ctx, ref.Space, ref.ID, true)
	if err != nil || len(ids) == 0 {
		return JobRef{Kind: jobCompileEvents, Status: "complete"}, err
	}
	if err := f.projectSessionChunks(ctx, ids); err != nil {
		return JobRef{}, err
	}
	jobs, err := f.planAndEnqueueCompile(ctx, ref.Space, ref.ID, ids, WriteNormal, 1, PlanningOptions{})
	if len(jobs) == 0 {
		return JobRef{Kind: jobCompileEvents, Status: "complete"}, err
	}
	return jobs[0], err
}

// SealImport closes a group of source sessions and creates only final-sized
// compiler jobs. Raw evidence that the planner intentionally excludes remains
// searchable and is marked raw_only rather than looking like failed work.
func (f *Fabric) SealImport(ctx context.Context, refs []ContextRef, options ImportPlanningOptions) ([]JobRef, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	eventIDs := make([]string, 0)
	space := ""
	contextID := ""
	for index := range refs {
		refs[index].Space = normalizeSpace(refs[index].Space)
		if refs[index].ClosedAt.IsZero() {
			refs[index].ClosedAt = f.now()
		}
		if err := f.upsertContext(ctx, refs[index]); err != nil {
			return nil, err
		}
		ids, err := f.contextEventIDs(ctx, refs[index].Space, refs[index].ID, true)
		if err != nil {
			return nil, err
		}
		eventIDs = append(eventIDs, ids...)
		if space == "" {
			space = refs[index].Space
			contextID = refs[index].ID
		}
	}
	if err := f.projectSessionChunks(ctx, eventIDs); err != nil {
		return nil, err
	}
	maxCalls := options.MaxCompilerCalls
	if maxCalls <= 0 {
		maxCalls = f.options.CompileMaxCalls
	}
	planning := PlanningOptions{Mode: WriteImport, MaxSources: options.MaxSources,
		MaxSourcesPerSession: options.MaxSourcesPerSession, MaxSourceRunes: options.MaxSourceRunes}
	return f.planAndEnqueueCompile(ctx, space, contextID, eventIDs, WriteImport, maxCalls, planning)
}

func (f *Fabric) enqueueCompileJob(ctx context.Context, space, contextID string, mode MemoryWriteMode) (JobRef, error) {
	ids, err := f.contextEventIDs(ctx, space, contextID, true)
	if err != nil || len(ids) == 0 {
		return JobRef{}, err
	}
	return f.enqueueCompileEvents(ctx, space, contextID, ids, mode)
}

func (f *Fabric) enqueueCompileEvents(ctx context.Context, space, contextID string, eventIDs []string, mode MemoryWriteMode) (JobRef, error) {
	maxCalls := 1
	if mode == WriteImport {
		maxCalls = f.options.CompileMaxCalls
	}
	jobs, err := f.planAndEnqueueCompile(ctx, space, contextID, eventIDs, mode, maxCalls, PlanningOptions{})
	if len(jobs) == 0 {
		return JobRef{Kind: jobCompileEvents, Status: "complete"}, err
	}
	return jobs[0], err
}

func (f *Fabric) enqueueCompileEventBatch(ctx context.Context, space, contextID string,
	candidates []SemanticCandidate, mode MemoryWriteMode) (JobRef, error) {
	eventIDs, sources := candidatesToCompileInput(candidates)
	payload := compileJobPayload{EventIDs: eventIDs, Sources: sources, ContextID: contextID, Mode: mode}
	return f.enqueueJob(ctx, jobCompileEvents, space, contextID,
		contentHash(space, contextID, string(mode), compileSourcesHash(sources)), payload, f.now())
}

func (f *Fabric) planAndEnqueueCompile(ctx context.Context, space, contextID string, eventIDs []string,
	mode MemoryWriteMode, maxCalls int, planning PlanningOptions) ([]JobRef, error) {
	eventIDs = uniqueStrings(eventIDs)
	if len(eventIDs) == 0 {
		return nil, nil
	}
	events, err := f.loadEvents(ctx, eventIDs)
	if err != nil {
		return nil, err
	}
	if f.options.Compiler == nil || f.options.RemoteProcessing == RemoteProcessingOff {
		return nil, f.markRawOnlyEvents(ctx, eventIDs)
	}
	if planning.Mode == "" {
		planning.Mode = mode
	}
	if planning.MaxSources <= 0 {
		planning.MaxSources = f.options.CompileMaxSources
	}
	if planning.MaxSourcesPerSession <= 0 {
		planning.MaxSourcesPerSession = f.options.CompileSourcesPerSession
	}
	if planning.MaxSourceRunes <= 0 {
		planning.MaxSourceRunes = f.options.CompileSourceRunes
	}
	plan, err := f.planSemanticEvents(ctx, events, planning)
	if err != nil {
		return nil, err
	}
	if err := f.markSemanticSkippedEvents(ctx, plan.SkippedEventIDs); err != nil {
		return nil, err
	}
	batches, dropped, err := f.packCompileCandidates(plan.Candidates, mode, maxCalls)
	if err != nil {
		return nil, err
	}
	if err := f.markSemanticSkippedEvents(ctx, dropped); err != nil {
		return nil, err
	}
	jobs := make([]JobRef, 0, len(batches))
	for _, batch := range batches {
		job, err := f.enqueueCompileEventBatch(ctx, space, contextID, batch, mode)
		if err != nil {
			return jobs, err
		}
		jobs = append(jobs, job)
		ids, _ := candidatesToCompileInput(batch)
		if err := f.markEventSemanticStatus(ctx, ids, SemanticProposed, SemanticEventDurable); err != nil {
			return jobs, err
		}
	}
	return jobs, nil
}

func (f *Fabric) planSemanticEvents(ctx context.Context, events []RawEvent, options PlanningOptions) (SemanticPlan, error) {
	planner := f.options.Planner
	if planner == nil {
		planner = NewLocalSemanticPlanner(f.options.Vectorizer)
	}
	return planner.Plan(ctx, events, options)
}

func (f *Fabric) packCompileCandidates(candidates []SemanticCandidate, mode MemoryWriteMode,
	maxCalls int) ([][]SemanticCandidate, []string, error) {
	if maxCalls <= 0 {
		maxCalls = 1
	}
	var batches [][]SemanticCandidate
	var current []SemanticCandidate
	var dropped []string
	requestFits := func(values []SemanticCandidate) (bool, error) {
		if len(values) > f.options.CompileSourcesPerCall {
			return false, nil
		}
		_, sources := candidatesToCompileInput(values)
		tokens := 0
		for _, source := range sources {
			tokens += estimateTokens(source.Text)
		}
		request := CompileRequest{Mode: mode, Sources: sources,
			MaxInputTokens:  f.options.CompileBatchTokens,
			MaxOutputTokens: f.compileOutputBudget(tokens, len(sources)),
			MaxNodes:        f.compileNodeBudget(len(sources))}
		estimated, err := estimateCompileInputTokens(f.options.Compiler, request)
		return estimated <= request.MaxInputTokens, err
	}
	flush := func() {
		if len(current) > 0 {
			batches = append(batches, current)
			current = nil
		}
	}
	for _, candidate := range candidates {
		trial := append(append([]SemanticCandidate(nil), current...), candidate)
		fits, err := requestFits(trial)
		if err != nil {
			return nil, nil, err
		}
		if fits {
			current = trial
			continue
		}
		flush()
		if len(batches) >= maxCalls {
			dropped = append(dropped, candidate.EventID)
			continue
		}
		fits, err = requestFits([]SemanticCandidate{candidate})
		if err != nil {
			return nil, nil, err
		}
		if !fits {
			dropped = append(dropped, candidate.EventID)
			continue
		}
		current = []SemanticCandidate{candidate}
	}
	if len(current) > 0 && len(batches) < maxCalls {
		flush()
	} else if len(current) > 0 {
		for _, candidate := range current {
			dropped = append(dropped, candidate.EventID)
		}
	}
	if len(batches) > maxCalls {
		for _, batch := range batches[maxCalls:] {
			for _, candidate := range batch {
				dropped = append(dropped, candidate.EventID)
			}
		}
		batches = batches[:maxCalls]
	}
	return batches, uniqueStrings(dropped), nil
}

func candidatesToCompileInput(candidates []SemanticCandidate) ([]string, []CompileSource) {
	eventIDs := make([]string, 0, len(candidates))
	sources := make([]CompileSource, 0, len(candidates))
	for _, candidate := range candidates {
		eventIDs = append(eventIDs, candidate.EventID)
		sources = append(sources, candidate.Source)
	}
	return uniqueStrings(eventIDs), sources
}

func compileSourcesHash(sources []CompileSource) string {
	parts := make([]string, 0, len(sources))
	for _, source := range sources {
		parts = append(parts, source.SourceRef+"\x1e"+source.SessionRef+"\x1e"+source.Actor+"\x1e"+
			formatFabricTime(source.OccurredAt)+"\x1e"+source.Text)
	}
	return contentHash(parts...)
}

func (f *Fabric) markRawOnlyEvents(ctx context.Context, eventIDs []string) error {
	return f.markEventSemanticStatus(ctx, eventIDs, SemanticRawOnly, SemanticEventDurable, SemanticProposed)
}

func (f *Fabric) markSemanticSkippedEvents(ctx context.Context, eventIDs []string) error {
	return f.markEventSemanticStatus(ctx, eventIDs, SemanticSkipped, SemanticEventDurable, SemanticProposed)
}

func (f *Fabric) markEventSemanticStatus(ctx context.Context, eventIDs []string, status SemanticStatus,
	allowed ...SemanticStatus) error {
	eventIDs = uniqueStrings(eventIDs)
	if len(eventIDs) == 0 {
		return nil
	}
	args := []any{status}
	for _, id := range eventIDs {
		args = append(args, id)
	}
	query := `UPDATE events SET semantic_status=? WHERE event_id IN (` + placeholders(len(eventIDs)) + `)`
	if len(allowed) > 0 {
		query += ` AND semantic_status IN (` + placeholders(len(allowed)) + `)`
		for _, value := range allowed {
			args = append(args, value)
		}
	}
	_, err := f.ledger.ExecContext(ctx, query, args...)
	return err
}

func bindCompilerSources(nodes []MemoryDraft, aliases []IdentityAliasProposal, sources []CompileSource,
	eventIDs []string) ([]MemoryDraft, []IdentityAliasProposal, int, error) {
	if len(sources) != len(eventIDs) {
		return nil, nil, 0, errors.New("semantic compiler source mapping is inconsistent")
	}
	byRef := make(map[string]string, len(sources))
	allowedIDs := make(map[string]struct{}, len(eventIDs))
	for index, source := range sources {
		byRef[source.SourceRef] = eventIDs[index]
		allowedIDs[eventIDs[index]] = struct{}{}
	}
	bind := func(spans []SourceSpan) bool {
		if len(spans) == 0 {
			return false
		}
		for index := range spans {
			if id := byRef[strings.TrimSpace(spans[index].SourceRef)]; id != "" {
				spans[index].EventID = id
			}
			if _, ok := allowedIDs[spans[index].EventID]; !ok {
				return false
			}
		}
		return true
	}
	boundNodes := make([]MemoryDraft, 0, len(nodes))
	boundAliases := make([]IdentityAliasProposal, 0, len(aliases))
	dropped := 0
	for index := range nodes {
		if !bind(nodes[index].Sources) {
			dropped++
			continue
		}
		boundNodes = append(boundNodes, nodes[index])
	}
	for index := range aliases {
		if !bind(aliases[index].Sources) {
			dropped++
			continue
		}
		boundAliases = append(boundAliases, aliases[index])
	}
	return boundNodes, boundAliases, dropped, nil
}

func compilerResponseEventIDs(nodes []MemoryDraft, aliases []IdentityAliasProposal) []string {
	var ids []string
	for _, node := range nodes {
		for _, source := range node.Sources {
			ids = append(ids, source.EventID)
		}
	}
	for _, alias := range aliases {
		for _, source := range alias.Sources {
			ids = append(ids, source.EventID)
		}
	}
	return uniqueStrings(ids)
}

func differenceStrings(values, excluded []string) []string {
	blocked := make(map[string]struct{}, len(excluded))
	for _, value := range excluded {
		blocked[value] = struct{}{}
	}
	result := make([]string, 0, len(values))
	for _, value := range uniqueStrings(values) {
		if _, exists := blocked[value]; !exists {
			result = append(result, value)
		}
	}
	return result
}

func (f *Fabric) compileEventsJob(ctx context.Context, job fabricJob) error {
	var payload compileJobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return err
	}
	if f.options.Compiler == nil || f.options.RemoteProcessing == RemoteProcessingOff {
		return nil
	}
	request := MemoryRequest{Space: job.Space, ContextID: payload.ContextID,
		SourceEventIDs: payload.EventIDs, CompileSources: payload.Sources, Mode: payload.Mode,
		RequireSemantic: isSynchronousWriteMode(payload.Mode)}
	recovered, err := f.recoverQuarantinedDrafts(ctx, request)
	if err != nil || recovered {
		return err
	}
	_, err = f.compileDurableEvents(ctx, request)
	if err != nil && !request.RequireSemantic && isEmptyCompilerOutputBudgetError(err) {
		// Bulk/deferred writes promise durable raw recall, not a successful
		// semantic projection for every batch. A response that spent its entire
		// output budget without producing a node cannot be repaired locally and
		// must not trigger another API call. Record the omission explicitly so
		// health checks can distinguish it from failed or unfinished work.
		return f.markSemanticSkippedEvents(ctx, request.SourceEventIDs)
	}
	return err
}

func isEmptyCompilerOutputBudgetError(err error) bool {
	var contractErr *CompileContractError
	return errors.As(err, &contractErr) &&
		strings.Contains(strings.ToLower(contractErr.Reason), "exhausted the output budget without a semantic result")
}

func (f *Fabric) recoverQuarantinedDrafts(ctx context.Context, request MemoryRequest) (bool, error) {
	allowed := make(map[string]struct{}, len(request.SourceEventIDs))
	for _, id := range request.SourceEventIDs {
		allowed[id] = struct{}{}
	}
	rows, err := f.ledger.QueryContext(ctx, `SELECT payload_json FROM memory_nodes
		WHERE space=? AND context_id=? AND status=? ORDER BY created_at`,
		normalizeSpace(request.Space), request.ContextID, SemanticQuarantined)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var drafts []MemoryDraft
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return false, err
		}
		var payload struct {
			Draft MemoryDraft `json:"draft"`
		}
		if json.Unmarshal([]byte(encoded), &payload) != nil || len(payload.Draft.Sources) == 0 {
			continue
		}
		belongsToBatch := true
		for _, source := range payload.Draft.Sources {
			if _, ok := allowed[source.EventID]; !ok {
				belongsToBatch = false
				break
			}
		}
		if !belongsToBatch {
			continue
		}
		normalized, _, validateErr := f.validateDraft(ctx, payload.Draft)
		if validateErr == nil {
			drafts = append(drafts, normalized)
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if len(drafts) == 0 {
		return false, nil
	}
	if _, err := f.commitDrafts(ctx, request, drafts); err != nil {
		return true, err
	}
	if len(request.SourceEventIDs) > 0 {
		_, err = f.ledger.ExecContext(ctx, `UPDATE events SET semantic_status=? WHERE event_id IN (`+
			placeholders(len(request.SourceEventIDs))+`)`, appendAny([]any{SemanticActive}, request.SourceEventIDs)...)
		if err != nil {
			return true, err
		}
	}
	return true, nil
}

func (f *Fabric) loadContext(ctx context.Context, space, contextID string) (ContextRef, error) {
	ref := ContextRef{ID: contextID, Space: normalizeSpace(space)}
	if contextID == "" {
		return ref, nil
	}
	var opened, closed string
	err := f.ledger.QueryRowContext(ctx, `SELECT parent_id, context_type, label, opened_at, closed_at
		FROM contexts WHERE context_id=? AND space=?`, contextID, ref.Space).
		Scan(&ref.ParentID, &ref.Type, &ref.Label, &opened, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		return ref, nil
	}
	ref.OpenedAt, ref.ClosedAt = parseFabricTime(opened), parseFabricTime(closed)
	return ref, err
}

func (f *Fabric) loadWriteNeighborhood(ctx context.Context, space, query string, limit int) ([]MemoryNode, error) {
	terms := ftsTerms(query, 12)
	if terms == "" {
		return nil, nil
	}
	rows, err := f.index.QueryContext(ctx, `SELECT d.resource_id FROM document_fts
		JOIN documents d ON d.doc_id=document_fts.doc_id
		WHERE document_fts MATCH ? AND d.space=? AND d.resource_kind='node'
		ORDER BY bm25(document_fts) LIMIT ?`, terms, normalizeSpace(space), limit)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return f.loadMemoryNodes(ctx, ids)
}

func (f *Fabric) identitiesForText(ctx context.Context, space, text string, limit int) ([]Identity, error) {
	aliases := queryTokens(text, 32)
	if len(aliases) == 0 {
		return nil, nil
	}
	placeholders := placeholders(len(aliases))
	args := []any{normalizeSpace(space)}
	for _, alias := range aliases {
		args = append(args, alias)
	}
	args = append(args, limit)
	rows, err := f.ledger.QueryContext(ctx, `SELECT DISTINCT i.identity_id, i.space, i.canonical,
		i.identity_type, i.display_name FROM identity_aliases a JOIN identities i ON i.identity_id=a.identity_id
		WHERE a.space=? AND a.normalized_alias IN (`+placeholders+`) AND a.status='active' LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Identity
	for rows.Next() {
		var identity Identity
		if err := rows.Scan(&identity.ID, &identity.Space, &identity.Canonical, &identity.Type, &identity.DisplayName); err != nil {
			return nil, err
		}
		result = append(result, identity)
	}
	return result, rows.Err()
}

func (f *Fabric) applyAliasProposals(ctx context.Context, space string, proposals []IdentityAliasProposal) error {
	for _, proposal := range proposals {
		if strings.TrimSpace(proposal.Canonical) == "" || len(proposal.Aliases) == 0 {
			continue
		}
		if _, err := f.validateSourceSpans(ctx, proposal.Sources); err != nil {
			continue
		}
		tx, err := f.ledger.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		identity, err := resolveOrCreateIdentityTx(ctx, tx, normalizeSpace(space), proposal.Canonical, proposal.Type, f.now())
		if err == nil {
			for _, alias := range append(proposal.Aliases, proposal.Canonical) {
				normalized := normalizeClaim(alias)
				if normalized == "" {
					continue
				}
				sourceID := ""
				if len(proposal.Sources) > 0 {
					sourceID = proposal.Sources[0].EventID
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO identity_aliases(
					space, normalized_alias, identity_id, source_event_id, method, created_at)
					VALUES (?, ?, ?, ?, 'compiler_grounded', ?) ON CONFLICT DO NOTHING`,
					normalizeSpace(space), normalized, identity.ID, sourceID, formatFabricTime(f.now()))
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fabric) validateSourceSpans(ctx context.Context, spans []SourceSpan) (map[string]RawEvent, error) {
	_, events, err := f.normalizeSourceSpans(ctx, spans)
	return events, err
}

func (f *Fabric) normalizeSourceSpans(ctx context.Context, spans []SourceSpan) ([]SourceSpan, map[string]RawEvent, error) {
	if len(spans) == 0 {
		return nil, nil, errors.New("semantic memory has no source spans")
	}
	ids := make([]string, 0, len(spans))
	for _, span := range spans {
		ids = append(ids, span.EventID)
	}
	events, err := f.loadEvents(ctx, uniqueStrings(ids))
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]RawEvent, len(events))
	for _, event := range events {
		byID[event.ID] = event
	}
	normalized := make([]SourceSpan, 0, len(spans))
	for _, original := range spans {
		span := original
		event, ok := byID[span.EventID]
		if !ok {
			return nil, nil, fmt.Errorf("semantic source event %q does not exist", span.EventID)
		}
		if strings.TrimSpace(span.Text) != "" {
			ranges := findSourceQuoteRanges(event.Content, span.Text)
			if len(ranges) == 0 {
				// Redaction preserves rune positions, so a quote containing masked
				// secrets can still be mapped safely back to the original event.
				ranges = findSourceQuoteRanges(redactSecrets(event.Content), span.Text)
			}
			if len(ranges) == 0 {
				return nil, nil, fmt.Errorf("semantic source quote for event %q does not occur verbatim", span.EventID)
			}
			for _, item := range ranges {
				grounded := span
				grounded.StartRune, grounded.EndRune, grounded.Text = item[0], item[1], ""
				grounded, err = normalizeSourceSpanRange(grounded, event.Content)
				if err != nil {
					return nil, nil, err
				}
				normalized = append(normalized, grounded)
			}
			continue
		}
		span, err = normalizeSourceSpanRange(span, event.Content)
		if err != nil {
			return nil, nil, err
		}
		normalized = append(normalized, span)
	}
	return normalized, byID, nil
}

func normalizeSourceSpanRange(span SourceSpan, content string) (SourceSpan, error) {
	length := len([]rune(content))
	if span.EndRune > length && (span.StartRune == 0 || span.EndRune-length <= 16) {
		span.EndRune = length
	}
	if span.StartRune < 0 || span.EndRune <= span.StartRune || span.EndRune > length {
		return span, fmt.Errorf("semantic source span %s[%d:%d] is outside %d runes", span.EventID,
			span.StartRune, span.EndRune, length)
	}
	return span, nil
}

func findSourceQuoteRanges(source, quote string) [][2]int {
	if start, end, found := findSourceQuoteRange(source, quote); found {
		return [][2]int{{start, end}}
	}
	parts := strings.Split(strings.ReplaceAll(quote, "…", "..."), "...")
	if len(parts) < 2 {
		return nil
	}
	sourceRunes := []rune(source)
	offset := 0
	ranges := make([][2]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		start, end, found := findSourceQuoteRange(string(sourceRunes[offset:]), part)
		if !found {
			return nil
		}
		start += offset
		end += offset
		ranges = append(ranges, [2]int{start, end})
		offset = end
	}
	if len(ranges) < 2 {
		return nil
	}
	return ranges
}

func findSourceQuoteRange(source, quote string) (int, int, bool) {
	quote = strings.TrimSpace(quote)
	if quote == "" {
		return 0, 0, false
	}
	if byteIndex := strings.Index(source, quote); byteIndex >= 0 {
		start := utf8.RuneCountInString(source[:byteIndex])
		return start, start + utf8.RuneCountInString(quote), true
	}
	haystack, positions := normalizedQuoteRunes(source)
	needle, _ := normalizedQuoteRunes(quote)
	if len(needle) == 0 || len(needle) > len(haystack) {
		return 0, 0, false
	}
	for start := 0; start+len(needle) <= len(haystack); start++ {
		matched := true
		for offset := range needle {
			if haystack[start+offset] != needle[offset] {
				matched = false
				break
			}
		}
		if matched {
			return positions[start], positions[start+len(needle)-1] + 1, true
		}
	}
	return 0, 0, false
}

func normalizedQuoteRunes(text string) ([]rune, []int) {
	var normalized []rune
	var positions []int
	spacePending := false
	spacePosition := 0
	for index, value := range []rune(text) {
		if unicode.IsSpace(value) {
			if len(normalized) > 0 && !spacePending {
				spacePending = true
				spacePosition = index
			}
			continue
		}
		if spacePending {
			normalized = append(normalized, ' ')
			positions = append(positions, spacePosition)
			spacePending = false
		}
		normalized = append(normalized, unicode.ToLower(value))
		positions = append(positions, index)
	}
	return normalized, positions
}

func (f *Fabric) validateDraft(ctx context.Context, draft MemoryDraft) (MemoryDraft, map[string]RawEvent, error) {
	draft.Statement = strings.TrimSpace(draft.Statement)
	normalizeDraftKind(&draft)
	if !validateNodeKind(draft.Kind) || draft.Statement == "" {
		return draft, nil, errors.New("semantic node kind and statement are required")
	}
	draft.Keys = normalizeStringList(draft.Keys, 4)
	draft.RetrievalCues = normalizeStringList(draft.RetrievalCues, 4)
	normalizedSources, events, err := f.normalizeSourceSpans(ctx, draft.Sources)
	if err != nil {
		return draft, nil, err
	}
	draft.Sources = normalizedSources
	draft = normalizeGroundedDraftProjection(draft, events)
	if draft.Kind == NodeClaim {
		if strings.TrimSpace(draft.Subject) == "" || !validateFacet(draft.Facet) ||
			strings.TrimSpace(draft.AttributeKey) == "" {
			return draft, nil, errors.New("claim subject, facet, and attribute_key are required")
		}
		if draft.ClaimType == "" {
			draft.ClaimType = claimTypeForFacet(draft.Facet)
		}
		if draft.Value.Kind == "" {
			draft.Value = ClaimValue{Kind: ValueText, Text: draft.Statement}
		}
	}
	if draft.EvidenceMode == "" {
		draft.EvidenceMode = EvidenceInferred
	}
	if draft.Value.Kind == ValueNumber && draft.EvidenceMode != EvidenceInferred {
		needle := strconv.FormatFloat(draft.Value.Number, 'g', -1, 64)
		if !sourcesContain(events, draft.Sources, needle) {
			return draft, nil, fmt.Errorf("numeric claim value %s is not grounded in its source spans", needle)
		}
	}
	if draft.Value.Kind == ValueTime && !draft.Value.Time.IsZero() && draft.EvidenceMode != EvidenceInferred {
		date := draft.Value.Time.UTC().Format("2006-01-02")
		var grounded bool
		draft.Sources, grounded = groundTimeSources(events, draft.Sources, draft.Value.Time)
		if !grounded {
			return draft, nil, fmt.Errorf("time claim value %s is not grounded in its source spans", date)
		}
	}
	return draft, events, nil
}

func normalizeDraftKind(draft *MemoryDraft) {
	switch ClaimType(strings.ToLower(strings.TrimSpace(string(draft.Kind)))) {
	case ClaimFact, ClaimState, ClaimPreference, ClaimConstraint:
		if draft.ClaimType == "" {
			draft.ClaimType = ClaimType(draft.Kind)
		}
		draft.Kind = NodeClaim
	}
}

// normalizeGroundedDraftProjection repairs only structural omissions that can
// be derived from cited evidence or compiler-provided retrieval keys. It never
// changes the statement or typed value.
func normalizeGroundedDraftProjection(draft MemoryDraft, events map[string]RawEvent) MemoryDraft {
	if draft.Kind == NodeClaim {
		switch strings.ToLower(strings.TrimSpace(string(draft.Facet))) {
		case "episode":
			draft.Kind = NodeEpisode
			draft.Facet = ""
			draft.AttributeKey = ""
		case "procedure":
			draft.Kind = NodeProcedure
			draft.Facet = ""
			draft.AttributeKey = ""
		}
	}
	if draft.Kind != NodeClaim {
		return draft
	}
	if strings.TrimSpace(draft.Subject) == "" {
		draft.Subject = commonSourceActor(draft.Sources, events)
		if draft.Subject != "" && strings.TrimSpace(draft.SubjectType) == "" {
			draft.SubjectType = "actor"
		}
	}
	if strings.TrimSpace(draft.AttributeKey) == "" && len(draft.Keys) > 0 {
		draft.AttributeKey = draft.Keys[0]
	}
	if draft.ClaimType == "" && validateFacet(draft.Facet) {
		draft.ClaimType = claimTypeForFacet(draft.Facet)
	}
	return draft
}

func commonSourceActor(spans []SourceSpan, events map[string]RawEvent) string {
	actor := ""
	for _, span := range spans {
		candidate := strings.TrimSpace(events[span.EventID].Actor)
		if candidate == "" {
			continue
		}
		if actor == "" {
			actor = candidate
			continue
		}
		if !strings.EqualFold(actor, candidate) {
			return ""
		}
	}
	return actor
}

func claimTypeForFacet(facet Facet) ClaimType {
	switch facet {
	case FacetPreference:
		return ClaimPreference
	case FacetConstraint:
		return ClaimConstraint
	case FacetState, FacetConfiguration, FacetLocation, FacetGoal, FacetProcedureState:
		return ClaimState
	default:
		return ClaimFact
	}
}

func sourcesContain(events map[string]RawEvent, spans []SourceSpan, needle string) bool {
	needle = strings.ToLower(strings.TrimSpace(needle))
	for _, span := range spans {
		runes := []rune(events[span.EventID].Content)
		if span.StartRune >= 0 && span.EndRune <= len(runes) && span.EndRune > span.StartRune {
			if strings.Contains(strings.ToLower(string(runes[span.StartRune:span.EndRune])), needle) {
				return true
			}
		}
	}
	return false
}

func groundTimeSources(events map[string]RawEvent, spans []SourceSpan, value time.Time) ([]SourceSpan, bool) {
	value = value.UTC()
	if sourcesContain(events, spans, value.Format("2006-01-02")) {
		return spans, true
	}
	for _, span := range spans {
		event := events[span.EventID]
		if containsAny(strings.ToLower(sourceSpanText(event, span)), timeGroundingForms(value, event.OccurredAt)) {
			return spans, true
		}
	}
	seen := map[string]struct{}{}
	for _, span := range spans {
		if _, ok := seen[span.EventID]; ok {
			continue
		}
		seen[span.EventID] = struct{}{}
		event := events[span.EventID]
		start, end, ok := findAnyRuneRange(event.Content, timeGroundingForms(value, event.OccurredAt))
		if !ok {
			continue
		}
		return append(spans, SourceSpan{EventID: span.EventID, StartRune: start, EndRune: end, Role: "support"}), true
	}
	return spans, false
}

func timeGroundingForms(value, eventTime time.Time) []string {
	value = value.UTC()
	month := strings.ToLower(value.Month().String())
	shortMonth := month
	if len(shortMonth) > 3 {
		shortMonth = shortMonth[:3]
	}
	day := strconv.Itoa(value.Day())
	ordinal := day + ordinalSuffix(value.Day())
	year := strconv.Itoa(value.Year())
	forms := []string{
		month + " " + day + ", " + year, month + " " + ordinal + ", " + year,
		month + " " + day + " " + year, month + " " + ordinal + " " + year,
		shortMonth + " " + day + ", " + year, shortMonth + " " + ordinal + ", " + year,
		day + " " + month + " " + year, ordinal + " " + month + " " + year,
		fmt.Sprintf("%d/%d/%d", int(value.Month()), value.Day(), value.Year()),
		fmt.Sprintf("%02d/%02d/%d", int(value.Month()), value.Day(), value.Year()),
		fmt.Sprintf("%d-%d-%d", value.Year(), int(value.Month()), value.Day()),
		fmt.Sprintf("%d年%d月%d日", value.Year(), int(value.Month()), value.Day()),
	}
	if eventTime.IsZero() {
		return forms
	}
	eventTime = eventTime.UTC()
	inferred := time.Date(eventTime.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
	if inferred.After(eventTime.Add(24 * time.Hour)) {
		inferred = inferred.AddDate(-1, 0, 0)
	}
	if inferred.Year() != value.Year() {
		return forms
	}
	return append(forms,
		month+" "+day, month+" "+ordinal, shortMonth+" "+day, shortMonth+" "+ordinal,
		day+" "+month, ordinal+" "+month,
		fmt.Sprintf("%d/%d", int(value.Month()), value.Day()),
		fmt.Sprintf("%02d/%02d", int(value.Month()), value.Day()),
		fmt.Sprintf("%d月%d日", int(value.Month()), value.Day()),
	)
}

func sourceSpanText(event RawEvent, span SourceSpan) string {
	runes := []rune(event.Content)
	if span.StartRune < 0 || span.EndRune > len(runes) || span.EndRune <= span.StartRune {
		return ""
	}
	return string(runes[span.StartRune:span.EndRune])
}

func containsAny(text string, values []string) bool {
	for _, value := range values {
		if value != "" && strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func findAnyRuneRange(text string, values []string) (int, int, bool) {
	lower := strings.ToLower(text)
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		byteIndex := strings.Index(lower, value)
		if byteIndex < 0 {
			continue
		}
		start := utf8.RuneCountInString(lower[:byteIndex])
		return start, start + utf8.RuneCountInString(value), true
	}
	return 0, 0, false
}

func ordinalSuffix(day int) string {
	if day%100 >= 11 && day%100 <= 13 {
		return "th"
	}
	switch day % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	default:
		return "th"
	}
}

func placeholders(count int) string {
	if count <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func appendAny(prefix []any, values []string) []any {
	result := append([]any(nil), prefix...)
	for _, value := range values {
		result = append(result, value)
	}
	return result
}
