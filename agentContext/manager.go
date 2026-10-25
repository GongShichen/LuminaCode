package agentContext

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/llmclient"
	"github.com/google/uuid"
)

var ErrStaleContext = errors.New("context changed while the request was being prepared")
var ErrContextBudgetExceeded = errors.New("required context exceeds the configured input token budget")
var ErrEmptySummary = errors.New("compactor returned an empty summary")

type CompactMode string

const (
	CompactAuto     CompactMode = "auto"
	CompactManual   CompactMode = "manual"
	CompactReactive CompactMode = "reactive"
)

type Attachment struct {
	ID       string  `json:"id"`
	Source   string  `json:"source"`
	Lifetime string  `json:"lifetime"`
	Message  Message `json:"message"`
}

type ContextRegion struct {
	MessageIDs []string `json:"message_ids"`
	Summary    string   `json:"summary"`
}

type TokenBudget struct {
	System    int  `json:"system"`
	Messages  int  `json:"messages"`
	Tools     int  `json:"tools"`
	Total     int  `json:"total"`
	Limit     int  `json:"limit"`
	Estimated bool `json:"estimated"`
}

type ContextSnapshot struct {
	Version             int             `json:"version"`
	Revision            uint64          `json:"revision"`
	Generation          uint64          `json:"generation"`
	SystemPrompt        string          `json:"system_prompt"`
	Messages            []Message       `json:"messages"`
	Attachments         []Attachment    `json:"attachments,omitempty"`
	Regions             []ContextRegion `json:"regions,omitempty"`
	ConsecutiveFailures int             `json:"consecutive_failures"`
	Budget              TokenBudget     `json:"budget"`
}

type CompiledContext struct {
	SystemPrompt string           `json:"system_prompt"`
	Messages     []map[string]any `json:"messages"`
	ToolSchemas  []map[string]any `json:"tool_schemas"`
	Budget       TokenBudget      `json:"budget"`
	Digest       string           `json:"digest"`
}

type BuildInput struct {
	SystemPrompt string
	ToolSchemas  []map[string]any
	Evidence     []Message
	RestoreFiles []map[string]string
	Normalize    func([]map[string]any) []map[string]any
}

type UsageObservation struct {
	RequestID         string `json:"request_id"`
	InputTokens       int    `json:"input_tokens"`
	OutputTokens      int    `json:"output_tokens"`
	CacheReadTokens   int    `json:"cache_read_tokens"`
	CacheCreateTokens int    `json:"cache_create_tokens"`
}

type SummaryResult struct {
	Text  string
	Usage UsageObservation
}

type CompactionResult struct {
	Usage           []UsageObservation
	BaseRevision    uint64
	Generation      uint64
	Next            ContextSnapshot
	Compiled        CompiledContext
	Stats           CompressionStats
	Attempted       bool
	Succeeded       bool
	HistoryReplaced bool
}

type SummaryFunc func(context.Context, string, string) (SummaryResult, error)

// ContextManager owns one agent's model working view. Preparation never mutates
// published state; the runtime commits the candidate only after durable writes.
type ContextManager struct {
	mu            sync.RWMutex
	cfg           config.Config
	state         ContextSnapshot
	summarize     SummaryFunc
	customSummary bool
	lastInput     BuildInput
	cache         CacheState
}

func NewContextManager(cfg config.Config, summarize SummaryFunc) *ContextManager {
	m := &ContextManager{cfg: cfg, state: ContextSnapshot{Version: 3, Generation: 1}, customSummary: summarize != nil}
	if summarize == nil {
		summarize = defaultSummarizer(cfg)
	}
	m.summarize = summarize
	return m
}

func cloneContext(s ContextSnapshot) ContextSnapshot {
	raw, _ := json.Marshal(s)
	var result ContextSnapshot
	_ = json.Unmarshal(raw, &result)
	return result
}

func (m *ContextManager) Snapshot() ContextSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneContext(m.state)
}

