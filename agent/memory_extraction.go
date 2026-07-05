package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"LuminaCode/config"
	"LuminaCode/memory"
	coretools "LuminaCode/tools"
)

const extractionResultPreviewChars = 500

var ExtractionAgentDef = AgentDef{
	Name:           "auto-memory-extract",
	Description:    "Background agent that extracts persistent memories from conversation context",
	ToolsAllowlist: stringSet("read_file", "write_file", "edit_file", "grep_search", "glob_match", "run_shell"),
	MaxTurns:       5,
	PermissionMode: "inherit",
}

type ExtractionConfig struct {
	TurnsBetweenExtractions int
	MaxExtractionTurns      int
	ContextMessageCount     int
	CustomPromptPath        string
}

func DefaultExtractionConfig() ExtractionConfig {
	return ExtractionConfig{TurnsBetweenExtractions: 5, MaxExtractionTurns: 5, ContextMessageCount: 8}
}

func BuildExtractionRegistry(baseRegistry *coretools.ToolRegistry) *coretools.ToolRegistry {
	return BuildFilteredRegistry(baseRegistry, ExtractionAgentDef)
}

type ExtractionRunner func(ctx context.Context, prompt, systemPrompt string, filteredRegistry *coretools.ToolRegistry, extraContext coretools.ExecutionContext) (string, error)

type ExtractionController struct {
	Config           config.Config
	BaseRegistry     *coretools.ToolRegistry
	ExtractionConfig ExtractionConfig
	Runner           ExtractionRunner

	mu             sync.Mutex
	currentRunning bool
	currentCancel  context.CancelFunc
	currentRunID   uint64
	pendingContext *extractionContext
	lastResult     *string
}

type extractionContext struct {
	Messages      []map[string]any
	TurnCount     int
	UserTurnCount int
	State         *AgentState
	MemoryDir     string
}

func NewExtractionController(cfg config.Config, baseRegistry *coretools.ToolRegistry, extractionConfig ...ExtractionConfig) *ExtractionController {
	ec := DefaultExtractionConfig()
	if len(extractionConfig) > 0 {
		ec = extractionConfig[0]
	}
	return &ExtractionController{Config: cfg, BaseRegistry: baseRegistry, ExtractionConfig: ec}
}

func (c *ExtractionController) ShouldExtract(state *AgentState) bool {
	if state == nil {
		return false
	}
	turnsSince := state.UserTurnCount - state.LastExtractionUserTurn
	if turnsSince < c.ExtractionConfig.TurnsBetweenExtractions {
		return false
	}
	if state.MemoryWritesSinceExtraction {
		return false
	}
	return true
}

func (c *ExtractionController) Schedule(_ context.Context, state *AgentState, memoryDir string) bool {
	if !c.ShouldExtract(state) {
		return false
	}
	recent := recentMessages(state.Messages, c.ExtractionConfig.ContextMessageCount)
	payload := &extractionContext{
		Messages:      recent,
		TurnCount:     state.TurnCount,
		UserTurnCount: state.UserTurnCount,
		State:         state,
		MemoryDir:     memoryDir,
	}
	c.mu.Lock()
	if c.currentRunning {
		c.pendingContext = payload
		c.mu.Unlock()
		return false
	}
	runCtx, cancel := context.WithCancel(context.Background())
	c.currentRunning = true
	c.currentCancel = cancel
	c.currentRunID++
	runID := c.currentRunID
	c.mu.Unlock()
	go c.runExtraction(runCtx, payload, runID)
	return true
}

func (c *ExtractionController) HasPendingResult() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastResult != nil
}

func (c *ExtractionController) ConsumeResult() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastResult == nil {
		return ""
	}
	result := *c.lastResult
	c.lastResult = nil
	return result
}

func (c *ExtractionController) Cancel() {
	c.mu.Lock()
	cancel := c.currentCancel
	c.currentCancel = nil
	c.currentRunning = false
	c.currentRunID++
	defer c.mu.Unlock()
	c.pendingContext = nil
	c.lastResult = nil
	if cancel != nil {
		cancel()
	}
}

