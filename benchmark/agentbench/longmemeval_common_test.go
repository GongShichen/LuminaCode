package agentbench

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOnlyLongMemEvalUsesMemoryBenchmarkOrchestration(t *testing.T) {
	if !isMemoryBenchmarkSuite(SuiteLongMemEval) {
		t.Fatal("LongMemEval was not routed to its phased orchestrator")
	}
	if isMemoryBenchmarkSuite("memoryarena") {
		t.Fatal("retired MemoryArena implementation is still routed")
	}
}

func TestCompletedLongMemEvalCheckpointRequiresUsableAnswer(t *testing.T) {
	complete := CaseResult{Case: CaseSpec{ID: "case"}, Hypothesis: "answer"}
	if !completedLongMemEvalCheckpoint(complete) {
		t.Fatal("usable checkpoint was not accepted")
	}
	complete.ErrorType = "answer_failed"
	if completedLongMemEvalCheckpoint(complete) {
		t.Fatal("failed checkpoint was accepted")
	}
	complete.ErrorType, complete.Hypothesis = "", ""
	if completedLongMemEvalCheckpoint(complete) {
		t.Fatal("empty checkpoint answer was accepted")
	}
}

func TestNormalizeAnswerTreatsOrdinalSuffixAsPresentation(t *testing.T) {
	if !answerContainsExpected("February 14 (Valentine's Day).", "February 14th") {
		t.Fatal("equivalent calendar date failed local answer proxy normalization")
	}
}

func TestValidateLongMemEvalPredictionsRejectsIncompleteAndDuplicateResults(t *testing.T) {
	results := []CaseResult{
		{Case: CaseSpec{ID: "ok"}, Hypothesis: "answer"},
		{Case: CaseSpec{ID: "empty"}, Hypothesis: " "},
		{Case: CaseSpec{ID: "failed"}, Hypothesis: "answer", ErrorType: "answer_failed"},
		{Case: CaseSpec{ID: "ok"}, Hypothesis: "duplicate"},
	}
	err := validateLongMemEvalPredictions(results)
	if err == nil {
		t.Fatal("invalid LongMemEval predictions were accepted")
	}
	for _, want := range []string{"duplicate_id=1", "empty_answer=1", "error_result=1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestLongMemEvalEvaluatorScriptAddsConfiguredModels(t *testing.T) {
	dir := t.TempDir()
	evaluator := filepath.Join(dir, "evaluate_qa.py")
	source := `model_zoo = {
    'deepseek-v4-pro': ('deepseek-v4-pro', 'deepseek'),
}

@backoff.on_exception(backoff.expo, (openai.RateLimitError,
                                    openai.APIError))
def chat_completions_with_backoff(client, **kwargs):
    return client.chat.completions.create(**kwargs)

if __name__ == '__main__':
    metric_client = OpenAI(
        api_key=openai_api_key,
        base_url=openai_api_base,
    )
    with open(result_file, 'w') as out_f:
        logs = []
        for entry in tqdm(hypotheses):
            completion = chat_completions_with_backoff(metric_client, **kwargs)
            print(json.dumps(entry), file=out_f)
`
	if err := os.WriteFile(evaluator, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := longMemEvalEvaluatorScript(evaluator, filepath.Join(dir, "predictions.jsonl"),
		"mimo-v2.5-pro", "deepseek-v4-pro[1M]")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(script)
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`model_zoo["mimo-v2.5-pro"] = ("mimo-v2.5-pro", 'deepseek')`,
		`model_zoo["deepseek-v4-pro[1M]"] = ("deepseek-v4-pro[1M]", 'deepseek')`,
		"for seconds in (10, 30, 60)",
		"max_tries=4",
		"giveup=_lumina_evaluator_giveup",
		"timeout=60.0",
		"max_retries=0",
		"_existing_by_id = {}",
		"out_f.flush()",
		longMemEvalEvaluatorUsagePrefix,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("patched evaluator missing %q:\n%s", want, body)
		}
	}
	if python, err := exec.LookPath("python3"); err == nil {
		if output, err := exec.Command(python, "-m", "py_compile", script).CombinedOutput(); err != nil {
			t.Fatalf("patched evaluator is not valid Python: %v\n%s", err, output)
		}
	}
}

func TestLongMemEvalEvaluatorUsageMarkersAreParsedAndHidden(t *testing.T) {
	output := "before\n\r 50%|progress|" + longMemEvalEvaluatorUsagePrefix +
		`{"case_id":"q1","model":"judge","input_tokens":12,"cache_read_input_tokens":5,"output_tokens":2}` +
		"\nafter\n"
	usage := parseLongMemEvalEvaluatorUsage(output)
	if len(usage) != 1 || usage[0].CaseID != "q1" || usage[0].InputTokens != 12 ||
		usage[0].CacheReadInputTokens != 5 || usage[0].OutputTokens != 2 {
		t.Fatalf("unexpected evaluator usage: %+v", usage)
	}
	clean := stripLongMemEvalEvaluatorUsage(output)
	if strings.Contains(clean, longMemEvalEvaluatorUsagePrefix) || !strings.Contains(clean, "before\n\r 50%|progress|\nafter") {
		t.Fatalf("usage marker was not cleanly stripped: %q", clean)
	}
}

func TestNormalizeOpenAIBaseURL(t *testing.T) {
	for input, want := range map[string]string{
		"https://api.deepseek.com/anthropic/":                  "https://api.deepseek.com/v1",
		"https://example.test/v1/chat/completions":             "https://example.test/v1",
		"https://example.test/anthropic/messages":              "https://example.test/v1",
		"https://token-plan-cn.xiaomimimo.com/anthropic":       "https://token-plan-cn.xiaomimimo.com/v1",
		" https://token-plan-cn.xiaomimimo.com/v1/ ":           "https://token-plan-cn.xiaomimimo.com/v1",
		" https://already-openai.example.test/api/openai/v1/ ": "https://already-openai.example.test/api/openai/v1",
	} {
		if got := normalizeOpenAIBaseURL(input); got != want {
			t.Fatalf("normalizeOpenAIBaseURL(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestLongMemEvalRetrievalReferenceUsesEndOfQuestionDay(t *testing.T) {
	questionTime := parseLongMemEvalDate("2023/03/28 (Tue) 01:25")
	got := longMemEvalRetrievalReferenceTime(questionTime)
	if got.Format(time.RFC3339Nano) != "2023-03-28T23:59:59.999999999Z" {
		t.Fatalf("retrieval reference=%s", got.Format(time.RFC3339Nano))
	}
}

func TestNormalizeMemoryChecksumSpaceCanonicalizesSeparators(t *testing.T) {
	for input, want := range map[string]string{
		" Project:Case_With Spaces ": "project:case-with-spaces",
		"___":                        "default",
		"project:user/name#2":        "project:user/name#2",
	} {
		if got := normalizeMemoryChecksumSpace(input); got != want {
			t.Fatalf("normalizeMemoryChecksumSpace(%q)=%q, want %q", input, got, want)
		}
	}
}
