package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"LuminaCode/agent"
	"LuminaCode/backend"
	luminacli "LuminaCode/cli"
	"LuminaCode/config"
	"LuminaCode/longmemory"
	"LuminaCode/maintenance"
	"LuminaCode/session"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"
	luminaui "LuminaCode/ui"

	"github.com/google/uuid"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func runMemoryCLI(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lumina-backend memory <list|search|get|delete|archive|approve|restore|prioritize|deprioritize|supersede|export|import|used|doctor>")
	}
	cfg := config.NewConfig()
	if !cfg.LongTermMemoryEnabled {
		return fmt.Errorf("long-term memory is disabled")
	}
	ctx := context.Background()
	store, err := longmemory.Open(ctx, cfg.LongTermMemoryStore)
	if err != nil {
		return err
	}
	defer store.Close()
	switch args[0] {
	case "list":
		flags := flag.NewFlagSet("memory list", flag.ContinueOnError)
		includeInactive := flags.Bool("all", false, "include inactive memories")
		includeExpired := flags.Bool("expired", false, "include expired memories")
		limit := flags.Int("limit", 50, "max items")
		scopeType := flags.String("scope-type", "", "filter by scope type")
		scopeKey := flags.String("scope-key", "", "filter by scope key")
		memoryType := flags.String("type", "", "filter by memory type")
		tag := flags.String("tag", "", "filter by tag")
		createdAfter := flags.String("created-after", "", "filter by created_at lower bound, RFC3339 or YYYY-MM-DD")
		createdBefore := flags.String("created-before", "", "filter by created_at upper bound, RFC3339 or YYYY-MM-DD")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		opts := memoryCLIOptions(*limit, *includeInactive, *includeExpired, *scopeType, *scopeKey, *memoryType, *tag, *createdAfter, *createdBefore)
		items, err := store.List(ctx, opts)
		if err != nil {
			return err
		}
		return writeJSON(os.Stdout, items)
	case "search":
		flags := flag.NewFlagSet("memory search", flag.ContinueOnError)
		limit := flags.Int("limit", cfg.MemoryAtomMaxSelected, "max items")
		includeInactive := flags.Bool("all", false, "include inactive memories")
		includeExpired := flags.Bool("expired", false, "include expired memories")
		scopeType := flags.String("scope-type", "", "filter by scope type")
		scopeKey := flags.String("scope-key", "", "filter by scope key")
		memoryType := flags.String("type", "", "filter by memory type")
		tag := flags.String("tag", "", "filter by tag")
		createdAfter := flags.String("created-after", "", "filter by created_at lower bound, RFC3339 or YYYY-MM-DD")
		createdBefore := flags.String("created-before", "", "filter by created_at upper bound, RFC3339 or YYYY-MM-DD")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		query := strings.TrimSpace(strings.Join(flags.Args(), " "))
		opts := memoryCLIOptions(*limit, *includeInactive, *includeExpired, *scopeType, *scopeKey, *memoryType, *tag, *createdAfter, *createdBefore)
		opts.Query = query
		if len(opts.Scopes) == 0 {
			opts.Scopes = longmemory.RuntimeScopes(cfg.CWD, "main", "", "")
		}
		opts.MaxCandidates = cfg.MemoryFTSCandidates
		opts.ContextMaxRunes = cfg.MemoryContextMaxTokens * 4
		items, err := store.Search(ctx, opts)
		if err != nil {
			return err
		}
		return writeJSON(os.Stdout, items)
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("memory get requires memory_id")
		}
		entry, err := store.Get(ctx, args[1])
		if err != nil {
			return err
		}
		return writeJSON(os.Stdout, entry)
	case "delete":
		flags := flag.NewFlagSet("memory delete", flag.ContinueOnError)
		hard := flags.Bool("hard", false, "physically delete the memory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() == 0 {
			return fmt.Errorf("memory delete requires memory_id")
		}
		return store.Delete(ctx, flags.Arg(0), *hard)
	case "archive":
		if len(args) < 2 {
			return fmt.Errorf("memory archive requires memory_id")
		}
		return store.Archive(ctx, args[1], "manual_archive")
	case "approve":
		if len(args) < 2 {
			return fmt.Errorf("memory approve requires memory_id")
		}
		return store.Approve(ctx, args[1])
	case "restore":
		if len(args) < 2 {
			return fmt.Errorf("memory restore requires memory_id")
		}
		return store.Restore(ctx, args[1])
	case "prioritize":
		flags := flag.NewFlagSet("memory prioritize", flag.ContinueOnError)
		importance := flags.Float64("importance", 1, "importance 0..1")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() == 0 {
			return fmt.Errorf("memory prioritize requires memory_id")
		}
		return store.UpdateImportance(ctx, flags.Arg(0), *importance)
	case "deprioritize":
		if len(args) < 2 {
			return fmt.Errorf("memory deprioritize requires memory_id")
		}
		return store.Deprioritize(ctx, args[1])
	case "supersede":
		flags := flag.NewFlagSet("memory supersede", flag.ContinueOnError)
		newID := flags.String("new", "", "existing replacement memory_id")
		candidatePath := flags.String("candidate", "", "JSON/JSONL/Markdown candidate file containing replacement memory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() == 0 {
			return fmt.Errorf("memory supersede requires old memory_id")
		}
		oldID := flags.Arg(0)
		if *candidatePath != "" {
			before, err := store.List(ctx, longmemory.SearchOptions{Limit: 100000, IncludeInactive: true, IncludeExpired: true})
			if err != nil {
				return err
			}
			seen := map[string]struct{}{}
			for _, entry := range before {
				seen[entry.MemoryID] = struct{}{}
			}
			if _, err := backend.ImportMemoryCandidates(ctx, store, *candidatePath, nil); err != nil {
				return err
			}
			after, err := store.List(ctx, longmemory.SearchOptions{Limit: 100000, IncludeInactive: true, IncludeExpired: true})
			if err != nil {
				return err
			}
			var replacement string
			for _, entry := range after {
				if _, ok := seen[entry.MemoryID]; !ok {
					replacement = entry.MemoryID
					break
				}
			}
			if replacement == "" {
				return fmt.Errorf("candidate import did not create a replacement memory")
			}
			return store.Supersede(ctx, oldID, replacement)
		}
		if *newID == "" {
			return fmt.Errorf("memory supersede requires --new or --candidate")
		}
		return store.Supersede(ctx, oldID, *newID)
	case "export":
		flags := flag.NewFlagSet("memory export", flag.ContinueOnError)
		format := flags.String("format", "markdown", "export format")
		out := flags.String("out", "", "output directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if strings.ToLower(*format) != "markdown" {
			return fmt.Errorf("unsupported memory export format: %s", *format)
		}
		dir, err := longmemory.ExportMarkdown(ctx, store, *out)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, dir)
		return nil
	case "import":
		if len(args) < 2 {
			return fmt.Errorf("memory import requires an explicit JSON/JSONL/Markdown file or directory path")
		}
		n, err := backend.ImportMemoryCandidates(ctx, store, args[1], nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "imported %d memories\n", n)
		return nil
	case "used":
		flags := flag.NewFlagSet("memory used", flag.ContinueOnError)
		limit := flags.Int("limit", 100, "max records")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		records, err := store.ListUsed(ctx, *limit)
		if err != nil {
			return err
		}
		return writeJSON(os.Stdout, records)
	case "doctor":
		if len(cfg.MemoryConfigErrors) > 0 {
			return fmt.Errorf("invalid memory configuration: %s", strings.Join(cfg.MemoryConfigErrors, "; "))
		}
		if !cfg.MemoryEmbeddingEnabled {
			return fmt.Errorf("memory embedding is disabled")
		}
		embedder, err := longmemory.SharedLocalEmbedder(cfg.MemoryEmbeddingModel, cfg.MemoryEmbeddingModelDir)
		if err != nil {
			return err
		}
		vectors, err := embedder.Embed(ctx, []string{"LuminaCode memory embedding self check"}, longmemory.EmbeddingQuery)
		if err != nil {
			return err
		}
		dimensions := 0
		if len(vectors) > 0 {
			dimensions = len(vectors[0])
		}
		if len(vectors) != 1 || dimensions != embedder.Dimensions() {
			return fmt.Errorf("memory embedding self check returned shape %d x %d", len(vectors), dimensions)
		}
		catalog, catalogErr := store.InspectCatalog(ctx, nil)
		if catalogErr != nil {
			return catalogErr
		}
		return writeJSON(os.Stdout, map[string]any{"status": "ready", "model": embedder.Model(),
			"dimensions": embedder.Dimensions(), "model_dir": cfg.MemoryEmbeddingModelDir,
			"indexed_memories": catalog.TotalMemories, "indexed_sessions": catalog.TotalSessions,
			"indexed_chunks": catalog.TotalChunks, "indexed_atoms": catalog.TotalAtoms,
			"embedding_batch_size":                cfg.MemoryEmbeddingBatchSize,
			"embedding_batch_wait_ms":             cfg.MemoryEmbeddingBatchWaitMS,
			"embedding_execution_timeout_seconds": cfg.MemoryEmbeddingExecutionTimeout,
			"coverage_max_facets":                 cfg.MemoryCoverageMaxFacets,
			"coverage_completion_rounds":          cfg.MemoryCoverageCompletionRounds,
			"coverage_support_target":             cfg.MemoryCoverageSupportTarget,
			"coverage_residual_trigger":           cfg.MemoryCoverageResidualTrigger,
			"coverage_min_marginal_gain":          cfg.MemoryCoverageMinMarginalGain,
			"atom_structural_context_enabled":     cfg.MemoryAtomStructuralContextEnabled,
			"atom_structural_context_max_tokens":  cfg.MemoryAtomStructuralContextTokens,
			"query_expansion_additional_wait_ms":  cfg.MemoryQueryExpansionAdditionalWait,
			"lifecycle_enabled":                   cfg.MemoryLifecycleEnabled,
			"maintenance_interval_seconds":        cfg.MemoryMaintenanceIntervalSeconds,
			"archive_value_threshold":             cfg.MemoryArchiveValueThreshold, "auto_hard_delete": false})
	default:
		return fmt.Errorf("unknown memory command: %s", args[0])
	}
}

