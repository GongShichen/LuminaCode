package memory

import (
	"context"
	"math"
	"sort"
	"strings"
)

const (
	defaultPlanSources           = 32
	defaultPlanSourcesPerSession = 2
	defaultPlanSourceRunes       = 900
	semanticPlanningRunes        = 384
	semanticPrototypeThreshold   = 0.78
	semanticPrototypeMargin      = 0.05
)

type localSemanticPlanner struct {
	vectorizer Vectorizer
}

func NewLocalSemanticPlanner(vectorizer Vectorizer) SemanticPlanner {
	return &localSemanticPlanner{vectorizer: vectorizer}
}

func (p *localSemanticPlanner) Plan(ctx context.Context, events []RawEvent, options PlanningOptions) (SemanticPlan, error) {
	plans, err := p.PlanBatch(ctx, []SemanticPlanningBatch{{Events: events, Options: options}})
	if err != nil {
		return SemanticPlan{}, err
	}
	return plans[0], nil
}

type semanticPlanningWork struct {
	events     []RawEvent
	options    PlanningOptions
	candidates []SemanticCandidate
	ambiguous  []int
}

func (p *localSemanticPlanner) PlanBatch(ctx context.Context, batches []SemanticPlanningBatch) ([]SemanticPlan, error) {
	work := make([]semanticPlanningWork, len(batches))
	uniqueTexts := make([]string, 0)
	textIndex := map[string]int{}
	for batchIndex, batch := range batches {
		item := semanticPlanningWork{events: batch.Events, options: normalizePlanningOptions(batch.Options),
			candidates: make([]SemanticCandidate, 0, len(batch.Events))}
		for eventIndex, event := range batch.Events {
			if isDeterministicSource(event.SourceKind) || isLowSignalSemanticEvent(event) {
				continue
			}
			score := deterministicSemanticScore(event, item.options.Mode)
			if score > 0 {
				item.candidates = append(item.candidates,
					semanticCandidateForEvent(event, score, item.options.MaxSourceRunes))
				continue
			}
			if !isPrototypeCandidate(event) {
				continue
			}
			if p.vectorizer == nil {
				item.candidates = append(item.candidates,
					semanticCandidateForEvent(event, 1, item.options.MaxSourceRunes))
				continue
			}
			if len(item.ambiguous) < 64 {
				item.ambiguous = append(item.ambiguous, eventIndex)
				text := semanticPlanningText(event.Content)
				if _, exists := textIndex[text]; !exists {
					textIndex[text] = len(uniqueTexts)
					uniqueTexts = append(uniqueTexts, text)
				}
			}
		}
		work[batchIndex] = item
	}

	if p.vectorizer != nil && len(uniqueTexts) > 0 {
		prototypeTexts := append(append([]string(nil), semanticMemoryPrototypes...), genericAssistantPrototype)
		prototypeVectors, err := p.vectorizer.Embed(ctx, prototypeTexts, VectorContent)
		if err != nil {
			return nil, err
		}
		if len(prototypeVectors) != len(prototypeTexts) {
			return nil, &planningVectorCountError{Expected: len(prototypeTexts), Actual: len(prototypeVectors)}
		}
		vectors := make([][]float32, len(uniqueTexts))
		const planningEmbeddingBatch = 256
		for start := 0; start < len(uniqueTexts); start += planningEmbeddingBatch {
			end := minIntMemory(start+planningEmbeddingBatch, len(uniqueTexts))
			values, err := p.vectorizer.Embed(ctx, uniqueTexts[start:end], VectorContent)
			if err != nil {
				return nil, err
			}
			if len(values) != end-start {
				return nil, &planningVectorCountError{Expected: end - start, Actual: len(values)}
			}
			copy(vectors[start:end], values)
		}
		generic := prototypeVectors[len(prototypeVectors)-1]
		for batchIndex := range work {
			item := &work[batchIndex]
			for _, eventIndex := range item.ambiguous {
				vector := vectors[textIndex[semanticPlanningText(item.events[eventIndex].Content)]]
				best := -1.0
				for prototype := range semanticMemoryPrototypes {
					score := cosineSimilarity(vector, prototypeVectors[prototype])
					if score > best {
						best = score
					}
				}
				genericScore := cosineSimilarity(vector, generic)
				if best >= semanticPrototypeThreshold && best-genericScore >= semanticPrototypeMargin {
					item.candidates = append(item.candidates,
						semanticCandidateForEvent(item.events[eventIndex], best, item.options.MaxSourceRunes))
				}
			}
		}
	}

	plans := make([]SemanticPlan, len(work))
	for index := range work {
		plans[index] = finalizeSemanticPlan(work[index].events, work[index].candidates, work[index].options)
	}
	return plans, nil
}

type planningVectorCountError struct {
	Expected int
	Actual   int
}

func (e *planningVectorCountError) Error() string {
	return "semantic planner vector count mismatch"
}

