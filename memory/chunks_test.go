package memory

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPackChunkPartsFillsWindowsAcrossEventBoundaries(t *testing.T) {
	parts := make([]chunkPart, 10)
	for index := range parts {
		parts[index] = chunkPart{
			Text:       strings.Repeat(string(rune('a'+index)), 600),
			EventID:    fmt.Sprintf("event-%d", index),
			ContextID:  "context",
			SessionID:  "session",
			Actor:      "user",
			OccurredAt: time.Unix(int64(index), 0).UTC(),
		}
	}

	chunks := packChunkParts(parts, 1_000, 100)
	if len(chunks) != 7 {
		t.Fatalf("chunk count = %d, want 7 compact sliding windows", len(chunks))
	}
	seen := map[string]bool{}
	for index, chunk := range chunks {
		size := 0
		for _, part := range chunk {
			size += len([]rune(part.Text))
			seen[part.EventID] = true
		}
		if size == 0 || size > 1_000 {
			t.Fatalf("chunk %d size = %d, want 1..1000", index, size)
		}
	}
	if len(seen) != len(parts) {
		t.Fatalf("source mapping covered %d/%d events", len(seen), len(parts))
	}
}
