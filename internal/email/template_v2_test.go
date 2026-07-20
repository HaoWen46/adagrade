package email

import (
	"strings"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/domain"
)

// --- RenderRegradeResult (spec §5) -------------------------------------------------

func sampleResultData(turn, maxTurns int, replyTo string) ResultData {
	return ResultData{
		AssessmentName: "Midterm 2",
		StudentName:    "Student One",
		Turn:           turn,
		MaxTurns:       maxTurns,
		Problems: []ResultProblem{
			{
				Number:    1,
				Complaint: "The base case n=1 was marked wrong.",
				Outcome:   "regraded",
				Note:      "Base case does earn partial credit per rubric line 2.",
				NewScore:  "9",
				Max:       "10",
			},
			{
				Number:    4,
				Complaint: "My exchange argument handles ties.",
				Outcome:   "upheld",
				Note:      "The -2 deduction stands; ties are not handled correctly.",
			},
		},
		ReplyTo:        replyTo,
		FormatTemplate: "<p1>\nyour complaint here\n</p1>",
	}
}

func TestRenderRegradeResult_Subject(t *testing.T) {
	d := sampleResultData(1, 3, "regrade+v2.9.2.123.sig@inbound.example.edu")
	out, err := RenderRegradeResult(d)
	if err != nil {
		t.Fatal(err)
	}
	want := "Midterm 2 — regrade result #1"
	if out.Subject != want {
		t.Errorf("subject: got %q want %q", out.Subject, want)
	}
	if out.To != "" {
		t.Errorf("To must be left for the caller to set: got %q", out.To)
	}
}

func TestRenderRegradeResult_PerProblemSections(t *testing.T) {
	d := sampleResultData(1, 3, "regrade+v2.9.2.123.sig@inbound.example.edu")
	out, err := RenderRegradeResult(d)
	if err != nil {
		t.Fatal(err)
	}
	wantSubstrings := []string{
		"1",                                   // problem number surfaces somewhere (heading or label)
		"The base case n=1 was marked wrong.", // quoted complaint, problem 1
		"regraded",
		"Base case does earn partial credit per rubric line 2.",
		"9/10",                               // new score when regraded
		"My exchange argument handles ties.", // quoted complaint, problem 4
		"upheld",
		"The -2 deduction stands; ties are not handled correctly.",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(out.TextBody, s) {
			t.Errorf("result text body missing %q\n---\n%s", s, out.TextBody)
		}
	}
}

func TestRenderRegradeResult_UpheldProblemOmitsNewScore(t *testing.T) {
	d := sampleResultData(1, 3, "regrade+v2.9.2.123.sig@inbound.example.edu")
	out, err := RenderRegradeResult(d)
	if err != nil {
		t.Fatal(err)
	}
	// Problem 4 (upheld, no NewScore/Max set) must not render a bare "/" total.
	if strings.Contains(out.TextBody, "/.") {
		t.Error("upheld problem must not render a bare slash total")
	}
	// Crude structural check: the upheld problem's own section shouldn't carry
	// "New score" language since NewScore is empty.
	idx := strings.Index(out.TextBody, "My exchange argument handles ties.")
	if idx == -1 {
		t.Fatal("problem 4 complaint not found")
	}
	section := out.TextBody[idx:]
	if end := strings.Index(section, "attempt"); end != -1 {
		section = section[:end]
	}
	if strings.Contains(section, "New score") {
		t.Errorf("upheld problem section must not mention a new score:\n%s", section)
	}
}

func TestRenderRegradeResult_AttemptCounter(t *testing.T) {
	d := sampleResultData(2, 3, "regrade+v2.9.3.123.sig@inbound.example.edu")
	out, err := RenderRegradeResult(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.TextBody, "attempt 2 of 3") {
		t.Errorf("text body missing attempt counter 'attempt 2 of 3':\n%s", out.TextBody)
	}
	if !strings.Contains(out.HTMLBody, "attempt 2 of 3") {
		t.Errorf("html body missing attempt counter 'attempt 2 of 3':\n%s", out.HTMLBody)
	}
}

func TestRenderRegradeResult_NonFinalCarriesNextTurnTokenReplyTo(t *testing.T) {
	replyTo := "regrade+v2.9.2.999.sig@inbound.example.edu"
	d := sampleResultData(1, 3, replyTo) // turn 1 of 3 — not final
	out, err := RenderRegradeResult(d)
	if err != nil {
		t.Fatal(err)
	}
	if out.ReplyTo != replyTo {
		t.Errorf("ReplyTo: got %q want %q", out.ReplyTo, replyTo)
	}
	// The literal copy-paste format template must be present for a non-final result.
	if !strings.Contains(out.TextBody, d.FormatTemplate) {
		t.Errorf("text body missing the literal format template:\n%s", out.TextBody)
	}
}

