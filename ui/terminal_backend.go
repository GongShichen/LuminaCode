package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"LuminaCode/agent"
	luminacli "LuminaCode/cli"
	"LuminaCode/config"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"

	"github.com/chzyer/readline"
	"golang.org/x/term"
)

type TerminalRendererBackend struct {
	*TerminalUIBackendMixin
	in            *bufio.Reader
	out           io.Writer
	errOut        io.Writer
	registry      *coretools.ToolRegistry
	execCtx       coretools.ExecutionContext
	toolBuffer    []coretools.ToolCall
	lineEditor    *readline.Instance
	skillRegistry *skills.SkillRegistry
	inputFile     *os.File
	outputFile    *os.File
	interactive   bool
	history       []string
	historyIndex  int
	inputDraft    string
}

func NewRendererBackend(name string, in io.Reader, out io.Writer, errOut io.Writer) RendererBackend {
	return NewFullscreenRendererBackend(in, out, errOut)
}

func ConfigureRendererBackend(backend RendererBackend, registry *coretools.ToolRegistry, execCtx coretools.ExecutionContext) {
	if configurable, ok := backend.(interface {
		SetRegistry(*coretools.ToolRegistry)
		SetExecContext(coretools.ExecutionContext)
	}); ok {
		configurable.SetRegistry(registry)
		configurable.SetExecContext(execCtx)
	}
}

func RenderBackendWelcome(backend RendererBackend, sessionID string, skillRegistry *skills.SkillRegistry) {
	if welcome, ok := backend.(interface {
		RenderWelcome(string, *skills.SkillRegistry)
	}); ok {
		welcome.RenderWelcome(sessionID, skillRegistry)
	}
}

func ReadBackendInput(backend RendererBackend, state any) (string, bool) {
	if input, ok := backend.(interface {
		GetInput(any) (string, bool)
	}); ok {
		return input.GetInput(state)
	}
	return "", false
}

func ResetBackendForNewSession(backend RendererBackend) {
	if resetter, ok := backend.(interface{ ResetForNewSession() }); ok {
		resetter.ResetForNewSession()
	}
}

func BackendOutputWriter(backend RendererBackend) io.Writer {
	if provider, ok := backend.(interface{ OutputWriter() io.Writer }); ok {
		return provider.OutputWriter()
	}
	return os.Stdout
}

func NewTerminalRendererBackend(in io.Reader, out io.Writer, errOut io.Writer) *TerminalRendererBackend {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	backend := &TerminalRendererBackend{
		out:    out,
		errOut: errOut,
	}
	if reader, ok := in.(*bufio.Reader); ok {
		backend.in = reader
	} else {
		backend.in = bufio.NewReader(in)
	}
	backend.initLineEditor(in, out, errOut)
	backend.TerminalUIBackendMixin = NewTerminalUIBackendMixin(backend.renderTaskSnapshot)
	return backend
}

func (b *TerminalRendererBackend) PrepareRuntime() {}

func (b *TerminalRendererBackend) initLineEditor(in io.Reader, out io.Writer, errOut io.Writer) {
	inputFile, inOK := in.(*os.File)
	outputFile, outOK := out.(*os.File)
	if !inOK || !outOK || !term.IsTerminal(int(inputFile.Fd())) || !term.IsTerminal(int(outputFile.Fd())) {
		return
	}
	b.inputFile = inputFile
	b.outputFile = outputFile
	b.interactive = true
	b.history = loadLuminaHistory(luminaHistoryFile())
	b.historyIndex = len(b.history)
	historyFile := luminaHistoryFile()
	_ = os.MkdirAll(filepath.Dir(historyFile), 0o755)
	editor, err := readline.NewEx(&readline.Config{
		Prompt:            "> ",
		HistoryFile:       historyFile,
		HistorySearchFold: true,
		AutoComplete:      terminalReadlineCompleter{backend: b},
		InterruptPrompt:   "",
		EOFPrompt:         "",
		Stdin:             inputFile,
		Stdout:            out,
		Stderr:            errOut,
	})
	if err == nil {
		b.lineEditor = editor
	}
}

