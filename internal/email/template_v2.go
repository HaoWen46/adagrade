// Regrade v2 templates (spec §5-§7, D59-D62): the TA-clicked per-problem result
// email, the TA-clicked unparsed-row reminder, and the TA-notify handoff email.
// Same rendering idioms as template.go: text/template + html/template pair per
// message, decimal strings never parsed to float64, html/template auto-escapes
// every student/TA-authored string.
package email

import (
	"bytes"
	"fmt"
	htmltemplate "html/template"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/HaoWen46/adagrade/internal/domain"
)

// formatProblemNums renders a slice of problem numbers as "1, 4" — used
// anywhere a template needs to name filed/contested problems by number only
// (never alongside complaint text in the same field).
func formatProblemNums(nums []int) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// RenderRegradeResult (spec §5)
// ---------------------------------------------------------------------------

// ResultProblem is one contested problem's adjudication within a result email.
// Outcome must be "upheld" or "regraded"; NewScore/Max are only rendered when
// Outcome is "regraded" AND both are non-empty (same C2 omit-don't-blank rule
// as v1's RegradeResolutionData).
type ResultProblem struct {
	Number    int    // the problem number as printed on the exam / the student's <pN> tag.
	Complaint string // the student's complaint text for this problem, quoted verbatim.
	Outcome   string // "upheld" | "regraded"
	Note      string // TA's note explaining the verdict.
	NewScore  string // decimal string; only rendered when Outcome=="regraded" and non-empty.
	Max       string // decimal string; paired with NewScore.
}

// ResultData is the input to RenderRegradeResult, sent when a TA clicks
// "send result" on a FILED request whose every sub-item now has a verdict
// (spec §5, hard-gated server-side — RenderRegradeResult itself does not
// re-check gating; that is the send handler's job, not the rendering layer's).
type ResultData struct {
	AssessmentName string
	StudentName    string
	Problems       []ResultProblem

	// Turn is this result email's ordinal (the "#N" in the subject and the
	// numerator of "attempt N of MAX"). MaxTurns is ADAMARKER_REGRADE_MAX as
	// read at the request's receipt time (spec §4: in-flight tokens carry
	// their own turn, so a mid-term MAX change stays coherent per-thread).
	Turn, MaxTurns int

	// ReplyTo is the NEXT-turn token's mailto address (turn N+1) — present on
	// every result email including #MAX, where consuming it fires the
	// handoff rather than another adjudication round (spec §4). Rendering
	// does not distinguish "adjudication token" from "handoff token"; both are
	// just this email's Reply-To. The FINAL flag (Turn == MaxTurns) instead
	// changes the BODY COPY to say so explicitly.
	ReplyTo string

	// FormatTemplate is the literal copy-paste <pN>...</pN> block shown so the
	// student can reply correctly (spec §5 "the format template"). Empty on a
	// final-turn (#MAX) result is still allowed by this renderer — the
	// caller decides whether replying is still worth illustrating; the
	// final-turn copy already makes clear that further replies are handled by
	// the TA directly rather than through this format.
	FormatTemplate string
}

func (d ResultData) isFinal() bool { return d.MaxTurns > 0 && d.Turn >= d.MaxTurns }

type resultProblemView struct {
	ResultProblem
	OutcomeWord string
	HasNewScore bool
}

type resultView struct {
	ResultData
	ProblemViews []resultProblemView
	IsFinal      bool
	HasReplyTo   bool
}

func (d ResultData) view() (resultView, error) {
	views := make([]resultProblemView, 0, len(d.Problems))
	for _, p := range d.Problems {
		var word string
		switch p.Outcome {
		case "upheld":
			word = "upheld"
		case "regraded":
			word = "regraded"
		default:
			return resultView{}, fmt.Errorf("email: render regrade result: problem %d: unknown outcome %q (want upheld|regraded)", p.Number, p.Outcome)
		}
		views = append(views, resultProblemView{
			ResultProblem: p,
			OutcomeWord:   word,
			HasNewScore:   p.Outcome == "regraded" && p.NewScore != "",
		})
	}
	return resultView{
		ResultData:   d,
		ProblemViews: views,
		IsFinal:      d.isFinal(),
		HasReplyTo:   d.ReplyTo != "",
	}, nil
}

