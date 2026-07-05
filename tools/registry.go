package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type ToolRegistry struct {
	tools      map[string]Tool
	order      []string
	deferred   *DeferredToolIndex
	aliases    map[string]string
	deprecated map[string]string
}

func NewToolRegistry(items ...Tool) *ToolRegistry {
	r := &ToolRegistry{
		tools:      map[string]Tool{},
		deferred:   NewDeferredToolIndex(),
		aliases:    map[string]string{},
		deprecated: map[string]string{},
	}
	for _, item := range items {
		r.Register(item)
	}
	return r
}

func (r *ToolRegistry) Register(tool Tool) {
	if tool == nil {
		return
	}
	if tool.ShouldDefer() {
		r.deferred.Add(tool)
		return
	}
	r.registerActive(tool, tool.Name())
}

func (r *ToolRegistry) Get(name string) Tool {
	if tool, ok := r.tools[name]; ok {
		return tool
	}
	if canonical, ok := r.aliases[name]; ok {
		return r.tools[canonical]
	}
	return nil
}

func (r *ToolRegistry) ResolveName(name string) (string, string) {
	if _, ok := r.tools[name]; ok {
		return name, ""
	}
	if canonical, ok := r.aliases[name]; ok {
		return canonical, r.deprecated[name]
	}
	return "", ""
}

func (r *ToolRegistry) EnrichForRender(call ToolCall, execCtx ExecutionContext) any {
	canonical, _ := r.ResolveName(call.Name)
	var tool Tool
	if canonical != "" {
		tool = r.tools[canonical]
	}
	if tool == nil {
		return fallbackInput{Raw: copyInputMap(call.Input)}
	}
	input, err := tool.DecodeInput(call.Input)
	if err != nil {
		return fallbackInput{Raw: copyInputMap(call.Input)}
	}
	backfiller, ok := tool.(ObservableInputBackfiller)
	if !ok {
		return input
	}
	enriched := backfiller.BackfillObservableInput(input, execCtx)
	if enriched == nil {
		return input
	}
	return enriched
}

func (r *ToolRegistry) ListTools() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, name := range r.order {
		if tool, ok := r.tools[name]; ok {
			out = append(out, tool)
		}
	}
	return out
}

type fallbackInput struct {
	Raw map[string]any
}

func (f fallbackInput) String() string {
	return fmt.Sprint(f.Raw)
}

func (r *ToolRegistry) FilteredCopy(allow, deny map[string]struct{}, readOnlyOnly, enabledOnly bool) *ToolRegistry {
	filtered := NewToolRegistry()
	for _, tool := range r.ListTools() {
		if enabledOnly && !tool.IsEnabled() {
			continue
		}
		if allow != nil {
			if _, ok := allow[tool.Name()]; !ok {
				continue
			}
		}
		if _, ok := deny[tool.Name()]; ok {
			continue
		}
		if readOnlyOnly && !tool.IsReadOnly(nil) {
			continue
		}
		filtered.Register(tool)
	}
	return filtered
}

func (r *ToolRegistry) GetDeferredTools() map[string]Tool {
	return r.deferred.GetAll()
}

func (r *ToolRegistry) GetDeferredToolNames() []string {
	return r.deferred.Names()
}

func (r *ToolRegistry) ActivateTool(name string) Tool {
	tool := r.deferred.Activate(name)
	if tool != nil {
		r.registerActive(tool, tool.Name())
	}
	return tool
}

func (r *ToolRegistry) SearchDeferred(query string) []Tool {
	return r.deferred.Search(query)
}

func (r *ToolRegistry) GetAPISchemas() []map[string]any {
	return r.GetAPISchemasFiltered(true, nil, false)
}

func (r *ToolRegistry) GetReadOnlyTools() []Tool {
	var out []Tool
	for _, tool := range r.ListTools() {
		if tool.IsReadOnly(nil) {
			out = append(out, tool)
		}
	}
	return out
}