func (b *TerminalRendererBackend) RenderWelcome(sessionID string, skillRegistry *skills.SkillRegistry) {
	b.skillRegistry = skillRegistry
	cwd := stringFromAny(b.execCtx["cwd"])
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	fmt.Fprintf(b.out, "LuminaCode REPL. Session %s. Type /exit to quit.\n", sessionID)
	fmt.Fprintf(b.out, "Path: %s\n", luminacli.FormatCWDForDisplay(cwd, 38))
	if skillRegistry != nil {
		fmt.Fprintf(b.out, "Skills: %d loaded\n", len(skillRegistry.ListAll()))
	}
}

func luminaHistoryFile() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".Lumina", "history")
	}
	return filepath.Join(home, ".Lumina", "history")
}

func (b *TerminalRendererBackend) GetInput(state any) (string, bool) {
	symbol := ">"
	if yoloState, ok := state.(interface{ YoloEnabled() bool }); ok && yoloState.YoloEnabled() {
		symbol = "!"
	}
	if b.interactive {
		if status := b.contextStatusText(state); status != "" {
			fmt.Fprintln(b.out, status)
		}
		line, ok := b.readInteractiveLine(symbol + " ")
		return strings.TrimSpace(line), ok
	}
	if b.lineEditor != nil {
		b.lineEditor.SetPrompt(symbol + " ")
		line, err := b.lineEditor.Readline()
		if errors.Is(err, readline.ErrInterrupt) || errors.Is(err, io.EOF) {
			fmt.Fprintln(b.out)
			return "", false
		}
		if err == nil {
			return strings.TrimSpace(line), true
		}
	}
	draft := b.consumeInputDraft()
	fmt.Fprintf(b.out, "%s %s", symbol, draft)
	line, err := b.in.ReadString('\n')
	if err != nil && len(line) == 0 {
		fmt.Fprintln(b.out)
		return "", false
	}
	return strings.TrimSpace(draft + line), true
}

func (b *TerminalRendererBackend) SetInputDraft(draft string) {
	b.inputDraft = draft
}

func (b *TerminalRendererBackend) consumeInputDraft() string {
	draft := b.inputDraft
	b.inputDraft = ""
	return draft
}

func (b *TerminalRendererBackend) contextStatusText(state any) string {
	cfg, ok := b.execCtx["config"].(config.Config)
	if !ok {
		return ""
	}
	var agentState *agent.AgentState
	switch typed := state.(type) {
	case *agent.AgentState:
		agentState = typed
	case agent.AgentState:
		agentState = &typed
	}
	snapshot := BuildContextWindowSnapshot(cfg, agentState)
	return FormatContextWindowStatus(snapshot.ModelName, snapshot.UsedTokens, snapshot.LimitTokens, 16)
}

func (b *TerminalRendererBackend) readInteractiveLine(prompt string) (string, bool) {
	if b.inputFile == nil || b.outputFile == nil {
		return "", false
	}
	fd := int(b.inputFile.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return "", false
	}
	defer func() {
		_ = term.Restore(fd, oldState)
	}()

	line := []rune(b.consumeInputDraft())
	cursor := len(line)
	selected := 0
	historyDraft := ""
	b.historyIndex = len(b.history)
	b.renderInteractiveInput(prompt, line, cursor, selected)
	for {
		key, text, err := b.readTerminalInputToken()
		if err != nil {
			fmt.Fprintln(b.out)
			return strings.TrimSpace(string(line)), len(line) > 0
		}
		switch key {
		case "enter":
			fmt.Fprint(b.out, "\r\033[J")
			fmt.Fprintf(b.out, "%s%s\n", prompt, string(line))
			value := strings.TrimSpace(string(line))
			if value != "" {
				b.appendHistory(value)
			}
			return value, true
		case "c-c", "c-d":
			fmt.Fprint(b.out, "\r\033[J\n")
			return "", false
		case "backspace":
			if cursor > 0 {
				line = append(line[:cursor-1], line[cursor:]...)
				cursor--
				selected = 0
			}
		case "left":
			if cursor > 0 {
				cursor--
			}
		case "right":
			if cursor < len(line) {
				cursor++
			}
		case "up":
			completions := b.currentSlashCompletions(line, cursor)
			if len(completions) > 0 {
				selected = (selected - 1 + len(completions)) % len(completions)
			} else if len(b.history) > 0 {
				if b.historyIndex == len(b.history) {
					historyDraft = string(line)
				}
				if b.historyIndex > 0 {
					b.historyIndex--
					line = []rune(b.history[b.historyIndex])
					cursor = len(line)
				}
			}
		case "down":
			completions := b.currentSlashCompletions(line, cursor)
			if len(completions) > 0 {
				selected = (selected + 1) % len(completions)
			} else if len(b.history) > 0 && b.historyIndex < len(b.history) {
				b.historyIndex++
				if b.historyIndex == len(b.history) {
					line = []rune(historyDraft)
				} else {
					line = []rune(b.history[b.historyIndex])
				}
				cursor = len(line)
			}
		case "tab":
			completions := b.currentSlashCompletions(line, cursor)
			if len(completions) > 0 {
				if selected < 0 || selected >= len(completions) {
					selected = 0
				}
				line, cursor = applyTerminalCompletion(line, cursor, completions[selected])
				selected = 0
			}
		default:
			if text != "" {
				for _, r := range text {
					if r == 0 || unicode.IsControl(r) {
						continue
					}
					line = append(line[:cursor], append([]rune{r}, line[cursor:]...)...)
					cursor++
				}
				selected = 0
				b.historyIndex = len(b.history)
			}
		}
		completions := b.currentSlashCompletions(line, cursor)
		if selected >= len(completions) {
			selected = 0
		}
		b.renderInteractiveInput(prompt, line, cursor, selected)
	}
}

