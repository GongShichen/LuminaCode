package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"LuminaCode/longmemory"
	coretools "LuminaCode/tools"
)

type extractMemoryCandidateInput struct {
	Action           string                `json:"action" jsonschema:"required,enum=create,enum=update,enum=supersede,enum=ignore"`
	TargetMemoryID   string                `json:"target_memory_id,omitempty"`
	MemoryID         string                `json:"memory_id,omitempty"`
	ScopeType        longmemory.ScopeType  `json:"scope_type" jsonschema:"required,enum=user,enum=project,enum=team,enum=agent_type,enum=team_agent"`
	ScopeKey         string                `json:"scope_key,omitempty"`
	MemoryType       longmemory.MemoryType `json:"memory_type" jsonschema:"required,enum=semantic,enum=episodic,enum=procedural,enum=preference,enum=feedback,enum=project,enum=reference"`
	Title            string                `json:"title" jsonschema:"required"`
	Summary          string                `json:"summary" jsonschema:"required"`
	Content          string                `json:"content" jsonschema:"required"`
	Tags             []string              `json:"tags"`
	Entities         []string              `json:"entities"`
	Importance       float64               `json:"importance"`
	Confidence       float64               `json:"confidence"`
	EpistemicStatus  string                `json:"epistemic_status,omitempty" jsonschema:"enum=reported,enum=observed,enum=derived,enum=suggested,enum=hypothetical,enum=questioned"`
	SourceMessageIDs []string              `json:"source_message_ids" jsonschema:"required,minItems=1"`
	SourcePaths      []string              `json:"source_paths,omitempty"`
}

type extractMemoryFactInput struct {
	MemoryIndex int            `json:"memory_index" jsonschema:"required"`
	Subject     string         `json:"subject" jsonschema:"required"`
	Predicate   string         `json:"predicate" jsonschema:"required"`
	Object      string         `json:"object" jsonschema:"required"`
	Qualifiers  map[string]any `json:"qualifiers,omitempty"`
	Confidence  float64        `json:"confidence"`
	ValidFrom   string         `json:"valid_from,omitempty"`
	ValidUntil  string         `json:"valid_until,omitempty"`
}

type extractMemoryEdgeInput struct {
	FromMemoryIndex int                 `json:"from_memory_index" jsonschema:"required"`
	ToMemoryIndex   int                 `json:"to_memory_index" jsonschema:"required"`
	EdgeType        longmemory.EdgeType `json:"edge_type" jsonschema:"required,enum=related_to,enum=supports,enum=contradicts,enum=derived_from,enum=next_event"`
	Weight          float64             `json:"weight"`
	Confidence      float64             `json:"confidence"`
}

type extractMemoryCoreBlockInput struct {
	ScopeType   longmemory.ScopeType `json:"scope_type" jsonschema:"required,enum=user,enum=project,enum=team,enum=agent_type,enum=team_agent"`
	ScopeKey    string               `json:"scope_key,omitempty"`
	Label       string               `json:"label" jsonschema:"required"`
	Description string               `json:"description,omitempty"`
	Content     string               `json:"content" jsonschema:"required"`
}

type extractMemoryBatchInput struct {
	Memories   []extractMemoryCandidateInput `json:"memories"`
	Facts      []extractMemoryFactInput      `json:"facts,omitempty"`
	Edges      []extractMemoryEdgeInput      `json:"edges,omitempty"`
	CoreBlocks []extractMemoryCoreBlockInput `json:"core_blocks,omitempty"`
}

type extractMemoryBatchTool struct {
	coretools.BaseTool
	mu       sync.Mutex
	captured *extractMemoryBatchInput
}

func newExtractMemoryBatchTool() *extractMemoryBatchTool {
	return &extractMemoryBatchTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:            "ExtractMemoryBatch",
		Description:     "Submit durable cross-session memories with exact source message IDs.",
		InputPrototype:  extractMemoryBatchInput{},
		ReadOnly:        coretools.BoolPtr(true),
		ConcurrencySafe: coretools.BoolPtr(false),
		Destructive:     coretools.BoolPtr(false),
		MaxOutputChars:  2000,
	}}}
}

func (t *extractMemoryBatchTool) Execute(_ context.Context, _ coretools.ExecutionContext, input any) (string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	var batch extractMemoryBatchInput
	if err := json.Unmarshal(encoded, &batch); err != nil {
		return "", err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.captured != nil {
		return "", errors.New("ExtractMemoryBatch may only be called once")
	}
	t.captured = &batch
	return "Memory batch accepted.", nil
}

func (t *extractMemoryBatchTool) batchJSON() (string, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.captured == nil {
		return "", false, nil
	}
	batch := longmemory.ExtractionBatch{}
	for _, input := range t.captured.Memories {
		batch.Memories = append(batch.Memories, longmemory.Candidate{
			Action: input.Action, TargetMemoryID: input.TargetMemoryID, MemoryID: input.MemoryID,
			ScopeType: input.ScopeType, ScopeKey: input.ScopeKey, MemoryType: input.MemoryType,
			Title: input.Title, Summary: input.Summary, Content: input.Content,
			Tags: append([]string(nil), input.Tags...), Entities: append([]string(nil), input.Entities...),
			Importance: input.Importance, Confidence: input.Confidence,
			EpistemicStatus:  input.EpistemicStatus,
			SourceMessageIDs: append([]string(nil), input.SourceMessageIDs...), SourcePaths: append([]string(nil), input.SourcePaths...),
		})
	}
	for _, input := range t.captured.Facts {
		batch.Facts = append(batch.Facts, longmemory.Fact{MemoryIndex: input.MemoryIndex,
			Subject: input.Subject, Predicate: input.Predicate, Object: input.Object,
			Qualifiers: input.Qualifiers, Confidence: input.Confidence,
			ValidFrom: parseExtractionToolTime(input.ValidFrom), ValidUntil: parseExtractionToolTime(input.ValidUntil)})
	}
	for _, input := range t.captured.Edges {
		batch.Edges = append(batch.Edges, longmemory.Edge{FromMemoryIndex: input.FromMemoryIndex,
			ToMemoryIndex: input.ToMemoryIndex, Type: input.EdgeType, Weight: input.Weight, Confidence: input.Confidence})
	}
	for _, input := range t.captured.CoreBlocks {
		batch.CoreBlocks = append(batch.CoreBlocks, longmemory.CoreBlock{ScopeType: input.ScopeType,
			ScopeKey: input.ScopeKey, Label: input.Label, Description: input.Description, Content: input.Content})
	}
	encoded, err := json.Marshal(batch)
	return string(encoded), true, err
}

func parseExtractionToolTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}