func (m *ContextManager) Restore(s ContextSnapshot) error {
	if s.Version != 0 && s.Version != 3 {
		return fmt.Errorf("unsupported context version %d", s.Version)
	}
	s.Version = 3
	if s.Generation == 0 {
		s.Generation = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Generation != s.Generation {
		m.lastInput = BuildInput{}
		m.cache = CacheState{}
	}
	m.state = cloneContext(s)
	if s.Revision == 0 {
		m.lastInput = BuildInput{}
	}
	return nil
}

func (m *ContextManager) Commit(result CompactionResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Revision != result.BaseRevision || m.state.Generation != result.Generation {
		return ErrStaleContext
	}
	next := cloneContext(result.Next)
	next.Revision = m.state.Revision + 1
	if result.HistoryReplaced {
		m.cache = CacheState{}
	}
	m.state = next
	return nil
}

func (m *ContextManager) Configure(cfg config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
	if !m.customSummary {
		m.summarize = defaultSummarizer(cfg)
	}
}

func (m *ContextManager) Build(ctx context.Context, input BuildInput) (CompactionResult, error) {
	return m.prepare(ctx, CompactAuto, input)
}

func (m *ContextManager) Compact(ctx context.Context, mode CompactMode, inputs ...BuildInput) (CompactionResult, error) {
	var input BuildInput
	if len(inputs) > 0 {
		input = inputs[0]
	} else {
		m.mu.RLock()
		input = m.lastInput
		m.mu.RUnlock()
		input.SystemPrompt = m.Snapshot().SystemPrompt
	}
	return m.prepare(ctx, mode, input)
}

func RequestBudget(system string, messages, tools []map[string]any, limit int) TokenBudget {
	// Re-estimate the current view after every edit. A usage anchor for an older
	// request cannot account for changed system prompts, schemas or folded history.
	raw, _ := json.Marshal(tools)
	b := TokenBudget{System: RoughEstimate(system), Messages: RoughEstimate(messages), Tools: len([]rune(string(raw))) / 4, Limit: limit, Estimated: true}
	b.Total = b.System + b.Messages + b.Tools
	return b
}

func (m *ContextManager) Validate(c CompiledContext) (CompiledContext, error) {
	m.mu.RLock()
	limit := m.cfg.CompressionContextLimit()
	m.mu.RUnlock()
	c.Budget = RequestBudget(c.SystemPrompt, c.Messages, c.ToolSchemas, limit)
	raw, _ := json.Marshal([]any{c.SystemPrompt, c.Messages, c.ToolSchemas})
	c.Digest = fmt.Sprintf("%x", sha256.Sum256(raw))
	if c.Budget.Total > limit {
		return c, fmt.Errorf("%w: %d/%d tokens", ErrContextBudgetExceeded, c.Budget.Total, limit)
	}
	return c, nil
}

func projectRegions(messages []map[string]any, regions []ContextRegion) []map[string]any {
	skip := map[string]bool{}
	insert := map[string]string{}
	for _, region := range regions {
		if len(region.MessageIDs) == 0 {
			continue
		}
		insert[region.MessageIDs[0]] = region.Summary
		for _, id := range region.MessageIDs {
			skip[id] = true
		}
	}
	var result []map[string]any
	for _, msg := range messages {
		id, _ := msg["context_id"].(string)
		if summary, ok := insert[id]; ok {
			result = append(result, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": summary}}, "metadata": map[string]any{"source": "compaction"}})
		}
		if !skip[id] {
			result = append(result, msg)
		}
	}
	return result
}

func materialize(s ContextSnapshot, history []map[string]any) []map[string]any {
	messages := projectRegions(history, s.Regions)
	at := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i]["role"] == "user" && !hasToolResultBlock(messages[i]) {
			at = i
			break
		}
	}
	attachments := make([]map[string]any, 0, len(s.Attachments))
	for _, a := range s.Attachments {
		attachments = append(attachments, a.Message.Map())
	}
	result := append([]map[string]any{}, messages[:at]...)
	result = append(result, attachments...)
	return append(result, messages[at:]...)
}