func (c *ExtractionController) runExtraction(ctx context.Context, payload *extractionContext, runID uint64) {
	defer func() {
		c.mu.Lock()
		pending := c.pendingContext
		c.pendingContext = nil
		if pending != nil {
			runCtx, cancel := context.WithCancel(context.Background())
			c.currentCancel = cancel
			c.currentRunning = true
			c.currentRunID++
			nextRunID := c.currentRunID
			c.mu.Unlock()
			go c.runExtraction(runCtx, pending, nextRunID)
			return
		}
		if c.currentRunID == runID {
			c.currentRunning = false
			c.currentCancel = nil
		}
		c.mu.Unlock()
	}()
	indexContent := memory.LoadMemoryIndex(payload.MemoryDir)
	manifest := indexContent
	if manifest == "" {
		manifest = "(no indexed memories yet)"
	}
	prompt := BuildExtractionPrompt(payload.Messages, manifest, c.ExtractionConfig.MaxExtractionTurns)
	systemPrompt := LoadExtractionSystemPrompt(firstNonEmptyString(c.ExtractionConfig.CustomPromptPath, c.Config.MemoryExtractionPromptPath))
	filtered := BuildExtractionRegistry(c.BaseRegistry)
	absMemoryDir, err := filepath.Abs(payload.MemoryDir)
	if err != nil {
		absMemoryDir = payload.MemoryDir
	}
	extraContext := coretools.ExecutionContext{
		"allowed_write_roots":    []string{absMemoryDir},
		"skip_read_before_edit":  true,
		"system_prompt_override": systemPrompt,
	}
	runner := c.Runner
	if runner == nil {
		runner = c.defaultRunner(payload.State)
	}
	result, err := runner(ctx, prompt, systemPrompt, filtered, extraContext)
	if err != nil {
		return
	}
	_, _ = memory.WriteMemoryIndex(payload.MemoryDir)
	formatted := FormatExtractionResult(result)
	c.mu.Lock()
	c.lastResult = &formatted
	c.mu.Unlock()
	if payload.State != nil {
		payload.State.LastExtractionTurn = payload.State.TurnCount
		payload.State.LastExtractionUserTurn = payload.State.UserTurnCount
		payload.State.MemoryWritesSinceExtraction = false
	}
}

func (c *ExtractionController) defaultRunner(parentState *AgentState) ExtractionRunner {
	return func(ctx context.Context, prompt, systemPrompt string, filteredRegistry *coretools.ToolRegistry, extraContext coretools.ExecutionContext) (string, error) {
		model := c.Config.APIModel
		if c.Config.ExtractionModel != nil && *c.Config.ExtractionModel != "" {
			model = *c.Config.ExtractionModel
		}
		sub := NewSubAgent(c.Config, filteredRegistry, ExtractionAgentDef, parentState, model, "auto-memory-extract", extraContext)
		return sub.Run(ctx, prompt)
	}
}

func BuildExtractionPrompt(messagesSlice []map[string]any, existingManifest string, maxTurns int) string {
	var msgLines []string
	for _, msg := range messagesSlice {
		role := stringFromAny(msg["role"])
		if role == "" {
			role = "unknown"
		}
		msgLines = append(msgLines, "## "+role+"\n"+formatExtractionMessageContent(msg["content"]))
	}
	conversationText := strings.Join(msgLines, "\n\n")
	return fmt.Sprintf(`## Existing MEMORY.md index (check before creating duplicates)

%s

## Recent conversation

%s

## Instructions

1. Use the existing MEMORY.md index above to avoid duplicate memories.
2. Read any existing memory files that overlap with new insights.
3. Write NEW memory files only for genuinely new, reusable information.
4. Do NOT write information already in existing memory files.
5. Keep MEMORY.md in sync with any memory files you create or update.

Be selective — only extract insights that would help in a FUTURE,
unrelated coding session. Skip trivial facts and ephemeral details.

You have %d turns maximum. Aim for 2 turns:
Turn 1: parallel reads. Turn 2: parallel writes.
Return a brief summary of what you saved (or why you saved nothing).`, existingManifest, conversationText, maxTurns)
}

