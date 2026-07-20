package grading

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// buildSampleAssessment seeds an assessment with nProblems problems and
// nStudents students (every student answers every problem, one accepted-masked
// page each), a fake-provider method, and returns (assessmentID,
// methodVersionID). Modeled on buildRun, which is fixed to one problem.
func buildSampleAssessment(t *testing.T, h *spotCheckHarness, nProblems, nStudents int) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	q := h.st.Q

	asm, err := q.CreateAssessment(ctx, db.CreateAssessmentParams{Kind: "exam", Name: "Sample"})
	if err != nil {
		t.Fatalf("create assessment: %v", err)
	}
	tenPts, _ := store.Num("10")
	half, _ := store.Num("0.5")
	problems := make([]db.Problem, 0, nProblems)
	for p := 1; p <= nProblems; p++ {
		prob, err := q.CreateProblem(ctx, db.CreateProblemParams{
			AssessmentID: asm.ID, Number: int32(p), Title: "P", MaxPoints: tenPts, Position: int32(p),
		})
		if err != nil {
			t.Fatalf("create problem: %v", err)
		}
		rv, err := q.CreateRubricVersion(ctx, db.CreateRubricVersionParams{ProblemID: prob.ID, ScoreIncrement: half})
		if err != nil {
			t.Fatalf("create rubric version: %v", err)
		}
		if _, err := q.CreateRubricCriterion(ctx, db.CreateRubricCriterionParams{
			RubricVersionID: rv.ID, Position: 1, Description: "A", Points: tenPts,
		}); err != nil {
			t.Fatalf("create criterion: %v", err)
		}
		problems = append(problems, prob)
	}

	tpl, err := q.CreatePromptTemplateVersion(ctx, db.CreatePromptTemplateVersionParams{
		Name: "t-sample", SystemTemplate: "sys {{.ProblemNumber}}", UserTemplate: "usr {{.MaxPoints}}",
	})
	if err != nil {
		t.Fatalf("create prompt template: %v", err)
	}
	method, err := q.CreateGradingMethod(ctx, "fake sample method")
	if err != nil {
		t.Fatalf("create method: %v", err)
	}
	cfg, _ := json.Marshal(MethodConfig{
		Provider: "fake", Model: "fake-vision-1", ReaskCap: 2,
		PromptTemplateVersionID: tpl.ID,
	})
	mv, err := q.CreateMethodVersion(ctx, db.CreateMethodVersionParams{MethodID: method.ID, Config: cfg})
	if err != nil {
		t.Fatalf("create method version: %v", err)
	}

	for i := 0; i < nStudents; i++ {
		sid := "smp" + string(rune('a'+i))
		student, err := q.UpsertStudent(ctx, db.UpsertStudentParams{StudentID: sid, Name: sid, Email: sid + "@x.edu"})
		if err != nil {
			t.Fatalf("upsert student: %v", err)
		}
		sub, err := q.CreateSubmission(ctx, db.CreateSubmissionParams{
			AssessmentID: asm.ID, StudentID: student.ID, OriginalFilename: sid + ".pdf",
			SourceRef: "blob/" + sid, SourceSha256: "sha-" + sid, SourceKind: "pdf", PageCount: int32(nProblems),
		})
		if err != nil {
			t.Fatalf("create submission: %v", err)
		}
		for pi, prob := range problems {
			answer, err := q.EnsureAnswer(ctx, db.EnsureAnswerParams{AssessmentID: asm.ID, StudentID: student.ID, ProblemID: prob.ID})
			if err != nil {
				t.Fatalf("ensure answer: %v", err)
			}
			page, err := q.CreateAnswerPage(ctx, db.CreateAnswerPageParams{
				AnswerID: answer.ID, PageIndex: 0, SubmissionID: sub.ID, PdfPageIndex: int32(pi),
				ImageRef: "raw/" + sid, ImageSha256: "img-sha-" + sid, ImageWidth: 100, ImageHeight: 100,
			})
			if err != nil {
				t.Fatalf("create answer page: %v", err)
			}
			maskedKey := "assessments/masked/" + sid + "-" + prob.Title + ".jpg"
			if _, _, err := h.blobs.Put(ctx, maskedKey, bytesReader([]byte("fake-jpeg"))); err != nil {
				t.Fatalf("put masked blob: %v", err)
			}
			if _, err := q.SetPageMasked(ctx, db.SetPageMaskedParams{
				ID: page.ID, MaskedImageRef: pgtype.Text{String: maskedKey, Valid: true},
			}); err != nil {
				t.Fatalf("set page masked: %v", err)
			}
			if _, err := q.SetMaskReview(ctx, db.SetMaskReviewParams{ID: page.ID, MaskReviewStatus: "accepted"}); err != nil {
				t.Fatalf("accept mask review: %v", err)
			}
		}
	}
	return asm.ID, mv.ID
}