func (m *ContextManager) prepare(ctx context.Context, mode CompactMode, input BuildInput) (CompactionResult, error) {
	m.mu.RLock()
	cfg, summarize := m.cfg, m.summarize
	m.mu.RUnlock()
	m.mu.Lock()
	m.lastInput = input
	m.mu.Unlock()
	s := m.Snapshot()
	r := CompactionResult{BaseRevision: s.Revision, Generation: s.Generation, Next: s}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	r.Next.SystemPrompt = input.SystemPrompt
	history := MessageMaps(s.Messages)
	limit := cfg.CompressionContextLimit()
	soft := int(float64(limit) * cfg.CompressionThreshold())
	count := func(h []map[string]any) int {
		return RequestBudget(input.SystemPrompt, materialize(r.Next, h), input.ToolSchemas, limit).Total
	}
	before := count(history)
	r.Stats.TokensBefore = before
	target := soft
	if mode == CompactReactive {
		target = min(soft, max(0, before-1))
	}
	should := before > soft || mode != CompactAuto
	var compactErr error
	if should && len(history) > 0 {
		// Restored files are optional hints. Evict an older restoration before
		// reducing history, and never accumulate copies across compactions.
		kept := r.Next.Attachments[:0]
		for _, attachment := range r.Next.Attachments {
			if attachment.Source != "post_compact_files" {
				kept = append(kept, attachment)
			}
		}
		r.Next.Attachments = kept
		r.Attempted = true
		old := count(history)
		beforeChars := totalResultChars(history)
		history = SnipMessages(history)
		r.Stats.SnipRemoved = max(0, beforeChars-totalResultChars(history))
		r.Stats.LevelReached = 1
		r.Stats.SnipTokensFreed = max(0, old-count(history))
		if count(history) > target || mode != CompactAuto {
			old = count(history)
			cleared := CountClearedToolResult(history)
			history, _ = MicroCompactMessages(history, true, 1)
			r.Stats.MicroCleared = CountClearedToolResult(history) - cleared
			r.Stats.MicroTruncated = r.Stats.MicroCleared
			r.Stats.LevelReached = 2
			r.Stats.MicroTokensFreed = max(0, old-count(history))
		}
		if count(history) > target || mode != CompactAuto {
			old = count(history)
			// Fold only a safe old prefix; retained tool results keep their calls.
			end := SafeTailStart(history, 10)
			used := map[string]bool{}
			currentUser := -1
			for i := len(history) - 1; i >= 0; i-- {
				if history[i]["role"] == "user" && !hasToolResultBlock(history[i]) && !isTransientContextMessage(history[i]) {
					currentUser = i
					used[stringFromMap(history[i], "context_id")] = true
					break
				}
			}
			_ = currentUser
			for _, region := range r.Next.Regions {
				for _, id := range region.MessageIDs {
					used[id] = true
				}
			}
			for start := 0; start < end; {
				for start < end && used[stringFromMap(history[start], "context_id")] {
					start++
				}
				stop := start
				for stop < end && !used[stringFromMap(history[stop], "context_id")] {
					stop++
				}
				if stop-start >= 3 {
					prefix := CollapseMessages(history[start:stop], 1)
					summary := "[Earlier conversation summarized]"
					if len(prefix) > 0 {
						summary = textFromMessage(prefix[0])
					}
					region := ContextRegion{Summary: summary}
					for _, msg := range history[start:stop] {
						region.MessageIDs = append(region.MessageIDs, stringFromMap(msg, "context_id"))
					}
					r.Next.Regions = append(r.Next.Regions, region)
					r.Stats.LevelReached = 3
				}
				start = max(stop, start+1)
			}
			r.Stats.CollapsedTokensFreed = max(0, old-count(history))
			r.Stats.CollapseCount = max(0, len(history)-len(projectRegions(history, r.Next.Regions)))
		}
		tail := SafeTailStart(history, 4)
		forceSummary := mode == CompactManual && tail > 0
		if (count(history) > target || forceSummary) && tail > 0 && s.ConsecutiveFailures < MaxConsecutiveFailure {
			r.Stats.LevelReached = 4
			r.Stats.AutoTriggered = true
			old = count(history)
			evidence := history
			if len(input.Evidence) > 0 {
				evidence = MessageMaps(input.Evidence)
			}
			prompt := boundedSummaryPrompt(evidence, max(128, limit-100))
			var summary string
			for attempt := 0; attempt <= MaxPtlRetries; attempt++ {
				observation, summaryErr := summarize(ctx, "Create a concise factual handoff preserving goals, constraints, decisions, tool findings, failures and the next step. Return text only.", prompt)
				summary, compactErr = observation.Text, summaryErr
				if observation.Usage.InputTokens > 0 || observation.Usage.OutputTokens > 0 {
					r.Usage = append(r.Usage, observation.Usage)
				}
				if compactErr == nil && strings.TrimSpace(summary) == "" {
					compactErr = ErrEmptySummary
				}
				if compactErr == nil || !isPtlError(compactErr) || attempt == MaxPtlRetries {
					break
				}
				prompt = TruncateRunes(prompt, max(64, len([]rune(prompt))/2))
			}
			if compactErr != nil {
				r.Next.ConsecutiveFailures++
			} else {
				replacement := BuildCompactionReplacementHistory(history, summary, 2)
				replacement.Recent = history[tail:]
				handoff := buildCompactionHandoffMessage(replacement)
				handoff["metadata"] = map[string]any{"source": "compaction"}
				candidate := []map[string]any{handoff}
				for i := tail - 1; i >= 0; i-- {
					msg := history[i]
					if msg["role"] == "user" && !hasToolResultBlock(msg) && !isTransientContextMessage(msg) {
						candidate = append(candidate, msg)
						break
					}
				}
				candidate = append(candidate, replacement.Recent...)
				saved := r.Next.Regions
				r.Next.Regions = nil
				if count(candidate) < old {
					history = CloneMessages(candidate)
					r.HistoryReplaced = true
					r.Next.ConsecutiveFailures = 0
					r.Stats.AutoTokensFreed = old - count(history)
				} else {
					r.Next.Regions = saved
				}
			}
		}
	}
	r.Next.Messages = MessagesFromMaps("working", history)
	messages := materialize(r.Next, history)
	if r.HistoryReplaced {
		// File restoration is optional context and cannot consume mandatory space.
		for _, f := range input.RestoreFiles[max(0, len(input.RestoreFiles)-PostCompactMaxFilesToRestore):] {
			restoreLimit := min(limit, min(soft, max(0, before-1)))
			remaining := restoreLimit - RequestBudget(input.SystemPrompt, messages, input.ToolSchemas, limit).Total - 64
			if remaining <= 0 {
				break
			}
			content := TruncateRunes(f["content"], min(PostCompactMaxTokenPerFile*4, remaining*3))
			if content == "" {
				continue
			}
			msg := map[string]any{"role": "user", "isMeta": true, "metadata": map[string]any{"source": "post_compact_files"}, "content": []map[string]any{{"type": "text", "text": "[System: Post-compact memory restoration]\n--- File: " + f["path"] + " ---\n" + content}}}
			candidate := append(append([]map[string]any{}, messages...), msg)
			if RequestBudget(input.SystemPrompt, candidate, input.ToolSchemas, limit).Total <= limit {
				a := Attachment{ID: "file:" + f["path"], Source: "post_compact_files", Lifetime: "run", Message: MessageFromMap("file", 0, msg)}
				r.Next.Attachments = append(r.Next.Attachments, a)
				messages = candidate
			}
		}
	}
	if input.Normalize != nil {
		messages = input.Normalize(messages)
	}
	r.Compiled = CompiledContext{SystemPrompt: input.SystemPrompt, Messages: messages, ToolSchemas: input.ToolSchemas}
	var budgetErr error
	r.Compiled, budgetErr = m.Validate(r.Compiled)
	r.Next.Budget = r.Compiled.Budget
	kept := r.Next.Attachments[:0]
	for _, attachment := range r.Next.Attachments {
		if attachment.Lifetime != "request" {
			kept = append(kept, attachment)
		}
	}
	r.Next.Attachments = kept
	r.Stats.TokensAfter = r.Compiled.Budget.Total
	r.Succeeded = r.Stats.TokensAfter < before || r.HistoryReplaced
	if compactErr != nil {
		return r, compactErr
	}
	return r, budgetErr
}

