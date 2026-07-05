package agent

type StreamEvent struct {
	Type     string         `json:"type"`
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func NewStreamEvent(eventType, content string, metadata map[string]any) StreamEvent {
	if metadata == nil {
		metadata = map[string]any{}
	}
	return StreamEvent{Type: eventType, Content: content, Metadata: metadata}
}
