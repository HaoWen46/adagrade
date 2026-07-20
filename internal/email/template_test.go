package email

import (
	"strings"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/domain"
)

func sampleGradeEmailData(corrected bool) GradeEmailData {
	return GradeEmailData{
		AssessmentName: "Midterm 2",
		StudentName:    "Student One",
		Problems: []ProblemBreakdown{
			{
				Label: "Problem 1",
				Criteria: []CriterionLine{
					{Name: "Correctness", Score: "8", Max: "10", Comment: "Minor sign error."},
					{Name: "Style", Score: "5", Max: "5", Comment: ""},
				},
			},
			{
				Label: "Problem 2",
				Criteria: []CriterionLine{
					{Name: "Proof", Score: "12", Max: "15", Comment: "Missing base case."},
				},
			},
		},
		Total:           "25",
		Max:             "30",
		ReplyTo:         "regrade+v1.9.123.sig@inbound.example.edu",
		RegradeDeadline: time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC),
		Corrected:       corrected,
		FormatTemplate:  RegradeReplyFormatTemplate,
	}
}

func TestRenderGradeEmail_SubjectAndReplyTo(t *testing.T) {
	d := sampleGradeEmailData(false)

	out, err := RenderGradeEmail(d)
	if err != nil {
		t.Fatal(err)
	}
	if out.Subject != "Midterm 2 — results" {
		t.Errorf("subject: got %q", out.Subject)
	}
	// RenderGradeEmail does not address the message — GradeEmailData carries no
	// recipient field (per brief); the caller (publish service, which has the
	// roster email) sets OutboundEmail.To before calling EmailProvider.Send.
	if out.To != "" {
		t.Errorf("To must be left for the caller to set: got %q", out.To)
	}
	if out.ReplyTo != d.ReplyTo {
		t.Errorf("reply-to: got %q want %q", out.ReplyTo, d.ReplyTo)
	}
}

func TestRenderGradeEmail_TextBodyKeyLines(t *testing.T) {
	d := sampleGradeEmailData(false)
	out, err := RenderGradeEmail(d)
	if err != nil {
		t.Fatal(err)
	}
	wantSubstrings := []string{
		"Midterm 2",
		"Student One",
		"Problem 1",
		"Correctness",
		"8/10",
		"Minor sign error.",
		"Style",
		"5/5",
		"Problem 2",
		"Proof",
		"12/15",
		"Missing base case.",
		"25/30",
		"2026-07-17",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(out.TextBody, s) {
			t.Errorf("text body missing %q\n---\n%s", s, out.TextBody)
		}
	}
}

func TestRenderGradeEmail_HTMLBodyKeyLines(t *testing.T) {
	d := sampleGradeEmailData(false)
	out, err := RenderGradeEmail(d)
	if err != nil {
		t.Fatal(err)
	}
	wantSubstrings := []string{
		"Midterm 2",
		"Student One",
		"Problem 1",
		"Correctness",
		"25/30",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(out.HTMLBody, s) {
			t.Errorf("html body missing %q\n---\n%s", s, out.HTMLBody)
		}
	}
	if !strings.Contains(out.HTMLBody, "<html") {
		t.Error("html body does not look like HTML")
	}
}

func TestRenderGradeEmail_HTMLEscapesStudentContent(t *testing.T) {
	d := sampleGradeEmailData(false)
	d.StudentName = "Bobby <script>alert(1)</script>"
	d.Problems[0].Criteria[0].Comment = "<b>injected</b> & more"

	out, err := RenderGradeEmail(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.HTMLBody, "<script>") {
		t.Error("html/template must escape student-controlled content")
	}
	if strings.Contains(out.HTMLBody, "<b>injected</b>") {
		t.Error("html/template must escape comment content")
	}
}

func TestRenderGradeEmail_CorrectedFlagChangesLanguage(t *testing.T) {
	notCorrected, err := RenderGradeEmail(sampleGradeEmailData(false))
	if err != nil {
		t.Fatal(err)
	}
	corrected, err := RenderGradeEmail(sampleGradeEmailData(true))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(notCorrected.TextBody, "corrected") || strings.Contains(notCorrected.Subject, "orrected") {
		t.Errorf("non-corrected send must not claim to be corrected: subject=%q", notCorrected.Subject)
	}
	if !strings.Contains(corrected.TextBody, "orrected") && !strings.Contains(corrected.Subject, "orrected") {
		t.Errorf("corrected re-publish must say so somewhere: subject=%q body=%q", corrected.Subject, corrected.TextBody)
	}
}

