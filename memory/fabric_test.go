package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingCompiler struct {
	mu      sync.Mutex
	calls   int
	fail    error
	compile func(CompileRequest) CompileResponse
}

type sourceArtifactCompiler struct {
	mu       sync.Mutex
	apiCalls int
	cache    map[string]CompileResponse
}

func (c *sourceArtifactCompiler) Compile(_ context.Context, request CompileRequest) (CompileResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := compileSourcesHash(request.Sources)
	if cached, ok := c.cache[key]; ok {
		cached.CacheHit = true
		return cached, nil
	}
	c.apiCalls++
	source := request.Sources[0]
	response := CompileResponse{Nodes: []MemoryDraft{{Kind: NodeClaim, ClaimType: ClaimPreference,
		Statement: "The user prefers Vim.", Subject: "user", Facet: FacetPreference, AttributeKey: "editor",
		Value: ClaimValue{Kind: ValueText, Text: "Vim"}, EvidenceMode: EvidenceUserDeclared,
		Sources: []SourceSpan{{SourceRef: source.SourceRef, Text: "I prefer Vim."}}, Keys: []string{"editor"}}},
		Usage: APIUsage{Calls: 1, InputTokens: 10, OutputTokens: 5}}
	if c.cache == nil {
		c.cache = map[string]CompileResponse{}
	}
	cached := response
	cached.Usage = APIUsage{}
	c.cache[key] = cached
	return response, nil
}

type compilerWithUsageError struct {
	usage APIUsage
	err   error
}

func (c compilerWithUsageError) Compile(context.Context, CompileRequest) (CompileResponse, error) {
	return CompileResponse{Usage: c.usage}, c.err
}

func (c *recordingCompiler) Compile(_ context.Context, request CompileRequest) (CompileResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.fail != nil {
		return CompileResponse{}, c.fail
	}
	if c.compile != nil {
		return c.compile(request), nil
	}
	return CompileResponse{}, nil
}

func (c *recordingCompiler) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type recordingAdjudicator struct {
	mu       sync.Mutex
	calls    int
	decision ConflictDecision
	last     AdjudicationRequest
}

func (a *recordingAdjudicator) Adjudicate(_ context.Context, request AdjudicationRequest) (AdjudicationResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.last = request
	response := AdjudicationResponse{Decision: a.decision, Usage: APIUsage{Calls: 1, InputTokens: 30, OutputTokens: 8, Model: "judge"}}
	if a.decision == DecisionSupersedes {
		latest := request.Conflict.Members[0]
		for _, member := range request.Conflict.Members[1:] {
			if member.CreatedAt.After(latest.CreatedAt) || member.ValidFrom.After(latest.ValidFrom) {
				latest = member
			}
		}
		response.WinnerIDs = []string{latest.ID}
		for _, member := range request.Conflict.Members {
			if member.ID != latest.ID {
				response.LoserIDs = append(response.LoserIDs, member.ID)
			}
			for _, source := range member.Sources {
				response.SupportIDs = append(response.SupportIDs, source.EventID)
			}
		}
	}
	return response, nil
}

func (a *recordingAdjudicator) Calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

type tinyVectorizer struct{}

func (tinyVectorizer) Model() string   { return "tiny-local" }
func (tinyVectorizer) Dimensions() int { return 2 }
func (tinyVectorizer) Embed(_ context.Context, texts []string, _ VectorPurpose) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for index, text := range texts {
		if strings.Contains(strings.ToLower(text), "atlas") {
			result[index] = []float32{1, 0}
		} else {
			result[index] = []float32{0, 1}
		}
	}
	return result, nil
}

type queryDelayVectorizer struct {
	delay time.Duration
}