func (r *ToolRegistry) GetAPISchemasFiltered(enabledOnly bool, deny map[string]struct{}, readOnlyOnly bool) []map[string]any {
	tools := r.ListTools()
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if enabledOnly && !tool.IsEnabled() {
			continue
		}
		if _, ok := deny[tool.Name()]; ok {
			continue
		}
		if readOnlyOnly && !tool.IsReadOnly(nil) {
			continue
		}
		out = append(out, tool.ToAPISchema())
	}
	return out
}

func (r *ToolRegistry) Execute(ctx context.Context, call ToolCall, execCtx ExecutionContext) ToolResult {
	canonical, warning := r.ResolveName(call.Name)
	tool := r.tools[canonical]
	if tool == nil {
		return ToolResult{
			ToolUseID: call.ID,
			Content: fmt.Sprintf(
				"<tool_use_error>\nUnknown tool: '%s'.\nAvailable tools: %s\nPlease use one of the available tools listed above.\n</tool_use_error>",
				call.Name,
				strings.Join(r.toolNames(), ", "),
			),
			IsError: true,
		}
	}

	input, err := tool.DecodeInput(call.Input)
	if err != nil {
		return ToolResult{
			ToolUseID: call.ID,
			Content: fmt.Sprintf(
				"<tool_use_error>\nInvalid input for tool '%s'.\nError: %s\nThe tool expects the following schema:\n  %v\nPlease correct the parameters and try again.\n</tool_use_error>",
				tool.Name(),
				err,
				tool.ToAPISchema()["input_schema"],
			),
			IsError: true,
		}
	}

	if ok, msg := tool.ValidateInput(execCtx, input); !ok {
		return ToolResult{
			ToolUseID: call.ID,
			Content: fmt.Sprintf(
				"<tool_use_error>\nValidation failed for '%s': %s\nPlease address the issue above and retry.\n</tool_use_error>",
				tool.Name(),
				msg,
			),
			IsError: true,
		}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, tool.Timeout())
	defer cancel()

	type execResult struct {
		output string
		err    error
	}
	done := make(chan execResult, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- execResult{err: fmt.Errorf("%v", recovered)}
			}
		}()
		output, err := tool.Execute(timeoutCtx, execCtx, input)
		done <- execResult{output: output, err: err}
	}()

	select {
	case <-timeoutCtx.Done():
		return ToolResult{
			ToolUseID: call.ID,
			Content: fmt.Sprintf(
				"<tool_use_error>\nTool '%s' timed out after %.0fs.\nThe operation took too long to complete. Consider:\n  - Breaking the task into smaller steps.\n  - Using a more targeted query or path.\n  - Increasing the timeout if this is expected to be slow.\nDo NOT retry the exact same call - it will time out again.\n</tool_use_error>",
				tool.Name(),
				tool.Timeout().Seconds(),
			),
			IsError: true,
		}
	case result := <-done:
		if result.err != nil {
			return ToolResult{
				ToolUseID: call.ID,
				Content: fmt.Sprintf(
					"<tool_use_error>\nUnexpected error executing '%s': %s\nDo NOT repeat the exact same call. Diagnose the error and try an alternative approach.\n</tool_use_error>",
					tool.Name(),
					result.err,
				),
				IsError: true,
			}
		}
		output := result.output
		if warning != "" {
			output = fmt.Sprintf("[Deprecation warning: %s]\n\n%s", warning, output)
		}
		return ToolResult{ToolUseID: call.ID, Content: output}
	}
}

func (r *ToolRegistry) registerActive(tool Tool, canonicalName string) {
	if _, exists := r.tools[canonicalName]; !exists {
		r.order = append(r.order, canonicalName)
	}
	r.tools[canonicalName] = tool
	for _, alias := range tool.Aliases() {
		r.aliases[alias] = canonicalName
	}
	for alias, warning := range tool.DeprecatedAliases() {
		r.aliases[alias] = canonicalName
		r.deprecated[alias] = warning
	}
}

func (r *ToolRegistry) toolNames() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func copyInputMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