func LoadExtractionSystemPrompt(customPath string) string {
	paths := []string{}
	if customPath != "" {
		paths = append(paths, customPath)
	}
	if root := config.FindLuminaRoot(""); root != "" {
		paths = append(paths, config.LuminaResourcePath(root, "SYSTEM", "extraction_system.md"))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.TrimSpace(string(data))
		}
	}
	return strings.TrimSpace(extractionSystemPromptFallback)
}

func FormatExtractionResult(agentResult string) string {
	summary := strings.TrimSpace(agentResult)
	summary = truncateExtractionRunes(summary, extractionResultPreviewChars)
	if summary == "" {
		return ""
	}
	return "<system-reminder note=\"auto-memory\">\nBackground memory extraction completed:\n" + summary + "\n</system-reminder>"
}

func recentMessages(messages []map[string]any, limit int) []map[string]any {
	if limit <= 0 || len(messages) <= limit {
		return append([]map[string]any(nil), messages...)
	}
	return append([]map[string]any(nil), messages[len(messages)-limit:]...)
}

func formatExtractionMessageContent(raw any) string {
	switch content := raw.(type) {
	case []map[string]any:
		return formatExtractionBlocks(content)
	case []any:
		blocks := make([]map[string]any, 0, len(content))
		for _, item := range content {
			if block, ok := item.(map[string]any); ok {
				blocks = append(blocks, block)
			}
		}
		return formatExtractionBlocks(blocks)
	case string:
		return content
	default:
		return fmt.Sprint(raw)
	}
}

func formatExtractionBlocks(blocks []map[string]any) string {
	var parts []string
	for _, block := range blocks {
		switch block["type"] {
		case "text":
			parts = append(parts, stringFromAny(block["text"]))
		case "tool_use":
			parts = append(parts, fmt.Sprintf("[tool: %s id=%s]", stringFromAny(block["name"]), stringFromAny(block["id"])))
		case "tool_result":
			content := stringFromAny(block["content"])
			if len([]rune(content)) > 200 {
				content = truncateExtractionRunes(content, 200) + "...(truncated)"
			}
			parts = append(parts, "[tool_result: "+content+"]")
		}
	}
	return strings.Join(parts, "\n")
}

func truncateExtractionRunes(text string, limit int) string {
	runes := []rune(text)
	if limit < 0 || len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

const extractionSystemPromptFallback = `You are a background memory extraction agent. Your job is to identify useful, reusable information from the recent conversation and save it as memory files in the memory directory.

## What to extract
- **user** — User role, preferences, knowledge, coding conventions.
  Save discoveries about how the user works.
- **feedback** — Behavioral corrections from the user. Include **Why:** and **How to apply:** lines. Also capture confirmations of non-obvious approaches that worked.
- **project** — Ongoing work, decisions, deadlines. Convert relative dates to absolute dates.
- **reference** — Pointers to external systems (issue trackers, dashboards, docs).

## What NOT to extract
- Code patterns, architecture, file paths — these are in the code
- Git history — use git log / git blame
- Debugging solutions — the fix is in the code
- Content already in LUMINA.md files
- Ephemeral task details or in-progress work
- Trivial one-off facts

## Strategy (2 turns typical, 5 max)
Turn 1: Use the provided manifest and read only existing memory files that overlap with potential new memories. Check for duplicates.
Turn 2+: Write new memory files for genuinely new information. Update existing files only if adding significant new detail.

## File format
Each memory file is Markdown with YAML frontmatter:

---
name: {{short-kebab-case-slug}}
description: {{one-line summary}}
metadata:
  type: {{user|feedback|project|reference}}
---

{{content}}

After creating or updating memory files, update MEMORY.md so it remains a compact index of all available memory files.`