// TestRenderGradeEmail_ReplyToIncludesFormatBlock covers the 2026-07-10 UX fix
// (D55 amendment): a monitored grade email must show the literal <pN> reply
// format so students can file a parseable regrade without guessing.
func TestRenderGradeEmail_ReplyToIncludesFormatBlock(t *testing.T) {
	d := sampleGradeEmailData(false)
	out, err := RenderGradeEmail(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.TextBody, "exactly this format") {
		t.Errorf("text body missing the format instruction sentence:\n%s", out.TextBody)
	}
	if !strings.Contains(out.TextBody, d.FormatTemplate) {
		t.Errorf("text body missing the literal format template:\n%s", out.TextBody)
	}
	// HTML must show the tags as escaped literals inside a <pre> block — a raw
	// <p1> would be swallowed as markup by mail clients.
	if !strings.Contains(out.HTMLBody, "<pre>") {
		t.Errorf("html body missing the <pre> format block:\n%s", out.HTMLBody)
	}
	if !strings.Contains(out.HTMLBody, "&lt;p1&gt;") || !strings.Contains(out.HTMLBody, "&lt;/p2&gt;") {
		t.Errorf("html body must escape the literal tags:\n%s", out.HTMLBody)
	}
	if strings.Contains(out.HTMLBody, "<p1>") {
		t.Errorf("html body must not contain a raw unescaped <p1> tag:\n%s", out.HTMLBody)
	}
}

func TestRenderGradeEmail_NoReplyToOmitsFormatBlock(t *testing.T) {
	d := sampleGradeEmailData(false)
	d.ReplyTo = ""
	out, err := RenderGradeEmail(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{out.TextBody, out.HTMLBody} {
		if strings.Contains(body, "p1>") || strings.Contains(body, "this format") {
			t.Errorf("unmonitored email must omit the reply-format block:\n%s", body)
		}
	}
}

func TestRenderGradeEmail_NoReplyToOmitsRegradeInstructions(t *testing.T) {
	d := sampleGradeEmailData(false)
	d.ReplyTo = ""

	out, err := RenderGradeEmail(d)
	if err != nil {
		t.Fatal(err)
	}
	if out.ReplyTo != "" {
		t.Errorf("ReplyTo must stay empty: got %q", out.ReplyTo)
	}
	if strings.Contains(out.TextBody, "regrade") {
		t.Errorf("body must not mention regrade instructions when replies aren't monitored:\n%s", out.TextBody)
	}
	if !strings.Contains(out.TextBody, "not monitored") {
		t.Errorf("body should say replies are not monitored:\n%s", out.TextBody)
	}
}

// TestRenderGradeEmail_ProblemCommentRendersAsOwnNoteLine covers B4: a
// whole-problem comment (e.g. a grader's overall note, or a regrade
// adjudication note folded back into the next grade email) must render as its
// own "Note: …" line under the problem, not appended onto the last
// criterion's score line — where it previously read as if it belonged to
// that one criterion instead of the problem as a whole.
func TestRenderGradeEmail_ProblemCommentRendersAsOwnNoteLine(t *testing.T) {
	d := sampleGradeEmailData(false)
	d.Problems[0].Comment = "Regrade turn 2: awarded partial credit for the alternate approach."

	out, err := RenderGradeEmail(d)
	if err != nil {
		t.Fatal(err)
	}

	wantLine := "Note: Regrade turn 2: awarded partial credit for the alternate approach."
	if !strings.Contains(out.TextBody, wantLine) {
		t.Errorf("text body missing own Note line:\n%s", out.TextBody)
	}
	// The problem comment must not be glued onto the last criterion's score
	// line (the B4 bug): "Style" is Problem 1's last criterion and carries no
	// per-criterion comment of its own, so its line must stay bare.
	if strings.Contains(out.TextBody, "Style: 5/5 — Regrade turn 2") {
		t.Errorf("problem comment must not be appended to a criterion line:\n%s", out.TextBody)
	}
	// The Note line must sit under Problem 1, before Problem 2's block starts.
	noteIdx := strings.Index(out.TextBody, wantLine)
	problem2Idx := strings.Index(out.TextBody, "Problem 2")
	if noteIdx == -1 || problem2Idx == -1 || noteIdx > problem2Idx {
		t.Errorf("Note line must be placed under Problem 1, before Problem 2:\n%s", out.TextBody)
	}

	if !strings.Contains(out.HTMLBody, "Note: Regrade turn 2: awarded partial credit for the alternate approach.") {
		t.Errorf("html body missing own Note line:\n%s", out.HTMLBody)
	}
	if strings.Contains(out.HTMLBody, "Style: 5/5 — Regrade turn 2") {
		t.Errorf("html: problem comment must not be appended to a criterion line:\n%s", out.HTMLBody)
	}
	htmlNoteIdx := strings.Index(out.HTMLBody, "Note: Regrade turn 2")
	htmlProblem2Idx := strings.Index(out.HTMLBody, "Problem 2")
	if htmlNoteIdx == -1 || htmlProblem2Idx == -1 || htmlNoteIdx > htmlProblem2Idx {
		t.Errorf("html: Note line must be placed under Problem 1, before Problem 2:\n%s", out.HTMLBody)
	}
}

// TestRenderGradeEmail_NoProblemCommentOmitsNoteLine is the negative case: the
// sample fixture's problems carry no whole-problem Comment, so neither
// rendered body should show a "Note:" line at all.
func TestRenderGradeEmail_NoProblemCommentOmitsNoteLine(t *testing.T) {
	d := sampleGradeEmailData(false)
	out, err := RenderGradeEmail(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.TextBody, "Note:") {
		t.Errorf("no problem carries a Comment; text body must not show a Note line:\n%s", out.TextBody)
	}
	if strings.Contains(out.HTMLBody, "Note:") {
		t.Errorf("no problem carries a Comment; html body must not show a Note line:\n%s", out.HTMLBody)
	}
}

// TestRenderGradeEmail_HTMLEscapesProblemComment mirrors
// TestRenderGradeEmail_HTMLEscapesStudentContent for the new field: a
// whole-problem Comment is still student-facing content and must go through
// html/template's auto-escaping like every other field.
func TestRenderGradeEmail_HTMLEscapesProblemComment(t *testing.T) {
	d := sampleGradeEmailData(false)
	d.Problems[0].Comment = "<b>injected</b> & more"

	out, err := RenderGradeEmail(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.HTMLBody, "<b>injected</b>") {
		t.Error("html/template must escape the problem comment content")
	}
}

func TestRenderRegradeConfirmation_KeyLines(t *testing.T) {
	d := RegradeConfirmationData{
		AssessmentName:   "Midterm 2",
		StudentName:      "Student Two",
		ReceivedAt:       time.Date(2026, 7, 5, 9, 30, 0, 0, time.UTC),
		FiledProblemNums: []int{2},
		Turn:             1,
		MaxTurns:         3,
	}
	out, err := RenderRegradeConfirmation(d)
	if err != nil {
		t.Fatal(err)
	}
	if out.To != "" {
		t.Errorf("To must be left for the caller to set: got %q", out.To)
	}
	for _, s := range []string{"Midterm 2", "Student Two", "received"} {
		if !strings.Contains(out.TextBody, s) {
			t.Errorf("confirmation text missing %q:\n%s", s, out.TextBody)
		}
	}
	if !strings.Contains(out.HTMLBody, "<html") {
		t.Error("confirmation html body does not look like HTML")
	}
}

func TestRenderRegradeResolution_UpheldVsRegraded(t *testing.T) {
	base := RegradeResolutionData{
		AssessmentName: "Midterm 2",
		StudentName:    "Student Three",
		ResolutionNote: "Re-checked part (b); original score stands.",
	}

	upheld := base
	upheld.Outcome = "upheld"
	outUpheld, err := RenderRegradeResolution(upheld)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(outUpheld.TextBody), "upheld") {
		t.Errorf("upheld resolution should say so:\n%s", outUpheld.TextBody)
	}
	if !strings.Contains(outUpheld.TextBody, "Re-checked part (b)") {
		t.Errorf("resolution note missing:\n%s", outUpheld.TextBody)
	}

	regraded := base
	regraded.Outcome = "regraded"
	regraded.NewTotal = "27"
	regraded.Max = "30"
	outRegraded, err := RenderRegradeResolution(regraded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(outRegraded.TextBody), "updated") && !strings.Contains(strings.ToLower(outRegraded.TextBody), "regraded") {
		t.Errorf("regraded resolution should say so:\n%s", outRegraded.TextBody)
	}
	if !strings.Contains(outRegraded.TextBody, "27/30") {
		t.Errorf("new total missing:\n%s", outRegraded.TextBody)
	}
}