func memoryCLIOptions(limit int, includeInactive, includeExpired bool, scopeType, scopeKey, memoryType, tag, createdAfter, createdBefore string) longmemory.SearchOptions {
	opts := longmemory.SearchOptions{Limit: limit, IncludeInactive: includeInactive, IncludeExpired: includeExpired}
	if strings.TrimSpace(scopeType) != "" && strings.TrimSpace(scopeKey) != "" {
		opts.Scopes = []longmemory.Scope{{Type: longmemory.ScopeType(scopeType), Key: scopeKey}}
	}
	if strings.TrimSpace(memoryType) != "" {
		opts.Types = []longmemory.MemoryType{longmemory.MemoryType(memoryType)}
	}
	if strings.TrimSpace(tag) != "" {
		opts.Tags = []string{tag}
	}
	opts.CreatedAfter = parseMemoryCLITime(createdAfter)
	opts.CreatedBefore = parseMemoryCLITime(createdBefore)
	return opts
}

func parseMemoryCLITime(text string) time.Time {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "daemon" {
		return backend.RunDaemonCLI(args[1:])
	}
	if len(args) > 0 && args[0] == "shutdown" {
		return backend.RunShutdownCLI(args[1:])
	}
	if len(args) > 0 && args[0] == "memory" {
		return runMemoryCLI(args[1:])
	}
	flags := flag.NewFlagSet("lumina", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)

	prompt := flags.String("prompt", "", "Single-shot mode: execute one prompt and exit.")
	promptShort := flags.String("p", "", "Single-shot mode: execute one prompt and exit.")
	model := flags.String("model", "", "Model to use.")
	apiType := flags.String("api-type", "", "API request format: openai_compatible, anthropic, or auto.")
	apiKey := flags.String("api-key", "", "API key.")
	baseURL := flags.String("base-url", "", "API base URL.")
	maxTokens := flags.Int("max-tokens", 0, "Context window tokens used for local compression.")
	yolo := flags.Bool("yolo", false, "Skip permission prompts and OS sandbox isolation.")
	cwd := flags.String("cwd", "", "Working directory.")
	verbose := flags.Bool("verbose", false, "Enable debug output.")
	verboseShort := flags.Bool("v", false, "Enable debug output.")
	bare := flags.Bool("bare", false, "Disable long-term memory and other persistent features.")
	harnessMode := flags.String("harness-mode", "", "Benchmark harness mode. Supported: terminal-bench.")
	listFlag := flags.Bool("list", false, "List saved session files and exit.")
	storageFlag := flags.Bool("storage", false, "Show session storage usage and exit.")
	cleanupFlag := flags.Bool("cleanup", false, "Dry-run session storage cleanup and exit.")
	enforceCleanup := flags.Bool("enforce", false, "Actually apply --cleanup actions.")
	resume := flags.String("resume", "", "Resume a previous session by ID.")

	if err := flags.Parse(args); err != nil {
		return err
	}
	if *prompt == "" && *promptShort != "" {
		*prompt = *promptShort
	}
	verboseEnabled := *verbose || *verboseShort

	cfg := config.NewConfig()
	if *model != "" {
		cfg.APIModel = *model
		config.PinFields(&cfg, "api_model")
	}
	if *apiType != "" {
		cfg.APIType = *apiType
		config.PinFields(&cfg, "api_type")
	}
	if *apiKey != "" {
		cfg.APIKey = *apiKey
		config.PinFields(&cfg, "api_key")
	}
	if *baseURL != "" {
		cfg.APIBaseURL = *baseURL
		config.PinFields(&cfg, "api_base_url")
	}
	if *maxTokens > 0 {
		cfg.APIMaxTokens = *maxTokens
		config.PinFields(&cfg, "api_max_tokens")
	}
	if *yolo {
		cfg.Yolo = true
	}
	if *cwd != "" {
		abs, err := filepath.Abs(*cwd)
		if err != nil {
			return err
		}
		cfg.CWD = abs
	}
	if *bare {
		cfg.LongTermMemoryEnabled = false
	}
	if *harnessMode != "" {
		cfg.HarnessMode = strings.TrimSpace(*harnessMode)
		config.PinFields(&cfg, "harness_mode")
	}
	config.ApplyHarnessDefaults(&cfg)
	if cfg.LongTermMemoryEnabled {
		if err := cfg.ValidateMemoryConfig(); err != nil {
			return err
		}
		store, err := longmemory.Open(context.Background(), cfg.LongTermMemoryStore)
		if err != nil {
			return fmt.Errorf("open long-term memory store: %w", err)
		}
		_ = store.Close()
	}

	store := session.NewStore(cfg.SessionDir)
	if *listFlag {
		printSessions(store)
		return nil
	}
	if *storageFlag || *cleanupFlag {
		report, err := maintenance.Cleanup(cfg, maintenance.Options{Enforce: *cleanupFlag && *enforceCleanup})
		if err != nil {
			return err
		}
		printStorageReport(os.Stdout, report)
		return nil
	}
	if *prompt == "" {
		return fmt.Errorf("interactive Go TUI has been removed. Use the TypeScript frontend command 'lumina', or run 'lumina-backend -p <prompt>' for headless mode")
	}

	if verboseEnabled && cfg.LongTermMemoryEnabled {
		fmt.Printf("[debug] Long-term memory:  %s\n", cfg.LongTermMemoryStore)
	}

	if cfg.APIKey == "" {
		return fmt.Errorf("no API key configured. Set LUMINA_API_KEY or pass --api-key")
	}
	if cfg.APIBaseURL == "" {
		return fmt.Errorf("no API base URL configured. Set LUMINA_API_BASE_URL or pass --base-url")
	}
	if verboseEnabled {
		fmt.Printf("[debug] API URL: %s\n", cfg.APIBaseURL)
		fmt.Printf("[debug] Model:   %s\n", cfg.APIModel)
		fmt.Printf("[debug] APIType: %s\n", cfg.APIType)
		fmt.Printf("[debug] CWD:     %s\n", cfg.CWD)
	}

	engine := agent.NewQueryEngine(&cfg)
	sessionID := ""
	var state *agent.AgentState
	if *resume != "" {
		if resumed := store.LoadState(*resume); resumed != nil {
			state = resumed
			sessionID = *resume
			if recovery := store.LoadSkillRecovery(*resume); recovery != nil {
				engine.CoreEngine.ImportSkillRecoverySnapshot(recovery)
			}
			if tasks := store.LoadTaskRuntimeSnapshot(*resume); tasks != nil {
				engine.CoreEngine.TaskRuntime.ImportSnapshot(tasks)
			}
			fmt.Printf("Resumed session %s - %d msgs, %d turns, YOLO=%t\n", *resume, len(state.Messages), state.TurnCount, state.PermissionState != nil && state.PermissionState.YoloMode)
		} else if messages := store.Load(*resume); len(messages) > 0 {
			legacyState := agent.NewAgentState()
			legacyState.Messages = messages
			state = &legacyState
			sessionID = *resume
			fmt.Printf("Resumed session %s (%d messages) - legacy format, permissions reset.\n", *resume, len(messages))
		} else {
			fmt.Printf("Session %s not found. Starting fresh.\n", *resume)
		}
	}
	if *prompt != "" {
		return runPrompt(context.Background(), engine, *prompt, state)
	}
	return runREPL(context.Background(), engine, state, store, sessionID)
}

