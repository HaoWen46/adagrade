package transcribe

import (
	"encoding/json"
	"fmt"
)

// ToolName is the forced-tool name for the transcription call, matching the
// repo's structured-output convention (internal/llm/openaicompat uses a forced
// function call rather than trusting free-form JSON).
const ToolName = "submit_transcription"

// PromptVersion is part of the transcription cache key. Bump it whenever the
// prompt or schema below changes: a cached transcription produced by an older
// contract must not be silently reused under a newer one, or an emitter fix
// would appear to work while old rows kept their old shape.
const PromptVersion = "transcribe/v1"

// SystemPrompt frames the task. The two hard rules exist because of documented
// 2026 failure modes: models emit fluent, compilable, entirely fabricated LaTeX
// when a page is hard to read, and they "helpfully" tidy a student's work into
// something better than what was written — which would silently corrupt the
// professor's grading input.
const SystemPrompt = `You transcribe one student's handwritten exam answer from an algorithms course. The pages may mix English, Traditional Chinese (Taiwan), mathematics, and pseudocode.

Transcribe EXACTLY what is written. Two rules override everything else:

1. NEVER invent, complete, correct, or improve the student's work. If the student wrote something wrong, transcribe the wrong thing. If a step is missing, leave it missing. You are a transcriber, not a tutor.
2. NEVER guess at text you cannot read. Mark unreadable spans as [illegible] and lower your confidence. A confident wrong transcription is far worse than an admitted gap, because it will be graded as if the student wrote it.

Do not write LaTeX documents, preambles, or environments. Emit only the structured blocks described by the tool schema.`

// UserPrompt is the per-answer instruction. It is deliberately free of any
// student identifier: identity is bound locally from the ID crop, never asked
// of the provider.
func UserPrompt(problemNumber int) string {
	return fmt.Sprintf(`Transcribe the attached answer page(s) for Problem %d, in order.

Block kinds:
- "prose": running text. Put mathematics inline between single dollar signs, e.g. "runs in $O(n \log n)$ time". Traditional Chinese stays as-is, unescaped.
- "math": one displayed equation. Give the LaTeX WITHOUT delimiters — "T(n) = 2T(n/2) + O(n)", not "$$...$$".
- "code": pseudocode, verbatim, newlines and indentation preserved exactly. Indentation carries meaning here; do not reflow it.
- "list": enumerated points; each item follows the "prose" rules.

Use only standard mathematical LaTeX commands (\frac, \sum, \leq, \begin{aligned}, …). Never use \def, \newcommand, \input, or any macro-defining or file-reading command — such fragments are discarded.

If a page is blank, return no blocks and set confidence to "illegible".`, problemNumber)
}

// BuildSchema returns the constrained-decoding JSON schema for the block
// contract. It mirrors internal/grading.BuildOutputSchema's shape (closed
// objects, explicit enums) so both paths behave the same under strict decoders.
func BuildSchema() []byte {
	block := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{
				"type": "string",
				"enum": []string{string(BlockProse), string(BlockMath), string(BlockCode), string(BlockList)},
			},
			"text": map[string]any{
				"type":        "string",
				"description": "Content for prose, math, and code blocks. Empty for list blocks.",
			},
			"items": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Content for list blocks. Empty for all other kinds.",
			},
		},
		"required":             []string{"kind", "text", "items"},
		"additionalProperties": false,
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"blocks": map[string]any{"type": "array", "items": block},
			"confidence": map[string]any{
				"type": "string",
				"enum": []string{"high", "medium", "low", "illegible"},
			},
		},
		"required":             []string{"blocks", "confidence"},
		"additionalProperties": false,
	}
	b, _ := json.Marshal(schema)
	return b
}

// wireDoc is the model's response shape.
type wireDoc struct {
	Blocks []struct {
		Kind  string   `json:"kind"`
		Text  string   `json:"text"`
		Items []string `json:"items"`
	} `json:"blocks"`
	Confidence string `json:"confidence"`
}

// ParseResponse validates a model response into a Doc. Unknown block kinds are
// rejected rather than coerced: the emitter switches exhaustively on kind, so a
// silently-dropped block would read as "the student wrote nothing here", which
// is exactly the ambiguity the whole export contract works to eliminate.
func ParseResponse(raw []byte) (Doc, string, error) {
	var w wireDoc
	if err := json.Unmarshal(raw, &w); err != nil {
		// Never quote raw: it is the student's answer text.
		return Doc{}, "", fmt.Errorf("transcribe: response is not valid JSON (content withheld): %w", err)
	}
	switch w.Confidence {
	case "high", "medium", "low", "illegible":
	default:
		return Doc{}, "", fmt.Errorf("transcribe: confidence %q is not one of high/medium/low/illegible", w.Confidence)
	}

	doc := Doc{Blocks: make([]Block, 0, len(w.Blocks))}
	for i, b := range w.Blocks {
		kind := BlockKind(b.Kind)
		switch kind {
		case BlockProse, BlockMath, BlockCode, BlockList:
		default:
			return Doc{}, "", fmt.Errorf("transcribe: block %d has unknown kind %q", i, b.Kind)
		}
		// The model sometimes files content in the wrong field (a list's text
		// in "text", a prose block's sentences split into "items"). Re-file
		// rather than drop: dropping would silently erase student writing,
		// which is the one thing this pipeline may never do (2026-07-30 audit).
		if kind == BlockList && len(b.Items) == 0 {
			if b.Text == "" {
				continue // no content anywhere; dropping is lossless
			}
			doc.Blocks = append(doc.Blocks, Block{Kind: BlockProse, Text: b.Text})
			continue
		}
		if kind != BlockList && b.Text == "" {
			if len(b.Items) == 0 {
				continue // likewise
			}
			doc.Blocks = append(doc.Blocks, Block{Kind: BlockList, Items: b.Items})
			continue
		}
		doc.Blocks = append(doc.Blocks, Block{Kind: kind, Text: b.Text, Items: b.Items})
	}
	return doc, w.Confidence, nil
}