func stringFromMap(m map[string]any, key string) string { s, _ := m[key].(string); return s }

func boundedSummaryPrompt(messages []map[string]any, tokenLimit int) string {
	var b strings.Builder
	b.WriteString("Summarize this session, including the following evidence:\n")
	for _, message := range messages {
		if isTransientContextMessage(message) {
			continue
		}
		b.WriteString("\n[" + stringFromMap(message, "role") + "] ")
		if text, ok := message["content"].(string); ok {
			b.WriteString(TruncateRunes(text, 500))
			continue
		}
		blocks, _ := contentBlocks(message["content"])
		for _, block := range blocks {
			raw, _ := json.Marshal(block)
			b.WriteString(TruncateRunes(string(raw), 1200))
			b.WriteByte('\n')
		}
	}
	text := b.String()
	runes := []rune(text)
	limit := max(256, tokenLimit*3)
	if len(runes) > limit {
		half := (limit - 40) / 2
		text = string(runes[:half]) + "\n[intermediate excerpts omitted]\n" + string(runes[len(runes)-half:])
	}
	return text
}

func defaultSummarizer(cfg config.Config) SummaryFunc {
	return func(ctx context.Context, system, prompt string) (SummaryResult, error) {
		summaryCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		result := SummaryResult{Usage: UsageObservation{RequestID: uuid.NewString()}}
		client, err := llmclient.Create(cfg, cfg.APIModel, 500, nil, api.DefaultRetryConfigPtr())
		if err != nil {
			return result, err
		}
		var text strings.Builder
		for item := range client.StreamChat(summaryCtx, system, []map[string]any{{"role": "user", "content": prompt}}, nil, nil) {
			if item.Err != nil {
				return result, item.Err
			}
			switch item.Event["type"] {
			case "text_delta":
				if delta, ok := item.Event["text"].(string); ok {
					text.WriteString(delta)
				}
			case "usage":
				result.Usage.InputTokens = getInt(item.Event, "input_tokens")
				result.Usage.OutputTokens = getInt(item.Event, "output_tokens")
				result.Usage.CacheReadTokens = getInt(item.Event, "cache_read_input_tokens")
				result.Usage.CacheCreateTokens = getInt(item.Event, "cache_creation_input_tokens")
			case "error":
				return result, fmt.Errorf("compactor stream: %v", item.Event["message"])
			}
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Text = strings.TrimSpace(text.String())
		if result.Text == "" {
			return result, ErrEmptySummary
		}
		return result, nil
	}
}

func Render(snapshot ContextSnapshot) []map[string]any {
	return materialize(snapshot, MessageMaps(snapshot.Messages))
}

func (s ContextSnapshot) Clone() ContextSnapshot { return cloneContext(s) }
