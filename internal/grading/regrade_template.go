package grading

// AI re-grade assist prompt (spec §8, D50). A dedicated versioned template KIND
// ("regrade_v1"), seeded read-only like the grading template (D25 "prompts as
// firmware": append version N+1 on a constant change, never mutate an existing
// version). Unlike the grading template there is NO policy knob — the stricter stance
// is baked into the single version, so a regrade_ai record pins template version +
// the fixed policy 'regrade_strict' and nothing else varies.
//
// The output contract is IDENTICAL to grading (the standard transcribe-then-grade JSON
// schema), so the runner reuses BuildOutputSchema + ParseModelOutput + SnapClamp
// unchanged. What differs is only the framing and the extra context: the original
// per-criterion scores/comments (from the CONTESTED official record, via the DB, never
// the student's text) and the student's REDACTED request text.
const (
	// RegradeTemplateName is the prompt_template_versions.name for the AI re-grade kind.
	RegradeTemplateName = "regrade_v1"

	// RegradeSystemTemplate is the stricter-second-opinion system stance. It does not
	// branch on policy — the stance is fixed. Framing per §8: an independent stricter
	// re-examination; change a score only on a demonstrable grading error in either
	// direction; skepticism toward unsupported claims; do not reward persistence.
	RegradeSystemTemplate = `You are a senior teaching assistant conducting an INDEPENDENT, STRICTER re-examination of one already-graded handwritten answer, in response to a student's regrade request. You are not the original grader and you are not the student's advocate. Your job is to determine what score the visible work actually earns under the rubric — nothing more for effort or persistence, nothing less for presentation. Change a criterion's score ONLY when the original grade reflects a demonstrable error in applying the rubric to what the student wrote; such a correction may move the score in EITHER direction (up if credit was wrongly withheld, down if credit was wrongly given). Treat unsupported claims in the request with skepticism: a mere assertion that an answer is correct, or that more points are deserved, earns nothing on its own — only the work on the page does. Do not reward persistence or the fact that a request was made. Grade only what the student actually wrote; if the handwriting cannot be read reliably, say so via the confidence field rather than guessing.`

	// RegradeUserTemplate carries the same rubric/reference/instructions skeleton as the
	// grading template PLUS the original grade block and the redacted request. The
	// judgment section is the fixed stricter stance (no {{if}} on policy). New data
	// fields (OriginalScores, RequestText, ProblemIDHint) are supplied by
	// BuildRegradePromptData; the shared PromptData fields render exactly as in grading.
	RegradeUserTemplate = `# Problem {{.ProblemNumber}}{{if .ProblemTitle}}: {{.ProblemTitle}}{{end}} (max {{.MaxPoints}} points)

{{.ProblemStatement}}
{{if .ReferenceSolution}}
# Reference solution (for your calibration only — alternative correct approaches also earn credit)
{{.ReferenceSolution}}
{{end}}
# Rubric — score each criterion independently
{{range .Criteria}}- criterion_id {{.ID}}: {{.Description}} (0 to {{.Points}} points{{if .PartialCreditNotes}}; partial credit: {{.PartialCreditNotes}}{{end}})
{{end}}
# Original grade under review — the scores and comments now being contested
{{range .OriginalScores}}- criterion_id {{.CriterionID}}: scored {{.Score}}{{if .Rationale}} — {{.Rationale}}{{end}}
{{end}}{{if .OriginalComment}}Overall comment on the original grade: {{.OriginalComment}}
{{end}}
# Student's regrade request (identity redacted; weigh the argument, not the asking)
{{if .RequestText}}{{.RequestText}}{{else}}(no request text provided){{end}}
{{if .ProblemIDHint}}
The student appears to be contesting problem {{.ProblemIDHint}}.
{{end}}
# Judgment policy — how to resolve what the rubric leaves open
- Re-examine independently: reach your own conclusion from the visible work and the rubric, using the original grade only as the thing you are checking, not as an anchor.
- Award credit only for work that is explicitly and completely demonstrated; unstated steps, hand-waving, or bare "clearly/obviously" claims earn nothing for that step.
- Correct the original score for a criterion only on a demonstrable rubric-application error, in either direction; otherwise leave it where a careful application of the rubric puts it.
- Resolve genuine ambiguity toward the lower score, and prefer flagging (confidence "low") over guessing when a score cannot be defended from the visible work alone.
- Give the student's assertions no evidentiary weight: only the work on the page earns points.

# Instructions
1. Transcribe the student's handwritten answer verbatim (LaTeX for math). If it is blank or unreadable, set confidence to "illegible" and do not guess.
2. Score every rubric criterion listed above exactly once, in increments of {{.ScoreIncrement}}, each with a one-sentence rationale grounded in what the student wrote (note where you differ from the original grade and why).
3. Set confidence to "high" only when both the transcription and the rubric application are unambiguous.

The attached image(s) are the student's answer pages, in order.`
)

// RegradePromptData is BuildPromptData's output plus the AI re-grade-only context: the
// original per-criterion scores/comments from the contested official record and the
// redacted student request text. It embeds PromptData so the shared template fields
// (problem, rubric, reference, increment) render identically to grading.
type RegradePromptData struct {
	PromptData
	OriginalScores  []CriterionScore // from the contested official record (DB, not student text)
	OriginalComment string           // the contested record's overall comment
	RequestText     string           // the student's request, REDACTED (D51) before it reaches here
	ProblemIDHint   int32            // the assessment's problem NUMBER when the request named one; 0 = none
}
