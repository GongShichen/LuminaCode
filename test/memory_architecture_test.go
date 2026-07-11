package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"LuminaCode/longmemory"
)

func TestMemoryRuntimeDoesNotDependOnBenchmarkPackages(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(current))
	for _, directory := range []string{"agent", "memory", "longmemory"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if strings.Contains(string(content), `LuminaCode/benchmark`) {
				t.Fatalf("memory runtime imports benchmark package: %s", path)
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func TestMemoryPublicQueryTypesContainNoEvaluationFields(t *testing.T) {
	payload, err := json.Marshal(struct {
		Query    longmemory.MemoryQuery
		Document longmemory.RetrievalDocument
	}{})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(payload))
	for _, forbidden := range []string{"benchmark", "question_type", "expected_answer", "gold", "has_answer"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("production memory type contains evaluation field %q: %s", forbidden, text)
		}
	}
}

func TestMemoryBenchmarkAdapterCannotWriteStorageTablesDirectly(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(current))
	err := filepath.WalkDir(filepath.Join(root, "benchmark"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		lower := strings.ToLower(string(content))
		for _, statement := range []string{"insert into memories", "insert into memory_facts", "insert into memory_entities", "insert into memory_session_index"} {
			if strings.Contains(lower, statement) {
				t.Fatalf("benchmark adapter writes memory storage directly: %s contains %q", path, statement)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
