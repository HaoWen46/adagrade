// Templates for outbound grade + regrade emails (spec §3, §5–§6). Each message is
// rendered twice — text/template for the plain-text body, html/template for the
// HTML alternative — so student-controlled content (names, comments, resolution
// notes) is auto-escaped in the HTML path. Points/totals are decimal strings
// throughout; this package never parses them to float64 (CLAUDE.md /
// global-constraints rule).
package email

import (
	"bytes"
	"fmt"
	htmltemplate "html/template"
	texttemplate "text/template"
	"time"

	"github.com/HaoWen46/adagrade/internal/domain"
)

// CriterionLine is one rubric criterion's score line within a problem.
type CriterionLine struct {
	Name    string
	Score   string
	Max     string
	Comment string
}

// ProblemBreakdown is one problem's criteria within a grade email.
type ProblemBreakdown struct {
	Label    string
	Criteria []CriterionLine
	// Comment is a whole-problem note (a grader's overall comment, or a
	// regrade adjudication note folded back into the next grade email) —
	// distinct from any single criterion's Comment. It renders as its own
	// "Note: …" line under the problem in both templates, never appended onto
	// a criterion's score line (B4: gluing a whole-problem note onto the last
	// criterion line made it read as if it belonged to that one criterion
	// rather than the problem as a whole). Empty ⇒ no Note line is rendered.
	Comment string
}

// RegradeReplyFormatTemplate is the literal copy-paste block shown to students so they
// reply in the exact <pN>…</pN> format the parser requires (regrade spec §2). Two
// problems are illustrated; the student uses the tags for whichever problems they
// contest. It lives here (not in httpapi) because both the grade email and the regrade
// result/reminder emails embed it, and httpapi already imports this package.
const RegradeReplyFormatTemplate = `<p1>
your complaint about problem 1 here
</p1>
<p2>
your complaint about problem 2 here
</p2>`

// GradeEmailData is the input to RenderGradeEmail. It carries no recipient
// address — the caller (publish service) knows the roster email and sets
// OutboundEmail.To after rendering.
type GradeEmailData struct {
	AssessmentName  string
	StudentName     string
	Problems        []ProblemBreakdown
	Total, Max      string
	ReplyTo         string // regrade+<signed-token>@inbound.<domain>; empty ⇒ replies not monitored.
	RegradeDeadline time.Time
	Corrected       bool // true for a re-publish ("corrected results") send.

	// FormatTemplate is the literal copy-paste <pN>...</pN> block (normally
	// RegradeReplyFormatTemplate), rendered only when ReplyTo is set — same
	// caller-passed pattern as ResultData/ReminderData.
	FormatTemplate string
}

// RegradeConfirmationData is the input to RenderRegradeConfirmation, sent
// automatically the moment a request FILES (spec §5) — i.e. as soon as
// ParseBlocks finds >=1 valid block, regardless of adjudication outcome
// (which arrives later via the result email). It carries no token: replies to
// the confirmation are physically inert (spec §4 — confirmation/reminder/
// TA-notify carry no Reply-To at all).
type RegradeConfirmationData struct {
	AssessmentName string
	StudentName    string
	ReceivedAt     time.Time

	// FiledProblemNums lists WHICH problem numbers filed on this turn (the
	// tag numbers from the student's reply, e.g. [1, 4]) — numbers only, no
	// complaint text (the confirmation is not a result, just a receipt).
	FiledProblemNums []int
	// Turn/MaxTurns render the "attempt N of MAX" counter every student-facing
	// v2 template carries.
	Turn, MaxTurns int
}

// RegradeResolutionData is the input to RenderRegradeResolution, sent when a TA
// resolves a queued regrade request (spec §6). Outcome must be "upheld" or
// "regraded"; NewTotal/Max are only used when Outcome is "regraded" AND both are
// non-empty. When the caller cannot determine a fresh total (no live publish item and
// nothing official yet, C2), it leaves NewTotal empty and the "New total" line is
// genuinely omitted rather than rendered as "/".
type RegradeResolutionData struct {
	AssessmentName string
	StudentName    string
	Outcome        string // "upheld" | "regraded"
	NewTotal, Max  string // decimal strings; only rendered when Outcome=="regraded" and NewTotal != ""
	ResolutionNote string
}

// gradeEmailView is the template-facing shape derived from GradeEmailData —
// html/template escapes every string field automatically, so no extra
// sanitization is needed here.
type gradeEmailView struct {
	GradeEmailData
	HasReplyTo      bool
	RegradeDeadline string // formatted, not the raw time.Time
	ResultsWord     string // "results" | "corrected results"
}

func (d GradeEmailData) view() gradeEmailView {
	word := "results"
	if d.Corrected {
		word = "corrected results"
	}
	return gradeEmailView{
		GradeEmailData:  d,
		HasReplyTo:      d.ReplyTo != "",
		RegradeDeadline: d.RegradeDeadline.Format("2006-01-02"),
		ResultsWord:     word,
	}
}

