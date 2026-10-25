package agentContext

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// Message is the provider-independent, durable identity of a conversation item.
// Fields retain unknown provider data losslessly; provider response IDs are not
// used as message identities because streamed responses can share an ID.
type Message struct {
	ID      string                     `json:"id"`
	Seq     int64                      `json:"seq,omitempty"`
	Role    string                     `json:"role"`
	Source  string                     `json:"source,omitempty"`
	Content []ContentBlock             `json:"content,omitempty"`
	Fields  map[string]json.RawMessage `json:"fields"`
}

type ContentBlock struct {
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	ToolID string          `json:"tool_id,omitempty"`
	Name   string          `json:"name,omitempty"`
	Raw    json.RawMessage `json:"raw"`
	Input  json.RawMessage `json:"input,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

func MessageFromMap(namespace string, index int, value map[string]any) Message {
	raw, _ := json.Marshal(value)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	m := Message{Fields: fields}
	_ = json.Unmarshal(fields["role"], &m.Role)
	_ = json.Unmarshal(fields["context_id"], &m.ID)
	_ = json.Unmarshal(fields["event_seq"], &m.Seq)
	var metadata struct {
		Source string `json:"source"`
	}
	_ = json.Unmarshal(fields["metadata"], &metadata)
	m.Source = metadata.Source
	if m.ID == "" {
		digest := sha256.Sum256(raw)
		m.ID = fmt.Sprintf("%s:%d:%x", namespace, index, digest[:8])
	}
	var blocks []json.RawMessage
	if json.Unmarshal(fields["content"], &blocks) == nil && blocks != nil {
		m.Content = make([]ContentBlock, 0, len(blocks))
		delete(m.Fields, "content")
		for _, block := range blocks {
			var data struct {
				Type, Text, Name string
				ID               string `json:"id"`
				ToolID           string `json:"tool_use_id"`
			}
			_ = json.Unmarshal(block, &data)
			id := data.ToolID
			if id == "" {
				id = data.ID
			}
			var extras map[string]json.RawMessage
			_ = json.Unmarshal(block, &extras)
			input, result := extras["input"], extras["content"]
			delete(extras, "type")
			delete(extras, "input")
			delete(extras, "content")
			if data.Type == "text" {
				delete(extras, "text")
			}
			if data.Type == "tool_use" {
				delete(extras, "id")
				delete(extras, "name")
			}
			if data.Type == "tool_result" {
				delete(extras, "tool_use_id")
			}
			extraRaw, _ := json.Marshal(extras)
			m.Content = append(m.Content, ContentBlock{Type: data.Type, Text: data.Text, ToolID: id, Name: data.Name, Raw: extraRaw, Input: input, Result: result})
		}
	}
	delete(m.Fields, "role")
	delete(m.Fields, "context_id")
	delete(m.Fields, "event_seq")
	return m
}

func (m Message) Map() map[string]any {
	raw, _ := json.Marshal(m.Fields)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var result map[string]any
	_ = decoder.Decode(&result)
	if result == nil {
		result = map[string]any{}
	}
	result["role"] = m.Role
	if m.Content != nil {
		blocks := make([]map[string]any, 0, len(m.Content))
		for _, block := range m.Content {
			blocks = append(blocks, block.Map())
		}
		result["content"] = blocks
	}
	if m.Source != "" {
		metadata, _ := result["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["source"] = m.Source
		result["metadata"] = metadata
	}
	result["context_id"] = m.ID
	if m.Seq != 0 {
		result["event_seq"] = m.Seq
	}
	// Preserve the native block shape used by the existing tool adapters.
	if blocks, ok := contentBlocks(result["content"]); ok {
		result["content"] = blocks
	}
	return result
}

func MessagesFromMaps(namespace string, values []map[string]any) []Message {
	result := make([]Message, 0, len(values))
	for i, value := range values {
		result = append(result, MessageFromMap(namespace, i, value))
	}
	return result
}

func MessageMaps(values []Message) []map[string]any {
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		result = append(result, value.Map())
	}
	return result
}

func CloneMessages(values []map[string]any) []map[string]any {
	return MessageMaps(MessagesFromMaps("message", values))
}

// SafeTailStart expands a retained tail until every tool result retains its
// originating call. It never fabricates a call merely to fit a compression cut.
func SafeTailStart(messages []map[string]any, keep int) int {
	start := max(0, len(messages)-keep)
	for {
		next := start
		needed := map[string]bool{}
		for _, msg := range messages[start:] {
			blocks, _ := contentBlocks(msg["content"])
			for _, b := range blocks {
				if b["type"] == "tool_result" {
					if id, ok := b["tool_use_id"].(string); ok {
						needed[id] = true
					}
				}
			}
		}
		for i := 0; i < start; i++ {
			blocks, _ := contentBlocks(messages[i]["content"])
			for _, b := range blocks {
				if b["type"] == "tool_use" {
					if id, ok := b["id"].(string); ok && needed[id] {
						next = min(next, i)
					}
				}
			}
		}
		if next == start {
			return start
		}
		start = next
	}
}

func (b ContentBlock) Map() map[string]any {
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(b.Raw))
	decoder.UseNumber()
	_ = decoder.Decode(&value)
	if value == nil {
		value = map[string]any{}
	}
	value["type"] = b.Type
	if b.Type == "text" {
		value["text"] = b.Text
	}
	if b.Type == "tool_use" {
		value["id"] = b.ToolID
		value["name"] = b.Name
	}
	if b.Type == "tool_result" {
		value["tool_use_id"] = b.ToolID
	}
	if b.Input != nil {
		var input any
		d := json.NewDecoder(bytes.NewReader(b.Input))
		d.UseNumber()
		if d.Decode(&input) == nil {
			value["input"] = input
		}
	}
	if b.Result != nil {
		var result any
		d := json.NewDecoder(bytes.NewReader(b.Result))
		d.UseNumber()
		if d.Decode(&result) == nil {
			value["content"] = result
		}
	}
	return value
}