func (queryDelayVectorizer) Model() string   { return "query-delay-local" }
func (queryDelayVectorizer) Dimensions() int { return 2 }
func (v queryDelayVectorizer) Embed(ctx context.Context, texts []string, purpose VectorPurpose) ([][]float32, error) {
	if purpose == VectorQuery {
		select {
		case <-time.After(v.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return tinyVectorizer{}.Embed(ctx, texts, purpose)
}

func openTestFabric(t *testing.T, compiler SemanticCompiler, adjudicator ConflictAdjudicator, vectorizer Vectorizer) *Fabric {
	t.Helper()
	options := DefaultFabricOptions(t.TempDir())
	options.Compiler = compiler
	options.Adjudicator = adjudicator
	options.Vectorizer = vectorizer
	options.StartWorkers = false
	fabric, err := OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fabric.Close() })
	return fabric
}

func TestOpenFabricSupportsConcurrentStores(t *testing.T) {
	ctx := context.Background()
	const stores = 20
	base := t.TempDir()
	errors := make(chan error, stores)
	var group sync.WaitGroup
	for index := 0; index < stores; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			options := DefaultFabricOptions(filepath.Join(base, fmt.Sprintf("store-%d", index)))
			options.StartWorkers = false
			fabric, err := OpenFabric(ctx, options)
			if err == nil {
				err = fabric.Close()
			}
			errors <- err
		}(index)
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func testEvent(id, content string, at time.Time) RawEvent {
	return RawEvent{ID: id, Space: "test", ContextID: "ctx", SessionID: "session",
		Actor: "user", SourceKind: "conversation", Content: content, OccurredAt: at}
}

func draftForEvent(event RawEvent, value string, facet Facet, mode EvidenceMode) MemoryDraft {
	return MemoryDraft{Kind: NodeClaim, ClaimType: ClaimFact, Statement: event.Content,
		Subject: "workspace", SubjectType: "project", Facet: facet, AttributeKey: "mode",
		Value: ClaimValue{Kind: ValueText, Text: value}, ValidFrom: event.OccurredAt,
		EvidenceMode: mode, Sources: []SourceSpan{{EventID: event.ID, SourceRef: event.SourceRef, StartRune: 0,
			EndRune: len([]rune(event.Content)), Text: event.Content, Role: "support"}}, Keys: []string{"workspace", "mode"}}
}

func eventForCompileSource(source CompileSource) RawEvent {
	return RawEvent{SourceRef: source.SourceRef, SessionID: source.SessionRef, Actor: source.Actor,
		Content: source.Text, OccurredAt: source.OccurredAt}
}

func TestFabricAppendIsDurableSearchableAndZeroAPI(t *testing.T) {
	compiler := &recordingCompiler{}
	fabric := openTestFabric(t, compiler, nil, nil)
	event := testEvent("event-1", "Project Atlas uses the quartz deployment profile.", time.Now().UTC())
	result, err := fabric.AppendEvents(context.Background(), []RawEvent{event}, IngestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Durable || len(result.EventIDs) != 1 {
		t.Fatalf("unexpected ingest result: %+v", result)
	}
	search, err := fabric.Search(context.Background(), SearchRequest{Space: "test", Query: "Atlas quartz"})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Evidence) == 0 || !strings.Contains(search.Evidence[0].Content, "Atlas") {
		t.Fatalf("raw event was not recalled: %+v", search)
	}
	if compiler.Calls() != 0 {
		t.Fatalf("ordinary append made %d compiler calls", compiler.Calls())
	}
	report, err := fabric.Doctor(context.Background())
	if err != nil || !report.Healthy {
		t.Fatalf("doctor failed: report=%+v err=%v", report, err)
	}
}

func TestFabricExplicitRememberCallsCompilerExactlyOnce(t *testing.T) {
	compiler := &recordingCompiler{compile: func(request CompileRequest) CompileResponse {
		event := eventForCompileSource(request.Sources[0])
		return CompileResponse{Nodes: []MemoryDraft{draftForEvent(event, "quartz", FacetProfile, EvidenceUserDeclared)},
			Usage: APIUsage{Calls: 1, InputTokens: 20, OutputTokens: 10, Model: "compiler"}}
	}}
	fabric := openTestFabric(t, compiler, nil, nil)
	event := testEvent("event-explicit", "Remember that workspace mode is quartz.", time.Now().UTC())
	result, err := fabric.Remember(context.Background(), MemoryRequest{Space: "test", ContextID: "ctx",
		Events: []RawEvent{event}, Mode: WriteExplicit, RequireSemantic: true})
	if err != nil {
		t.Fatal(err)
	}
	if compiler.Calls() != 1 || result.SemanticStatus != SemanticActive || len(result.MemoryIDs) != 1 {
		t.Fatalf("unexpected explicit commit: calls=%d result=%+v", compiler.Calls(), result)
	}
	if result.Usage.Calls != 1 {
		t.Fatalf("unexpected API usage: %+v", result.Usage)
	}
}

func TestFabricSharedSourceArtifactRemapsLocalEventIDs(t *testing.T) {
	compiler := &sourceArtifactCompiler{}
	occurredAt := time.Date(2026, time.July, 20, 10, 0, 0, 0, time.UTC)
	commit := func(eventID string) string {
		fabric := openTestFabric(t, compiler, nil, nil)
		event := testEvent(eventID, "I prefer Vim.", occurredAt)
		event.SourceRef = "shared-session:0001"
		result, err := fabric.Remember(context.Background(), MemoryRequest{Space: "test", ContextID: "ctx",
			Events: []RawEvent{event}, Mode: WriteExplicit, RequireSemantic: true})
		if err != nil {
			t.Fatal(err)
		}
		nodes, err := fabric.loadMemoryNodes(context.Background(), result.MemoryIDs)
		if err != nil || len(nodes) != 1 || len(nodes[0].Sources) != 1 {
			t.Fatalf("unexpected remapped nodes: nodes=%+v err=%v", nodes, err)
		}
		return nodes[0].Sources[0].EventID
	}
	if got := commit("event-case-a"); got != "event-case-a" {
		t.Fatalf("first artifact mapped to %q", got)
	}
	if got := commit("event-case-b"); got != "event-case-b" {
		t.Fatalf("cached artifact mapped to %q", got)
	}
	compiler.mu.Lock()
	defer compiler.mu.Unlock()
	if compiler.apiCalls != 1 {
		t.Fatalf("shared artifact made %d API calls, want 1", compiler.apiCalls)
	}
}

func TestFabricRepairsGroundedProjectionOmissions(t *testing.T) {
	compiler := &recordingCompiler{compile: func(request CompileRequest) CompileResponse {
		event := eventForCompileSource(request.Sources[0])
		return CompileResponse{Nodes: []MemoryDraft{{
			Kind: NodeClaim, Statement: event.Content, Facet: FacetPreference,
			Sources: []SourceSpan{{SourceRef: event.SourceRef, Text: event.Content}},
			Keys:    []string{"editor"}, EvidenceMode: EvidenceUserDeclared,
		}}}
	}}
	fabric := openTestFabric(t, compiler, nil, nil)
	event := testEvent("event-projection", "The user prefers Vim.", time.Now().UTC())
	result, err := fabric.Remember(context.Background(), MemoryRequest{Space: "test", ContextID: "ctx",
		Events: []RawEvent{event}, Mode: WriteExplicit, RequireSemantic: true})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := fabric.loadMemoryNodes(context.Background(), result.MemoryIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Subject != "user" || nodes[0].AttributeKey != "editor" ||
		nodes[0].ClaimType != ClaimPreference {
		t.Fatalf("unexpected normalized projection: %+v", nodes)
	}
}

func TestFabricDoesNotGuessUngroundedProjectionKeys(t *testing.T) {
	compiler := &recordingCompiler{compile: func(request CompileRequest) CompileResponse {
		event := eventForCompileSource(request.Sources[0])
		return CompileResponse{Nodes: []MemoryDraft{{
			Kind: NodeClaim, Statement: event.Content, Facet: FacetPreference,
			Sources:      []SourceSpan{{SourceRef: event.SourceRef, Text: event.Content}},
			EvidenceMode: EvidenceUserDeclared,
		}}}
	}}
	fabric := openTestFabric(t, compiler, nil, nil)
	event := testEvent("event-no-key", "The user prefers a particular editor.", time.Now().UTC())
	result, err := fabric.Remember(context.Background(), MemoryRequest{Space: "test", ContextID: "ctx",
		Events: []RawEvent{event}, Mode: WriteExplicit, RequireSemantic: true})
	if err == nil || result.SemanticStatus != SemanticQuarantined {
		t.Fatalf("missing compiler key should remain quarantined: result=%+v err=%v", result, err)
	}
}

func TestFabricClampsWholeEventSpanAndGroundsNaturalDate(t *testing.T) {
	compiler := &recordingCompiler{compile: func(request CompileRequest) CompileResponse {
		event := eventForCompileSource(request.Sources[0])
		return CompileResponse{Nodes: []MemoryDraft{{
			Kind: NodeClaim, ClaimType: ClaimState,
			Statement: "User has worn leather boots since January 15th.", Subject: "user",
			Facet: FacetState, AttributeKey: "leather-boots",
			Value:        ClaimValue{Kind: ValueTime, Time: time.Date(2023, time.January, 15, 0, 0, 0, 0, time.UTC)},
			EvidenceMode: EvidenceUserDeclared,
			Sources:      []SourceSpan{{SourceRef: event.SourceRef, Text: "I've worn them daily since January 15th."}},
			Keys:         []string{"leather boots"},
		}}}
	}}
	fabric := openTestFabric(t, compiler, nil, nil)
	event := testEvent("event-natural-date", "I'm looking for advice about my leather boots. I've worn them daily since January 15th.",
		time.Date(2023, time.May, 20, 3, 29, 0, 0, time.UTC))
	result, err := fabric.Remember(context.Background(), MemoryRequest{Space: "test", ContextID: "ctx",
		Events: []RawEvent{event}, Mode: WriteExplicit, RequireSemantic: true})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := fabric.loadMemoryNodes(context.Background(), result.MemoryIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}
	length := len([]rune(event.Content))
	for _, source := range nodes[0].Sources {
		if source.EndRune > length {
			t.Fatalf("source span was not clamped: %+v length=%d", source, length)
		}
	}
	clamped, _, err := fabric.normalizeSourceSpans(context.Background(), []SourceSpan{{
		EventID: event.ID, StartRune: 0, EndRune: length + 64,
	}})
	if err != nil || len(clamped) != 1 || clamped[0].EndRune != length {
		t.Fatalf("whole-event span was not clamped: spans=%+v err=%v", clamped, err)
	}
	if _, grounded := groundTimeSources(map[string]RawEvent{event.ID: event}, nodes[0].Sources,
		time.Date(2023, time.January, 15, 0, 0, 0, 0, time.UTC)); !grounded {
		t.Fatalf("natural date was not grounded: %+v", nodes[0].Sources)
	}
}

func TestFabricRejectsLargeOffsetSpanOutsideEvent(t *testing.T) {
	fabric := openTestFabric(t, nil, nil, nil)
	event := testEvent("event-bad-offset", "short evidence", time.Now().UTC())
	if _, err := fabric.AppendEvents(context.Background(), []RawEvent{event}, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	_, _, err := fabric.normalizeSourceSpans(context.Background(), []SourceSpan{{
		EventID: event.ID, StartRune: 5, EndRune: 500,
	}})
	if err == nil {
		t.Fatal("expected a non-zero, widely overlong source span to be rejected")
	}
}

func TestFabricGroundsEllipsisQuoteAsDisjointExactSpans(t *testing.T) {
	fabric := openTestFabric(t, nil, nil, nil)
	event := testEvent("event-ellipsis", "Alpha first detail with words. Other text. Beta second detail.", time.Now().UTC())
	if _, err := fabric.AppendEvents(context.Background(), []RawEvent{event}, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	spans, _, err := fabric.normalizeSourceSpans(context.Background(), []SourceSpan{{
		EventID: event.ID, Text: "first detail...Beta second",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 {
		t.Fatalf("grounded spans = %d, want 2: %+v", len(spans), spans)
	}
	runes := []rune(event.Content)
	if got := string(runes[spans[0].StartRune:spans[0].EndRune]); got != "first detail" {
		t.Fatalf("first grounded quote = %q", got)
	}
	if got := string(runes[spans[1].StartRune:spans[1].EndRune]); got != "Beta second" {
		t.Fatalf("second grounded quote = %q", got)
	}
}

func TestFabricNormalizesClaimTypeUsedAsNodeKind(t *testing.T) {
	fabric := openTestFabric(t, nil, nil, nil)
	event := testEvent("event-kind", "I prefer Vim.", time.Now().UTC())
	if _, err := fabric.AppendEvents(context.Background(), []RawEvent{event}, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	draft, _, err := fabric.validateDraft(context.Background(), MemoryDraft{
		Kind: NodeKind("preference"), Statement: "User prefers Vim.", Subject: "user",
		Facet: FacetPreference, AttributeKey: "editor", Value: ClaimValue{Kind: ValueText, Text: "Vim"},
		Sources: []SourceSpan{{EventID: event.ID, Text: "I prefer Vim."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if draft.Kind != NodeClaim || draft.ClaimType != ClaimPreference {
		t.Fatalf("normalized kind=%q claim_type=%q", draft.Kind, draft.ClaimType)
	}
}

func TestFabricComputesSourceOffsetsFromCompilerQuote(t *testing.T) {
	compiler := &recordingCompiler{compile: func(request CompileRequest) CompileResponse {
		event := eventForCompileSource(request.Sources[0])
		return CompileResponse{Nodes: []MemoryDraft{{
			Kind: NodeClaim, ClaimType: ClaimPreference, Statement: "User prefers Vim for editing.",
			Subject: "user", Facet: FacetPreference, AttributeKey: "editor",
			Value: ClaimValue{Kind: ValueText, Text: "Vim"}, EvidenceMode: EvidenceUserDeclared,
			Sources: []SourceSpan{{SourceRef: event.SourceRef, StartRune: 9_000, EndRune: 10_000,
				Text: "THE USER PREFERS VIM\tFOR EDITING."}}, Keys: []string{"editor"},
		}}}
	}}
	fabric := openTestFabric(t, compiler, nil, nil)
	event := testEvent("event-quoted-source", "前文。The user prefers Vim\nfor editing. 后文。", time.Now().UTC())
	result, err := fabric.Remember(context.Background(), MemoryRequest{Space: "test", ContextID: "ctx",
		Events: []RawEvent{event}, Mode: WriteExplicit, RequireSemantic: true})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := fabric.loadMemoryNodes(context.Background(), result.MemoryIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || len(nodes[0].Sources) != 1 {
		t.Fatalf("unexpected grounded nodes: %+v", nodes)
	}
	source := nodes[0].Sources[0]
	if source.StartRune != 3 || source.EndRune != 36 || source.Text != "" {
		t.Fatalf("quote was not converted to durable local offsets: %+v", source)
	}
}

func TestFabricComputesQuoteOffsetsInLongMultiEventBatch(t *testing.T) {
	const quote = "The migration target is the quartz profile."
	prefix := strings.Repeat("历史记录", 1_500)
	compiler := &recordingCompiler{compile: func(request CompileRequest) CompileResponse {
		if len(request.Sources) != 2 {
			t.Fatalf("compiler sources = %d, want 2", len(request.Sources))
		}
		return CompileResponse{Nodes: []MemoryDraft{{
			Kind: NodeClaim, ClaimType: ClaimFact, Statement: quote,
			Subject: "workspace", Facet: FacetConfiguration, AttributeKey: "migration-profile",
			Value: ClaimValue{Kind: ValueText, Text: "quartz"}, EvidenceMode: EvidenceUserDeclared,
			Sources: []SourceSpan{{SourceRef: request.Sources[1].SourceRef, Text: quote}},
			Keys:    []string{"migration profile"},
		}}}
	}}
	fabric := openTestFabric(t, compiler, nil, nil)
	first := testEvent("event-long-first", strings.Repeat("unrelated context ", 200), time.Now().UTC())
	second := testEvent("event-long-second", prefix+quote+" 尾声", time.Now().UTC().Add(time.Second))
	result, err := fabric.Remember(context.Background(), MemoryRequest{Space: "test", ContextID: "ctx",
		Events: []RawEvent{first, second}, Mode: WriteExplicit, RequireSemantic: true})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := fabric.loadMemoryNodes(context.Background(), result.MemoryIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || len(nodes[0].Sources) != 1 ||
		nodes[0].Sources[0].StartRune != len([]rune(prefix)) ||
		nodes[0].Sources[0].EndRune != len([]rune(prefix+quote)) {
		t.Fatalf("long multi-event quote was not grounded locally: %+v", nodes)
	}
}

func TestFabricRejectsCompilerQuoteMissingFromEvent(t *testing.T) {
	compiler := &recordingCompiler{compile: func(request CompileRequest) CompileResponse {
		return CompileResponse{Nodes: []MemoryDraft{{
			Kind: NodeClaim, ClaimType: ClaimFact, Statement: "The workspace uses onyx.",
			Subject: "workspace", Facet: FacetConfiguration, AttributeKey: "profile",
			Value: ClaimValue{Kind: ValueText, Text: "onyx"}, EvidenceMode: EvidenceUserDeclared,
			Sources: []SourceSpan{{SourceRef: request.Sources[0].SourceRef, Text: "The workspace uses onyx."}},
			Keys:    []string{"workspace profile"},
		}}}
	}}
	fabric := openTestFabric(t, compiler, nil, nil)
	event := testEvent("event-missing-quote", "The workspace uses quartz.", time.Now().UTC())
	result, err := fabric.Remember(context.Background(), MemoryRequest{Space: "test", ContextID: "ctx",
		Events: []RawEvent{event}, Mode: WriteExplicit, RequireSemantic: true})
	if err == nil || result.SemanticStatus != SemanticQuarantined {
		t.Fatalf("ungrounded quote became active: result=%+v err=%v", result, err)
	}
}

func TestFabricNormalCompileJobQuarantinesWithoutFailingFlush(t *testing.T) {
	compiler := &recordingCompiler{compile: func(request CompileRequest) CompileResponse {
		return CompileResponse{Nodes: []MemoryDraft{{
			Kind: NodeClaim, ClaimType: ClaimFact, Statement: "Invented semantic claim.",
			Subject: "user", Facet: FacetProfile, AttributeKey: "invented",
			Value: ClaimValue{Kind: ValueText, Text: "invented"}, EvidenceMode: EvidenceUserDeclared,
			Sources: []SourceSpan{{SourceRef: request.Sources[0].SourceRef, Text: "This quote is absent."}},
			Keys:    []string{"invented"},
		}}}
	}}
	fabric := openTestFabric(t, compiler, nil, nil)
	event := testEvent("event-background-quarantine", "Durable raw evidence remains searchable.", time.Now().UTC())
	if _, err := fabric.AppendEvents(context.Background(), []RawEvent{event},
		IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.SealContext(context.Background(), ContextRef{ID: event.ContextID, Space: event.Space}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(context.Background()); err != nil {
		t.Fatalf("normal background quarantine stopped the write pipeline: %v", err)
	}
	var failedJobs int
	if err := fabric.ledger.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status='failed'`).Scan(&failedJobs); err != nil {
		t.Fatal(err)
	}
	var eventStatus SemanticStatus
	if err := fabric.ledger.QueryRow(`SELECT semantic_status FROM events WHERE event_id=?`, event.ID).Scan(&eventStatus); err != nil {
		t.Fatal(err)
	}
	if failedJobs != 0 || eventStatus != SemanticQuarantined || compiler.Calls() != 1 {
		t.Fatalf("unexpected background result: failed_jobs=%d event_status=%s", failedJobs, eventStatus)
	}
}

func TestSecretRedactionPreservesRuneOffsets(t *testing.T) {
	input := "凭据 password=秘密值 后续证据"
	redacted := redactSecrets(input)
	if len([]rune(redacted)) != len([]rune(input)) {
		t.Fatalf("redaction changed rune offsets: input=%q redacted=%q", input, redacted)
	}
}

func TestFabricCompileJobReusesRepairableQuarantine(t *testing.T) {
	compiler := &recordingCompiler{}
	fabric := openTestFabric(t, compiler, nil, nil)
	event := testEvent("event-quarantine-reuse", "The user prefers concise answers.", time.Now().UTC())
	if _, err := fabric.AppendEvents(context.Background(), []RawEvent{event},
		IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	draft := MemoryDraft{
		Kind: NodeClaim, ClaimType: ClaimPreference, Statement: event.Content,
		Subject: "user", Facet: FacetPreference, AttributeKey: "response-style",
		Value: ClaimValue{Kind: ValueText, Text: "concise"}, EvidenceMode: EvidenceUserDeclared,
		Sources: []SourceSpan{{EventID: event.ID, StartRune: 0, EndRune: len([]rune(event.Content)) + 64}},
		Keys:    []string{"response style"},
	}
	request := MemoryRequest{Space: event.Space, ContextID: event.ContextID,
		SourceEventIDs: []string{event.ID}, Mode: WriteNormal, RequireSemantic: true}
	if err := fabric.persistQuarantine(context.Background(), request, draft, errors.New("old validator failure")); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.SealContext(context.Background(), ContextRef{ID: event.ContextID, Space: event.Space}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if compiler.Calls() != 0 {
		t.Fatalf("repairable quarantine triggered %d redundant compiler calls", compiler.Calls())
	}
	var active int
	if err := fabric.ledger.QueryRow(`SELECT COUNT(*) FROM memory_nodes WHERE status=? AND statement=?`,
		SemanticActive, event.Content).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active recovered nodes = %d, want 1", active)
	}
}

func TestFabricUsageObserverSeesCompilerUsageOnResponseError(t *testing.T) {
	options := DefaultFabricOptions(t.TempDir())
	options.StartWorkers = false
	options.Compiler = compilerWithUsageError{usage: APIUsage{Calls: 1, InputTokens: 90,
		OutputTokens: 12, Model: "compiler"}, err: errors.New("invalid structured response")}
	var observed APIUsageEvent
	options.UsageObserver = func(_ context.Context, event APIUsageEvent) error {
		observed = event
		return nil
	}
	fabric, err := OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer fabric.Close()
	event := testEvent("event-usage-error", "Remember the quartz profile.", time.Now().UTC())
	_, err = fabric.Remember(context.Background(), MemoryRequest{Space: "test", ContextID: "ctx",
		Events: []RawEvent{event}, Mode: WriteExplicit, RequireSemantic: true})
	if err == nil {
		t.Fatal("expected compiler error")
	}
	if observed.Stage != APIStageSemanticCompile || observed.Usage.InputTokens != 90 ||
		observed.Usage.OutputTokens != 12 || observed.Error == "" {
		t.Fatalf("unexpected observed usage: %+v", observed)
	}
}

func TestFabricCriticalConflictUsesIndependentAdjudicator(t *testing.T) {
	compiler := &recordingCompiler{compile: func(request CompileRequest) CompileResponse {
		event := eventForCompileSource(request.Sources[0])
		value := "quartz"
		if strings.Contains(event.Content, "onyx") {
			value = "onyx"
		}
		return CompileResponse{Nodes: []MemoryDraft{draftForEvent(event, value, FacetProfile, EvidenceUserDeclared)},
			Usage: APIUsage{Calls: 1, InputTokens: 20, OutputTokens: 10, Model: "compiler"}}
	}}
	judge := &recordingAdjudicator{decision: DecisionSupersedes}
	fabric := openTestFabric(t, compiler, judge, nil)
	start := time.Now().UTC().Add(-time.Hour)
	first := testEvent("event-old", "Workspace mode is quartz.", start)
	if _, err := fabric.Remember(context.Background(), MemoryRequest{Space: "test", Events: []RawEvent{first},
		Mode: WriteCorrection, RequireSemantic: true}); err != nil {
		t.Fatal(err)
	}
	second := testEvent("event-new", "Correction: workspace mode is onyx.", start.Add(time.Hour))
	result, err := fabric.Remember(context.Background(), MemoryRequest{Space: "test", Events: []RawEvent{second},
		Mode: WriteCorrection, RequireSemantic: true})
	if err != nil {
		t.Fatal(err)
	}
	if compiler.Calls() != 2 || judge.Calls() != 1 {
		t.Fatalf("calls compiler=%d judge=%d", compiler.Calls(), judge.Calls())
	}
	if len(result.ConflictIDs) != 1 || len(result.ResolutionIDs) != 1 || result.SemanticStatus != SemanticActive {
		t.Fatalf("unexpected conflict commit: %+v", result)
	}
	if result.Usage.Calls != 2 {
		t.Fatalf("compiler and judge usage were not combined: %+v", result.Usage)
	}
	judge.mu.Lock()
	defer judge.mu.Unlock()
	for _, member := range judge.last.Conflict.Members {
		if len(member.Sources) == 0 || member.Sources[0].Text == "" {
			t.Fatalf("adjudicator did not receive grounded source excerpts: %+v", member)
		}
	}
}

func TestFabricUncertainConflictPreservesOldActiveView(t *testing.T) {
	compiler := &recordingCompiler{compile: func(request CompileRequest) CompileResponse {
		event := eventForCompileSource(request.Sources[0])
		value := "quartz"
		if strings.Contains(event.Content, "onyx") {
			value = "onyx"
		}
		return CompileResponse{Nodes: []MemoryDraft{draftForEvent(event, value, FacetProfile, EvidenceUserDeclared)}}
	}}
	judge := &recordingAdjudicator{decision: DecisionUncertain}
	fabric := openTestFabric(t, compiler, judge, nil)
	start := time.Now().UTC().Add(-time.Hour)
	if _, err := fabric.Remember(context.Background(), MemoryRequest{Space: "test",
		Events: []RawEvent{testEvent("old", "Workspace mode is quartz.", start)}, Mode: WriteCorrection, RequireSemantic: true}); err != nil {
		t.Fatal(err)
	}
	second, err := fabric.Remember(context.Background(), MemoryRequest{Space: "test",
		Events: []RawEvent{testEvent("new", "Workspace mode may instead be onyx.", start.Add(time.Hour))},
		Mode:   WriteCorrection, RequireSemantic: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.SemanticStatus != SemanticPendingResolution {
		t.Fatalf("uncertain conflict status = %s", second.SemanticStatus)
	}
	var active, pending int
	if err := fabric.ledger.QueryRow(`SELECT COUNT(*) FROM memory_nodes WHERE status=?`, SemanticActive).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := fabric.ledger.QueryRow(`SELECT COUNT(*) FROM memory_nodes WHERE status=?`, SemanticPendingResolution).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if active != 1 || pending != 1 {
		t.Fatalf("uncertain resolution changed active view: active=%d pending=%d", active, pending)
	}
	search, err := fabric.Search(context.Background(), SearchRequest{Space: "test", Query: "workspace mode"})
	if err != nil {
		t.Fatal(err)
	}
	if !search.Insufficient || len(search.Conflicts) != 1 {
		t.Fatalf("unresolved conflict was silently hidden: %+v", search)
	}
}

func TestFabricDeterministicToolProjectionUsesZeroAPI(t *testing.T) {
	compiler := &recordingCompiler{}
	judge := &recordingAdjudicator{decision: DecisionSupersedes}
	fabric := openTestFabric(t, compiler, judge, nil)
	event := RawEvent{ID: "tool-event", Space: "test", ContextID: "ctx", Actor: "tool",
		SourceKind: "command", Content: "go test ./memory succeeded", OccurredAt: time.Now().UTC(),
		Metadata: map[string]string{"command": "go test ./memory", "project": "LuminaCode"}}
	result, err := fabric.AppendEvents(context.Background(), []RawEvent{event}, IngestOptions{SemanticPolicy: SemanticDeterministic})
	if err != nil {
		t.Fatal(err)
	}
	if result.SemanticStatus != SemanticActive || compiler.Calls() != 0 || judge.Calls() != 0 {
		t.Fatalf("deterministic projection used API: result=%+v compiler=%d judge=%d",
			result, compiler.Calls(), judge.Calls())
	}
}

func TestFabricCompilerFailureKeepsRawEvidence(t *testing.T) {
	compiler := &recordingCompiler{fail: errors.New("compiler offline")}
	fabric := openTestFabric(t, compiler, nil, nil)
	event := testEvent("durable-on-failure", "Atlas fallback remains enabled.", time.Now().UTC())
	result, err := fabric.Remember(context.Background(), MemoryRequest{Space: "test", Events: []RawEvent{event},
		Mode: WriteExplicit, RequireSemantic: true})
	if err == nil || !result.Durable {
		t.Fatalf("expected durable semantic failure, result=%+v err=%v", result, err)
	}
	search, searchErr := fabric.Search(context.Background(), SearchRequest{Space: "test", Query: "Atlas fallback"})
	if searchErr != nil || len(search.Evidence) == 0 {
		t.Fatalf("raw recall failed after compiler failure: result=%+v err=%v", search, searchErr)
	}
}

func TestFabricLocalVectorIndexAndIdempotentRestart(t *testing.T) {
	dir := t.TempDir()
	options := DefaultFabricOptions(dir)
	options.StartWorkers = false
	options.Vectorizer = tinyVectorizer{}
	fabric, err := OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	event := testEvent("vector-event", "Atlas uses a local vector index.", time.Now().UTC())
	if _, err := fabric.AppendEvents(context.Background(), []RawEvent{event, event}, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.SealContext(context.Background(), ContextRef{ID: event.ContextID, Space: event.Space}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Close(); err != nil {
		t.Fatal(err)
	}
	fabric, err = OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer fabric.Close()
	var eventCount, docCount int
	_ = fabric.ledger.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&eventCount)
	_ = fabric.index.QueryRow(`SELECT COUNT(*) FROM documents`).Scan(&docCount)
	if eventCount != 1 || docCount != 2 {
		t.Fatalf("idempotent restart counts events=%d docs=%d", eventCount, docCount)
	}
	vectorDocs, vectorErr := fabric.searchVector(context.Background(), "test", "Atlas index",
		analyzeMemoryQuery("Atlas index"), 16)
	if vectorErr != nil {
		t.Fatalf("direct local vector search failed: %v", vectorErr)
	}
	if len(vectorDocs) == 0 {
		var shadowCount int
		_ = fabric.index.QueryRow(`SELECT COUNT(*) FROM _vec_memory_vectors`).Scan(&shadowCount)
		t.Fatalf("vector index returned no documents (shadow rows=%d)", shadowCount)
	}
	search, err := fabric.Search(context.Background(), SearchRequest{Space: "test", Query: "Atlas index",
		IncludeDiagnostics: true})
	if err != nil || len(search.Evidence) == 0 || search.Diagnostics.VectorCandidates == 0 {
		t.Fatalf("local vector search failed: result=%+v err=%v", search, err)
	}
}

func TestFabricIsolatesIncompatibleVectorModelGenerations(t *testing.T) {
	dir := t.TempDir()
	options := DefaultFabricOptions(dir)
	options.StartWorkers = false
	options.Vectorizer = tinyVectorizer{}
	fabric, err := OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := fabric.index.ExecContext(ctx, `INSERT INTO _vec_memory_vectors(
		dataset_id, id, content, meta, embedding) VALUES (?, ?, ?, ?, ?)`,
		vectorDataset("test", "content"), "legacy-vector", "legacy content", `{}`, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.index.ExecContext(ctx, `INSERT INTO index_meta(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, vectorModelMetaKey, "retired-model"); err != nil {
		t.Fatal(err)
	}

	documents, err := fabric.searchVector(ctx, "test", "legacy content",
		analyzeMemoryQuery("legacy content"), 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 0 {
		t.Fatalf("incompatible vector generation leaked into search: %+v", documents)
	}
	var rows int
	if err := fabric.index.QueryRowContext(ctx, `SELECT COUNT(*) FROM _vec_memory_vectors`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("read-only compatibility check mutated vector rows: %d", rows)
	}
	if err := fabric.Close(); err != nil {
		t.Fatal(err)
	}
	fabric, err = OpenFabric(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer fabric.Close()
	if err := fabric.index.QueryRowContext(ctx, `SELECT COUNT(*) FROM _vec_memory_vectors`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("model generation switch retained %d incompatible vectors", rows)
	}
	var contract string
	if err := fabric.index.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key=?`,
		vectorModelMetaKey).Scan(&contract); err != nil {
		t.Fatal(err)
	}
	if contract != (tinyVectorizer{}).Model() {
		t.Fatalf("vector model contract=%q, want %q", contract, (tinyVectorizer{}).Model())
	}
}

func TestFabricExpandsRelevantUserEventsFromMatchedContext(t *testing.T) {
	ctx := context.Background()
	fabric := openTestFabric(t, nil, nil, tinyVectorizer{})
	started := time.Now().UTC().Add(-time.Hour)
	events := []RawEvent{
		testEvent("play-topic", "I attended a production at the local community theater.", started),
		testEvent("play-answer", "I later confirmed that the performance was The Glass Menagerie.", started.Add(time.Minute)),
	}
	if _, err := fabric.AppendEvents(ctx, events, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.SealContext(ctx, ContextRef{ID: events[0].ContextID, Space: events[0].Space}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := fabric.Search(ctx, SearchRequest{Space: "test", Query: "What play did I attend?",
		ReferenceTime: time.Now().UTC(), IncludeDiagnostics: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	sourceOccurrences := 0
	for _, evidence := range result.Evidence {
		sourceOccurrences += strings.Count(evidence.Content, "The Glass Menagerie")
		if strings.Contains(evidence.Content, "The Glass Menagerie") {
			for _, reason := range evidence.MatchReasons {
				if reason == "context-expand" {
					found = true
					break
				}
			}
		}
	}
	if !found {
		t.Fatalf("context expansion omitted the confirming event: result=%+v", result)
	}
	if sourceOccurrences != 1 {
		t.Fatalf("context expansion duplicated its source event %d times: result=%+v", sourceOccurrences, result)
	}
}

func TestFabricContextBudgetPreservesMultipleSourceEvents(t *testing.T) {
	ctx := context.Background()
	fabric := openTestFabric(t, nil, nil, tinyVectorizer{})
	started := time.Now().UTC().Add(-time.Hour)
	user := testEvent("calibration-question", "What routines are used to calibrate laboratory sensors?", started)
	longPrefix := strings.Repeat("The manual describes general maintenance and safety checks. ", 40)
	assistant := testEvent("calibration-answer", longPrefix+
		"The Resonance Sweep is performed by technicians while the sensor array is oscillating.", started.Add(time.Minute))
	assistant.Actor = "assistant"
	if _, err := fabric.AppendEvents(ctx, []RawEvent{user, assistant}, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := fabric.Search(ctx, SearchRequest{Space: "test",
		Query:         "Which calibration routine did you say technicians perform while the sensor array is oscillating?",
		ReferenceTime: time.Now().UTC(), IncludeDiagnostics: true})
	if err != nil {
		t.Fatal(err)
	}
	foundAnswer, foundQuestion := false, false
	for _, evidence := range result.Evidence {
		if strings.Contains(evidence.Content, "Resonance Sweep") {
			foundAnswer = true
		}
		if strings.Contains(evidence.Content, "What routines are used") {
			foundQuestion = true
		}
		if evidence.ResourceKind == "context" && strings.Count(evidence.Content, "\n") > 1 {
			t.Fatalf("source-internal newlines leaked into context event boundaries: %q", evidence.Content)
		}
	}
	if !foundAnswer || !foundQuestion {
		t.Fatalf("one long source consumed the context budget: %+v", result)
	}
}

func TestQueryTokensUseShapeOnlyAndAddSingulars(t *testing.T) {
	tokens := queryTokens("I've been inviting colleagues to conferences lately", 32)
	joined := " " + strings.Join(tokens, " ") + " "
	if strings.Contains(joined, " i ") {
		t.Fatalf("single ASCII token was retained: %v", tokens)
	}
	for _, wanted := range []string{" colleague ", " conference "} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("query tokens missing singular %q: %v", wanted, tokens)
		}
	}
}

func TestFabricContextExpansionReplacesOnlySourceOverlappingProjections(t *testing.T) {
	ctx := context.Background()
	fabric := openTestFabric(t, nil, nil, tinyVectorizer{})
	started := time.Now().UTC().Add(-time.Hour)
	events := []RawEvent{
		testEvent("coupon-source", "I redeemed a $5 coupon on coffee creamer from an email.", started),
		testEvent("coupon-location", "I shop at Target and use its coupon app.", started.Add(time.Minute)),
	}
	if _, err := fabric.AppendEvents(ctx, events, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "partial-node", ResourceID: "partial-node", ResourceKind: "node", ContextID: events[0].ContextID,
			Content: "The coupon came from an email.", SourceEventIDs: []string{events[0].ID}}, score: 1},
		{document: searchDocument{ID: "session-chunk", ResourceKind: "chunk", ContextID: events[0].ContextID,
			Content: "session chunk", SourceEventIDs: []string{events[0].ID, events[1].ID}}, score: .9},
		{document: searchDocument{ID: "detached-chunk", ResourceKind: "chunk", ContextID: events[0].ContextID,
			Content: "detached session projection", SourceEventIDs: []string{"not-selected"}}, score: .8},
	}
	result, expanded := fabric.expandContextCandidates(ctx, "test", candidates,
		analyzeMemoryQuery("Where did I redeem the coupon?"), time.Now().UTC(), 2)
	evidence, nodeIDs, _ := selectEvidence(result, analyzeMemoryQuery("Where did I redeem the coupon?"), 8, 2500)
	if expanded != 1 || len(evidence) != 2 || evidence[0].ResourceKind != "context" ||
		!strings.Contains(evidence[0].Content, "Target") || evidence[1].ID != "detached-chunk" ||
		len(nodeIDs) != 1 || nodeIDs[0] != "partial-node" {
		t.Fatalf("factual context did not replace overlapping partial projections: %+v", result)
	}
}

func TestFabricContextExpansionGetsFreshBudgetAfterCandidateSearch(t *testing.T) {
	ctx := context.Background()
	fabric := openTestFabric(t, nil, nil, queryDelayVectorizer{delay: 150 * time.Millisecond})
	started := time.Now().UTC().Add(-time.Hour)
	events := []RawEvent{
		testEvent("fresh-budget-1", "I recorded the quartz deployment result.", started),
		testEvent("fresh-budget-2", "The quartz deployment completed successfully.", started.Add(time.Minute)),
	}
	if _, err := fabric.AppendEvents(ctx, events, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	fabric.options.SearchLatencyBudget = 100 * time.Millisecond
	result, err := fabric.Search(ctx, SearchRequest{Space: "test", Query: "What was the quartz deployment result?",
		ReferenceTime: time.Now().UTC(), MaxEvidence: 2, MaxContextTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(result.Route, "context-expand") || len(result.Evidence) == 0 ||
		result.Evidence[0].ResourceKind != "context" {
		t.Fatalf("context expansion reused the expired candidate-search budget: route=%v evidence=%+v",
			result.Route, result.Evidence)
	}
}

func TestContextDiversityUsesContextAggregateAsRepresentative(t *testing.T) {
	chunk := &rankedCandidate{document: searchDocument{ID: "chunk-a", ResourceKind: "chunk", ContextID: "a"},
		coverage: .9}
	other := &rankedCandidate{document: searchDocument{ID: "context-b", ResourceKind: "context", ContextID: "b"},
		coverage: .8}
	aggregate := &rankedCandidate{document: searchDocument{ID: "context-a", ResourceKind: "context", ContextID: "a"},
		coverage: .7}
	got := prioritizeContextDiversity([]*rankedCandidate{chunk, other, aggregate})
	if len(got) != 3 || got[0] != aggregate || got[1] != other || got[2] != chunk {
		t.Fatalf("context aggregate did not represent its source session: %+v", got)
	}
}

func TestAnalyzeMemoryQueryDoesNotAssignTaskModes(t *testing.T) {
	queries := []string{
		"How long is my daily commute to work?",
		"How long elapsed between the workshop and graduation?",
		"How many active connections do I currently have?",
		"What percentage change occurred?",
		"Any suggestions based on my history?",
	}
	for _, query := range queries {
		analysis := analyzeMemoryQuery(query)
		if len(analysis.literalTerms) != 0 {
			t.Fatalf("natural-language query %q acquired task metadata: %+v", query, analysis)
		}
	}
}

func TestPrioritizeContextDiversityKeepsOneProjectionPerContextFirst(t *testing.T) {
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "event-a", ContextID: "a"}, coverage: .8},
		{document: searchDocument{ID: "chunk-a", ContextID: "a"}, coverage: .7},
		{document: searchDocument{ID: "event-b", ContextID: "b"}, coverage: .6},
		{document: searchDocument{ID: "node-a", ContextID: "a"}, coverage: .5},
	}
	got := prioritizeContextDiversity(candidates)
	if len(got) != 4 || got[0].document.ID != "event-a" || got[1].document.ID != "event-b" {
		t.Fatalf("context diversity order=%+v", got)
	}
}

func TestQuotedTermsRemainNaturalWhileStructuredIdentifiersUseExactRoute(t *testing.T) {
	quoted := analyzeMemoryQuery(`What did I say about "signed up"?`)
	if len(quoted.literalTerms) != 1 {
		t.Fatalf("quoted natural-language query=%+v", quoted)
	}
	embeddedID := analyzeMemoryQuery("What happened to task_12345?")
	if len(embeddedID.literalTerms) != 1 {
		t.Fatalf("natural-language query with ID=%+v", embeddedID)
	}
	id := analyzeMemoryQuery("task_12345")
	if len(id.literalTerms) != 1 {
		t.Fatalf("standalone ID query=%+v", id)
	}
}

func TestAnalyzeMemoryQueryKeepsIdentifierLikeTermsAsSearchFeatures(t *testing.T) {
	queries := []string{
		"How many pre-1920 American coins are in my collection?",
		"Which radiative transfer model does SIAC_GEE use?",
		"How many fish did I catch before the trip on 7/22?",
	}
	for _, query := range queries {
		analysis := analyzeMemoryQuery(query)
		if len(analysis.tokens) == 0 {
			t.Fatalf("natural-language query %q produced no search terms: %+v", query, analysis)
		}
	}
}

func TestAnalyzeMemoryQueryPreservesStandaloneStructuredTargets(t *testing.T) {
	for _, query := range []string{
		"task_12345",
		"artifact12345",
		"550e8400-e29b-41d4-a716-446655440000",
		"/var/lib/lumina/item.json",
		`C:\Lumina\item.json`,
	} {
		if analysis := analyzeMemoryQuery(query); len(analysis.literalTerms) == 0 {
			t.Fatalf("standalone structured query %q lost its exact search feature: %+v", query, analysis)
		}
	}
}

func TestAnalyzeMemoryQueryDoesNotTreatNaturalHyphenatedWordAsID(t *testing.T) {
	analysis := analyzeMemoryQuery("How many weeks after acceptance did pre-departure orientation start?")
	if len(analysis.literalTerms) != 0 {
		t.Fatalf("natural hyphenated word became an exact ID: %+v", analysis)
	}
	joined := " " + strings.Join(analysis.tokens, " ") + " "
	if !strings.Contains(joined, " pre ") || !strings.Contains(joined, " departure ") ||
		strings.Contains(joined, " pre-departure ") {
		t.Fatalf("query tokens do not match FTS token boundaries: %v", analysis.tokens)
	}
}

func TestWeightQueryTermsUsesIndexStatisticsInsteadOfVocabulary(t *testing.T) {
	ctx := context.Background()
	fabric := openTestFabric(t, nil, nil, nil)
	started := time.Now().UTC().Add(-time.Hour)
	for index := 0; index < 6; index++ {
		content := "shared phrase appears in this context"
		if index == 0 {
			content += " with zirconium"
		}
		event := testEvent(fmt.Sprintf("idf-%d", index), content, started.Add(time.Duration(index)*time.Minute))
		event.ContextID = fmt.Sprintf("idf-context-%d", index)
		if _, err := fabric.AppendEvents(ctx, []RawEvent{event}, IngestOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	analysis := fabric.weightQueryTerms(ctx, "test", analyzeMemoryQuery("shared zirconium phrase"))
	if analysis.frequencies["zirconium"] >= analysis.frequencies["shared"] ||
		analysis.weights["zirconium"] <= analysis.weights["shared"] {
		t.Fatalf("query terms were not weighted by observed context frequency: %+v", analysis)
	}
}

func TestFabricAggregateSearchPreservesDistinctQuantitativeContexts(t *testing.T) {
	ctx := context.Background()
	fabric := openTestFabric(t, nil, nil, nil)
	started := time.Now().UTC().Add(-time.Hour)
	values := []string{"I paid $120 for the north turbine repair.", "I paid $80 for the east turbine repair.", "I paid $40 for the west turbine repair."}
	events := make([]RawEvent, 0, len(values)+4)
	for index, content := range values {
		event := testEvent(fmt.Sprintf("repair-%d", index), content, started.Add(time.Duration(index)*time.Minute))
		event.ContextID, event.SessionID = fmt.Sprintf("repair-context-%d", index), fmt.Sprintf("repair-session-%d", index)
		events = append(events, event)
	}
	for index := 0; index < 4; index++ {
		event := testEvent(fmt.Sprintf("noise-%d", index), "We discussed turbine repair planning and vendor recommendations.", started.Add(time.Duration(index+10)*time.Minute))
		event.ContextID, event.SessionID = fmt.Sprintf("noise-context-%d", index), fmt.Sprintf("noise-session-%d", index)
		events = append(events, event)
	}
	if _, err := fabric.AppendEvents(ctx, events, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := fabric.Search(ctx, SearchRequest{Space: "test", Query: "What was the total cost of the turbine repairs?",
		ReferenceTime: time.Now().UTC(), MaxEvidence: 4, IncludeDiagnostics: true})
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, evidence := range result.Evidence {
		joined += "\n" + evidence.Content
	}
	for _, value := range []string{"$120", "$80", "$40"} {
		if !strings.Contains(joined, value) {
			t.Fatalf("aggregate evidence omitted %s: route=%v evidence=%s", value, result.Route, joined)
		}
	}
}

func TestFabricAggregateCountPreservesDistinctNonNumericContexts(t *testing.T) {
	ctx := context.Background()
	fabric := openTestFabric(t, nil, nil, nil)
	started := time.Now().UTC().Add(-time.Hour)
	values := []string{
		"I visited the clinic doctor for an allergy consultation.",
		"I visited the clinic doctor for a sleep consultation.",
		"I visited the clinic doctor for a skin consultation.",
	}
	events := make([]RawEvent, 0, len(values)+4)
	for index, content := range values {
		event := testEvent(fmt.Sprintf("visit-%d", index), content, started.Add(time.Duration(index)*time.Minute))
		event.ContextID, event.SessionID = fmt.Sprintf("visit-context-%d", index), fmt.Sprintf("visit-session-%d", index)
		events = append(events, event)
	}
	for index := 0; index < 4; index++ {
		event := testEvent(fmt.Sprintf("directory-%d", index),
			"I reviewed 10 clinic doctor directory entries before making an appointment.",
			started.Add(time.Duration(index+10)*time.Minute))
		event.ContextID, event.SessionID = fmt.Sprintf("directory-context-%d", index), fmt.Sprintf("directory-session-%d", index)
		events = append(events, event)
	}
	if _, err := fabric.AppendEvents(ctx, events, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := fabric.Search(ctx, SearchRequest{Space: "test", Query: "How many clinic doctors did I visit?",
		ReferenceTime: time.Now().UTC(), MaxEvidence: 3, IncludeDiagnostics: true})
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, evidence := range result.Evidence {
		joined += "\n" + evidence.Content
	}
	for _, value := range []string{"allergy consultation", "sleep consultation", "skin consultation"} {
		if !strings.Contains(joined, value) {
			t.Fatalf("aggregate count evidence omitted %s: route=%v evidence=%s", value, result.Route, joined)
		}
	}
}

func TestFabricAggregateCountDoesNotPreferNumericChatterWithinContext(t *testing.T) {
	ctx := context.Background()
	fabric := openTestFabric(t, nil, nil, nil)
	started := time.Now().UTC().Add(-time.Hour)
	contents := []string{
		"I visited the clinic doctor for an allergy consultation.",
		"I reviewed 10 clinic doctor directory entries before making an appointment.",
		"I saved 20 clinic doctor profiles for later review.",
		"I compared 30 clinic doctor ratings online.",
		"I bookmarked 40 clinic doctor search results.",
	}
	events := make([]RawEvent, 0, len(contents))
	for index, content := range contents {
		events = append(events, testEvent(fmt.Sprintf("clinic-%d", index), content,
			started.Add(time.Duration(index)*time.Minute)))
	}
	if _, err := fabric.AppendEvents(ctx, events, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := fabric.Search(ctx, SearchRequest{Space: "test", Query: "How many different clinic doctors did I visit?",
		ReferenceTime: time.Now().UTC(), MaxEvidence: 1, IncludeDiagnostics: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Evidence) != 1 || !strings.Contains(result.Evidence[0].Content, "allergy consultation") {
		t.Fatalf("count target was displaced by numeric chatter: route=%v evidence=%+v", result.Route, result.Evidence)
	}
}

func TestFabricCurrentQuantityKeepsNumericStateWithinBusyContext(t *testing.T) {
	ctx := context.Background()
	fabric := openTestFabric(t, nil, nil, nil)
	started := time.Now().UTC().Add(-time.Hour)
	contents := []string{
		"I checked the control plane and there are currently 24 active worker processes.",
		"I will ask the active workers which dashboard they prefer.",
		"I plan to post an update for the active worker group.",
		"The active workers are discussing a new meeting format.",
		"I drafted a reminder for all active workers.",
	}
	events := make([]RawEvent, 0, len(contents))
	for index, content := range contents {
		events = append(events, testEvent(fmt.Sprintf("worker-%d", index), content,
			started.Add(time.Duration(index)*time.Minute)))
	}
	if _, err := fabric.AppendEvents(ctx, events, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := fabric.Search(ctx, SearchRequest{Space: "test",
		Query: "How many active worker processes do I currently have?", ReferenceTime: time.Now().UTC(), MaxEvidence: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Evidence) == 0 || !strings.Contains(result.Evidence[0].Content, "24 active worker processes") {
		t.Fatalf("current numeric state was crowded out by later chatter: %+v", result.Evidence)
	}
}

func TestMemorySearchSnippetCentersQueryTerms(t *testing.T) {
	content := "I track purchases in a spreadsheet. I recently chose a premium item for a formal event. " +
		"It was a significant purchase, $800, but it fit the occasion."
	snippet := extractMemorySearchSnippet(content, analyzeMemoryQuery("What was the cost of the premium item?"), 28)
	if !strings.Contains(snippet, "premium item") {
		t.Fatalf("snippet dropped query text: %q", snippet)
	}
}

func TestMemorySearchMultiWindowSnippetPreservesSupportingContextWithinBudget(t *testing.T) {
	content := strings.Repeat("A routine archival note fills this unrelated opening section. ", 5) +
		"The durable record says the cost was 37 credits. " +
		"The premium archive item was later cataloged under the same parcel record. " +
		strings.Repeat("A closing note describes ordinary storage. ", 5)
	analysis := analyzeMemoryQuery("What did the premium archive item cost?")
	snippet := extractMemorySearchMultiWindowSnippet(content, analysis, 64)
	if !strings.Contains(snippet, "37 credits") || !strings.Contains(snippet, "premium archive item") {
		t.Fatalf("multi-window snippet lost distant supporting context: %q", snippet)
	}
	if estimateTokens(snippet) > 64 {
		t.Fatalf("multi-window snippet exceeded its token budget: tokens=%d snippet=%q",
			estimateTokens(snippet), snippet)
	}
}

func TestMemorySearchMultiWindowSnippetIsDeterministicAndKeepsAdjacentContext(t *testing.T) {
	content := strings.Repeat("Background material describes an ordinary record. ", 8) +
		"The requested catalog entry appears in this section. " +
		"The following adjacent note records its final value as 84 credits. " +
		strings.Repeat("Closing material discusses routine storage. ", 8)
	analysis := analyzeMemoryQuery("What was the value of the requested catalog entry?")
	first := extractMemorySearchMultiWindowSnippet(content, analysis, 64)
	second := extractMemorySearchMultiWindowSnippet(content, analysis, 64)
	if first != second {
		t.Fatalf("multi-window snippet was not deterministic:\nfirst=%q\nsecond=%q", first, second)
	}
	if !strings.Contains(first, "requested catalog entry") || !strings.Contains(first, "84 credits") {
		t.Fatalf("multi-window snippet lost adjacent supporting context: %q", first)
	}
	if estimateTokens(first) > 64 {
		t.Fatalf("multi-window snippet exceeded its token budget: tokens=%d snippet=%q",
			estimateTokens(first), first)
	}
}

func TestMemorySearchMultiWindowSnippetPreservesCompleteAtomicSentence(t *testing.T) {
	content := "I am reviewing recent expenses and trying to build a practical budget. " +
		"I splurged on a luxury handbag from a designer for $1,200, but balanced it with ordinary purchases. " +
		"I would like a reusable spreadsheet template."
	analysis := analyzeMemoryQuery("What did I spend on the luxury handbag?")
	snippet := extractMemorySearchMultiWindowSnippet(content, analysis, 64)
	if !strings.Contains(snippet, "luxury handbag from a designer for $1,200") {
		t.Fatalf("sentence-aware snippet split an atomic fact: %q", snippet)
	}
	if estimateTokens(snippet) > 64 {
		t.Fatalf("sentence-aware snippet exceeded its token budget: tokens=%d snippet=%q",
			estimateTokens(snippet), snippet)
	}
}

func TestMemorySearchMultiWindowSnippetPreservesExplicitNegation(t *testing.T) {
	content := "We took a seven-day family road trip through several parks. " +
		"We did a lot of driving and hiking, but did not camp on that trip. " +
		"Later notes discuss possible destinations."
	analysis := analyzeMemoryQuery("How many days did I spend camping?")
	snippet := extractMemorySearchMultiWindowSnippet(content, analysis, 48)
	if !strings.Contains(snippet, "did not camp on that trip") {
		t.Fatalf("sentence-aware snippet dropped the event boundary: %q", snippet)
	}
	if estimateTokens(snippet) > 48 {
		t.Fatalf("sentence-aware snippet exceeded its token budget: tokens=%d snippet=%q",
			estimateTokens(snippet), snippet)
	}
}

func TestMemorySearchMultiWindowSnippetPreservesCompletedFactWithinDynamicBudget(t *testing.T) {
	content := "I am planning a trip and would like recommendations for trails and camping spots. " +
		"I just returned from a five-day camping trip in a national park. " +
		"The scenery was memorable."
	analysis := analyzeMemoryQuery("How many days did I spend camping?")
	snippet := extractMemorySearchMultiWindowSnippet(content, analysis, 72)
	if !strings.Contains(snippet, "five-day camping trip") {
		t.Fatalf("sentence-aware snippet dropped the completed fact: %q", snippet)
	}
}

func TestMemorySearchMultiWindowSnippetKeepsAdjacentReferentWithinDynamicBudget(t *testing.T) {
	content := "I need to document a vintage pendant that belonged to my grandmother. " +
		"I inherited it recently together with an old music box and a set of historic glassware. " +
		"Can you recommend storage for these old items?"
	analysis := analyzeMemoryQuery("How many antique items did I inherit?")
	snippet := extractMemorySearchMultiWindowSnippet(content, analysis, 56)
	if !strings.Contains(snippet, "vintage pendant") || !strings.Contains(snippet, "inherited it recently") {
		t.Fatalf("sentence-aware snippet separated a referent from its fact: %q", snippet)
	}
	if strings.Contains(snippet, "recommend storage") {
		t.Fatalf("sentence-aware snippet let a request displace factual context: %q", snippet)
	}
	if estimateTokens(snippet) > 56 {
		t.Fatalf("sentence-aware snippet exceeded its token budget: tokens=%d snippet=%q",
			estimateTokens(snippet), snippet)
	}
}

func TestQueryTokensAddShapeBasedInflectionVariants(t *testing.T) {
	tokens := " " + strings.Join(queryTokens("re-watched running inviting studied", 32), " ") + " "
	for _, wanted := range []string{" watch ", " run ", " invite ", " study "} {
		if !strings.Contains(tokens, wanted) {
			t.Fatalf("query tokens missing inflection variant %q: %s", wanted, tokens)
		}
	}
}

func TestContextExpansionUsesAffinityBeforeSeedRank(t *testing.T) {
	ctx := context.Background()
	fabric := openTestFabric(t, nil, nil, nil)
	started := time.Now().UTC().Add(-time.Hour)
	events := []RawEvent{
		testEvent("seed-1", "I browsed a cinema catalog.", started),
		testEvent("seed-2", "I organized a cinema shelf.", started.Add(time.Minute)),
		testEvent("seed-3", "I read a cinema newsletter.", started.Add(2*time.Minute)),
		testEvent("seed-4", "I updated a cinema watchlist.", started.Add(3*time.Minute)),
		testEvent("relevant", "I re-watched a cinema title last night.", started.Add(4*time.Minute)),
	}
	if _, err := fabric.AppendEvents(ctx, events, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	seedRanks := map[string]int{"seed-1": 1, "seed-2": 2, "seed-3": 3, "seed-4": 4}
	expansion, ok := fabric.buildContextExpansion(ctx, "test", "ctx",
		analyzeMemoryQuery("Which cinema title did I re-watch?"), time.Now().UTC(), seedRanks)
	if !ok || !strings.Contains(expansion.document.Content, "re-watched a cinema title") {
		t.Fatalf("query-affine event was displaced by seed rank: %+v", expansion)
	}
}

func TestRawEvidenceDoesNotRequireAComputationMode(t *testing.T) {
	result := SearchResult{Evidence: []Evidence{{Content: "My commute takes 45 minutes each way."}}}
	if memorySearchInsufficient(queryAnalysis{}, result) {
		t.Fatal("grounded raw evidence was marked insufficient")
	}
}

func TestFabricRelocatesVectorVirtualTableWithStore(t *testing.T) {
	ctx := context.Background()
	original := filepath.Join(t.TempDir(), "preparing")
	options := DefaultFabricOptions(original)
	options.StartWorkers = false
	options.Vectorizer = tinyVectorizer{}
	fabric, err := OpenFabric(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	event := testEvent("relocated-vector", "Atlas uses the relocated vector index.", time.Now().UTC())
	if _, err := fabric.AppendEvents(ctx, []RawEvent{event}, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.SealContext(ctx, ContextRef{ID: event.ContextID, Space: event.Space}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Close(); err != nil {
		t.Fatal(err)
	}

	relocated := filepath.Join(filepath.Dir(original), "ready")
	if err := os.Rename(original, relocated); err != nil {
		t.Fatal(err)
	}
	options = DefaultFabricOptions(relocated)
	options.StartWorkers = false
	options.Vectorizer = tinyVectorizer{}
	fabric, err = OpenFabric(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer fabric.Close()
	var statement string
	if err := fabric.index.QueryRow(`SELECT sql FROM sqlite_master WHERE name='memory_vectors'`).Scan(&statement); err != nil {
		t.Fatal(err)
	}
	if !vectorVirtualTableUsesPath(statement, filepath.Join(relocated, "index.sqlite")) {
		t.Fatalf("vector virtual table still uses the old store path: %s", statement)
	}
	documents, err := fabric.searchVector(ctx, "test", "relocated Atlas",
		analyzeMemoryQuery("relocated Atlas"), 16)
	if err != nil || len(documents) == 0 {
		t.Fatalf("relocated vector search failed: documents=%d err=%v", len(documents), err)
	}
}

func TestFabricForgetTombstoneAndPurge(t *testing.T) {
	fabric := openTestFabric(t, nil, nil, nil)
	first := testEvent("forget-one", "Atlas secret preference.", time.Now().UTC())
	second := testEvent("forget-two", "Quartz public preference.", time.Now().UTC())
	if _, err := fabric.AppendEvents(context.Background(), []RawEvent{first, second}, IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Forget(context.Background(), Selector{Space: "test", EventIDs: []string{first.ID}}, ForgetTombstone); err != nil {
		t.Fatal(err)
	}
	search, err := fabric.Search(context.Background(), SearchRequest{Space: "test", Query: "Atlas secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Evidence) != 0 {
		t.Fatalf("tombstoned evidence remained searchable: %+v", search.Evidence)
	}
	if err := fabric.Forget(context.Background(), Selector{Space: "test", EventIDs: []string{second.ID}}, ForgetPurge); err != nil {
		t.Fatal(err)
	}
	var count int
	_ = fabric.ledger.QueryRow(`SELECT COUNT(*) FROM events WHERE event_id=?`, second.ID).Scan(&count)
	if count != 0 {
		t.Fatalf("purged event remains in ledger")
	}
}