const gradeEmailTextTmpl = `{{.AssessmentName}} — {{.ResultsWord}}

Hi {{.StudentName}},
{{if .Corrected}}
This is a corrected version of your {{.AssessmentName}} results.
{{end}}
Here is your breakdown for {{.AssessmentName}}:
{{range .Problems}}
{{.Label}}:{{range .Criteria}}
  - {{.Name}}: {{.Score}}/{{.Max}}{{if .Comment}} — {{.Comment}}{{end}}{{end}}
{{if .Comment}}  Note: {{.Comment}}
{{end}}{{end}}
Total: {{.Total}}/{{.Max}}
{{if .HasReplyTo}}
If you believe there is an error, reply to this email before {{.RegradeDeadline}} to
request a regrade. Replies after this date will not be processed.

Name every problem you are contesting using exactly this format (one block per problem):

{{.FormatTemplate}}
{{else}}
Replies to this email are not monitored.
{{end}}`

const gradeEmailHTMLTmpl = `<!doctype html>
<html>
<body>
<h2>{{.AssessmentName}} — {{.ResultsWord}}</h2>
<p>Hi {{.StudentName}},</p>
{{if .Corrected}}<p>This is a corrected version of your {{.AssessmentName}} results.</p>{{end}}
<p>Here is your breakdown for {{.AssessmentName}}:</p>
{{range .Problems}}
<h3>{{.Label}}</h3>
<ul>
{{range .Criteria}}<li>{{.Name}}: {{.Score}}/{{.Max}}{{if .Comment}} — {{.Comment}}{{end}}</li>
{{end}}</ul>
{{if .Comment}}<p>Note: {{.Comment}}</p>
{{end}}{{end}}
<p><strong>Total: {{.Total}}/{{.Max}}</strong></p>
{{if .HasReplyTo}}
<p>If you believe there is an error, reply to this email before {{.RegradeDeadline}} to
request a regrade. Replies after this date will not be processed.</p>
<p>Name every problem you are contesting using exactly this format (one block per problem):</p>
<pre>{{.FormatTemplate}}</pre>
{{else}}
<p>Replies to this email are not monitored.</p>
{{end}}
</body>
</html>
`

var (
	gradeEmailText = texttemplate.Must(texttemplate.New("gradeEmailText").Parse(gradeEmailTextTmpl))
	gradeEmailHTML = htmltemplate.Must(htmltemplate.New("gradeEmailHTML").Parse(gradeEmailHTMLTmpl))
)

// RenderGradeEmail builds the outbound grade-notification message (spec §3).
// Subject is «assessment name» — results (or "— corrected results" for a
// re-publish). The returned OutboundEmail.To is empty; the caller addresses it.
func RenderGradeEmail(d GradeEmailData) (domain.OutboundEmail, error) {
	view := d.view()

	var textBuf bytes.Buffer
	if err := gradeEmailText.Execute(&textBuf, view); err != nil {
		return domain.OutboundEmail{}, fmt.Errorf("email: render grade text body: %w", err)
	}
	var htmlBuf bytes.Buffer
	if err := gradeEmailHTML.Execute(&htmlBuf, view); err != nil {
		return domain.OutboundEmail{}, fmt.Errorf("email: render grade html body: %w", err)
	}

	subject := fmt.Sprintf("%s — %s", d.AssessmentName, view.ResultsWord)
	return domain.OutboundEmail{
		ReplyTo:  d.ReplyTo,
		Subject:  subject,
		TextBody: textBuf.String(),
		HTMLBody: htmlBuf.String(),
	}, nil
}

const regradeConfirmationTextTmpl = `{{.AssessmentName}} — regrade request received

Hi {{.StudentName}},

We received your regrade request for {{.AssessmentName}} on {{.ReceivedAtFormatted}}.
Problem(s) filed: {{.FiledNumsFormatted}}.
This is attempt {{.Turn}} of {{.MaxTurns}}.

You will receive a result email once every filed problem has been reviewed.
Replies to this confirmation are not processed.
`

const regradeConfirmationHTMLTmpl = `<!doctype html>
<html>
<body>
<h2>{{.AssessmentName}} — regrade request received</h2>
<p>Hi {{.StudentName}},</p>
<p>We received your regrade request for {{.AssessmentName}} on {{.ReceivedAtFormatted}}.</p>
<p>Problem(s) filed: {{.FiledNumsFormatted}}.<br>
This is attempt {{.Turn}} of {{.MaxTurns}}.</p>
<p>You will receive a result email once every filed problem has been reviewed.
Replies to this confirmation are not processed.</p>
</body>
</html>
`

type regradeConfirmationView struct {
	RegradeConfirmationData
	ReceivedAtFormatted string
	FiledNumsFormatted  string // e.g. "1, 4" — numbers only, no complaint text.
}