func runPrompt(ctx context.Context, engine *agent.QueryEngine, prompt string, state *agent.AgentState) error {
	defer engine.Shutdown()
	if state == nil {
		s := agent.NewAgentState()
		state = &s
	}
	if engine.Config.Yolo && state.PermissionState != nil {
		state.PermissionState.YoloMode = true
	}
	reader := bufio.NewReader(os.Stdin)
	var firstErr error
	for event := range engine.SubmitMessage(ctx, prompt, state, uuid.NewString()[:12]) {
		switch event.Type {
		case "text":
			fmt.Fprint(os.Stdout, event.Content)
		case "error":
			if event.Content != "" {
				fmt.Fprintln(os.Stderr, event.Content)
				if firstErr == nil {
					firstErr = fmt.Errorf("%s", event.Content)
				}
			}
		case "permission_needed":
			resolveHeadlessPermission(engine, event, reader, os.Stderr)
		}
	}
	return firstErr
}

func resolveHeadlessPermission(engine *agent.QueryEngine, event agent.StreamEvent, reader *bufio.Reader, out io.Writer) {
	granted := engine.Config.Yolo
	decision := agent.PermissionOnce
	if !granted {
		if reason, ok := event.Metadata["reason"].(string); ok && strings.TrimSpace(reason) != "" {
			fmt.Fprintln(out, reason)
		}
		fmt.Fprintf(out, "Permission needed for %s. Allow once? [y/N] ", headlessPermissionName(event))
		answer, _ := reader.ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		switch answer {
		case "y", "yes", "once":
			granted = true
			decision = agent.PermissionOnce
		case "a", "always":
			granted = true
			decision = agent.PermissionAlways
		default:
			granted = false
			decision = agent.PermissionDeny
		}
	}
	if _, ok := event.Metadata["mcp_trust_request"]; ok {
		engine.ResolveMCPTrust(granted)
		return
	}
	if _, ok := event.Metadata["skill_shell_request"]; ok {
		engine.ResolveSkillPermission(granted)
		return
	}
	if !granted {
		decision = agent.PermissionDeny
	}
	engine.ResolvePermission(decision, event.Content)
}

