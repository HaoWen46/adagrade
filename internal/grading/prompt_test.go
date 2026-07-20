package grading

import (
	"strings"
	"testing"
)

// policyData returns PromptData with the given policy and just enough rubric to render.
func policyData(policy string) PromptData {
	return PromptData{
		ProblemNumber:    1,
		ProblemTitle:     "DP",
		ProblemStatement: "Solve it.",
		MaxPoints:        "10",
		ScoreIncrement:   "0.5",
		Policy:           policy,
		Criteria: []PromptCriterion{
			{ID: 1, Description: "Recurrence", Points: "6"},
		},
	}
}

// stance sentences that must appear in the SYSTEM template per policy, and the
// distinctive fragments that must NOT appear (belonging to other policies).
func TestRenderPrompt_SystemStancePerPolicy(t *testing.T) {
	cases := map[string]struct {
		want    string
		notWant []string
	}{
		PolicyLenient: {
			want:    "You are grading formative work: grade generously but honestly",
			notWant: []string{"high-stakes exam", "You are strict but fair: award exactly the credit"},
		},
		PolicyStrict: {
			want:    "You are grading a high-stakes exam: every point you award must be defensible",
			notWant: []string{"formative work", "You are strict but fair: award exactly the credit"},
		},
		PolicyStandard: {
			want:    "You are strict but fair: award exactly the credit the rubric supports",
			notWant: []string{"formative work", "high-stakes exam"},
		},
	}
	for policy, tc := range cases {
		t.Run(policy, func(t *testing.T) {
			out, err := RenderPrompt(DefaultSystemTemplate, policyData(policy))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("system[%s] missing stance %q\n---\n%s", policy, tc.want, out)
			}
			for _, nw := range tc.notWant {
				if strings.Contains(out, nw) {
					t.Errorf("system[%s] leaked other-policy text %q", policy, nw)
				}
			}
		})
	}
}

// distinctive first bullet of the judgment-policy section per policy.
func TestRenderPrompt_JudgmentBulletsPerPolicy(t *testing.T) {
	cases := map[string]struct {
		want    string
		notWant []string
	}{
		PolicyLenient: {
			want: "Resolve genuine ambiguity in the student's favor",
			notWant: []string{
				"Resolve genuine ambiguity toward the lower score",
				"Resolve genuine ambiguity the way a careful human grader",
			},
		},
		PolicyStrict: {
			want: "Resolve genuine ambiguity toward the lower score",
			notWant: []string{
				"Resolve genuine ambiguity in the student's favor",
				"Resolve genuine ambiguity the way a careful human grader",
			},
		},
		PolicyStandard: {
			want: "Resolve genuine ambiguity the way a careful human grader",
			notWant: []string{
				"Resolve genuine ambiguity in the student's favor",
				"Resolve genuine ambiguity toward the lower score",
			},
		},
	}
	for policy, tc := range cases {
		t.Run(policy, func(t *testing.T) {
			out, err := RenderPrompt(DefaultUserTemplate, policyData(policy))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("user[%s] missing bullet %q\n---\n%s", policy, tc.want, out)
			}
			for _, nw := range tc.notWant {
				if strings.Contains(out, nw) {
					t.Errorf("user[%s] leaked other-policy bullet %q", policy, nw)
				}
			}
		})
	}
}

// The judgment-policy section must sit between the rubric and the instructions.
func TestRenderPrompt_JudgmentSectionBeforeInstructions(t *testing.T) {
	out, err := RenderPrompt(DefaultUserTemplate, policyData(PolicyStandard))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	judgment := strings.Index(out, "# Judgment policy")
	instructions := strings.Index(out, "# Instructions")
	rubric := strings.Index(out, "# Rubric")
	if judgment < 0 || instructions < 0 || rubric < 0 {
		t.Fatalf("missing a section: rubric=%d judgment=%d instructions=%d\n%s", rubric, judgment, instructions, out)
	}
	if !(rubric < judgment && judgment < instructions) {
		t.Errorf("order wrong: rubric=%d judgment=%d instructions=%d", rubric, judgment, instructions)
	}
}

// v1-era templates that never reference .Policy must still render against the new
// PromptData (the extra field is simply unused).
func TestRenderPrompt_V1TemplateIgnoresPolicyField(t *testing.T) {
	const v1User = "# Problem {{.ProblemNumber}} (max {{.MaxPoints}} points)\n{{.ProblemStatement}}"
	out, err := RenderPrompt(v1User, policyData(PolicyStrict))
	if err != nil {
		t.Fatalf("v1 template render failed with new PromptData: %v", err)
	}
	if !strings.Contains(out, "# Problem 1 (max 10 points)") {
		t.Errorf("v1 render wrong: %q", out)
	}
}

// The UI-copy catalog must cover exactly the three policies with the right keys.
func TestPolicies_Catalog(t *testing.T) {
	if len(Policies) != 3 {
		t.Fatalf("expected 3 policies, got %d", len(Policies))
	}
	want := map[string]string{
		PolicyLenient:  "Lenient",
		PolicyStandard: "Standard",
		PolicyStrict:   "Strict",
	}
	for _, p := range Policies {
		label, ok := want[p.Key]
		if !ok {
			t.Errorf("unexpected policy key %q", p.Key)
			continue
		}
		if p.Label != label {
			t.Errorf("policy %q label = %q, want %q", p.Key, p.Label, label)
		}
		if p.Tagline == "" || p.WhenToUse == "" {
			t.Errorf("policy %q missing tagline/when-to-use", p.Key)
		}
		delete(want, p.Key)
	}
	if len(want) != 0 {
		t.Errorf("missing policies: %v", want)
	}
}
