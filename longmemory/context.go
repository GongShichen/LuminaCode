package longmemory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"LuminaCode/memory"
)

func BuildEvidenceContextMessage(packet EvidencePacket) map[string]any {
	if len(packet.Evidence) == 0 && len(packet.CoreBlocks) == 0 {
		return nil
	}
	parts := []string{
		"<system-reminder>",
		"Long-term evidence recalled for this task. Use the original excerpts, provenance, and timestamps below as point-in-time evidence.",
		"Distinguish user statements, assistant responses, tool observations, and derived facts. Compute from the evidence when needed, resolve updates by valid time and provenance, and express uncertainty only when the supplied evidence is genuinely insufficient. Recheck current files and external state before relying on mutable claims.",
	}
	var ids []string
	if len(packet.CoreBlocks) > 0 {
		parts = append(parts, "", "Core memory:")
		for _, block := range packet.CoreBlocks {
			parts = append(parts, fmt.Sprintf("- %s (%s/%s): %s", block.Label, block.ScopeType, block.ScopeKey, block.Content))
		}
	}
	for _, evidence := range packet.Evidence {
		ids = append(ids, evidence.MemoryID)
		parts = append(parts, "", formatEvidenceForContext(evidence))
	}
	if len(packet.Warnings) > 0 {
		parts = append(parts, "", "Retrieval warnings: "+strings.Join(packet.Warnings, "; "))
	}
	parts = append(parts, "</system-reminder>")
	msg := memory.BuildMetaUserMessage(strings.Join(parts, "\n"), memory.MemoryRecallSource)
	metadata, _ := msg["metadata"].(map[string]any)
	metadata["long_term_memory"] = true
	metadata["memory_ids"] = ids
	metadata["recall_ids"] = ids
	metadata["evidence_packet"] = true
	return msg
}

func ExportMarkdown(ctx context.Context, store *Store, dir string) (string, error) {
	if store == nil {
		return "", fmt.Errorf("memory store is nil")
	}
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join(filepath.Dir(store.Path()), "export")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	entries, err := store.List(ctx, SearchOptions{Limit: 100000, IncludeInactive: true})
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		name := safeExportName(entry)
		content := fmt.Sprintf(`---
memory_id: %s
scope_type: %s
scope_key: %s
memory_type: %s
status: %s
importance: %.3f
confidence: %.3f
source_session_id: %s
source_agent_id: %s
created_at: %s
updated_at: %s
tags: %s
entities: %s
---

# %s

%s
`, entry.MemoryID, entry.ScopeType, entry.ScopeKey, entry.MemoryType, entry.Status, entry.Importance, entry.Confidence,
			entry.SourceSessionID, entry.SourceAgentID, entry.CreatedAt.Format(time.RFC3339), entry.UpdatedAt.Format(time.RFC3339),
			strings.Join(entry.Tags, ", "), strings.Join(entry.Entities, ", "), entry.Title, entry.Content)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func formatEvidenceForContext(evidence Evidence) string {
	fields := []string{
		fmt.Sprintf("Evidence %s (%s/%s, %s, confidence %.2f, score %.3f)", evidence.MemoryID,
			evidence.ScopeType, evidence.ScopeKey, evidence.MemoryType, evidence.Confidence, evidence.Score),
		"Title: " + evidence.Title,
		"Excerpt: " + evidence.Text,
	}
	if role := strings.TrimSpace(fmt.Sprint(evidence.Metadata["role"])); role != "" && role != "<nil>" {
		fields = append(fields, "Provenance role: "+role)
	}
	if evidence.SourceSession != "" || len(evidence.SourceMessages) > 0 {
		fields = append(fields, fmt.Sprintf("Source: session=%s messages=%s", evidence.SourceSession, strings.Join(evidence.SourceMessages, ",")))
	}
	if len(evidence.DocumentIDs) > 1 {
		fields = append(fields, "Evidence chunks: "+strings.Join(evidence.DocumentIDs, ","))
	}
	if !evidence.ValidFrom.IsZero() || !evidence.ValidUntil.IsZero() {
		fields = append(fields, fmt.Sprintf("Valid time: %s to %s", formatContextTime(evidence.ValidFrom), formatContextTime(evidence.ValidUntil)))
	}
	return strings.Join(fields, "\n")
}

func formatContextTime(value time.Time) string {
	if value.IsZero() {
		return "open"
	}
	return value.UTC().Format(time.RFC3339)
}

func safeExportName(entry Entry) string {
	name := memory.SlugifyName(string(entry.ScopeType) + "-" + string(entry.MemoryType) + "-" + entry.Title)
	if name == "" {
		name = entry.MemoryID
	}
	return name + ".md"
}
