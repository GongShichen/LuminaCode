package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"LuminaCode/agent"
	"LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/longmemory"
	coretools "LuminaCode/tools"
)

type datasetSample struct {
	SampleID     string                     `json:"sample_id"`
	Conversation map[string]json.RawMessage `json:"conversation"`
	QA           []question                 `json:"qa"`
}

type dialogTurn struct {
	Speaker     string `json:"speaker"`
	DialogID    string `json:"dia_id"`
	Text        string `json:"text"`
	BLIPCaption string `json:"blip_caption"`
}

type question struct {
	Question          string   `json:"question"`
	Answer            any      `json:"answer"`
	AdversarialAnswer string   `json:"adversarial_answer"`
	Evidence          []string `json:"evidence"`
	Category          int      `json:"category"`
}

type caseResult struct {
	SampleID          string   `json:"sample_id"`
	QuestionIndex     int      `json:"question_index"`
	Category          int      `json:"category"`
	Question          string   `json:"question"`
	Answer            any      `json:"answer,omitempty"`
	AdversarialAnswer string   `json:"adversarial_answer,omitempty"`
	Evidence          []string `json:"evidence,omitempty"`
	Prediction        string   `json:"prediction"`
	F1                float64  `json:"f1"`
	Error             string   `json:"error,omitempty"`
	RetrievalMS       int64    `json:"retrieval_ms"`
	AnswerMS          int64    `json:"answer_ms"`
	MemoryItems       int      `json:"memory_items"`
	CompletedAt       string   `json:"completed_at"`
}

type sampleRuntime struct {
	sample        datasetSample
	cfg           config.Config
	referenceTime time.Time
}

func main() {
	dataPath := flag.String("data", "/Users/gsc/Documents/benchmark/locomo/data/locomo10.json", "LoCoMo dataset")
	outputDir := flag.String("output-dir", "", "report and work directory")
	parallel := flag.Int("parallel", 16, "concurrent QA cases")
	limit := flag.Int("limit", 0, "maximum QA cases; zero runs all")
	sampleLimit := flag.Int("sample-limit", 0, "maximum conversations; zero runs all")
	includeAdversarial := flag.Bool("include-adversarial", false, "include category 5")
	flag.Parse()
	if strings.TrimSpace(*outputDir) == "" {
		*outputDir = filepath.Join(os.Getenv("HOME"), "Documents", "benchmark", "reports", "locomo-"+time.Now().Format("20060102-150405"))
	}
	if err := run(context.Background(), *dataPath, *outputDir, *parallel, *limit, *sampleLimit, *includeAdversarial); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dataPath, outputDir string, parallel, limit, sampleLimit int, includeAdversarial bool) error {
	data, err := os.ReadFile(dataPath)
	if err != nil {
		return err
	}
	var samples []datasetSample
	if err := json.Unmarshal(data, &samples); err != nil {
		return err
	}
	if sampleLimit > 0 && sampleLimit < len(samples) {
		samples = samples[:sampleLimit]
	}
	if parallel < 1 {
		parallel = 1
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}
	workDir := filepath.Join(outputDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}

	runtimes := make(map[string]*sampleRuntime, len(samples))
	for index := range samples {
		runtime, err := indexSample(ctx, samples[index], workDir)
		if err != nil {
			return fmt.Errorf("index %s: %w", samples[index].SampleID, err)
		}
		runtimes[samples[index].SampleID] = runtime
		fmt.Printf("indexed %d/%d %s\n", index+1, len(samples), samples[index].SampleID)
	}

	type task struct {
		runtime *sampleRuntime
		index   int
	}
	var tasks []task
	for _, sample := range samples {
		for index, qa := range sample.QA {
			if qa.Category == 5 && !includeAdversarial {
				continue
			}
			tasks = append(tasks, task{runtime: runtimes[sample.SampleID], index: index})
			if limit > 0 && len(tasks) >= limit {
				break
			}
		}
		if limit > 0 && len(tasks) >= limit {
			break
		}
	}
	checkpointPath := filepath.Join(outputDir, "checkpoint.jsonl")
	done, err := loadCheckpoint(checkpointPath)
	if err != nil {
		return err
	}
	checkpoint, err := os.OpenFile(checkpointPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer checkpoint.Close()
	encoder := json.NewEncoder(checkpoint)
	var writeMu sync.Mutex
	queue := make(chan task)
	var workers sync.WaitGroup
	var completed atomic.Int64
	for range parallel {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := range queue {
				key := fmt.Sprintf("%s:%d", item.runtime.sample.SampleID, item.index)
				if _, exists := done[key]; exists {
					completed.Add(1)
					continue
				}
				result := answerCase(ctx, item.runtime, item.index)
				writeMu.Lock()
				_ = encoder.Encode(result)
				_ = checkpoint.Sync()
				writeMu.Unlock()
				count := completed.Add(1)
				fmt.Printf("completed %d/%d %s category=%d f1=%.3f error=%q\n", count, len(tasks), key, result.Category, result.F1, result.Error)
			}
		}()
	}
	for _, item := range tasks {
		queue <- item
	}
	close(queue)
	workers.Wait()
	return writeReport(checkpointPath, outputDir)
}

