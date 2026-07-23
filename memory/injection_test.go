package memory

import "testing"

func TestRecalledMemoryIDsAcceptsFabricResourceIDs(t *testing.T) {
	message := BuildRecalledMemoriesMessage([]MemoryRecall{{
		Content: "evidence", RecallIDs: []string{"evt_source", "node_claim", "conflict-set"},
	}})
	got := RecalledMemoryIDs([]map[string]any{message})
	for _, id := range []string{"evt_source", "node_claim", "conflict-set"} {
		if _, ok := got[id]; !ok {
			t.Fatalf("missing Fabric recall id %q in %#v", id, got)
		}
	}
}
