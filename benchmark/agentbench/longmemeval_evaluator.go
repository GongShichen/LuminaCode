package agentbench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	luminaapi "LuminaCode/api"
	"LuminaCode/config"
)

const longMemEvalEvaluatorUsagePrefix = "__LUMINA_EVAL_USAGE__"

type longMemEvalEvaluatorUsage struct {
	CaseID               string `json:"case_id"`
	Model                string `json:"model"`
	InputTokens          int    `json:"input_tokens"`
	CacheReadInputTokens int    `json:"cache_read_input_tokens"`
	OutputTokens         int    `json:"output_tokens"`
}

func runLongMemEvalEvaluator(ctx context.Context, benchmarkDir, predictionPath, datasetPath string,
	cfg config.Config) (string, string, []longMemEvalEvaluatorUsage) {
	metricModel := firstNonEmptyString(os.Getenv("LONGMEMEVAL_EVAL_MODEL"), cfg.APIModel, "mimo-v2.5-pro")
	apiKey := firstNonEmptyString(os.Getenv("LONGMEMEVAL_EVAL_API_KEY"), cfg.APIKey)
	if strings.TrimSpace(apiKey) == "" {
		return "not_run_missing_evaluator_api_key", "", nil
	}
	baseURL := normalizeOpenAIBaseURL(firstNonEmptyString(os.Getenv("LONGMEMEVAL_EVAL_BASE_URL"), cfg.APIBaseURL))
	evaluator := filepath.Join(benchmarkDir, "src", "evaluation", "evaluate_qa.py")
	if _, err := os.Stat(evaluator); err != nil {
		return "not_run_evaluator_missing", err.Error(), nil
	}
	python := filepath.Join(filepath.Dir(benchmarkDir), ".venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		return "not_run_evaluator_environment_missing", err.Error(), nil
	}

	evaluatorScript, err := longMemEvalEvaluatorScript(evaluator, predictionPath, metricModel,
		fallbackLongMemEvalEvaluatorModel(cfg))
	if err != nil {
		return "not_run_evaluator_prepare_failed", err.Error(), nil
	}
	if evaluatorScript != evaluator {
		defer os.Remove(evaluatorScript)
	}
	output, err := runLongMemEvalEvaluatorOnce(ctx, python, evaluatorScript, benchmarkDir, metricModel,
		predictionPath, datasetPath, apiKey, baseURL)
	primaryUsage := parseLongMemEvalEvaluatorUsage(output)
	output = stripLongMemEvalEvaluatorUsage(output)
	if err == nil {
		return "completed", strings.TrimSpace(output), primaryUsage
	}
	primaryOutput := strings.TrimSpace(output + "\n" + err.Error())
	if luminaapi.IsQuotaExhaustedMessage(primaryOutput) {
		return "failed_api_quota_exhausted", primaryOutput, primaryUsage
	}

	fallbackModel, fallbackKey, fallbackBaseURL := longMemEvalEvaluatorFallback(cfg)
	if fallbackModel == "" || fallbackKey == "" || fallbackBaseURL == "" {
		return "failed", primaryOutput, primaryUsage
	}
	fallbackOutput, fallbackErr := runLongMemEvalEvaluatorOnce(ctx, python, evaluatorScript, benchmarkDir,
		fallbackModel, predictionPath, datasetPath, fallbackKey, fallbackBaseURL)
	fallbackUsage := parseLongMemEvalEvaluatorUsage(fallbackOutput)
	fallbackOutput = stripLongMemEvalEvaluatorUsage(fallbackOutput)
	allUsage := append(primaryUsage, fallbackUsage...)
	if fallbackErr != nil {
		combined := fallbackOutput + "\n" + fallbackErr.Error()
		if luminaapi.IsQuotaExhaustedMessage(combined) {
			return "failed_api_quota_exhausted", strings.TrimSpace(primaryOutput +
				"\n\nfallback evaluator quota exhausted:\n" + combined), allUsage
		}
		return "failed", strings.TrimSpace(primaryOutput + "\n\nfallback evaluator failed:\n" + combined), allUsage
	}
	return "completed_fallback", strings.TrimSpace("primary evaluator failed:\n" + primaryOutput +
		"\n\nfallback evaluator completed:\n" + fallbackOutput), allUsage
}