func headlessPermissionName(event agent.StreamEvent) string {
	if _, ok := event.Metadata["mcp_trust_request"]; ok {
		return "mcp-project-trust"
	}
	if _, ok := event.Metadata["skill_shell_request"]; ok {
		return "skill-shell"
	}
	if event.Content != "" {
		return event.Content
	}
	return "tool"
}

func runREPL(ctx context.Context, engine *agent.QueryEngine, state *agent.AgentState, store *session.Store, sessionID string) error {
	if sessionID == "" {
		sessionID = uuid.NewString()[:12]
	}
	backend := luminaui.NewRendererBackend(engine.Config.UIBackend, os.Stdin, os.Stdout, os.Stderr)
	configureBackendForEngine(backend, engine)
	uiRuntime := luminaui.NewUiRuntime(engine, backend)
	defer engine.Shutdown()
	defer func() {
		if uiRuntime != nil {
			uiRuntime.Shutdown()
		}
	}()
	luminaui.RenderBackendWelcome(backend, sessionID, engine.SkillRegistry())
	if state != nil && len(state.Messages) > 0 {
		uiRuntime.MountStateSnapshot(state)
	}
	for {
		line, ok := luminaui.ReadBackendInput(backend, state)
		if !ok {
			return nil
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			resolvedLine, ok := resolveSkillPickerSlash(line, engine, backend)
			if !ok {
				continue
			}
			line = resolvedLine
			registry := engine.SkillRegistry()
			dispatch := luminacli.ClassifyREPLSlashCommand(line, registry, engine.Config.CWD)
			if dispatch.Kind == luminacli.SlashDispatchExit {
				fmt.Fprintln(luminaui.BackendOutputWriter(backend), "Goodbye.")
				return nil
			}
			handled, err := handleREPLSlashCommand(ctx, line, engine, &state, store, &sessionID, backend)
			if err != nil {
				return err
			}
			if handled {
				cmd := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "/")))
				cmdName, _, _ := strings.Cut(cmd, " ")
				if cmd == "clear" || cmdName == "resume" {
					uiRuntime.Shutdown()
					luminaui.ResetBackendForNewSession(backend)
					configureBackendForEngine(backend, engine)
					uiRuntime = luminaui.NewUiRuntime(engine, backend)
					if cmdName == "resume" && state != nil && len(state.Messages) > 0 {
						uiRuntime.MountStateSnapshot(state)
					}
				}
				continue
			}
		}
		uiRuntime.RunSubmitMessage(ctx, line, state, sessionID)
		state = engine.CoreEngine.LastState
		if store != nil && sessionID != "" {
			_ = store.SaveSnapshotWithRecovery(
				sessionID,
				state,
				engine.CoreEngine.ExportSkillRecoverySnapshot(),
				engine.CoreEngine.TaskRuntime.ExportSnapshot(),
			)
		}
	}
}

