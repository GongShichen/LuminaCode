package main

import (
	"flag"
	"fmt"
	"os"

	"LuminaCode/memory/localmodel"
)

func runModelsCLI(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lumina-backend models <prepare-bge-heads|verify-bge-heads|probe-bge>")
	}
	flags := flag.NewFlagSet("models "+args[0], flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	modelDir := flags.String("model-dir", "", "BGE-M3 model directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *modelDir == "" {
		return fmt.Errorf("--model-dir is required")
	}
	switch args[0] {
	case "prepare-bge-heads":
		return localmodel.PrepareBGEHeads(*modelDir)
	case "verify-bge-heads":
		return localmodel.VerifyBGEHeads(*modelDir)
	case "probe-bge":
		return localmodel.ProbeBGE(*modelDir)
	default:
		return fmt.Errorf("unknown models command: %s", args[0])
	}
}