func finalizeSemanticPlan(events []RawEvent, candidates []SemanticCandidate, options PlanningOptions) SemanticPlan {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return candidates[i].Source.OccurredAt.Before(candidates[j].Source.OccurredAt)
	})
	selected := make([]SemanticCandidate, 0, minIntMemory(len(candidates), options.MaxSources))
	perSession := map[string]int{}
	seenEvents := map[string]struct{}{}
	for _, candidate := range candidates {
		if len(selected) >= options.MaxSources {
			break
		}
		session := firstNonEmptyMemory(candidate.Source.SessionRef, candidate.Source.SourceRef)
		if perSession[session] >= options.MaxSourcesPerSession {
			continue
		}
		if _, exists := seenEvents[candidate.EventID]; exists {
			continue
		}
		perSession[session]++
		seenEvents[candidate.EventID] = struct{}{}
		selected = append(selected, candidate)
	}

	selectedIDs := map[string]struct{}{}
	for _, candidate := range selected {
		selectedIDs[candidate.EventID] = struct{}{}
	}
	plan := SemanticPlan{Candidates: selected}
	for _, event := range events {
		if _, selected := selectedIDs[event.ID]; !selected {
			plan.SkippedEventIDs = append(plan.SkippedEventIDs, event.ID)
		}
	}
	return plan
}

func semanticPlanningText(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= semanticPlanningRunes {
		return value
	}
	// Classification only needs the durable-memory signal. Preserve both the
	// setup and conclusion without sending the full long turn through BGE-M3.
	head := semanticPlanningRunes / 2
	tail := semanticPlanningRunes - head - 5
	return strings.TrimSpace(string(runes[:head])) + "\n...\n" +
		strings.TrimSpace(string(runes[len(runes)-tail:]))
}

func normalizePlanningOptions(options PlanningOptions) PlanningOptions {
	if options.MaxSources <= 0 {
		options.MaxSources = defaultPlanSources
	}
	if options.MaxSourcesPerSession <= 0 {
		options.MaxSourcesPerSession = defaultPlanSourcesPerSession
	}
	if options.MaxSourceRunes <= 0 {
		options.MaxSourceRunes = defaultPlanSourceRunes
	}
	return options
}

func deterministicSemanticScore(event RawEvent, mode MemoryWriteMode) float64 {
	actor := strings.ToLower(strings.TrimSpace(event.Actor))
	if isSynchronousWriteMode(mode) && actor != "assistant" {
		return 3
	}
	return 0
}

func isPrototypeCandidate(event RawEvent) bool {
	actor := strings.ToLower(strings.TrimSpace(event.Actor))
	if actor != "user" && actor != "human" {
		return false
	}
	text := strings.TrimSpace(event.Content)
	runes := len([]rune(text))
	return runes >= 20 && runes <= 1600 &&
		!strings.HasSuffix(text, "?") && !strings.HasSuffix(text, "？")
}

func semanticCandidateForEvent(event RawEvent, score float64, maxRunes int) SemanticCandidate {
	text := semanticSourceExcerpt(event.Content, maxRunes)
	return SemanticCandidate{EventID: event.ID, Score: score, Source: CompileSource{
		SourceRef: compilerSourceRef(event), SessionRef: sessionKey(event), Actor: event.Actor,
		Text: text, OccurredAt: event.OccurredAt,
	}}
}

func compilerSourceRef(event RawEvent) string {
	if value := strings.TrimSpace(event.SourceRef); value != "" {
		return value
	}
	return stableFabricID("src", sessionKey(event), event.Actor, formatFabricTime(event.OccurredAt), event.Content)
}

func sessionKey(event RawEvent) string {
	return firstNonEmptyMemory(strings.TrimSpace(event.SessionID), strings.TrimSpace(event.ContextID), "session")
}

func semanticSourceExcerpt(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	// Keep both ends for long explicit messages. Durable corrections and
	// conclusions frequently appear at the end, while identity and scope are
	// usually introduced at the start.
	head := maxRunes / 2
	tail := maxRunes - head - 5
	if tail < 1 {
		return strings.TrimSpace(string(runes[:maxRunes]))
	}
	return strings.TrimSpace(string(runes[:head])) + "\n...\n" +
		strings.TrimSpace(string(runes[len(runes)-tail:]))
}

func isDeterministicSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "tool", "command", "file", "observation":
		return true
	default:
		return false
	}
}

func cosineSimilarity(left, right []float32) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return -1
	}
	dot, leftNorm, rightNorm := 0.0, 0.0, 0.0
	for index := range left {
		l, r := float64(left[index]), float64(right[index])
		dot += l * r
		leftNorm += l * l
		rightNorm += r * r
	}
	if leftNorm == 0 || rightNorm == 0 {
		return -1
	}
	return dot / math.Sqrt(leftNorm*rightNorm)
}

var semanticMemoryPrototypes = []string{
	"A stable personal profile fact about the user that will matter in future conversations.",
	"A durable user preference or dislike that should guide future choices.",
	"A firm user constraint, requirement, boundary, or correction.",
	"A current user state, configuration, location, relationship, or long-running goal.",
	"A personal event or experience the user may refer to in a later conversation.",
	"A confirmed task outcome, successful workflow, failed command, or reusable procedure.",
}

const genericAssistantPrototype = "A general explanation, trivia answer, tutorial, or unconfirmed assistant suggestion that does not describe the user."
