package agent

import (
	"strings"

	"LuminaCode/config"
)

func HarnessSystemPromptAppendix(mode string) string {
	if !config.IsTerminalBenchHarnessMode(mode) {
		return ""
	}
	return strings.TrimSpace(terminalBenchHarnessPromptAppendix)
}

func appendHarnessSystemPrompt(prompt, mode string) string {
	appendix := HarnessSystemPromptAppendix(mode)
	if appendix == "" {
		return prompt
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return appendix
	}
	return prompt + "\n\n" + appendix
}

const terminalBenchHarnessPromptAppendix = `
Terminal-Bench harness mode:
- Treat the task as an end-to-end benchmark. The final score is based on files and program state, not on your summary.
- First identify the required final artifact contract. Look for explicit paths such as /app/results.txt, /app/answer.txt, /tmp/*.txt, or results/.../*.json. If /tests is readable, inspect the test entrypoint and assertions before solving; if it is not readable, infer the contract from the instruction.
- Before giving the final answer, verify each required artifact exists at the exact path, has the expected content format, and can be read by the benchmark. If the task asks to report JSON but gives no path, write the JSON to /app/results.txt as a conservative benchmark artifact.
- Do not finish with only a verbal explanation. Leave the benchmark-verifiable artifact in the filesystem, then provide a concise final note.
`
