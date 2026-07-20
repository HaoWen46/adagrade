package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// fixture bundles the minimal assessment -> problem -> rubric -> student -> answer
// chain most publish/regrade/pricing/spot-check tests need. Invented names/emails
// only (CLAUDE.md privacy rule) — this is fixture data, never real student PII.
type fixture struct {
	AssessmentID    int64
	ProblemID       int64
	RubricVersionID int64
	MethodVersionID int64
	StudentID       int64 // students.id (internal PK)
	AnswerID        int64
}

func mustFixture(t *testing.T, s *store.Store) fixture {
	t.Helper()
	ctx := context.Background()

	assessment, err := s.Q.CreateAssessment(ctx, db.CreateAssessmentParams{
		Kind: "exam", Name: t.Name() + "-assessment",
	})
	if err != nil {
		t.Fatalf("CreateAssessment: %v", err)
	}

	maxPoints, err := store.Num("10")
	if err != nil {
		t.Fatalf("Num: %v", err)
	}
	problem, err := s.Q.CreateProblem(ctx, db.CreateProblemParams{
		AssessmentID: assessment.ID, Number: 1, Title: "Problem 1",
		MaxPoints: maxPoints, Position: 1,
	})
	if err != nil {
		t.Fatalf("CreateProblem: %v", err)
	}

	increment, err := store.Num("0.5")
	if err != nil {
		t.Fatalf("Num: %v", err)
	}
	rubric, err := s.Q.CreateRubricVersion(ctx, db.CreateRubricVersionParams{
		ProblemID: problem.ID, ScoreIncrement: increment,
	})
	if err != nil {
		t.Fatalf("CreateRubricVersion: %v", err)
	}

	method, err := s.Q.CreateGradingMethod(ctx, t.Name()+"-method")
	if err != nil {
		t.Fatalf("CreateGradingMethod: %v", err)
	}
	mv, err := s.Q.CreateMethodVersion(ctx, db.CreateMethodVersionParams{
		MethodID: method.ID, Config: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateMethodVersion: %v", err)
	}

	student, err := s.Q.UpsertStudent(ctx, db.UpsertStudentParams{
		StudentID: t.Name() + "-B00000001", Name: "Test Student", Email: "student@example.test",
	})
	if err != nil {
		t.Fatalf("UpsertStudent: %v", err)
	}

	answer, err := s.Q.EnsureAnswer(ctx, db.EnsureAnswerParams{
		AssessmentID: assessment.ID, StudentID: student.ID, ProblemID: problem.ID,
	})
	if err != nil {
		t.Fatalf("EnsureAnswer: %v", err)
	}

	return fixture{
		AssessmentID:    assessment.ID,
		ProblemID:       problem.ID,
		RubricVersionID: rubric.ID,
		MethodVersionID: mv.ID,
		StudentID:       student.ID,
		AnswerID:        answer.ID,
	}
}

// mustOfficialRecord inserts a human grading_record with the given total and sets it
// official on the answer — the simplest way to satisfy the publish coverage gate in
// tests without a real grading run.
func mustOfficialRecord(t *testing.T, s *store.Store, answerID, rubricVersionID int64, total string) int64 {
	t.Helper()
	ctx := context.Background()

	totalNum, err := store.Num(total)
	if err != nil {
		t.Fatalf("Num: %v", err)
	}
	scores, err := json.Marshal([]map[string]any{{"criterion_id": 1, "score": total, "rationale": ""}})
	if err != nil {
		t.Fatalf("marshal scores: %v", err)
	}
	// grading_records_check requires created_by for source='human'.
	grader, err := s.Q.CreateUser(ctx, db.CreateUserParams{
		Email: "grader-" + t.Name() + "@example.test", Role: "ta", Active: true,
	})
	if err != nil {
		t.Fatalf("CreateUser (grader): %v", err)
	}
	rec, err := s.Q.InsertHumanRecord(ctx, db.InsertHumanRecordParams{
		AnswerID:        answerID,
		RubricVersionID: rubricVersionID,
		GradedImageShas: []string{},
		CriterionScores: scores,
		Total:           totalNum,
		Comment:         "looks good",
		Adjustments:     []byte(`[]`),
		CreatedBy:       pgtype.Int8{Int64: grader.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertHumanRecord: %v", err)
	}
	// Officials are derived since 0027; fixtures poke the pointer directly.
	if _, err := s.Pool.Exec(ctx,
		`UPDATE answers SET official_record_id = $2, official_set_at = now() WHERE id = $1`,
		answerID, rec.ID); err != nil {
		t.Fatalf("force official: %v", err)
	}
	return rec.ID
}

// modelRecordSeq disambiguates model_id across mustModelRecord calls within one test:
// grading_records_leaf_uniq is a unique index on (run_id, answer_id, model_id), so
// two records for the same (run, answer) need distinct model ids, same as two real
// leaves in one run would (different models in the method's panel).
var modelRecordSeq int

// mustModelRecord inserts a model grading_record tied to a run — used for spot-check
// and cost tests.
func mustModelRecord(t *testing.T, s *store.Store, runID, answerID, rubricVersionID, methodVersionID int64, total string, inputTok, outputTok int32, costUSD string) db.GradingRecord {
	t.Helper()
	ctx := context.Background()
	modelRecordSeq++

	totalNum, err := store.Num(total)
	if err != nil {
		t.Fatalf("Num total: %v", err)
	}
	scores, err := json.Marshal([]map[string]any{{"criterion_id": 1, "score": total, "rationale": ""}})
	if err != nil {
		t.Fatalf("marshal scores: %v", err)
	}
	var costNum pgtype.Numeric
	if costUSD != "" {
		costNum, err = store.Num(costUSD)
		if err != nil {
			t.Fatalf("Num cost: %v", err)
		}
	}
	rec, err := s.Q.InsertModelRecord(ctx, db.InsertModelRecordParams{
		AnswerID:        answerID,
		RunID:           pgtype.Int8{Int64: runID, Valid: true},
		Provider:        pgtype.Text{String: "test-provider", Valid: true},
		ModelID:         pgtype.Text{String: fmt.Sprintf("test-model-%d", modelRecordSeq), Valid: true},
		MethodVersionID: pgtype.Int8{Int64: methodVersionID, Valid: true},
		RubricVersionID: rubricVersionID,
		GradedImageShas: []string{},
		CriterionScores: scores,
		Total:           totalNum,
		Comment:         "ai grade",
		Adjustments:     []byte(`[]`),
		InputTokens:     pgtype.Int4{Int32: inputTok, Valid: true},
		OutputTokens:    pgtype.Int4{Int32: outputTok, Valid: true},
		CostUsd:         costNum,
	})
	if err != nil {
		t.Fatalf("InsertModelRecord: %v", err)
	}
	return rec
}

// mustRegradeAIRecord inserts a source='regrade_ai' grading_record (the stricter AI
// re-grade result, spec §8/migration 0024) with the given cost — used to prove the
// record's provider spend counts toward month-to-date (Finding 2).
func mustRegradeAIRecord(t *testing.T, s *store.Store, answerID, rubricVersionID int64, costUSD string) db.GradingRecord {
	t.Helper()
	ctx := context.Background()

	var costNum pgtype.Numeric
	if costUSD != "" {
		var err error
		costNum, err = store.Num(costUSD)
		if err != nil {
			t.Fatalf("Num cost: %v", err)
		}
	}
	rec, err := s.Q.InsertRegradeAIRecord(ctx, db.InsertRegradeAIRecordParams{
		AnswerID:        answerID,
		ModelID:         pgtype.Text{String: "regrade-model", Valid: true},
		RubricVersionID: rubricVersionID,
		GradedImageShas: []string{},
		CriterionScores: []byte(`[]`),
		Comment:         "stricter re-examination",
		Adjustments:     []byte(`[]`),
		CostUsd:         costNum,
		Policy:          pgtype.Text{String: "regrade_strict", Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertRegradeAIRecord: %v", err)
	}
	return rec
}

// mustRun creates a minimal grading_runs row.
func mustRun(t *testing.T, s *store.Store, f fixture) db.GradingRun {
	t.Helper()
	run, err := s.Q.CreateRun(context.Background(), db.CreateRunParams{
		AssessmentID:    f.AssessmentID,
		ScopeKind:       "assessment",
		ScopeID:         f.AssessmentID,
		MethodVersionID: f.MethodVersionID,
		ExecutionMode:   "sync",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run
}
