package grading

import (
	"bytes"
	"encoding/json"
	"fmt"
	"text/template"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// PromptData is what a prompt template renders against.
type PromptData struct {
	ProblemNumber     int32
	ProblemTitle      string
	ProblemStatement  string
	MaxPoints         string
	ScoreIncrement    string
	ReferenceSolution string
	Policy            string // grading stance (D25): lenient|standard|strict
	Criteria          []PromptCriterion
}

// PromptCriterion is one rubric criterion as seen by the template.
type PromptCriterion struct {
	ID                 int64
	Description        string
	Points             string
	PartialCreditNotes string
}

// BuildPromptData assembles template data from the pinned versions. policy is the
// method's configured grading stance (D25); pass the parsed config's Policy so the
// preview renders exactly what the runner ships.
func BuildPromptData(problem db.Problem, rv db.RubricVersion, criteria []db.RubricCriterium, refSolution, policy string) PromptData {
	d := PromptData{
		ProblemNumber:     problem.Number,
		ProblemTitle:      problem.Title,
		ProblemStatement:  problem.Statement,
		MaxPoints:         store.NumStr(problem.MaxPoints),
		ScoreIncrement:    store.NumStr(rv.ScoreIncrement),
		ReferenceSolution: refSolution,
		Policy:            policy,
	}
	for _, c := range criteria {
		d.Criteria = append(d.Criteria, PromptCriterion{
			ID: c.ID, Description: c.Description,
			Points: store.NumStr(c.Points), PartialCreditNotes: c.PartialCreditNotes,
		})
	}
	return d
}

// RenderPrompt executes one template body against the data.
func RenderPrompt(tmpl string, data PromptData) (string, error) {
	t, err := template.New("prompt").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("prompt template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("prompt render: %w", err)
	}
	return buf.String(), nil
}

// BuildOutputSchema constructs the constrained-decoding JSON schema for the
// transcribe-then-grade output (spec §5). Numeric bounds are NOT expressed here —
// strict decoders drop them; the app clamps in Go (D4).
func BuildOutputSchema(criteria []db.RubricCriterium) []byte {
	ids := make([]int64, 0, len(criteria))
	for _, c := range criteria {
		ids = append(ids, c.ID)
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"transcription": map[string]any{
				"type":        "string",
				"description": "Verbatim transcription of the handwritten answer; LaTeX for math/pseudocode.",
			},
			"confidence": map[string]any{
				"type": "string",
				"enum": []string{"high", "medium", "low", "illegible"},
			},
			"overall_comment": map[string]any{"type": "string"},
			"criteria": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"criterion_id": map[string]any{"type": "integer", "enum": ids},
						"score":        map[string]any{"type": "number"},
						"rationale":    map[string]any{"type": "string"},
					},
					"required":             []string{"criterion_id", "score", "rationale"},
					"additionalProperties": false,
				},
				"minItems": len(criteria),
				"maxItems": len(criteria),
			},
		},
		"required":             []string{"transcription", "confidence", "criteria"},
		"additionalProperties": false,
	}
	b, _ := json.Marshal(schema)
	return b
}

// The seeded default prompt template (versioned data from day one, D5). The template
// carries all three policy branches (D25); {{.Policy}} selects one at render time, so
// the versioned text is reproducible and each record pins template version + policy.
const (
	DefaultTemplateName = "transcribe-then-grade"

	DefaultSystemTemplate = `You are a teaching assistant grading one student's handwritten answer to one exam problem in an algorithms course. {{if eq .Policy "lenient"}}You are grading formative work: grade generously but honestly — the goal is to reward demonstrated understanding, not to police presentation.{{else if eq .Policy "strict"}}You are grading a high-stakes exam: every point you award must be defensible to a colleague from the visible work alone, and the burden of demonstration is on the student.{{else}}You are strict but fair: award exactly the credit the rubric supports — no more for effort, no less for presentation.{{end}} Grade only what the student actually wrote — never invent content, and never let presentation quality influence scores beyond what the rubric says. Score each rubric criterion independently. If the handwriting cannot be read reliably, say so via the confidence field instead of guessing.`

	DefaultUserTemplate = `# Problem {{.ProblemNumber}}{{if .ProblemTitle}}: {{.ProblemTitle}}{{end}} (max {{.MaxPoints}} points)

{{.ProblemStatement}}
{{if .ReferenceSolution}}
# Reference solution (for your calibration only — alternative correct approaches also earn credit)
{{.ReferenceSolution}}
{{end}}
# Rubric — score each criterion independently
{{range .Criteria}}- criterion_id {{.ID}}: {{.Description}} (0 to {{.Points}} points{{if .PartialCreditNotes}}; partial credit: {{.PartialCreditNotes}}{{end}})
{{end}}
# Judgment policy — how to resolve what the rubric leaves open
{{if eq .Policy "lenient"}}- When the work shows the right idea but the execution is incomplete or sloppy, award the credit that idea earns under the rubric.
- Resolve genuine ambiguity in the student's favor: if a step can plausibly be read as correct, read it as correct.
- Do not cascade errors: after a slip (arithmetic, copying), grade the following steps on their own logic.
- Reserve "low" or "illegible" confidence for work that is truly unreadable or absent.
{{else if eq .Policy "strict"}}- Award credit only for work that is explicitly and completely demonstrated; unstated steps, hand-waving, or bare "clearly/obviously" claims earn nothing for that step.
- Resolve genuine ambiguity toward the lower score.
- Do not give follow-through credit after an error unless the rubric's partial-credit notes explicitly grant it.
- Prefer flagging over guessing: when a score cannot be defended from the visible work alone, set confidence to "low" rather than award the benefit of the doubt.
{{else}}- Award exactly the credit the rubric text supports — no more for effort, no less for presentation.
- Resolve genuine ambiguity the way a careful human grader most plausibly would; if two readings are equally plausible, do not choose an extreme, and lower your confidence.
- Give follow-through credit after an error only where the rubric's partial-credit notes allow it.
- Set confidence to "low" when the rubric application is genuinely debatable.
{{end}}
# Instructions
1. Transcribe the student's handwritten answer verbatim (LaTeX for math). If it is blank or unreadable, set confidence to "illegible" and do not guess.
2. Score every rubric criterion listed above exactly once, in increments of {{.ScoreIncrement}}, each with a one-sentence rationale grounded in what the student wrote.
3. Set confidence to "high" only when both the transcription and the rubric application are unambiguous.

The attached image(s) are the student's answer pages, in order.`
)

// PolicyInfo is UI-facing copy for one grading policy (D25). This is the single home
// for the human-readable catalog; httpapi surfaces it directly.
type PolicyInfo struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Tagline   string `json:"tagline"`
	WhenToUse string `json:"when_to_use"`
}

// Policies is the ordered catalog shown in method-config UIs (D25).
var Policies = []PolicyInfo{
	{
		Key:       PolicyLenient,
		Label:     "Lenient",
		Tagline:   "Benefit of the doubt — reward the visible idea; ambiguity resolves in the student's favor.",
		WhenToUse: "Formative homework and practice, where the goal is learning and encouragement.",
	},
	{
		Key:       PolicyStandard,
		Label:     "Standard",
		Tagline:   "Rubric-faithful — exactly what the rubric supports, as a careful human TA would.",
		WhenToUse: "The default for most grading; consistent and defensible.",
	},
	{
		Key:       PolicyStrict,
		Label:     "Strict",
		Tagline:   "Exam standard — only complete, demonstrated work earns points; prefers flagging over guessing.",
		WhenToUse: "Finals and any grading that must survive appeals; also useful as a calibration probe.",
	},
}