var (
	regradeConfirmationText = texttemplate.Must(texttemplate.New("regradeConfirmationText").Parse(regradeConfirmationTextTmpl))
	regradeConfirmationHTML = htmltemplate.Must(htmltemplate.New("regradeConfirmationHTML").Parse(regradeConfirmationHTMLTmpl))
)

// RenderRegradeConfirmation builds the automatic receipt sent the moment a
// regrade request FILES (spec §5, D59). It lists WHICH problem numbers filed
// (numbers only — never complaint text, which is a later concern for the
// result email) and the attempt counter. The returned OutboundEmail carries NO
// Reply-To (spec §4: confirmation replies are physically inert) and its To is
// left empty for the caller to set.
func RenderRegradeConfirmation(d RegradeConfirmationData) (domain.OutboundEmail, error) {
	view := regradeConfirmationView{
		RegradeConfirmationData: d,
		ReceivedAtFormatted:     d.ReceivedAt.Format("2006-01-02 15:04 MST"),
		FiledNumsFormatted:      formatProblemNums(d.FiledProblemNums),
	}

	var textBuf bytes.Buffer
	if err := regradeConfirmationText.Execute(&textBuf, view); err != nil {
		return domain.OutboundEmail{}, fmt.Errorf("email: render regrade confirmation text body: %w", err)
	}
	var htmlBuf bytes.Buffer
	if err := regradeConfirmationHTML.Execute(&htmlBuf, view); err != nil {
		return domain.OutboundEmail{}, fmt.Errorf("email: render regrade confirmation html body: %w", err)
	}

	return domain.OutboundEmail{
		Subject:  fmt.Sprintf("%s — regrade request received", d.AssessmentName),
		TextBody: textBuf.String(),
		HTMLBody: htmlBuf.String(),
	}, nil
}

const regradeResolutionTextTmpl = `{{.AssessmentName}} — regrade {{.OutcomeWord}}

Hi {{.StudentName}},

Your regrade request for {{.AssessmentName}} has been reviewed.
{{if eq .Outcome "regraded"}}Your grade has been updated.{{if .HasNewTotal}} New total: {{.NewTotal}}/{{.Max}}.{{end}}
{{else}}The original grade has been upheld.
{{end}}
{{if .ResolutionNote}}Note from the reviewer: {{.ResolutionNote}}
{{end}}`

const regradeResolutionHTMLTmpl = `<!doctype html>
<html>
<body>
<h2>{{.AssessmentName}} — regrade {{.OutcomeWord}}</h2>
<p>Hi {{.StudentName}},</p>
<p>Your regrade request for {{.AssessmentName}} has been reviewed.</p>
{{if eq .Outcome "regraded"}}<p>Your grade has been updated.{{if .HasNewTotal}} New total: {{.NewTotal}}/{{.Max}}.{{end}}</p>
{{else}}<p>The original grade has been upheld.</p>
{{end}}
{{if .ResolutionNote}}<p>Note from the reviewer: {{.ResolutionNote}}</p>{{end}}
</body>
</html>
`

type regradeResolutionView struct {
	RegradeResolutionData
	OutcomeWord string
	HasNewTotal bool // Outcome=="regraded" && NewTotal != "" — gates the "New total" line (C2)
}

var (
	regradeResolutionText = texttemplate.Must(texttemplate.New("regradeResolutionText").Parse(regradeResolutionTextTmpl))
	regradeResolutionHTML = htmltemplate.Must(htmltemplate.New("regradeResolutionHTML").Parse(regradeResolutionHTMLTmpl))
)

// RenderRegradeResolution builds the email sent when a TA resolves a queued
// regrade request (spec §6). Outcome must be "upheld" or "regraded". The
// returned OutboundEmail.To is empty; the caller addresses it.
func RenderRegradeResolution(d RegradeResolutionData) (domain.OutboundEmail, error) {
	var word string
	switch d.Outcome {
	case "upheld":
		word = "upheld"
	case "regraded":
		word = "updated"
	default:
		return domain.OutboundEmail{}, fmt.Errorf("email: render regrade resolution: unknown outcome %q (want upheld|regraded)", d.Outcome)
	}
	view := regradeResolutionView{
		RegradeResolutionData: d,
		OutcomeWord:           word,
		HasNewTotal:           d.Outcome == "regraded" && d.NewTotal != "",
	}

	var textBuf bytes.Buffer
	if err := regradeResolutionText.Execute(&textBuf, view); err != nil {
		return domain.OutboundEmail{}, fmt.Errorf("email: render regrade resolution text body: %w", err)
	}
	var htmlBuf bytes.Buffer
	if err := regradeResolutionHTML.Execute(&htmlBuf, view); err != nil {
		return domain.OutboundEmail{}, fmt.Errorf("email: render regrade resolution html body: %w", err)
	}

	return domain.OutboundEmail{
		Subject:  fmt.Sprintf("%s — regrade %s", d.AssessmentName, word),
		TextBody: textBuf.String(),
		HTMLBody: htmlBuf.String(),
	}, nil
}
