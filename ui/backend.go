package ui

import "sort"

type InputDraftBackend interface {
	SetInputDraft(string)
}

func SetBackendInputDraft(backend RendererBackend, draft string) bool {
	if setter, ok := backend.(InputDraftBackend); ok {
		setter.SetInputDraft(draft)
		return true
	}
	return false
}

type TaskSignature struct {
	Status       any
	InputTokens  int
	OutputTokens int
	ToolUseCount int
	DurationMS   int
}

type TerminalUIBackendMixin struct {
	LastFrame                  RenderFrame
	ActiveModalState           map[string]any
	LastRenderedTaskSignatures map[string]TaskSignature
	RenderTaskSnapshot         func(map[string]any)
}

func NewTerminalUIBackendMixin(renderTaskSnapshot func(map[string]any)) *TerminalUIBackendMixin {
	return &TerminalUIBackendMixin{
		LastRenderedTaskSignatures: map[string]TaskSignature{},
		RenderTaskSnapshot:         renderTaskSnapshot,
	}
}

func (b *TerminalUIBackendMixin) Mount(initialFrame RenderFrame) {
	b.LastFrame = initialFrame
	b.LastRenderedTaskSignatures = map[string]TaskSignature{}
}

func (b *TerminalUIBackendMixin) Update(frame RenderFrame) {
	b.renderFrameUpdates(frame)
	b.LastFrame = frame
}

func (b *TerminalUIBackendMixin) ShowModal(modalState map[string]any) {
	b.ActiveModalState = modalState
}

func (b *TerminalUIBackendMixin) ClearModal() {
	b.ActiveModalState = nil
}

func (b *TerminalUIBackendMixin) Shutdown(finalSnapshot RenderFrame) {
	b.LastFrame = finalSnapshot
}

func (b *TerminalUIBackendMixin) renderFrameUpdates(frame RenderFrame) {
	if len(frame.Tasks) == 0 {
		return
	}
	previous := b.LastRenderedTaskSignatures
	if previous == nil {
		previous = map[string]TaskSignature{}
	}
	current := map[string]TaskSignature{}
	taskIDs := make([]string, 0, len(frame.Tasks))
	for taskID := range frame.Tasks {
		taskIDs = append(taskIDs, taskID)
	}
	sort.Strings(taskIDs)
	for _, taskID := range taskIDs {
		record := frame.Tasks[taskID]
		signature := taskSignature(record)
		current[taskID] = signature
		if previous[taskID] == signature {
			continue
		}
		if b.RenderTaskSnapshot != nil {
			b.RenderTaskSnapshot(record)
		}
	}
	b.LastRenderedTaskSignatures = current
}

func taskSignature(record map[string]any) TaskSignature {
	return TaskSignature{
		Status:       record["status"],
		InputTokens:  intFromAny(record["input_tokens"]),
		OutputTokens: intFromAny(record["output_tokens"]),
		ToolUseCount: intFromAny(record["tool_use_count"]),
		DurationMS:   intFromAny(record["duration_ms"]),
	}
}
