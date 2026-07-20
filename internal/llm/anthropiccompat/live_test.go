//go:build live

package anthropiccompat_test

// Opt-in live smoke test against a real Anthropic-compatible endpoint. Never runs in
// normal suites (build tag + env guards). Usage:
//
//	set -a; . ./.env; set +a
//	ADAMARKER_LIVE_IMAGE=/path/to/answer.jpg \
//	  go test -tags live -run TestLive -v ./internal/llm/anthropiccompat/
//
// It sends ONE synthetic (non-student) image through imaging.Mask and asserts the
// structured transcribe-then-grade output round-trips. Costs a fraction of a cent.

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/anthropiccompat"
)

func TestLive_QwenVisionGrade(t *testing.T) {
	apiKey := os.Getenv("QWEN_API_KEY")
	imgPath := os.Getenv("ADAMARKER_LIVE_IMAGE")
	if apiKey == "" || imgPath == "" {
		t.Skip("QWEN_API_KEY / ADAMARKER_LIVE_IMAGE not set")
	}
	baseURL := os.Getenv("QWEN_BASE_URL")
	if baseURL == "" {
		baseURL = "https://dashscope-intl.aliyuncs.com/apps/anthropic"
	}
	model := os.Getenv("ADAMARKER_LIVE_MODEL")
	if model == "" {
		model = "qwen3-vl-plus"
	}

	raw, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	masked, err := imaging.Mask(raw, []imaging.Region{{X: 0.24, Y: 0.03, W: 0.4, H: 0.12, Color: "#4a4a4a"}}, 85)
	if err != nil {
		t.Fatal(err)
	}

	schema := []byte(`{
		"type": "object",
		"properties": {
			"transcription": {"type": "string"},
			"confidence": {"type": "string", "enum": ["high", "medium", "low", "illegible"]},
			"overall_comment": {"type": "string"},
			"criteria": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"criterion_id": {"type": "integer", "enum": [1, 2]},
						"score": {"type": "number"},
						"rationale": {"type": "string"}
					},
					"required": ["criterion_id", "score", "rationale"]
				},
				"minItems": 2, "maxItems": 2
			}
		},
		"required": ["transcription", "confidence", "criteria"]
	}`)

	client := anthropiccompat.New("qwen", baseURL, apiKey)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	res, err := client.Grade(ctx, model, llm.Request{
		System: "You are a teaching assistant grading one handwritten answer against a rubric. Use the tool to submit your result.",
		Prompt: `# Problem 1 (max 10 points)
Give an O(n log n) sorting algorithm and justify its running time.

# Rubric
- criterion_id 1: Names a correct O(n log n) algorithm (0 to 4 points)
- criterion_id 2: Correct running-time justification (recurrence or tree argument) (0 to 6 points)

Transcribe the answer, then score each criterion with a rationale.`,
		Images:      []imaging.ProviderImage{masked},
		Schema:      schema,
		Temperature: 0,
	})
	if err != nil {
		t.Fatalf("live call failed: %v", err)
	}

	var out struct {
		Transcription string `json:"transcription"`
		Confidence    string `json:"confidence"`
		Criteria      []struct {
			CriterionID int64       `json:"criterion_id"`
			Score       json.Number `json:"score"`
			Rationale   string      `json:"rationale"`
		} `json:"criteria"`
	}
	if err := json.Unmarshal(res.JSON, &out); err != nil {
		t.Fatalf("structured output not parseable: %v\nraw: %s", err, res.JSON)
	}
	if len(out.Criteria) != 2 || out.Transcription == "" || out.Confidence == "" {
		t.Fatalf("unexpected output shape: %+v", out)
	}
	t.Logf("model=%s tokens in/out=%d/%d confidence=%s", res.Model, res.InputTokens, res.OutputTokens, out.Confidence)
	t.Logf("transcription: %.200s", out.Transcription)
	for _, c := range out.Criteria {
		t.Logf("criterion %d: %s — %.120s", c.CriterionID, c.Score.String(), c.Rationale)
	}
}
