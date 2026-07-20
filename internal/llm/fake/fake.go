// Package fake is a deterministic offline llm.Provider for tests and dev
// (docs/DECISIONS.md D11): the whole run pipeline is testable without a
// network or an API key.
//
// By default Grade fabricates a plausible grading payload by reading the
// criterion ids out of the request's JSON Schema
// (properties.criteria.items.properties.criterion_id.enum). A Script of Steps
// can force errors or malformed-but-parseable JSON to exercise retry and
// re-ask paths.
package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/HaoWen46/adagrade/internal/llm"
)

// Step scripts the behavior of one Grade call, consumed in order. Fields are
// checked in order: Err, then MalformedJSON, then Confidence override; a zero
// Step is just the default behavior.
type Step struct {
	Err           error  // return this error
	MalformedJSON bool   // return {"not":"valid-per-schema"} (parseable JSON, wrong shape)
	Confidence    string // override confidence for this call
}

// Provider is a deterministic, thread-safe fake. The zero value is usable.
type Provider struct {
	NameStr           string // default "fake"
	Script            []Step // optional; consumed in order, then default behavior
	ScorePerCriterion string // default "1"; must be a JSON number
	Confidence        string // default "high"

	mu    sync.Mutex
	Calls []llm.Request // recorded (for assertions); guard with mu when racing Grade
}

// Name implements llm.Provider.
func (p *Provider) Name() string {
	if p.NameStr == "" {
		return "fake"
	}
	return p.NameStr
}

// Grade implements llm.Provider. It records the request, applies the scripted
// Step for this call position (if any), then emits the canned grading JSON
// derived from the schema's criterion_id enum.
func (p *Provider) Grade(_ context.Context, model string, req llm.Request) (llm.Result, error) {
	p.mu.Lock()
	call := len(p.Calls) // position of this call; indexes Script
	p.Calls = append(p.Calls, req)

	var step Step
	if call < len(p.Script) {
		step = p.Script[call]
	}
	confidence := firstNonEmpty(step.Confidence, p.Confidence, "high")
	score := firstNonEmpty(p.ScorePerCriterion, "1")
	p.mu.Unlock()

	if step.Err != nil {
		return llm.Result{}, step.Err
	}

	result := llm.Result{Model: model, InputTokens: 100, OutputTokens: 50}

	if step.MalformedJSON {
		result.JSON = []byte(`{"not":"valid-per-schema"}`)
		return result, nil
	}

	ids, ok := criterionIDs(req.Schema)
	if !ok {
		return llm.Result{}, fmt.Errorf("fake: schema missing criteria criterion_id enum: %w", llm.ErrNoStructuredOutput)
	}

	scoreNum := json.Number(score)
	if _, err := strconv.ParseFloat(score, 64); err != nil {
		scoreNum = "1" // ScorePerCriterion must stay a JSON number
	}

	type critOut struct {
		CriterionID json.Number `json:"criterion_id"`
		Score       json.Number `json:"score"`
		Rationale   string      `json:"rationale"`
	}
	out := struct {
		Transcription  string    `json:"transcription"`
		Confidence     string    `json:"confidence"`
		OverallComment string    `json:"overall_comment"`
		Criteria       []critOut `json:"criteria"`
	}{
		Transcription:  "(fake transcription)",
		Confidence:     confidence,
		OverallComment: "fake comment",
		Criteria:       make([]critOut, 0, len(ids)),
	}
	for _, id := range ids {
		out.Criteria = append(out.Criteria, critOut{CriterionID: id, Score: scoreNum, Rationale: "fake rationale"})
	}

	js, err := json.Marshal(out)
	if err != nil {
		return llm.Result{}, fmt.Errorf("fake: marshal canned output: %w", err)
	}
	result.JSON = js
	return result, nil
}

// criterionIDs walks schema to properties.criteria.items.properties.
// criterion_id.enum and returns the numeric entries. ok is false when the
// schema is missing, unparseable, or lacks that path.
func criterionIDs(schema []byte) ([]json.Number, bool) {
	if len(schema) == 0 {
		return nil, false
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(schema))
	dec.UseNumber() // keep enum entries as json.Number so 1 stays "1", not "1e+00"
	if err := dec.Decode(&root); err != nil {
		return nil, false
	}

	cur := any(root)
	for _, key := range []string{"properties", "criteria", "items", "properties", "criterion_id", "enum"} {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	entries, ok := cur.([]any)
	if !ok {
		return nil, false
	}
	ids := make([]json.Number, 0, len(entries))
	for _, e := range entries {
		n, ok := e.(json.Number)
		if !ok {
			return nil, false // enum must be numbers
		}
		ids = append(ids, n)
	}
	return ids, true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// JSONStep scripts one Grade call for the ScriptedProvider: either an error
// (returned as-is, so tests can inject *llm.RateLimitError / *llm.
// ProviderUnavailableError to exercise the retry taxonomy) or a canned JSON
// payload placed verbatim into Result.JSON (a wrong-shape payload exercises the
// strict-parse re-ask path). Err takes precedence when both are set.
type JSONStep struct {
	Err  error
	JSON string
}

// ScriptedProvider returns canned JSON (or errors) for each Grade call in
// sequence, independent of the request schema — the identity path uses a schema
// with no criterion_id enum, which the default Provider can't fabricate. Once the
// script is exhausted it repeats the last step (or errors if empty). It is thread
// safe; the zero value with a nil script errors on first use.
type ScriptedProvider struct {
	NameStr string     // default "fake-scripted"
	Steps   []JSONStep // consumed in order; last step repeats when exhausted

	mu    sync.Mutex
	Calls []llm.Request
}

// Name implements llm.Provider.
func (p *ScriptedProvider) Name() string {
	if p.NameStr == "" {
		return "fake-scripted"
	}
	return p.NameStr
}

// Grade implements llm.Provider by replaying the next scripted step.
func (p *ScriptedProvider) Grade(_ context.Context, model string, req llm.Request) (llm.Result, error) {
	p.mu.Lock()
	call := len(p.Calls)
	p.Calls = append(p.Calls, req)
	var step JSONStep
	switch {
	case len(p.Steps) == 0:
		p.mu.Unlock()
		return llm.Result{}, fmt.Errorf("fake: ScriptedProvider has no steps")
	case call < len(p.Steps):
		step = p.Steps[call]
	default:
		step = p.Steps[len(p.Steps)-1]
	}
	p.mu.Unlock()

	if step.Err != nil {
		return llm.Result{}, step.Err
	}
	return llm.Result{Model: model, JSON: []byte(step.JSON), InputTokens: 10, OutputTokens: 5}, nil
}

// CallCount returns how many Grade calls have been recorded (thread safe).
func (p *ScriptedProvider) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Calls)
}
