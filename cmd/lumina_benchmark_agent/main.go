package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"LuminaCode/benchmark/agentbench"
	"LuminaCode/config"
)

func main() {
	prompt := flag.String("prompt", "", "Prompt text. If empty, stdin is used.")
	promptFile := flag.String("prompt-file", "", "Prompt file path. Takes precedence over -prompt.")
	cwd := flag.String("cwd", "", "Agent working directory.")
	transcriptPath := flag.String("transcript", "", "Optional JSONL transcript output path.")
	timelinePath := flag.String("timeline", "", "Optional timeline JSON output path.")
	resultPath := flag.String("result", "", "Optional structured result JSON output path.")
	finalPath := flag.String("final", "", "Optional final text output path.")
	sessionID := flag.String("session", "benchmark-agent", "Session id.")
	flag.Parse()

	userPrompt, err := resolvePrompt(*prompt, *promptFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cfg := config.NewConfig()
	cfg.Yolo = true
	cfg.AutoMemoryEnabled = false
	cfg.AutoMemoryDirectory = nil
	if *cwd != "" {
		abs, err := filepath.Abs(*cwd)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		cfg.CWD = abs
	}
	if cfg.APIKey == "" || cfg.APIBaseURL == "" || cfg.APIModel == "" {
		fmt.Fprintln(os.Stderr, "agent benchmark runner requires API key, base URL, and model configuration")
		os.Exit(2)
	}
	result := agentbench.HeadlessAgentRunner{}.Run(context.Background(), cfg, userPrompt, *sessionID)
	if *transcriptPath != "" {
		if err := writeJSONL(*transcriptPath, result.Events); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if *timelinePath != "" {
		if err := writeJSON(*timelinePath, result.Timeline); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if *resultPath != "" {
		if err := writeJSON(*resultPath, result); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if *finalPath != "" {
		if err := writeFile(*finalPath, []byte(result.FinalText)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	fmt.Print(result.FinalText)
	if result.ErrorType != "" {
		fmt.Fprintln(os.Stderr, result.ErrorType)
		os.Exit(1)
	}
}

func resolvePrompt(prompt, promptFile string) (string, error) {
	if strings.TrimSpace(promptFile) != "" {
		data, err := os.ReadFile(promptFile)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	if prompt != "" {
		return prompt, nil
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(data, '\n'))
}

func writeJSONL(path string, values any) error {
	rv := reflect.ValueOf(values)
	if rv.Kind() == reflect.Slice {
		var out []byte
		for i := 0; i < rv.Len(); i++ {
			line, err := json.Marshal(rv.Index(i).Interface())
			if err != nil {
				return err
			}
			out = append(out, line...)
			out = append(out, '\n')
		}
		return writeFile(path, out)
	}
	data, err := json.Marshal(values)
	if err != nil {
		return err
	}
	return writeFile(path, append(data, '\n'))
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