func (b *TerminalRendererBackend) readTerminalInputToken() (string, string, error) {
	r, _, err := b.in.ReadRune()
	if err != nil {
		return "", "", err
	}
	switch r {
	case '\r', '\n':
		return "enter", "", nil
	case '\x03':
		return "c-c", "", nil
	case '\x04':
		return "c-d", "", nil
	case '\t':
		return "tab", "", nil
	case '\x7f', '\b':
		return "backspace", "", nil
	case '\x1b':
		return b.readTerminalEscapeKey(), "", nil
	default:
		return "", string(r), nil
	}
}

func (b *TerminalRendererBackend) readTerminalEscapeKey() string {
	sequence := "\x1b"
	for b.in.Buffered() > 0 {
		next, err := b.in.ReadByte()
		if err != nil {
			break
		}
		sequence += string(next)
		if len(sequence) >= 3 && ((next >= 'A' && next <= 'Z') || next == '~') {
			break
		}
	}
	switch sequence {
	case "\x1b[A":
		return "up"
	case "\x1b[B":
		return "down"
	case "\x1b[C":
		return "right"
	case "\x1b[D":
		return "left"
	default:
		return "escape"
	}
}

func (b *TerminalRendererBackend) currentSlashCompletions(line []rune, cursor int) []luminacli.Completion {
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(line) {
		cursor = len(line)
	}
	before := string(line[:cursor])
	if !luminacli.SlashCompletionActive(before) {
		return nil
	}
	cwd := stringFromAny(b.execCtx["cwd"])
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return luminacli.CompleteInput(before, b.skillRegistry, cwd)
}

func (b *TerminalRendererBackend) renderInteractiveInput(prompt string, line []rune, cursor int, selected int) {
	completions := b.currentSlashCompletions(line, cursor)
	if selected < 0 || selected >= len(completions) {
		selected = 0
	}
	fmt.Fprint(b.out, "\r\033[J")
	fmt.Fprintf(b.out, "%s%s", prompt, string(line))
	shown := minInt(len(completions), 8)
	for i := 0; i < shown; i++ {
		completion := completions[i]
		label := firstNonEmpty(completion.Display, completion.Text)
		meta := completion.DisplayMeta
		if i == selected {
			fmt.Fprintf(b.out, "\n  \033[7m%-22s\033[0m %s", label, meta)
		} else {
			fmt.Fprintf(b.out, "\n  %-22s %s", label, meta)
		}
	}
	if len(completions) > shown {
		fmt.Fprintf(b.out, "\n  ... %d more", len(completions)-shown)
		shown++
	}
	if shown > 0 {
		fmt.Fprintf(b.out, "\033[%dA", shown)
	}
	fmt.Fprint(b.out, "\r")
	if cursorOffset := len([]rune(prompt)) + cursor; cursorOffset > 0 {
		fmt.Fprintf(b.out, "\033[%dC", cursorOffset)
	}
}