func createSampleRun(t *testing.T, h *spotCheckHarness, asmID, mvID, n int64) int64 {
	t.Helper()
	run, err := h.st.Q.CreateRun(t.Context(), db.CreateRunParams{
		AssessmentID: asmID, ScopeKind: "sample", ScopeID: n,
		MethodVersionID: mvID, ExecutionMode: "sync",
	})
	if err != nil {
		t.Fatalf("create sample run: %v", err)
	}
	return run.ID
}

// A sample-scope run plans exactly N items, stratified across problems.
func TestPlan_SampleScope_StratifiedItems(t *testing.T) {
	h := newSpotCheckHarness(t)
	ctx := context.Background()
	asmID, mvID := buildSampleAssessment(t, h, 2, 5) // pool: 10 answers, 5 per problem

	runID := createSampleRun(t, h, asmID, mvID, 4)
	if err := h.runner.Plan(ctx, runID); err != nil {
		t.Fatalf("plan: %v", err)
	}
	run, err := h.st.Q.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "running" {
		t.Fatalf("run status after plan = %q, want running (%s)", run.Status, run.Error.String)
	}
	items, err := h.st.Q.ListRunItems(ctx, db.ListRunItemsParams{RunID: runID, ItemLimit: 1000})
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("want 4 sampled items, got %d", len(items))
	}
	perProblem := map[int64]int{}
	for _, it := range items {
		rows, err := h.st.Q.AnswersWithProblems(ctx, []int64{it.AnswerID})
		if err != nil || len(rows) != 1 {
			t.Fatalf("answer lookup: %v", err)
		}
		perProblem[rows[0].ProblemID]++
	}
	for pid, n := range perProblem {
		if n != 2 {
			t.Fatalf("problem %d has %d sampled answers, want 2: %v", pid, perProblem, items)
		}
	}
}

// N larger than the pool clamps to the whole pool instead of failing.
func TestPlan_SampleScope_ClampsToPool(t *testing.T) {
	h := newSpotCheckHarness(t)
	ctx := context.Background()
	asmID, mvID := buildSampleAssessment(t, h, 2, 3) // pool: 6

	runID := createSampleRun(t, h, asmID, mvID, 50)
	if err := h.runner.Plan(ctx, runID); err != nil {
		t.Fatalf("plan: %v", err)
	}
	items, err := h.st.Q.ListRunItems(ctx, db.ListRunItemsParams{RunID: runID, ItemLimit: 1000})
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 6 {
		t.Fatalf("want whole pool (6), got %d", len(items))
	}
}

// A non-positive N fails the run with an actionable message, not a mask-gate error.
func TestPlan_SampleScope_RejectsNonPositiveN(t *testing.T) {
	h := newSpotCheckHarness(t)
	ctx := context.Background()
	asmID, mvID := buildSampleAssessment(t, h, 1, 2)

	runID := createSampleRun(t, h, asmID, mvID, 0)
	if err := h.runner.Plan(ctx, runID); err != nil {
		t.Fatalf("plan: %v", err)
	}
	run, err := h.st.Q.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "failed" {
		t.Fatalf("run status = %q, want failed", run.Status)
	}
	if run.Error.String != "sample size must be at least 1" {
		t.Fatalf("run error = %q, want sample-size message", run.Error.String)
	}
}