func resolveSkillPickerSlash(line string, engine *agent.QueryEngine, backend luminaui.RendererBackend) (string, bool) {
	cmd := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "/")))
	if cmd != "skill" {
		return line, true
	}
	out := luminaui.BackendOutputWriter(backend)
	if engine == nil {
		fmt.Fprintln(out, "Skills are disabled.")
		return "", false
	}
	registry := engine.SkillRegistry()
	if registry == nil {
		fmt.Fprintln(out, "Skills are disabled.")
		return "", false
	}
	visible := registry.ListUserInvocable(engine.Config.CWD)
	if len(visible) == 0 {
		fmt.Fprintln(out, "No visible skills for current directory.")
		return "", false
	}
	values := make([][2]string, 0, len(visible))
	for _, skill := range visible {
		values = append(values, [2]string{skill.CanonicalName, skillPickLabel(skill)})
	}
	picked := backend.PickFromList("Select Skill", values)
	if picked == nil || *picked == "" {
		return "", false
	}
	luminaui.SetBackendInputDraft(backend, "/"+*picked+" ")
	return "", false
}

func skillPickLabel(skill skills.SkillSpec) string {
	description := skill.Frontmatter.Description
	if skill.Frontmatter.ArgumentHint != nil && *skill.Frontmatter.ArgumentHint != "" {
		description = *skill.Frontmatter.ArgumentHint
	}
	return fmt.Sprintf("/%-24s %s", skill.CanonicalName, description)
}