// TestRenderRegradeResolution_RegradedOmitsTotalWhenUnknown covers C2's no-live-data
// case: a "regraded" resolution with no NewTotal available must say the grade was
// updated but MUST NOT print a bare "New total: /" — the line is omitted entirely.
func TestRenderRegradeResolution_RegradedOmitsTotalWhenUnknown(t *testing.T) {
	d := RegradeResolutionData{
		AssessmentName: "Midterm 2",
		StudentName:    "Student Five",
		Outcome:        "regraded",
		// NewTotal/Max intentionally empty — caller could not determine a fresh total.
	}
	out, err := RenderRegradeResolution(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{out.TextBody, out.HTMLBody} {
		if !strings.Contains(strings.ToLower(body), "updated") {
			t.Errorf("regraded resolution should still say the grade was updated:\n%s", body)
		}
		if strings.Contains(body, "New total") {
			t.Errorf("regraded resolution with no total must omit the New total line, got:\n%s", body)
		}
		if strings.Contains(body, "/.") || strings.Contains(body, ": /") {
			t.Errorf("regraded resolution must not render a bare slash total:\n%s", body)
		}
	}
}

func TestRenderRegradeResolution_RejectsUnknownOutcome(t *testing.T) {
	d := RegradeResolutionData{
		AssessmentName: "Midterm 2",
		StudentName:    "Student Four",
		Outcome:        "bogus",
	}
	if _, err := RenderRegradeResolution(d); err == nil {
		t.Fatal("unknown outcome must error")
	}
}

// Guard: RenderGradeEmail's return type is exactly domain.OutboundEmail (the
// interface Q3/pipeline code depends on), not a locally-defined lookalike.
var _ = func() domain.OutboundEmail {
	out, _ := RenderGradeEmail(sampleGradeEmailData(false))
	return out
}