func applyTerminalCompletion(line []rune, cursor int, completion luminacli.Completion) ([]rune, int) {
	length := completionReplacementLength(string(line[:cursor]), completion)
	if length < 0 {
		length = 0
	}
	if length > cursor {
		length = cursor
	}
	start := cursor - length
	replacement := []rune(completion.Text)
	next := make([]rune, 0, len(line)-length+len(replacement))
	next = append(next, line[:start]...)
	next = append(next, replacement...)
	next = append(next, line[cursor:]...)
	return next, start + len(replacement)
}

func loadLuminaHistory(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	history := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			history = append(history, line)
		}
	}
	return history
}

func (b *TerminalRendererBackend) appendHistory(line string) {
	if line == "" {
		return
	}
	if len(b.history) > 0 && b.history[len(b.history)-1] == line {
		return
	}
	b.history = append(b.history, line)
	path := luminaHistoryFile()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintln(f, line)
}

func (b *TerminalRendererBackend) ResetForNewSession() {
	b.toolBuffer = nil
	b.TerminalUIBackendMixin = NewTerminalUIBackendMixin(b.renderTaskSnapshot)
}

func (b *TerminalRendererBackend) SetRegistry(registry *coretools.ToolRegistry) {
	b.registry = registry
}

func (b *TerminalRendererBackend) SetExecContext(execCtx coretools.ExecutionContext) {
	b.execCtx = execCtx
}

func (b *TerminalRendererBackend) Shutdown(finalSnapshot RenderFrame) {
	b.TerminalUIBackendMixin.Shutdown(finalSnapshot)
	if b.lineEditor != nil {
		_ = b.lineEditor.Close()
		b.lineEditor = nil
	}
}

func (b *TerminalRendererBackend) RenderEvent(event agent.StreamEvent) {
	switch event.Type {
	case "text", "thinking":
		if event.Type == "text" {
			b.flushToolBuffer()
		}
		fmt.Fprint(b.out, event.Content)
	case "tool_call":
		b.bufferToolCall(event)
	case "tool_result":
		b.flushToolBuffer()
		if event.Content != "" {
			fmt.Fprintf(b.out, "[tool result] %s\n", event.Content)
		} else {
			b.renderToolResult(event)
		}
	case "error":
		b.flushToolBuffer()
		fmt.Fprintf(b.errOut, "\n%s\n", event.Content)
	case "cost":
		b.flushToolBuffer()
		fmt.Fprintf(b.out, "\n[cost] %s\n", event.Content)
	case "done":
		b.flushToolBuffer()
		if strings.TrimSpace(event.Content) != "" {
			fmt.Fprintln(b.out, event.Content)
		}
		fmt.Fprintln(b.out)
	default:
		if event.Content != "" {
			fmt.Fprintln(b.out, event.Content)
		}
	}
}

func (b *TerminalRendererBackend) bufferToolCall(event agent.StreamEvent) {
	input := map[string]any{}
	if raw, ok := event.Metadata["input"].(map[string]any); ok {
		input = raw
	}
	id := stringFromAny(event.Metadata["id"])
	name := firstNonEmpty(event.Content, stringFromAny(event.Metadata["tool_name"]))
	if len(b.toolBuffer) > 0 && b.toolBuffer[len(b.toolBuffer)-1].Name != name {
		b.flushToolBuffer()
	}
	b.toolBuffer = append(b.toolBuffer, coretools.ToolCall{ID: id, Name: name, Input: input})
}

func (b *TerminalRendererBackend) flushToolBuffer() {
	if len(b.toolBuffer) == 0 {
		return
	}
	buffered := b.toolBuffer
	b.toolBuffer = nil
	toolName := buffered[0].Name
	display := b.renderGroupedToolUse(toolName, buffered)
	if display == "" {
		display = toolName
	}
	fmt.Fprintf(b.out, "\n[tool] %s\n", display)
}