func configureBackendForEngine(backend luminaui.RendererBackend, engine *agent.QueryEngine) {
	if engine == nil || engine.CoreEngine == nil {
		return
	}
	luminaui.ConfigureRendererBackend(backend, engine.CoreEngine.Registry, coretools.ExecutionContext{
		"cwd":    engine.Config.CWD,
		"config": engine.Config,
	})
}

func handleREPLSlashCommand(ctx context.Context, line string, engine *agent.QueryEngine, stateRef **agent.AgentState, store *session.Store, sessionID *string, backend luminaui.RendererBackend) (bool, error) {
	out := luminaui.BackendOutputWriter(backend)
	cmd := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "/")))
	cmdName, _, _ := strings.Cut(cmd, " ")
	var state *agent.AgentState
	if stateRef != nil {
		state = *stateRef
	}
	var registry *skills.SkillRegistry
	cwd := ""
	if engine != nil {
		registry = engine.SkillRegistry()
		cwd = engine.Config.CWD
	}
	dispatch := luminacli.ClassifyREPLSlashCommand(line, registry, cwd)
	if dispatch.Kind == luminacli.SlashDispatchExit {
		return true, nil
	}
	if dispatch.Kind == luminacli.SlashDispatchSkill {
		return false, nil
	}
	if dispatch.Kind == luminacli.SlashDispatchUnknown {
		fmt.Fprintf(out, "Unknown command: %s (try /help)\n", line)
		return true, nil
	}
	switch {
	case cmd == "help":
		printCommandHelp(out, engine.SkillRegistry(), engine.Config.CWD)
		return true, nil
	case cmd == "clear":
		if stateRef != nil {
			*stateRef = nil
		}
		if engine != nil {
			engine.Reset()
			engine.ClearMCP()
		}
		if sessionID != nil {
			*sessionID = uuid.NewString()[:12]
			fmt.Fprintf(out, "Session cleared. New ID: %s\n", *sessionID)
		} else {
			fmt.Fprintln(out, "Session cleared.")
		}
		return true, nil
	case cmd == "save" || cmd == "s":
		if state == nil || store == nil || sessionID == nil || *sessionID == "" {
			fmt.Fprintln(out, "No active session.")
			return true, nil
		}
		if err := store.SaveSnapshotWithRecovery(*sessionID, state, engine.CoreEngine.ExportSkillRecoverySnapshot(), engine.CoreEngine.TaskRuntime.ExportSnapshot()); err != nil {
			return true, err
		}
		fmt.Fprintf(out, "Saved %s (%d msgs, %d turns)\n", *sessionID, len(state.Messages), state.TurnCount)
		return true, nil
	case cmd == "tokens":
		printTokens(out, state)
		return true, nil
	case cmdName == "yolo":
		if state == nil {
			fmt.Fprintln(out, "No active session - will apply to next prompt.")
			return true, nil
		}
		if state.PermissionState == nil {
			state.PermissionState = agent.BuildSubagentState(nil, "inherit").PermissionState
		}
		state.PermissionState.YoloMode = !state.PermissionState.YoloMode
		status := "OFF"
		if state.PermissionState.YoloMode {
			status = "ON"
		}
		fmt.Fprintf(out, "YOLO mode: %s\n", status)
		return true, nil
	case cmd == "storage":
		report, err := maintenance.Status(engine.Config, maintenance.Options{CurrentSessions: currentSessionSet(sessionID)})
		if err != nil {
			return true, err
		}
		printStorageReport(out, report)
		return true, nil
	case cmdName == "cleanup":
		enforce := hasCommandFlag(cmd, "--enforce")
		report, err := maintenance.Cleanup(engine.Config, maintenance.Options{Enforce: enforce, CurrentSessions: currentSessionSet(sessionID)})
		if err != nil {
			return true, err
		}
		printStorageReport(out, report)
		return true, nil
	case cmd == "pin" || cmd == "unpin":
		if store == nil || sessionID == nil || *sessionID == "" {
			fmt.Fprintln(out, "No active session.")
			return true, nil
		}
		pinned := cmd == "pin"
		meta, err := store.Pin(*sessionID, pinned)
		if err != nil {
			return true, err
		}
		status := "unpinned"
		if meta.Pinned {
			status = "pinned"
		}
		fmt.Fprintf(out, "Session %s is %s.\n", *sessionID, status)
		return true, nil
	case cmdName == "resume":
		if store == nil {
			return true, nil
		}
		parts := strings.Fields(cmd)
		if len(parts) < 2 {
			sessions := store.ListSessions()
			if len(sessions) == 0 {
				fmt.Fprintln(out, "No saved sessions.")
				return true, nil
			}
			values := make([][2]string, 0, len(sessions))
			for idx, meta := range sessions {
				if idx >= 20 {
					break
				}
				values = append(values, [2]string{
					meta.SessionID,
					fmt.Sprintf("%s  (%d msgs, %d turns)", meta.SessionID, meta.MessageCount, meta.TurnCount),
				})
			}
			picked := backend.PickFromList("Resume Session", values)
			if picked == nil || *picked == "" {
				return true, nil
			}
			parts = []string{"resume", *picked}
		}
		targetID := parts[1]
		if resumed := store.LoadState(targetID); resumed != nil {
			if stateRef != nil {
				*stateRef = resumed
			}
			if recovery := store.LoadSkillRecovery(targetID); recovery != nil {
				engine.CoreEngine.ImportSkillRecoverySnapshot(recovery)
			}
			if tasks := store.LoadTaskRuntimeSnapshot(targetID); tasks != nil {
				engine.CoreEngine.TaskRuntime.ImportSnapshot(tasks)
			}
			if sessionID != nil {
				*sessionID = targetID
			}
			fmt.Fprintf(out, "Resumed session %s (%d messages, %d turns)\n", targetID, len(resumed.Messages), resumed.TurnCount)
			return true, nil
		}
		if messages := store.Load(targetID); len(messages) > 0 {
			legacyState := agent.NewAgentState()
			legacyState.Messages = messages
			if stateRef != nil {
				*stateRef = &legacyState
			}
			if sessionID != nil {
				*sessionID = targetID
			}
			fmt.Fprintf(out, "Resumed session %s (%d messages) - legacy format, permissions reset.\n", targetID, len(messages))
			return true, nil
		}
		fmt.Fprintf(out, "Session %s not found.\n", targetID)
		return true, nil
	case cmd == "compact" || cmd == "compress":
		if state == nil {
			fmt.Fprintln(out, "No active session.")
			return true, nil
		}
		compacted, stats := engine.Compact(state)
		*state = compacted
		if stats.LevelReached >= 1 {
			fmt.Fprintf(out, "Context compressed: %d -> %d tokens (level %d)\n", stats.TokensBefore, stats.TokensAfter, stats.LevelReached)
		} else {
			fmt.Fprintln(out, "No compression needed.")
		}
		return true, nil
	case cmd == "skill":
		printVisibleSkills(out, engine.SkillRegistry(), engine.Config.CWD)
		return true, nil
	case cmd == "mcp":
		printMCPTools(out, engine.CoreEngine.Registry)
		return true, nil
	default:
		return false, nil
	}
}

