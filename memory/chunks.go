package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	semanticChunkTokens   = 768
	semanticChunkOverlap  = 96
	semanticRunesPerToken = 3
)

type chunkPart struct {
	Text       string
	EventID    string
	ContextID  string
	SessionID  string
	Actor      string
	OccurredAt time.Time
}

func (f *Fabric) projectSessionChunks(ctx context.Context, eventIDs []string) error {
	if f.options.Vectorizer == nil || len(eventIDs) == 0 {
		return nil
	}
	events, err := f.loadEvents(ctx, eventIDs)
	if err != nil {
		return err
	}
	groups := map[string][]RawEvent{}
	for _, event := range events {
		groups[sessionKey(event)] = append(groups[sessionKey(event)], event)
	}
	var ledgerSeq int64
	if err := f.ledger.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM outbox`).Scan(&ledgerSeq); err != nil {
		return err
	}
	contextIDs := make([]string, 0, len(groups))
	documents := make([]indexedDocument, 0)
	for session, values := range groups {
		sort.SliceStable(values, func(i, j int) bool {
			if values[i].OccurredAt.Equal(values[j].OccurredAt) {
				return values[i].ID < values[j].ID
			}
			return values[i].OccurredAt.Before(values[j].OccurredAt)
		})
		contextIDs = append(contextIDs, values[0].ContextID)
		documents = append(documents, sessionChunkDocuments(session, values, ledgerSeq)...)
	}
	newIDs := make(map[string]struct{}, len(documents))
	for _, document := range documents {
		newIDs[document.ID] = struct{}{}
	}
	stale, err := f.staleSessionChunkIDs(ctx, events[0].Space, uniqueStrings(contextIDs), newIDs)
	if err != nil {
		return err
	}
	if err := f.deleteIndexedResources(ctx, stale, ledgerSeq); err != nil {
		return err
	}
	return f.upsertIndexDocuments(ctx, documents)
}

func (f *Fabric) replaceSessionChunks(ctx context.Context, session string, events []RawEvent, ledgerSeq int64) error {
	if len(events) == 0 {
		return nil
	}
	documents := sessionChunkDocuments(session, events, ledgerSeq)
	newIDs := make(map[string]struct{}, len(documents))
	for _, document := range documents {
		newIDs[document.ID] = struct{}{}
	}
	stale, err := f.staleSessionChunkIDs(ctx, events[0].Space, []string{events[0].ContextID}, newIDs)
	if err != nil {
		return err
	}
	if err := f.deleteIndexedResources(ctx, stale, ledgerSeq); err != nil {
		return err
	}
	return f.upsertIndexDocuments(ctx, documents)
}

func (f *Fabric) staleSessionChunkIDs(ctx context.Context, space string, contextIDs []string,
	keep map[string]struct{}) ([]string, error) {
	if len(contextIDs) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(contextIDs)+1)
	args = append(args, space)
	for _, id := range contextIDs {
		args = append(args, id)
	}
	rows, err := f.index.QueryContext(ctx, `SELECT doc_id FROM documents WHERE space=? AND resource_kind='chunk'
		AND context_id IN (`+placeholders(len(contextIDs))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if _, exists := keep[id]; !exists {
			stale = append(stale, id)
		}
	}
	return stale, rows.Err()
}

func sessionChunkDocuments(session string, events []RawEvent, ledgerSeq int64) []indexedDocument {
	parts := chunkPartsForEvents(events)
	chunks := packChunkParts(parts, semanticChunkTokens*semanticRunesPerToken,
		semanticChunkOverlap*semanticRunesPerToken)
	documents := make([]indexedDocument, 0, len(chunks))
	for index, chunk := range chunks {
		var texts, sourceIDs, actors []string
		occurredAt := chunk[0].OccurredAt
		for _, part := range chunk {
			texts = append(texts, part.Text)
			sourceIDs = append(sourceIDs, part.EventID)
			actors = append(actors, part.Actor)
			if part.OccurredAt.Before(occurredAt) {
				occurredAt = part.OccurredAt
			}
		}
		content := strings.TrimSpace(strings.Join(texts, "\n"))
		docID := stableFabricID("chunk", events[0].Space, session, fmt.Sprintf("%d", index), content)
		documents = append(documents, indexedDocument{
			ID: docID, Space: events[0].Space, ResourceKind: "chunk", ResourceID: docID,
			Content: content, ContextID: events[0].ContextID, OccurredAt: occurredAt,
			Status: SemanticRawOnly, SourceEventIDs: uniqueStrings(sourceIDs), LedgerSeq: ledgerSeq,
			Keys: normalizeStringList(actors, 8), IndexVector: true, VectorText: content,
			Metadata: map[string]any{"session_id": session, "chunk_index": index},
		})
	}
	return documents
}

func chunkPartsForEvents(events []RawEvent) []chunkPart {
	parts := make([]chunkPart, 0, len(events))
	for _, event := range events {
		prefix := strings.TrimSpace(event.Actor) + ": "
		content := strings.TrimSpace(event.Content)
		if content == "" {
			continue
		}
		parts = append(parts, chunkPart{Text: prefix + content + "\n", EventID: event.ID,
			ContextID: event.ContextID, SessionID: event.SessionID, Actor: event.Actor, OccurredAt: event.OccurredAt})
	}
	return parts
}

func packChunkParts(parts []chunkPart, maxRunes, overlapRunes int) [][]chunkPart {
	if len(parts) == 0 || maxRunes <= 0 {
		return nil
	}
	overlapRunes = minIntMemory(maxIntMemory(0, overlapRunes), maxRunes/2)
	type locatedPart struct {
		part       chunkPart
		runes      []rune
		start, end int
	}
	located := make([]locatedPart, 0, len(parts))
	total := 0
	for _, part := range parts {
		runes := []rune(part.Text)
		if len(runes) == 0 {
			continue
		}
		located = append(located, locatedPart{part: part, runes: runes, start: total, end: total + len(runes)})
		total += len(runes)
	}
	step := maxRunes - overlapRunes
	if step < 1 {
		step = maxRunes
	}
	chunks := make([][]chunkPart, 0, (total+step-1)/step)
	for windowStart := 0; windowStart < total; windowStart += step {
		windowEnd := minIntMemory(total, windowStart+maxRunes)
		chunk := make([]chunkPart, 0)
		for _, item := range located {
			if item.end <= windowStart || item.start >= windowEnd {
				continue
			}
			start := maxIntMemory(windowStart, item.start) - item.start
			end := minIntMemory(windowEnd, item.end) - item.start
			fragment := item.part
			fragment.Text = string(item.runes[start:end])
			chunk = append(chunk, fragment)
		}
		if len(chunk) > 0 {
			chunks = append(chunks, chunk)
		}
		if windowEnd == total {
			break
		}
	}
	return chunks
}