func TestRenderRegradeResult_FinalTurnVariant(t *testing.T) {
	// Result #MAX (turn == maxTurns): final-attempt copy, handoff language.
	// Per spec, the token carried is the MAX+1 handoff token — still present as
	// ReplyTo (its CONSUMPTION triggers handoff, this template doesn't know
	// that; it just carries whatever token the caller minted) but the body must
	// say this was the final attempt and that further replies go to the TA.
	replyTo := "regrade+v2.9.4.999.sig@inbound.example.edu"
	d := sampleResultData(3, 3, replyTo) // turn 3 of 3 — final
	out, err := RenderRegradeResult(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.TextBody, "attempt 3 of 3") {
		t.Errorf("text body missing attempt counter 'attempt 3 of 3':\n%s", out.TextBody)
	}
	lower := strings.ToLower(out.TextBody)
	if !strings.Contains(lower, "final attempt") {
		t.Errorf("final-turn result must say this was the final attempt:\n%s", out.TextBody)
	}
	if !strings.Contains(lower, "ta") {
		t.Errorf("final-turn result must mention the problem's TA taking over:\n%s", out.TextBody)
	}
	if !strings.Contains(strings.ToLower(out.HTMLBody), "final attempt") {
		t.Errorf("final-turn result html must say this was the final attempt:\n%s", out.HTMLBody)
	}
}

func TestRenderRegradeResult_NonFinalDoesNotClaimFinal(t *testing.T) {
	d := sampleResultData(1, 3, "regrade+v2.9.2.999.sig@inbound.example.edu")
	out, err := RenderRegradeResult(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(out.TextBody), "final attempt") {
		t.Errorf("non-final result must not claim to be the final attempt:\n%s", out.TextBody)
	}
}

func TestRenderRegradeResult_HTMLEscapesComplaintAndNote(t *testing.T) {
	d := sampleResultData(1, 3, "regrade+v2.9.2.999.sig@inbound.example.edu")
	d.Problems[0].Complaint = "<script>alert(1)</script>"
	d.Problems[0].Note = "<b>injected</b>"
	out, err := RenderRegradeResult(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.HTMLBody, "<script>") {
		t.Error("html/template must escape complaint content")
	}
	if strings.Contains(out.HTMLBody, "<b>injected</b>") {
		t.Error("html/template must escape TA note content")
	}
}

func TestRenderRegradeResult_RejectsUnknownOutcome(t *testing.T) {
	d := sampleResultData(1, 3, "regrade+v2.9.2.999.sig@inbound.example.edu")
	d.Problems[0].Outcome = "bogus"
	if _, err := RenderRegradeResult(d); err == nil {
		t.Fatal("unknown per-problem outcome must error")
	}
}

// --- RenderRegradeConfirmation v2 (spec §5) — filed numbers + attempt counter -----

func TestRenderRegradeConfirmation_FiledNumbersAndAttemptCounter(t *testing.T) {
	d := RegradeConfirmationData{
		AssessmentName:   "Midterm 2",
		StudentName:      "Student Two",
		ReceivedAt:       time.Date(2026, 7, 5, 9, 30, 0, 0, time.UTC),
		FiledProblemNums: []int{1, 4},
		Turn:             1,
		MaxTurns:         3,
	}
	out, err := RenderRegradeConfirmation(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Midterm 2", "Student Two", "1", "4", "attempt 1 of 3"} {
		if !strings.Contains(out.TextBody, s) {
			t.Errorf("confirmation text missing %q:\n%s", s, out.TextBody)
		}
	}
	if !strings.Contains(out.TextBody, "not processed") && !strings.Contains(out.TextBody, "not be processed") {
		t.Errorf("confirmation must say replies to it are not processed:\n%s", out.TextBody)
	}
}

// TestRenderRegradeConfirmation_NoReplyTo is part of the §10 presence matrix:
// confirmation carries NO token / NO Reply-To, ever — even if a caller mistakenly
// tries to populate one, OutboundEmail.ReplyTo must come back empty because
// RegradeConfirmationData has no ReplyTo field to plumb through at all.
func TestRenderRegradeConfirmation_NoReplyTo(t *testing.T) {
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
	if out.ReplyTo != "" {
		t.Errorf("confirmation must never carry a Reply-To: got %q", out.ReplyTo)
	}
}

// --- RenderRegradeReminder (spec §7) -----------------------------------------------

func sampleReminderData() ReminderData {
	return ReminderData{
		AssessmentName: "Midterm 2",
		StudentName:    "Student Three",
		AnchorSubject:  "Midterm 2 — regrade result #1",
		AnchorSentAt:   time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC),
		Turn:           2,
		MaxTurns:       3,
		FormatTemplate: "<p1>\nyour complaint here\n</p1>",
	}
}