const resultTextTmpl = `{{.AssessmentName}} — regrade result #{{.Turn}}

Hi {{.StudentName}},

Your regrade request for {{.AssessmentName}} has been reviewed. This is attempt {{.Turn}} of {{.MaxTurns}}.
{{range .ProblemViews}}
Problem {{.Number}}:
  Your complaint: {{.Complaint}}
  Outcome: {{.OutcomeWord}}
{{if .Note}}  TA note: {{.Note}}
{{end}}{{if .HasNewScore}}  New score: {{.NewScore}}/{{.Max}}
{{end}}{{end}}
{{if .IsFinal}}This was your final attempt; further replies for these problems go to the
assigned TA directly rather than through this system.
{{else}}{{if .HasReplyTo}}If you are still not satisfied, reply to this email using the format below,
naming only the problem(s) you wish to contest again:

{{.FormatTemplate}}
{{end}}{{end}}`

const resultHTMLTmpl = `<!doctype html>
<html>
<body>
<h2>{{.AssessmentName}} — regrade result #{{.Turn}}</h2>
<p>Hi {{.StudentName}},</p>
<p>Your regrade request for {{.AssessmentName}} has been reviewed. This is attempt {{.Turn}} of {{.MaxTurns}}.</p>
{{range .ProblemViews}}
<h3>Problem {{.Number}}</h3>
<p><em>Your complaint:</em> {{.Complaint}}</p>
<p><strong>Outcome: {{.OutcomeWord}}</strong></p>
{{if .Note}}<p>TA note: {{.Note}}</p>{{end}}
{{if .HasNewScore}}<p>New score: {{.NewScore}}/{{.Max}}</p>{{end}}
{{end}}
{{if .IsFinal}}
<p>This was your final attempt; further replies for these problems go to the
assigned TA directly rather than through this system.</p>
{{else}}{{if .HasReplyTo}}
<p>If you are still not satisfied, reply to this email using the format below,
naming only the problem(s) you wish to contest again:</p>
<pre>{{.FormatTemplate}}</pre>
{{end}}{{end}}
</body>
</html>
`

var (
	resultText = texttemplate.Must(texttemplate.New("resultText").Parse(resultTextTmpl))
	resultHTML = htmltemplate.Must(htmltemplate.New("resultHTML").Parse(resultHTMLTmpl))
)

// RenderRegradeResult builds result email #N (spec §5): standalone numbered
// subject, per-problem sections (quoted complaint, outcome, TA note, new score
// when regraded), the attempt counter, and — except on the final turn — the
// next-turn Reply-To plus the literal format template. The final turn (Turn ==
// MaxTurns) swaps in handoff copy instead. The returned OutboundEmail.To is
// empty; the caller addresses it. RenderRegradeResult does not itself enforce
// the "every sub-item verdicted" gate — that is the send handler's
// responsibility (spec §5: "the gate lives in the send handler + a store-level
// check").
func RenderRegradeResult(d ResultData) (domain.OutboundEmail, error) {
	view, err := d.view()
	if err != nil {
		return domain.OutboundEmail{}, err
	}

	var textBuf bytes.Buffer
	if err := resultText.Execute(&textBuf, view); err != nil {
		return domain.OutboundEmail{}, fmt.Errorf("email: render regrade result text body: %w", err)
	}
	var htmlBuf bytes.Buffer
	if err := resultHTML.Execute(&htmlBuf, view); err != nil {
		return domain.OutboundEmail{}, fmt.Errorf("email: render regrade result html body: %w", err)
	}

	replyTo := d.ReplyTo
	if view.IsFinal {
		// The token is still minted and still consumable (its consumption is
		// what fires the handoff, per spec §4), so ReplyTo is intentionally
		// carried through unchanged on the final turn too — only the body
		// copy changes. Kept as a named branch (rather than folded into the
		// return) so the "final turns still carry a token" invariant is
		// visible at the call site, not just implied by falling through.
		replyTo = d.ReplyTo
	}

	return domain.OutboundEmail{
		ReplyTo:  replyTo,
		Subject:  fmt.Sprintf("%s — regrade result #%d", d.AssessmentName, d.Turn),
		TextBody: textBuf.String(),
		HTMLBody: htmlBuf.String(),
	}, nil
}

// ---------------------------------------------------------------------------
// RenderRegradeReminder (spec §7)
// ---------------------------------------------------------------------------