func currentSessionSet(sessionID *string) map[string]struct{} {
	if sessionID == nil || strings.TrimSpace(*sessionID) == "" {
		return nil
	}
	return map[string]struct{}{strings.TrimSpace(*sessionID): {}}
}

func hasCommandFlag(command, flag string) bool {
	for _, part := range strings.Fields(command) {
		if part == flag {
			return true
		}
	}
	return false
}

func printCommandHelp(out io.Writer, skillRegistry *skills.SkillRegistry, cwd string) {
	rows := luminacli.IterCommandHelpRows(skillRegistry, cwd)
	fmt.Fprintln(out, "Commands")
	for _, row := range rows {
		fmt.Fprintf(out, "  %-28s %s\n", row.Command, row.Description)
	}
}

func printVisibleSkills(out io.Writer, registry *skills.SkillRegistry, cwd string) {
	if registry == nil {
		fmt.Fprintln(out, "Skills are disabled.")
		return
	}
	visible := registry.ListUserInvocable(cwd)
	if len(visible) == 0 {
		fmt.Fprintln(out, "No visible skills for current directory.")
		return
	}
	fmt.Fprintln(out, "Visible Skills")
	for _, skill := range visible {
		description := skill.Frontmatter.Description
		if skill.Frontmatter.ArgumentHint != nil && *skill.Frontmatter.ArgumentHint != "" {
			description = *skill.Frontmatter.ArgumentHint
		}
		fmt.Fprintf(out, "  /%-24s %s\n", skill.CanonicalName, description)
	}
}

