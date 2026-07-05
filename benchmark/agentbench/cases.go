package agentbench

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func LoadCases(suite, casesPath string, limit int) ([]CaseSpec, error) {
	if suite == "" {
		return nil, errors.New("suite is required")
	}
	var cases []CaseSpec
	var err error
	if casesPath != "" {
		cases, err = loadCasesFromPath(casesPath)
	} else {
		cases, err = builtinCases(suite)
	}
	if err != nil {
		return nil, err
	}
	for i := range cases {
		if cases[i].Benchmark == "" {
			cases[i].Benchmark = suite
		}
		if cases[i].TimeoutSeconds <= 0 {
			cases[i].TimeoutSeconds = DefaultCaseTimeout
		}
		if err := validateCase(cases[i]); err != nil {
			return nil, fmt.Errorf("case %d: %w", i, err)
		}
	}
	if limit > 0 && limit < len(cases) {
		cases = cases[:limit]
	}
	return cases, nil
}

func loadCasesFromPath(path string) ([]CaseSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(filepath.Ext(path), ".jsonl") {
		var cases []CaseSpec
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var c CaseSpec
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				return nil, err
			}
			cases = append(cases, c)
		}
		return cases, scanner.Err()
	}
	var cases []CaseSpec
	if err := json.Unmarshal(data, &cases); err == nil {
		return cases, nil
	}
	var single CaseSpec
	if err := json.Unmarshal(data, &single); err != nil {
		return nil, err
	}
	return []CaseSpec{single}, nil
}

func builtinCases(suite string) ([]CaseSpec, error) {
	switch suite {
	case SuiteAiderPolyglotSmoke:
		return []CaseSpec{
			{
				ID:             "aider-polyglot-go-add",
				Benchmark:      suite,
				Prompt:         "Fix the failing Go test. Keep the implementation small, run the tests, and leave the repository in a passing state.",
				TestCommands:   []string{"go test ./..."},
				TimeoutSeconds: 900,
			},
			{
				ID:             "aider-polyglot-python-factorial",
				Benchmark:      suite,
				Prompt:         "Fix the failing Python unit test. Keep the implementation small, run the tests, and leave the repository in a passing state.",
				TestCommands:   []string{"python3 -m unittest"},
				TimeoutSeconds: 900,
			},
			{
				ID:             "aider-polyglot-js-title",
				Benchmark:      suite,
				Prompt:         "Fix the failing Node.js test. Keep the implementation small, run the tests, and leave the repository in a passing state.",
				TestCommands:   []string{"node title.test.js"},
				TimeoutSeconds: 900,
			},
		}, nil
	case SuiteTerminalBenchSmoke:
		return []CaseSpec{
			{
				ID:               "terminal-bench-create-artifact",
				Benchmark:        suite,
				Prompt:           "Create a file named result.txt containing exactly terminal-bench-ok, then verify it from the shell.",
				TestCommands:     []string{`test "$(cat result.txt)" = "terminal-bench-ok"`},
				ExpectedArtifact: "result.txt",
				TimeoutSeconds:   900,
			},
		}, nil
	case SuiteSWEBenchVerifiedSubset:
		return nil, errors.New("swebench_verified_subset requires -cases pointing to a local manifest in v1")
	default:
		return nil, fmt.Errorf("unknown suite %q", suite)
	}
}

func validateCase(c CaseSpec) error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("id is required")
	}
	if strings.TrimSpace(c.Benchmark) == "" {
		return errors.New("benchmark is required")
	}
	if strings.TrimSpace(c.Prompt) == "" {
		return errors.New("prompt is required")
	}
	if c.Repo != "" && c.WorkDir != "" {
		return errors.New("repo and workdir are mutually exclusive")
	}
	return nil
}