func (b *TerminalRendererBackend) renderGroupedToolUse(toolName string, calls []coretools.ToolCall) string {
	enriched := make([]any, 0, len(calls))
	for _, call := range calls {
		if b.registry != nil {
			enriched = append(enriched, b.registry.EnrichForRender(call, b.execCtx))
		} else {
			enriched = append(enriched, call.Input)
		}
	}
	switch toolName {
	case "read_file":
		var inputs []coretools.ReadFileInput
		for _, item := range enriched {
			if typed, ok := item.(coretools.ReadFileInput); ok {
				inputs = append(inputs, typed)
			}
		}
		if len(inputs) == len(enriched) {
			return coretools.RenderGroupedReadFileToolUse(inputs)
		}
	case "write_file":
		var inputs []coretools.WriteFileInput
		for _, item := range enriched {
			if typed, ok := item.(coretools.WriteFileInput); ok {
				inputs = append(inputs, typed)
			}
		}
		if len(inputs) == len(enriched) {
			return coretools.RenderGroupedWriteFileToolUse(inputs)
		}
	case "edit_file":
		var inputs []coretools.EditFileInput
		for _, item := range enriched {
			if typed, ok := item.(coretools.EditFileInput); ok {
				inputs = append(inputs, typed)
			}
		}
		if len(inputs) == len(enriched) {
			return coretools.RenderGroupedEditFileToolUse(inputs)
		}
	case "run_shell":
		var inputs []coretools.BashInput
		for _, item := range enriched {
			if typed, ok := item.(coretools.BashInput); ok {
				inputs = append(inputs, typed)
			}
		}
		if len(inputs) == len(enriched) {
			return coretools.RenderGroupedBashToolUse(inputs)
		}
	case "grep_search":
		var inputs []coretools.GrepSearchInput
		for _, item := range enriched {
			if typed, ok := item.(coretools.GrepSearchInput); ok {
				inputs = append(inputs, typed)
			}
		}
		if len(inputs) == len(enriched) {
			return coretools.RenderGroupedGrepSearchToolUse(inputs)
		}
	case "glob_match":
		var inputs []coretools.GlobMatchInput
		for _, item := range enriched {
			if typed, ok := item.(coretools.GlobMatchInput); ok {
				inputs = append(inputs, typed)
			}
		}
		if len(inputs) == len(enriched) {
			return coretools.RenderGroupedGlobMatchToolUse(inputs)
		}
	case "notebook_edit":
		var inputs []coretools.NotebookEditInput
		for _, item := range enriched {
			if typed, ok := item.(coretools.NotebookEditInput); ok {
				inputs = append(inputs, typed)
			}
		}
		if len(inputs) == len(enriched) {
			return coretools.RenderGroupedNotebookEditToolUse(inputs)
		}
	}
	if len(enriched) > 0 {
		return fmt.Sprint(enriched[0])
	}
	return toolName
}

func (b *TerminalRendererBackend) renderToolResult(event agent.StreamEvent) {
	toolName := stringFromAny(event.Metadata["tool_name"])
	result := stringFromAny(event.Metadata["result"])
	isError := truthy(event.Metadata["is_error"])
	if truthy(event.Metadata["denied"]) {
		fmt.Fprintln(b.out, "  Denied by user")
		return
	}
	rendered := ""
	switch toolName {
	case "read_file":
		rendered = coretools.RenderReadFileToolResult(result, isError)
	case "write_file":
		rendered = coretools.RenderWriteFileToolResult(result, isError)
	case "edit_file":
		rendered = coretools.RenderEditFileToolResult(result, isError)
	case "run_shell":
		rendered = coretools.RenderBashToolResult(result, isError)
	case "grep_search":
		rendered = coretools.RenderGrepSearchToolResult(result, isError)
	case "glob_match":
		rendered = coretools.RenderGlobMatchToolResult(result, isError)
	case "notebook_edit":
		rendered = coretools.RenderNotebookEditToolResult(result, isError)
	case "mcp_list_resources":
		rendered = coretools.RenderListMCPResourcesToolResult(result, isError)
	case "mcp_read_resource":
		rendered = coretools.RenderReadMCPResourceToolResult(result, isError)
	default:
		rendered = result
	}
	if rendered != "" {
		fmt.Fprintf(b.out, "[tool result] %s\n", rendered)
	}
}