func printMCPTools(out io.Writer, registry *coretools.ToolRegistry) {
	rows := mcpRowsFromRegistry(registry)
	if len(rows) == 0 {
		fmt.Fprintln(out, "No registered MCP tools in current session.")
		return
	}
	fmt.Fprintln(out, "Registered MCP Tools")
	fmt.Fprintf(out, "  %-36s %-9s %s\n", "Tool", "Kind", "Status")
	for _, row := range rows {
		fmt.Fprintf(out, "  %-36s %-9s %s\n", row.name, row.kind, row.status)
	}
}

type mcpToolRow struct {
	name   string
	kind   string
	status string
}

func mcpRowsFromRegistry(registry *coretools.ToolRegistry) []mcpToolRow {
	if registry == nil {
		return nil
	}
	resourceNames := map[string]struct{}{
		"mcp_list_resources": {},
		"mcp_read_resource":  {},
	}
	rowsByName := map[string]mcpToolRow{}
	for _, tool := range registry.ListTools() {
		name := tool.Name()
		if !isMCPToolName(name, resourceNames) {
			continue
		}
		rowsByName[name] = mcpToolRow{name: name, kind: mcpToolKind(name), status: "registered"}
	}
	for _, tool := range registry.GetDeferredTools() {
		name := tool.Name()
		if _, exists := rowsByName[name]; exists || !isMCPToolName(name, resourceNames) {
			continue
		}
		rowsByName[name] = mcpToolRow{name: name, kind: mcpToolKind(name), status: "deferred"}
	}
	names := make([]string, 0, len(rowsByName))
	for name := range rowsByName {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]mcpToolRow, 0, len(names))
	for _, name := range names {
		rows = append(rows, rowsByName[name])
	}
	return rows
}

func isMCPToolName(name string, resourceNames map[string]struct{}) bool {
	if strings.HasPrefix(name, "mcp__") {
		return true
	}
	_, ok := resourceNames[name]
	return ok
}

func mcpToolKind(name string) string {
	if strings.HasPrefix(name, "mcp__") {
		return "dynamic"
	}
	return "resource"
}

func printTokens(out io.Writer, state *agent.AgentState) {
	if state == nil {
		fmt.Fprintln(out, "No active session.")
		return
	}
	inputTokens := state.TotalInputTokens
	outputTokens := state.TotalOutputTokens
	fmt.Fprintln(out, "Session Tokens")
	fmt.Fprintf(out, "  Input tokens   %d\n", inputTokens)
	fmt.Fprintf(out, "  Output tokens  %d\n", outputTokens)
	fmt.Fprintf(out, "  Total tokens   %d\n", inputTokens+outputTokens)
	fmt.Fprintf(out, "  Turns          %d\n", state.TurnCount)
}

func printSessions(store *session.Store) {
	sessions := store.ListSessions()
	if len(sessions) == 0 {
		fmt.Println("No saved sessions.")
		return
	}
	fmt.Println("Saved sessions:")
	for _, meta := range sessions {
		when := time.Unix(0, int64(meta.LastUpdated*1e9)).Format("2006-01-02 15:04")
		fmt.Printf("  %s  - %d msgs, %d turns, last: %s\n", meta.SessionID, meta.MessageCount, meta.TurnCount, when)
	}
}

func printStorageReport(out io.Writer, report maintenance.Report) {
	mode := "dry-run"
	if report.Enforced {
		mode = "enforced"
	}
	fmt.Fprintf(out, "Storage %s\n", mode)
	fmt.Fprintf(out, "  Sessions: %d\n", report.SessionCount)
	fmt.Fprintf(out, "  Total:    %s\n", humanBytes(report.TotalBytes))
	fmt.Fprintf(out, "  Archive:  %s\n", report.ArchiveDir)
	if report.Enforced {
		fmt.Fprintf(out, "  Deleted:  %d sessions, %s freed\n", report.DeletedCount, humanBytes(report.FreedBytes))
	}
	if len(report.Actions) == 0 {
		fmt.Fprintln(out, "  Actions:  none")
		return
	}
	fmt.Fprintln(out, "Actions")
	for _, action := range report.Actions {
		status := "would remove"
		if action.Deleted {
			status = "removed"
		}
		if action.Error != "" {
			status = "error"
		}
		target := action.SessionID
		if target == "" {
			target = filepath.Base(action.Path)
		}
		fmt.Fprintf(out, "  %-12s %-36s %8s  %s\n", status, target, humanBytes(action.Bytes), action.Reason)
	}
}

func humanBytes(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(bytes)
	for _, unit := range units {
		value /= 1024
		if value < 1024 {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%.1f PB", value/1024)
}
