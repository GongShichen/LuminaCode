package test

import (
	"strings"
	"testing"

	adapter "LuminaCode/benchmark/longmemevalv2"
)

func TestLongMemEvalV2TrajectoryUsesVisibleEvidenceOnly(t *testing.T) {
	trajectory := map[string]any{
		"goal":      "Open the incident and verify its state",
		"start_url": "https://example.test/incidents",
		"states": []any{map[string]any{
			"url":         "https://example.test/incidents/42",
			"action":      "click incident 42",
			"observation": "Priority is P1 and state is In Progress",
			"thought":     "private chain of thought",
			"screenshot":  "screenshots/42/1.png",
		}},
		"outcome": map[string]any{"reward": 1, "response": "verified"},
	}
	messages := adapter.MessagesFromTrajectory(trajectory)
	if len(messages) != 4 {
		t.Fatalf("expected goal, initial URL, state, and outcome messages; got %d", len(messages))
	}
	joined := strings.ToLower(messageText(messages))
	for _, expected := range []string{"incident 42", "priority is p1", "in progress", "screenshots/42/1.png", "verified"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing visible evidence %q in %s", expected, joined)
		}
	}
	if strings.Contains(joined, "private chain of thought") {
		t.Fatalf("hidden reasoning leaked into ingestion: %s", joined)
	}
	if role, _ := messages[2]["role"].(string); role != "assistant" {
		t.Fatalf("visible trajectory state must enter the production evidence index, got role %q", role)
	}
}

func messageText(messages []map[string]any) string {
	var parts []string
	for _, message := range messages {
		blocks, _ := message["content"].([]map[string]any)
		for _, block := range blocks {
			if text, _ := block["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}