func TestRenderRegradeReminder_AnchoredContent(t *testing.T) {
	d := sampleReminderData()
	out, err := RenderRegradeReminder(d)
	if err != nil {
		t.Fatal(err)
	}
	wantSubstrings := []string{
		"Midterm 2",                     // assessment name
		"Student Three",                 // student name
		"Midterm 2 — regrade result #1", // exact anchor subject
		"2026-07-04",                    // sent date of anchor email
		"attempt 2 of 3",                // attempt counter
		d.FormatTemplate,                // literal format template
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(out.TextBody, s) {
			t.Errorf("reminder text missing %q:\n%s", s, out.TextBody)
		}
	}
	lower := strings.ToLower(out.TextBody)
	if !strings.Contains(lower, "not") {
		t.Errorf("reminder must state the attempt was NOT used:\n%s", out.TextBody)
	}
}

func TestRenderRegradeReminder_SaysReplyToAnchorNotReminder(t *testing.T) {
	d := sampleReminderData()
	out, err := RenderRegradeReminder(d)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(out.TextBody)
	if !strings.Contains(lower, "reply") {
		t.Errorf("reminder must instruct the student to reply to the anchor email:\n%s", out.TextBody)
	}
	// Must reference "this reminder" (or an equivalent) to clarify replies to the
	// reminder itself are not processed.
	if !strings.Contains(lower, "reminder") {
		t.Errorf("reminder must clarify replies to ITSELF are not processed:\n%s", out.TextBody)
	}
}

// TestRenderRegradeReminder_NoReplyTo is part of the §10 presence matrix.
func TestRenderRegradeReminder_NoReplyTo(t *testing.T) {
	d := sampleReminderData()
	out, err := RenderRegradeReminder(d)
	if err != nil {
		t.Fatal(err)
	}
	if out.ReplyTo != "" {
		t.Errorf("reminder must never carry a Reply-To: got %q", out.ReplyTo)
	}
}

func TestRenderRegradeReminder_HTMLEscapesStudentName(t *testing.T) {
	d := sampleReminderData()
	d.StudentName = "<script>alert(1)</script>"
	out, err := RenderRegradeReminder(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.HTMLBody, "<script>") {
		t.Error("html/template must escape student name content")
	}
}

// --- RenderTANotify (spec §6) -------------------------------------------------------

func sampleTANotifyData() TANotifyData {
	return TANotifyData{
		AssessmentName: "Midterm 2",
		StudentName:    "Student Four",
		StudentID:      "b09901001",
		StudentEmail:   "student.four@example.edu",
		DeepLink:       "https://ada-marker.example.edu/regrades/42",
		Problems: []TANotifyProblem{
			{
				Number:    1,
				Complaint: "The base case n=1 was marked wrong, still not satisfied.",
				History: []TANotifyHistoryEntry{
					{Turn: 1, Verdict: "regraded", Note: "Partial credit applied."},
				},
			},
			{
				Number:    4,
				Complaint: "The exchange argument point stands.",
				History: []TANotifyHistoryEntry{
					{Turn: 1, Verdict: "upheld", Note: "Deduction stands."},
					{Turn: 2, Verdict: "upheld", Note: "Still stands; see rubric line 9."},
				},
			},
		},
	}
}

func TestRenderTANotify_ContentFields(t *testing.T) {
	d := sampleTANotifyData()
	out, err := RenderTANotify(d)
	if err != nil {
		t.Fatal(err)
	}
	wantSubstrings := []string{
		"Midterm 2",
		"Student Four",
		"b09901001",
		"student.four@example.edu",
		"The base case n=1 was marked wrong, still not satisfied.",
		"Partial credit applied.",
		"The exchange argument point stands.",
		"Deduction stands.",
		"Still stands; see rubric line 9.",
		"https://ada-marker.example.edu/regrades/42",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(out.TextBody, s) {
			t.Errorf("TA-notify text missing %q:\n%s", s, out.TextBody)
		}
	}
}

// TestRenderTANotify_NoReplyTo is part of the §10 presence matrix.
func TestRenderTANotify_NoReplyTo(t *testing.T) {
	d := sampleTANotifyData()
	out, err := RenderTANotify(d)
	if err != nil {
		t.Fatal(err)
	}
	if out.ReplyTo != "" {
		t.Errorf("TA-notify must never carry a Reply-To: got %q", out.ReplyTo)
	}
}

func TestRenderTANotify_HTMLEscapesComplaintAndNote(t *testing.T) {
	d := sampleTANotifyData()
	d.Problems[0].Complaint = "<script>alert(1)</script>"
	d.Problems[0].History[0].Note = "<b>injected</b>"
	out, err := RenderTANotify(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.HTMLBody, "<script>") {
		t.Error("html/template must escape complaint content")
	}
	if strings.Contains(out.HTMLBody, "<b>injected</b>") {
		t.Error("html/template must escape history note content")
	}
}