func indexSample(ctx context.Context, sample datasetSample, workDir string) (*sampleRuntime, error) {
	projectRoot := filepath.Join(workDir, sample.SampleID, "project")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		return nil, err
	}
	cfg := config.NewConfigForCWD(projectRoot)
	cfg.CWD = projectRoot
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = filepath.Join(workDir, sample.SampleID, "memory.sqlite")
	cfg.SessionMemoryEnabled = false
	cfg.MemoryBackgroundExtractionEnabled = false
	cfg.HarnessMode = ""
	sessions := sessionNumbers(sample.Conversation)
	var latest time.Time
	for _, number := range sessions {
		var turns []dialogTurn
		if err := json.Unmarshal(sample.Conversation["session_"+strconv.Itoa(number)], &turns); err != nil {
			return nil, err
		}
		var dateText string
		_ = json.Unmarshal(sample.Conversation["session_"+strconv.Itoa(number)+"_date_time"], &dateText)
		occurredAt := parseLoCoMoTime(dateText)
		if occurredAt.After(latest) {
			latest = occurredAt
		}
		state := agent.NewAgentState()
		state.MemorySessionID = sample.SampleID + "-session-" + strconv.Itoa(number)
		state.MemoryAgentID = "conversation-import"
		state.MemoryAgentType = "conversation-import"
		for _, turn := range turns {
			text := turn.Speaker + ` said, "` + strings.TrimSpace(turn.Text) + `"`
			if strings.TrimSpace(turn.BLIPCaption) != "" {
				text += " and shared " + strings.TrimSpace(turn.BLIPCaption)
			}
			state.Messages = append(state.Messages, map[string]any{"id": turn.DialogID, "role": "user", "content": text,
				"timestamp": occurredAt.Format(time.RFC3339)})
			state.UserTurnCount++
		}
		controller := agent.NewExtractionController(cfg, coretools.NewToolRegistry())
		controller.SourceSessionID = state.MemorySessionID
		controller.SourceAgentID = "conversation-import"
		controller.StoreBusyTimeout = 15 * time.Minute
		for {
			count, err := controller.IngestMessages(ctx, &state)
			if err != nil {
				return nil, err
			}
			if count == 0 {
				break
			}
		}
	}
	if err := flushEmbeddings(ctx, cfg); err != nil {
		return nil, err
	}
	return &sampleRuntime{sample: sample, cfg: cfg, referenceTime: latest}, nil
}

func flushEmbeddings(ctx context.Context, cfg config.Config) error {
	embedder, err := longmemory.SharedLocalEmbedder(cfg.MemoryEmbeddingModel, cfg.MemoryEmbeddingModelDir)
	if err != nil {
		return err
	}
	scheduler := longmemory.SharedEmbeddingScheduler(embedder, longmemory.EmbeddingSchedulerOptions{
		BatchSize: cfg.MemoryEmbeddingBatchSize, BatchWait: time.Duration(cfg.MemoryEmbeddingBatchWaitMS) * time.Millisecond,
		QueryCacheEntries: cfg.MemoryEmbeddingQueryCacheEntries,
		ExecutionTimeout:  time.Duration(cfg.MemoryEmbeddingExecutionTimeout * float64(time.Second)),
	})
	store, err := longmemory.OpenWithBusyTimeout(ctx, cfg.LongTermMemoryStore, 15*time.Minute)
	if err != nil {
		return err
	}
	defer store.Close()
	for {
		result, err := store.RunMaintenance(ctx, scheduler, max(cfg.MemoryEmbeddingBatchSize*4, 128))
		if err != nil {
			return err
		}
		if result.Embedded+result.ChunkEmbedded+result.AtomEmbedded+result.SessionEmbedded == 0 {
			return nil
		}
	}
}