// ReminderData is the input to RenderRegradeReminder, sent when a TA clicks
// "Send reminder" on an unparsed row (0 valid blocks, live token — spec §7,
// D62). The reminder is anchored to the SPECIFIC email whose token is still
// live (never generic): it must name that email's exact subject and sent
// date so the student knows which message to go back and reply to.
type ReminderData struct {
	AssessmentName string
	StudentName    string

	// AnchorSubject/AnchorSentAt identify the email whose token is still live
	// (the grade email or an earlier result email) — the exact Subject header
	// and send timestamp of THAT message, not a generic re-statement.
	AnchorSubject string
	AnchorSentAt  time.Time

	// Turn/MaxTurns render "attempt N of MAX" — the attempt tied to the live
	// (unconsumed) token, so the counter reflects the attempt the student
	// still has available, not one they've already used.
	Turn, MaxTurns int

	// FormatTemplate is the literal copy-paste <pN>...</pN> block, repeated
	// here so the student doesn't have to dig up the original email to see
	// the exact format required.
	FormatTemplate string
}

type reminderView struct {
	ReminderData
	AnchorSentAtFormatted string
}

func (d ReminderData) view() reminderView {
	return reminderView{
		ReminderData:          d,
		AnchorSentAtFormatted: d.AnchorSentAt.Format("2006-01-02 15:04 MST"),
	}
}

const reminderTextTmpl = `{{.AssessmentName}} — reminder: your regrade attempt has not been used

Hi {{.StudentName}},

This is a reminder about {{.AssessmentName}}. Your reply to the email
"{{.AnchorSubject}}" (sent {{.AnchorSentAtFormatted}}) could not be read as a valid
regrade request, so that attempt was NOT used — attempt {{.Turn}} of {{.MaxTurns}} is
still available.

To submit your regrade request, reply to THAT email ("{{.AnchorSubject}}"),
not to this reminder — replies to this reminder are not processed. Use the
exact format below, naming only the problem(s) you wish to contest:

{{.FormatTemplate}}
`

const reminderHTMLTmpl = `<!doctype html>
<html>
<body>
<h2>{{.AssessmentName}} — reminder: your regrade attempt has not been used</h2>
<p>Hi {{.StudentName}},</p>
<p>This is a reminder about {{.AssessmentName}}. Your reply to the email
"{{.AnchorSubject}}" (sent {{.AnchorSentAtFormatted}}) could not be read as a valid
regrade request, so that attempt was NOT used — attempt {{.Turn}} of {{.MaxTurns}} is
still available.</p>
<p>To submit your regrade request, reply to THAT email ("{{.AnchorSubject}}"),
not to this reminder — replies to this reminder are not processed. Use the
exact format below, naming only the problem(s) you wish to contest:</p>
<pre>{{.FormatTemplate}}</pre>
</body>
</html>
`

var (
	reminderText = texttemplate.Must(texttemplate.New("reminderText").Parse(reminderTextTmpl))
	reminderHTML = htmltemplate.Must(htmltemplate.New("reminderHTML").Parse(reminderHTMLTmpl))
)

// RenderRegradeReminder builds the TA-clicked reminder for an unparsed row
// (spec §7, D62). It carries NO token and NO Reply-To — structurally
// reply-proof, so a reply to the reminder itself lands in the plain mailbox
// rather than re-entering the pipeline. The returned OutboundEmail.To is
// empty; the caller addresses it.
func RenderRegradeReminder(d ReminderData) (domain.OutboundEmail, error) {
	view := d.view()

	var textBuf bytes.Buffer
	if err := reminderText.Execute(&textBuf, view); err != nil {
		return domain.OutboundEmail{}, fmt.Errorf("email: render regrade reminder text body: %w", err)
	}
	var htmlBuf bytes.Buffer
	if err := reminderHTML.Execute(&htmlBuf, view); err != nil {
		return domain.OutboundEmail{}, fmt.Errorf("email: render regrade reminder html body: %w", err)
	}

	return domain.OutboundEmail{
		Subject:  fmt.Sprintf("%s — reminder: your regrade attempt has not been used", d.AssessmentName),
		TextBody: textBuf.String(),
		HTMLBody: htmlBuf.String(),
	}, nil
}

// ---------------------------------------------------------------------------
// RenderTANotify (spec §6)
// ---------------------------------------------------------------------------

// TANotifyHistoryEntry is one prior turn's verdict for a single problem,
// surfaced to the assigned TA on handoff so they have the full back-and-forth
// without needing app access.
type TANotifyHistoryEntry struct {
	Turn    int
	Verdict string // "upheld" | "regraded" — prior-turn outcome, shown verbatim (not re-validated; history is a record, not a new adjudication).
	Note    string
}

// TANotifyProblem is one contested problem being handed off to its assigned
// TA, with the student's final complaint and their full prior-turn history
// for that problem.
type TANotifyProblem struct {
	Number    int
	Complaint string // the FINAL turn's complaint text for this problem, verbatim.
	History   []TANotifyHistoryEntry
}