func TestRenderTANotify_MultiProblemOneEmail(t *testing.T) {
	// D60: a TA assigned two contested problems of the same student gets ONE
	// email covering both — this is enforced by the caller grouping sub-items
	// into one TANotifyData.Problems slice before calling RenderTANotify; this
	// test just verifies the render surfaces both when given both.
	d := sampleTANotifyData()
	out, err := RenderTANotify(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.TextBody, "1") || !strings.Contains(out.TextBody, "4") {
		t.Errorf("multi-problem TA notify must surface both problem numbers:\n%s", out.TextBody)
	}
}

// TestRenderTANotify_EmptyDeepLink_DropsLinkLine covers F4: an empty DeepLink (no
// ADAMARKER_APP_BASE_URL configured) must drop the "Open in app" line entirely rather
// than render a dead bare/empty link in either body.
func TestRenderTANotify_EmptyDeepLink_DropsLinkLine(t *testing.T) {
	d := sampleTANotifyData()
	d.DeepLink = ""
	out, err := RenderTANotify(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.TextBody, "Open in app") {
		t.Errorf("text body should drop the deep-link line when DeepLink is empty:\n%s", out.TextBody)
	}
	if strings.Contains(out.HTMLBody, "Open in app") {
		t.Errorf("html body should drop the deep-link line when DeepLink is empty:\n%s", out.HTMLBody)
	}
	if strings.Contains(out.HTMLBody, `href=""`) {
		t.Errorf("html body should not render an empty href when DeepLink is empty:\n%s", out.HTMLBody)
	}
}

// TestRenderTANotify_DeepLinkPresent_RendersLink is the positive complement: a non-empty
// DeepLink still renders the "Open in app" line and href (guards the F4 change against
// accidentally always dropping it).
func TestRenderTANotify_DeepLinkPresent_RendersLink(t *testing.T) {
	d := sampleTANotifyData()
	out, err := RenderTANotify(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.TextBody, "Open in app: "+d.DeepLink) {
		t.Errorf("text body should render the deep-link line when DeepLink is set:\n%s", out.TextBody)
	}
	if !strings.Contains(out.HTMLBody, `href="`+d.DeepLink+`"`) {
		t.Errorf("html body should render the deep-link href when DeepLink is set:\n%s", out.HTMLBody)
	}
}

func TestRenderTANotify_Subject(t *testing.T) {
	d := sampleTANotifyData()
	out, err := RenderTANotify(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Subject, "Midterm 2") {
		t.Errorf("TA-notify subject should name the assessment: got %q", out.Subject)
	}
	if out.To != "" {
		t.Errorf("To must be left for the caller to set: got %q", out.To)
	}
}

// --- §10 token-presence matrix, all four v2 templates together --------------------

func TestTokenPresenceMatrix(t *testing.T) {
	result := sampleResultData(1, 3, "regrade+v2.9.2.999.sig@inbound.example.edu")
	outResult, err := RenderRegradeResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if outResult.ReplyTo == "" {
		t.Error("result email (non-final) must carry a Reply-To token")
	}

	confirmation := RegradeConfirmationData{
		AssessmentName:   "Midterm 2",
		StudentName:      "Student One",
		ReceivedAt:       time.Now(),
		FiledProblemNums: []int{1},
		Turn:             1,
		MaxTurns:         3,
	}
	outConfirmation, err := RenderRegradeConfirmation(confirmation)
	if err != nil {
		t.Fatal(err)
	}
	if outConfirmation.ReplyTo != "" {
		t.Error("confirmation email must carry NO Reply-To")
	}

	reminder := sampleReminderData()
	outReminder, err := RenderRegradeReminder(reminder)
	if err != nil {
		t.Fatal(err)
	}
	if outReminder.ReplyTo != "" {
		t.Error("reminder email must carry NO Reply-To")
	}

	tanotify := sampleTANotifyData()
	outTANotify, err := RenderTANotify(tanotify)
	if err != nil {
		t.Fatal(err)
	}
	if outTANotify.ReplyTo != "" {
		t.Error("TA-notify email must carry NO Reply-To")
	}
}

// Guard: return types are exactly domain.OutboundEmail.
var (
	_ = func() domain.OutboundEmail {
		out, _ := RenderRegradeResult(sampleResultData(1, 3, "x"))
		return out
	}
	_ = func() domain.OutboundEmail {
		out, _ := RenderRegradeReminder(sampleReminderData())
		return out
	}
	_ = func() domain.OutboundEmail {
		out, _ := RenderTANotify(sampleTANotifyData())
		return out
	}
)