func answerCase(ctx context.Context, runtime *sampleRuntime, index int) caseResult {
	qa := runtime.sample.QA[index]
	result := caseResult{SampleID: runtime.sample.SampleID, QuestionIndex: index, Category: qa.Category,
		Question: qa.Question, Answer: qa.Answer, AdversarialAnswer: qa.AdversarialAnswer, Evidence: qa.Evidence,
		CompletedAt: time.Now().UTC().Format(time.RFC3339)}
	state := agent.NewAgentState()
	state.MemorySessionID = runtime.sample.SampleID + "-query"
	state.MemoryAgentID = "main"
	state.MemoryAgentType = "main"
	state.MemoryQueryTime = runtime.referenceTime
	factory := func(ctx context.Context, model string) (api.LLMClient, error) {
		return agent.CreateConfiguredLLMClient(runtime.cfg, model, 1024, nil, api.DefaultRetryConfigPtr())
	}
	started := time.Now()
	recalls := agent.RunMemoryRecallWithRuntime(ctx, runtime.cfg, &state, qa.Question, factory)
	result.RetrievalMS = time.Since(started).Milliseconds()
	result.MemoryItems = len(recalls)
	parts := make([]string, 0, len(recalls))
	for _, recall := range recalls {
		if text := strings.TrimSpace(recall.Content); text != "" {
			parts = append(parts, text)
		}
	}
	started = time.Now()
	prediction, err := generateAnswer(ctx, runtime.cfg, qa.Question, strings.Join(parts, "\n\n"))
	result.AnswerMS = time.Since(started).Milliseconds()
	result.Prediction = prediction
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.F1 = scoreAnswer(qa, prediction)
	return result
}

func generateAnswer(ctx context.Context, cfg config.Config, question, evidence string) (string, error) {
	client, err := agent.CreateConfiguredLLMClient(cfg, cfg.APIModel, 256, nil, api.DefaultRetryConfigPtr())
	if err != nil {
		return "", err
	}
	prompt := "Memory evidence:\n" + evidence + "\n\nQuestion: " + question +
		"\nAnswer with a short phrase using the evidence. If the evidence does not contain the answer, say 'No information available'."
	streamCtx := api.ContextWithStreamIdleTimeout(ctx, 10*time.Minute)
	var answer strings.Builder
	for event := range client.StreamChat(streamCtx, "Answer only from the supplied memory evidence.", []map[string]any{{"role": "user", "content": prompt}}, nil, nil) {
		if event.Err != nil {
			return answer.String(), event.Err
		}
		if event.Event["type"] == "text_delta" {
			answer.WriteString(fmt.Sprint(event.Event["text"]))
		}
		if event.Event["type"] == "error" {
			return answer.String(), errors.New(fmt.Sprint(event.Event["message"]))
		}
	}
	return strings.TrimSpace(answer.String()), nil
}

func sessionNumbers(conversation map[string]json.RawMessage) []int {
	var values []int
	for key := range conversation {
		if !strings.HasPrefix(key, "session_") || strings.HasSuffix(key, "_date_time") {
			continue
		}
		if value, err := strconv.Atoi(strings.TrimPrefix(key, "session_")); err == nil {
			values = append(values, value)
		}
	}
	sort.Ints(values)
	return values
}

func parseLoCoMoTime(value string) time.Time {
	parsed, err := time.ParseInLocation("3:04 pm on 2 January, 2006", strings.TrimSpace(value), time.UTC)
	if err == nil {
		return parsed.UTC()
	}
	return time.Now().UTC()
}

func loadCheckpoint(path string) (map[string]struct{}, error) {
	result := map[string]struct{}{}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var item caseResult
		if json.Unmarshal(scanner.Bytes(), &item) == nil {
			result[fmt.Sprintf("%s:%d", item.SampleID, item.QuestionIndex)] = struct{}{}
		}
	}
	return result, scanner.Err()
}

