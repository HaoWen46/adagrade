package grading

import (
	"strings"
	"testing"
)

func regradeData() RegradePromptData {
	return RegradePromptData{
		PromptData: PromptData{
			ProblemNumber:    3,
			ProblemTitle:     "DP",
			ProblemStatement: "Solve it.",
			MaxPoints:        "10",
			ScoreIncrement:   "0.5",
			// Policy is deliberately left empty — the regrade template must NOT branch on it.
			Criteria: []PromptCriterion{
				{ID: 1, Description: "Recurrence", Points: "6"},
				{ID: 2, Description: "Base case", Points: "4"},
			},
		},
		OriginalScores: []CriterionScore{
			{CriterionID: 1, Score: "4", Rationale: "recurrence mostly right"},
			{CriterionID: 2, Score: "2", Rationale: "base case incomplete"},
		},
		OriginalComment: "solid attempt",
		RequestText:     "I think my base case in part (b) is actually complete.",
		ProblemIDHint:   3,
	}
}

// The stricter stance must be present in the SYSTEM prompt, and no grading-policy
// branch language may leak (the regrade template has a single fixed stance).
func TestRenderRegradePrompt_SystemHasStricterStanceNoPolicyBranch(t *testing.T) {
	sys, err := RenderRegradePrompt(RegradeSystemTemplate, regradeData())
	if err != nil {
		t.Fatalf("render system: %v", err)
	}
	for _, want := range []string{"INDEPENDENT, STRICTER re-examination", "EITHER direction", "skepticism", "Do not reward persistence"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing stricter-framing phrase %q\n---\n%s", want, sys)
		}
	}
	// The three curated grading stances must not appear — this is not a policy-branched
	// template.
	for _, leaked := range []string{"formative work", "high-stakes exam", "You are strict but fair: award exactly"} {
		if strings.Contains(sys, leaked) {
			t.Errorf("regrade system prompt leaked grading-policy text %q", leaked)
		}
	}
}

// The USER prompt must carry the rubric, the ORIGINAL grade block, and the redacted
// request text, in that order relative to the judgment/instructions sections.
func TestRenderRegradePrompt_UserCarriesOriginalGradeAndRequest(t *testing.T) {
	user, err := RenderRegradePrompt(RegradeUserTemplate, regradeData())
	if err != nil {
		t.Fatalf("render user: %v", err)
	}
	for _, want := range []string{
		"# Rubric",
		"# Original grade under review",
		"criterion_id 1: scored 4 — recurrence mostly right",
		"criterion_id 2: scored 2 — base case incomplete",
		"Overall comment on the original grade: solid attempt",
		"# Student's regrade request",
		"I think my base case in part (b) is actually complete.",
		"contesting problem 3",
		"# Judgment policy",
		"# Instructions",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("regrade user prompt missing %q\n---\n%s", want, user)
		}
	}
	// Section ordering: rubric < original grade < request < judgment < instructions.
	order := []string{"# Rubric", "# Original grade under review", "# Student's regrade request", "# Judgment policy", "# Instructions"}
	last := -1
	for _, sec := range order {
		idx := strings.Index(user, sec)
		if idx < 0 {
			t.Fatalf("section %q missing", sec)
		}
		if idx <= last {
			t.Errorf("section %q out of order (idx %d <= previous %d)", sec, idx, last)
		}
		last = idx
	}
}

// A request with no text still renders (the placeholder), and no problem hint means no
// hint line — guards the {{if}} branches against missingkey=error.
func TestRenderRegradePrompt_EmptyRequestAndNoHint(t *testing.T) {
	d := regradeData()
	d.RequestText = ""
	d.ProblemIDHint = 0
	d.OriginalComment = ""
	user, err := RenderRegradePrompt(RegradeUserTemplate, d)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(user, "(no request text provided)") {
		t.Errorf("empty request should render the placeholder:\n%s", user)
	}
	if strings.Contains(user, "appears to be contesting problem") {
		t.Errorf("no hint should omit the contesting-problem line:\n%s", user)
	}
}