// TANotifyData is the input to RenderTANotify, sent when a final-turn (#MAX)
// result token is consumed with >=1 valid block, per problem's assigned TA
// (spec §6, D60/D61). This is deliberate PII-to-authorized-grader over email:
// student name, id, and email travel in the body — the same trust class as
// the outbound grade email, and explicitly authorized by D61. It is NEVER to
// be logged; only the rendered OutboundEmail is sent, never captured in a log
// line by any caller.
//
// One TANotifyData == one email to one TA about one student, covering EVERY
// problem that TA is assigned among this student's contested problems (D60:
// "one email per (TA, student, problem-group)") — the caller is responsible
// for grouping sub-items by (TA, student) before constructing this struct;
// RenderTANotify itself just renders whatever Problems it's given.
type TANotifyData struct {
	AssessmentName string
	StudentName    string
	StudentID      string
	StudentEmail   string
	Problems       []TANotifyProblem
	// DeepLink is the absolute app URL for the TA to open the request in-app (whole-
	// branch review F4: ADAMARKER_APP_BASE_URL + "/regrades/{id}"). Empty means no
	// app base URL is configured — the caller must NOT pass a bare relative path (it
	// would be dead in any mail client); RenderTANotify drops the "Open in app" line
	// entirely when this is empty.
	DeepLink string
}

const taNotifyTextTmpl = `{{.AssessmentName}} — regrade handoff for {{.StudentName}}

A student's regrade request has reached the final attempt and is now handed off
to you directly.

Student: {{.StudentName}} ({{.StudentID}}, {{.StudentEmail}})
Assessment: {{.AssessmentName}}
{{range .Problems}}
Problem {{.Number}}:
  Complaint: {{.Complaint}}
{{if .History}}  Prior attempts:
{{range .History}}    Attempt {{.Turn}}: {{.Verdict}}{{if .Note}} — {{.Note}}{{end}}
{{end}}{{end}}{{end}}
{{if .DeepLink}}Open in app: {{.DeepLink}}

{{end}}Please respond to the student directly from your own mailbox; the system will
not send any further automated messages for this thread.
`

const taNotifyHTMLTmpl = `<!doctype html>
<html>
<body>
<h2>{{.AssessmentName}} — regrade handoff for {{.StudentName}}</h2>
<p>A student's regrade request has reached the final attempt and is now handed off
to you directly.</p>
<p>Student: {{.StudentName}} ({{.StudentID}}, {{.StudentEmail}})<br>
Assessment: {{.AssessmentName}}</p>
{{range .Problems}}
<h3>Problem {{.Number}}</h3>
<p><em>Complaint:</em> {{.Complaint}}</p>
{{if .History}}<p>Prior attempts:</p>
<ul>
{{range .History}}<li>Attempt {{.Turn}}: {{.Verdict}}{{if .Note}} — {{.Note}}{{end}}</li>
{{end}}</ul>
{{end}}{{end}}
{{if .DeepLink}}<p><a href="{{.DeepLink}}">Open in app</a></p>
{{end}}<p>Please respond to the student directly from your own mailbox; the system will
not send any further automated messages for this thread.</p>
</body>
</html>
`

var (
	taNotifyText = texttemplate.Must(texttemplate.New("taNotifyText").Parse(taNotifyTextTmpl))
	taNotifyHTML = htmltemplate.Must(htmltemplate.New("taNotifyHTML").Parse(taNotifyHTMLTmpl))
)

// RenderTANotify builds the internal handoff email to an assigned TA (spec
// §6, D60/D61). It carries NO Reply-To (the TA replies to the student
// personally from their own mailbox, per spec — this is not a system
// round-trip). The returned OutboundEmail.To is empty; the caller (which
// knows the TA's address) addresses it.
func RenderTANotify(d TANotifyData) (domain.OutboundEmail, error) {
	var textBuf bytes.Buffer
	if err := taNotifyText.Execute(&textBuf, d); err != nil {
		return domain.OutboundEmail{}, fmt.Errorf("email: render TA notify text body: %w", err)
	}
	var htmlBuf bytes.Buffer
	if err := taNotifyHTML.Execute(&htmlBuf, d); err != nil {
		return domain.OutboundEmail{}, fmt.Errorf("email: render TA notify html body: %w", err)
	}

	return domain.OutboundEmail{
		Subject:  fmt.Sprintf("%s — regrade handoff for %s", d.AssessmentName, d.StudentName),
		TextBody: textBuf.String(),
		HTMLBody: htmlBuf.String(),
	}, nil
}
