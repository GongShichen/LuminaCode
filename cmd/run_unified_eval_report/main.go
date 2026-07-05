package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"LuminaCode/benchmark"
)

type tierFlags []string

func (t *tierFlags) String() string { return fmt.Sprint([]string(*t)) }

func (t *tierFlags) Set(value string) error {
	switch value {
	case "smoke", "core", "stress":
		*t = append(*t, value)
		return nil
	default:
		return fmt.Errorf("invalid tier %q", value)
	}
}

func main() {
	var tiers tierFlags
	outputDir := flag.String("output-dir", "docs/reports", "Directory for timestamped report files.")
	workDir := flag.String("work-dir", ".tmp/unified-eval-run", "Temporary working directory for benchmark materialization.")
	keep := flag.Int("keep", 4, "How many timestamped report files to retain.")
	baselineProfile := flag.String("baseline-profile", "", "Optional fixed baseline profile for candidate-vs-baseline comparison.")
	flag.Var(&tiers, "tier", "Optional benchmark tier filter. May be provided multiple times.")
	flag.Parse()

	var profile *string
	if *baselineProfile != "" {
		if _, err := benchmark.GetProfile(*baselineProfile); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		profile = baselineProfile
	}
	os.Exit(benchmark.RunUnifiedEvalReport(context.Background(), benchmark.UnifiedReportOptions{
		OutputDir:       *outputDir,
		WorkDir:         *workDir,
		Keep:            *keep,
		BaselineProfile: profile,
		Tiers:           []string(tiers),
	}))
}