func parseLongMemEvalEvaluatorUsage(output string) []longMemEvalEvaluatorUsage {
	var result []longMemEvalEvaluatorUsage
	for _, line := range strings.Split(output, "\n") {
		marker := strings.Index(line, longMemEvalEvaluatorUsagePrefix)
		if marker < 0 {
			continue
		}
		var usage longMemEvalEvaluatorUsage
		payload := line[marker+len(longMemEvalEvaluatorUsagePrefix):]
		if json.NewDecoder(strings.NewReader(payload)).Decode(&usage) == nil {
			result = append(result, usage)
		}
	}
	return result
}

func stripLongMemEvalEvaluatorUsage(output string) string {
	lines := strings.Split(output, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if marker := strings.Index(line, longMemEvalEvaluatorUsagePrefix); marker >= 0 {
			line = strings.TrimRight(line[:marker], "\r \t")
			if strings.TrimSpace(line) == "" {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func fallbackLongMemEvalEvaluatorModel(cfg config.Config) string {
	model, _, _ := longMemEvalEvaluatorFallback(cfg)
	return model
}

func longMemEvalEvaluatorFallback(cfg config.Config) (string, string, string) {
	if !cfg.FallbackAPIEnabled {
		return "", "", ""
	}
	model := firstNonEmptyString(os.Getenv("LONGMEMEVAL_EVAL_FALLBACK_MODEL"), cfg.FallbackAPIModel,
		"deepseek-v4-pro")
	apiKey := firstNonEmptyString(os.Getenv("LONGMEMEVAL_EVAL_FALLBACK_API_KEY"),
		os.Getenv("DEEPSEEK_API_KEY"), cfg.FallbackAPIKey)
	baseURL := firstNonEmptyString(os.Getenv("LONGMEMEVAL_EVAL_FALLBACK_BASE_URL"),
		os.Getenv("DEEPSEEK_BASE_URL"), cfg.FallbackAPIBaseURL, "https://api.deepseek.com")
	return model, apiKey, normalizeOpenAIBaseURL(baseURL)
}

func runLongMemEvalEvaluatorOnce(ctx context.Context, python, evaluator, benchmarkDir, metricModel,
	predictionPath, datasetPath, apiKey, baseURL string) (string, error) {
	command := exec.CommandContext(ctx, python, evaluator, metricModel, predictionPath, datasetPath)
	command.Dir = benchmarkDir
	command.Env = append(os.Environ(), "PYTHONPATH="+benchmarkDir, "DEEPSEEK_API_KEY="+apiKey,
		"DEEPSEEK_BASE_URL="+baseURL)
	output, err := command.CombinedOutput()
	return string(output), err
}

func longMemEvalEvaluatorScript(evaluator, predictionPath, primaryModel, fallbackModel string) (string, error) {
	_ = predictionPath
	models := []string{primaryModel, fallbackModel}
	source, err := os.ReadFile(evaluator)
	if err != nil {
		return "", err
	}
	patch := ""
	for _, model := range models {
		if model == "" || longMemEvalOfficialEvaluatorModel(model) ||
			strings.Contains(patch, fmt.Sprintf("%q", model)) {
			continue
		}
		patch += fmt.Sprintf("model_zoo[%q] = (%q, 'deepseek')\n", model, model)
	}
	const marker = "\n\n@backoff.on_exception"
	content := string(source)
	if !strings.Contains(content, marker) {
		return "", fmt.Errorf("unsupported evaluator format: missing backoff marker")
	}
	content = strings.Replace(content, marker, "\n\n"+patch+marker, 1)
	const backoffDecorator = `@backoff.on_exception(backoff.expo, (openai.RateLimitError,
                                    openai.APIError))`
	const boundedBackoff = `def _lumina_evaluator_wait():
    for seconds in (10, 30, 60):
        yield seconds


def _lumina_evaluator_giveup(exc):
    text = str(exc).lower()
    return getattr(exc, 'status_code', None) == 402 or any(fragment in text for fragment in (
        'quota exhausted', 'insufficient quota', 'insufficient balance',
        'payment required', 'billing hard limit',
    ))


@backoff.on_exception(_lumina_evaluator_wait, (openai.RateLimitError,
                                               openai.APIError),
                      max_tries=4, giveup=_lumina_evaluator_giveup)`
	if !strings.Contains(content, backoffDecorator) {
		return "", fmt.Errorf("unsupported evaluator format: missing evaluator backoff decorator")
	}
	content = strings.Replace(content, backoffDecorator, boundedBackoff, 1)

	const clientMarker = `    metric_client = OpenAI(
        api_key=openai_api_key,
        base_url=openai_api_base,
    )`
	const boundedClient = `    metric_client = OpenAI(
        api_key=openai_api_key,
        base_url=openai_api_base,
        timeout=60.0,
        max_retries=0,
    )`
	if !strings.Contains(content, clientMarker) {
		return "", fmt.Errorf("unsupported evaluator format: missing evaluator client")
	}
	content = strings.Replace(content, clientMarker, boundedClient, 1)

	const loopMarker = `    with open(result_file, 'w') as out_f:
        logs = []
        for entry in tqdm(hypotheses):`
	const resumableLoop = `    _existing_by_id = {}
    if os.path.exists(result_file):
        try:
            for _line in open(result_file).readlines():
                _entry = json.loads(_line)
                if _entry.get('question_id') and 'autoeval_label' in _entry:
                    _existing_by_id[_entry['question_id']] = _entry
        except Exception:
            _existing_by_id = {}

    with open(result_file, 'w') as out_f:
        logs = []
        for _entry in hypotheses:
            _existing = _existing_by_id.get(_entry['question_id'])
            if _existing is None:
                continue
            logs.append(_existing)
            print(json.dumps(_existing), file=out_f)
            _qtype = qid2qtype.get(_entry['question_id'])
            if _qtype is not None:
                qtype2acc[_qtype].append(1 if _existing['autoeval_label']['label'] else 0)
        out_f.flush()
        _pending = [entry for entry in hypotheses if entry['question_id'] not in _existing_by_id]
        for entry in tqdm(_pending):`
	if !strings.Contains(content, loopMarker) {
		return "", fmt.Errorf("unsupported evaluator format: missing evaluator loop")
	}
	content = strings.Replace(content, loopMarker, resumableLoop, 1)
	const resultWrite = "            print(json.dumps(entry), file=out_f)"
	if !strings.Contains(content, resultWrite) {
		return "", fmt.Errorf("unsupported evaluator format: missing evaluator result write")
	}
	content = strings.Replace(content, resultWrite, resultWrite+"\n            out_f.flush()", 1)

	const completionMarker = "            completion = chat_completions_with_backoff(metric_client, **kwargs)"
	if !strings.Contains(content, completionMarker) {
		return "", fmt.Errorf("unsupported evaluator format: missing evaluator completion call")
	}
	usagePatch := completionMarker + `
            _usage = getattr(completion, 'usage', None)
            if _usage is not None:
                _details = getattr(_usage, 'prompt_tokens_details', None)
                _cached = int(getattr(_details, 'cached_tokens', 0) or 0)
                _prompt = int(getattr(_usage, 'prompt_tokens', 0) or 0)
                print('__LUMINA_EVAL_USAGE__' + json.dumps({
                    'case_id': entry['question_id'],
                    'model': metric_model_short,
                    'input_tokens': max(0, _prompt - _cached),
                    'cache_read_input_tokens': _cached,
                    'output_tokens': int(getattr(_usage, 'completion_tokens', 0) or 0),
                }), flush=True)`
	content = strings.Replace(content, completionMarker, usagePatch, 1)
	file, err := os.CreateTemp("", "lumina-longmemeval-evaluator-*.py")
	if err != nil {
		return "", err
	}
	scriptPath := file.Name()
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		_ = os.Remove(scriptPath)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(scriptPath)
		return "", err
	}
	if err := os.Chmod(scriptPath, 0o600); err != nil {
		_ = os.Remove(scriptPath)
		return "", err
	}
	return scriptPath, nil
}

func longMemEvalOfficialEvaluatorModel(model string) bool {
	switch strings.TrimSpace(model) {
	case "llama-3.1-70b-instruct", "gpt-4o-mini", "gpt-4o", "deepseek-v4-pro":
		return true
	default:
		return false
	}
}

func normalizeOpenAIBaseURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	baseURL = strings.TrimSuffix(baseURL, "/chat/completions")
	for _, suffix := range []string{"/anthropic/messages", "/anthropic"} {
		if strings.HasSuffix(baseURL, suffix) {
			baseURL = strings.TrimRight(strings.TrimSuffix(baseURL, suffix), "/")
			if !strings.HasSuffix(baseURL, "/v1") {
				baseURL += "/v1"
			}
			break
		}
	}
	return baseURL
}