func writeReport(checkpointPath, outputDir string) error {
	file, err := os.Open(checkpointPath)
	if err != nil {
		return err
	}
	defer file.Close()
	var results []caseResult
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var item caseResult
		if json.Unmarshal(scanner.Bytes(), &item) == nil {
			results = append(results, item)
		}
	}
	type aggregate struct {
		Count int
		Sum   float64
	}
	byCategory := map[int]aggregate{}
	var total aggregate
	for _, item := range results {
		if item.Error != "" {
			continue
		}
		value := byCategory[item.Category]
		value.Count++
		value.Sum += item.F1
		byCategory[item.Category] = value
		total.Count++
		total.Sum += item.F1
	}
	report := map[string]any{"cases": len(results), "scored": total.Count, "mean_f1": divide(total.Sum, total.Count), "categories": map[string]any{}}
	categories := report["categories"].(map[string]any)
	for category, value := range byCategory {
		categories[strconv.Itoa(category)] = map[string]any{"count": value.Count, "mean_f1": divide(value.Sum, value.Count)}
	}
	encoded, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(filepath.Join(outputDir, "report.json"), append(encoded, '\n'), 0o644); err != nil {
		return err
	}
	type predictionSample struct {
		SampleID string           `json:"sample_id"`
		QA       []map[string]any `json:"qa"`
	}
	grouped := map[string][]caseResult{}
	for _, item := range results {
		grouped[item.SampleID] = append(grouped[item.SampleID], item)
	}
	ids := make([]string, 0, len(grouped))
	for id := range grouped {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	predictions := make([]predictionSample, 0, len(ids))
	for _, id := range ids {
		items := grouped[id]
		sort.Slice(items, func(left, right int) bool { return items[left].QuestionIndex < items[right].QuestionIndex })
		qa := make([]map[string]any, 0, len(items))
		for _, item := range items {
			qa = append(qa, map[string]any{"question": item.Question, "answer": item.Answer,
				"adversarial_answer": item.AdversarialAnswer, "evidence": item.Evidence,
				"category": item.Category, "lumina_prediction": item.Prediction, "lumina_f1": item.F1})
		}
		predictions = append(predictions, predictionSample{SampleID: id, QA: qa})
	}
	predictionJSON, _ := json.MarshalIndent(predictions, "", "  ")
	return os.WriteFile(filepath.Join(outputDir, "predictions.json"), append(predictionJSON, '\n'), 0o644)
}

func divide(value float64, count int) float64 {
	if count == 0 {
		return 0
	}
	return value / float64(count)
}

func scoreAnswer(qa question, prediction string) float64 {
	if qa.Category == 5 {
		lower := strings.ToLower(prediction)
		if strings.Contains(lower, "no information available") || strings.Contains(lower, "not mentioned") {
			return 1
		}
		return 0
	}
	if qa.Category == 1 {
		predictions := splitAnswers(prediction)
		answers := splitAnswers(answerText(qa.Answer))
		var total float64
		for _, answer := range answers {
			best := 0.0
			for _, candidate := range predictions {
				if score := tokenF1(candidate, answer); score > best {
					best = score
				}
			}
			total += best
		}
		return divide(total, len(answers))
	}
	answer := answerText(qa.Answer)
	if qa.Category == 3 {
		answer = strings.TrimSpace(strings.Split(answer, ";")[0])
	}
	return tokenF1(prediction, answer)
}

func splitAnswers(value string) []string {
	parts := strings.Split(value, ",")
	result := parts[:0]
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func answerText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, answerText(item))
		}
		return strings.Join(parts, ", ")
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

func tokenF1(prediction, answer string) float64 {
	left, right := normalizedTokens(prediction), normalizedTokens(answer)
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	counts := map[string]int{}
	for _, token := range left {
		counts[token]++
	}
	common := 0
	for _, token := range right {
		if counts[token] > 0 {
			common++
			counts[token]--
		}
	}
	if common == 0 {
		return 0
	}
	precision, recall := float64(common)/float64(len(left)), float64(common)/float64(len(right))
	return 2 * precision * recall / (precision + recall)
}

func normalizedTokens(value string) []string {
	value = strings.ToLower(strings.ReplaceAll(value, ",", ""))
	value = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsSpace(r) {
			return r
		}
		return ' '
	}, value)
	var result []string
	for _, token := range strings.Fields(value) {
		if token != "a" && token != "an" && token != "the" && token != "and" {
			result = append(result, token)
		}
	}
	return result
}