func (b *TerminalRendererBackend) AskPermission(prompt any, dangerous bool) string {
	name, input := permissionPromptNameAndInput(prompt)
	if name == "mcp-project-trust" {
		target := stringFromAny(input["command"])
		fmt.Fprint(b.out, "\nPermission needed to trust project MCP servers")
		if target != "" {
			fmt.Fprintf(b.out, ": %s", target)
		}
		fmt.Fprint(b.out, ". Allow once? [y/N] ")
		return luminacli.NormalizePermissionAnswer(b.readLine())
	}
	if strings.HasPrefix(name, "skill-shell:") {
		skillName := strings.TrimPrefix(name, "skill-shell:")
		command := stringFromAny(input["command"])
		fmt.Fprint(b.out, "\nPermission needed for skill shell command")
		if skillName != "" {
			fmt.Fprintf(b.out, " in %s", skillName)
		}
		if command != "" {
			fmt.Fprintf(b.out, ": %s", command)
		}
		fmt.Fprint(b.out, ". Allow once? [y/N] ")
		return luminacli.NormalizePermissionAnswer(b.readLine())
	}
	if name == "" {
		name = "permission"
	}
	fmt.Fprintf(b.out, "\nPermission needed for %s", name)
	if dangerous {
		fmt.Fprint(b.out, " (risk: high)")
	}
	fmt.Fprint(b.out, ". Allow once? [y/N] ")
	return luminacli.NormalizePermissionAnswer(b.readLine())
}

func (b *TerminalRendererBackend) PickFromList(title string, values [][2]string) *string {
	if len(values) == 0 {
		return nil
	}
	fmt.Fprintf(b.out, "\n%s\n", title)
	for idx, value := range values {
		fmt.Fprintf(b.out, "  %d. %s\n", idx+1, value[1])
	}
	fmt.Fprint(b.out, "Select number (Enter to cancel): ")
	answer := strings.TrimSpace(b.readLine())
	if answer == "" {
		return nil
	}
	for idx, value := range values {
		if answer == fmt.Sprint(idx+1) {
			selected := value[0]
			return &selected
		}
	}
	return nil
}

func (b *TerminalRendererBackend) OutputWriter() io.Writer {
	return b.out
}

func (b *TerminalRendererBackend) renderTaskSnapshot(record map[string]any) {
	taskID := stringFromAny(record["task_id"])
	label := firstNonEmpty(stringFromAny(record["worker_label"]), taskID)
	status := firstNonEmpty(stringFromAny(record["status"]), "unknown")
	fmt.Fprintf(
		b.out,
		"  [task] %s status=%s tools=%d usage=%d/%d tok duration=%dms\n",
		label,
		status,
		intFromAny(record["tool_use_count"]),
		intFromAny(record["input_tokens"]),
		intFromAny(record["output_tokens"]),
		intFromAny(record["duration_ms"]),
	)
}

func (b *TerminalRendererBackend) readLine() string {
	line, err := b.in.ReadString('\n')
	if err != nil && len(line) == 0 {
		return ""
	}
	return strings.TrimSpace(line)
}

func permissionPromptNameAndInput(prompt any) (string, map[string]any) {
	switch value := prompt.(type) {
	case coretools.ToolCall:
		return value.Name, value.Input
	case *coretools.ToolCall:
		if value == nil {
			return "", nil
		}
		return value.Name, value.Input
	case map[string]any:
		name := stringFromAny(value["name"])
		input, _ := value["input"].(map[string]any)
		if input == nil {
			input = map[string]any{}
		}
		return name, input
	default:
		return fmt.Sprint(prompt), map[string]any{}
	}
}

type terminalReadlineCompleter struct {
	backend *TerminalRendererBackend
}

func (c terminalReadlineCompleter) Do(line []rune, pos int) ([][]rune, int) {
	if c.backend == nil {
		return nil, 0
	}
	if pos < 0 {
		pos = 0
	}
	if pos > len(line) {
		pos = len(line)
	}
	before := string(line[:pos])
	cwd := stringFromAny(c.backend.execCtx["cwd"])
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	completions := luminacli.CompleteInput(before, c.backend.skillRegistry, cwd)
	if len(completions) == 0 {
		return nil, 0
	}
	length := completionReplacementLength(before, completions[0])
	items := make([][]rune, 0, len(completions))
	for _, completion := range completions {
		items = append(items, []rune(completionSuffix(before, completion)))
	}
	return items, length
}

func completionReplacementLength(before string, completion luminacli.Completion) int {
	if completion.StartPosition < 0 {
		return -completion.StartPosition
	}
	return completion.StartPosition
}

func completionSuffix(before string, completion luminacli.Completion) string {
	length := completionReplacementLength(before, completion)
	if length <= 0 {
		return completion.Text
	}
	runes := []rune(before)
	if length > len(runes) {
		length = len(runes)
	}
	fragment := string(runes[len(runes)-length:])
	if strings.HasPrefix(completion.Text, fragment) {
		return strings.TrimPrefix(completion.Text, fragment)
	}
	return completion.Text
}
