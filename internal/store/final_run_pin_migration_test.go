package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

func TestMigration0035_BackfillsExactFinalRunAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	dsn := storetest.DSN(t)
	if err := store.MigrateDownTo(ctx, dsn, 34); err != nil {
		t.Fatalf("migrate down to 0034: %v", err)
	}

	var methodID, methodVersionID int64
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO grading_methods (name) VALUES ('migration pin method') RETURNING id`,
	).Scan(&methodID); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO grading_method_versions (method_id, version, config) VALUES ($1, 1, '{}') RETURNING id`, methodID,
	).Scan(&methodVersionID); err != nil {
		t.Fatal(err)
	}

	var pinnedAssessmentID, noRunAssessmentID int64
	for _, row := range []struct {
		name string
		id   *int64
	}{
		{"legacy method with runs", &pinnedAssessmentID},
		{"legacy method without run", &noRunAssessmentID},
	} {
		if err := s.Pool.QueryRow(ctx, `
			INSERT INTO assessments (kind, name, final_source_kind, final_method_id)
			VALUES ('exam', $1, 'method', $2) RETURNING id`, row.name, methodID,
		).Scan(row.id); err != nil {
			t.Fatal(err)
		}
	}

	var olderRunID, newestRunID int64
	for _, dest := range []*int64{&olderRunID, &newestRunID} {
		if err := s.Pool.QueryRow(ctx, `
			INSERT INTO grading_runs
				(assessment_id, scope_kind, scope_id, method_version_id, status, finished_at)
			VALUES ($1, 'assessment', $1, $2, 'completed', now()) RETURNING id`,
			pinnedAssessmentID, methodVersionID,
		).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}

	var problemID, rubricID int64
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO problems (assessment_id, number, title, max_points, position)
		VALUES ($1, 1, 'Synthetic problem', 10, 1) RETURNING id`, pinnedAssessmentID,
	).Scan(&problemID); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO rubric_versions (problem_id, version) VALUES ($1, 1) RETURNING id`, problemID,
	).Scan(&rubricID); err != nil {
		t.Fatal(err)
	}

	answerIDs := make([]int64, 2)
	for i := range answerIDs {
		var studentID int64
		if err := s.Pool.QueryRow(ctx, `
			INSERT INTO students (student_id, name, email)
			VALUES ($1, $2, $3) RETURNING id`,
			"migration-student-"+string(rune('1'+i)), "Synthetic Student", "migration"+string(rune('1'+i))+"@example.test",
		).Scan(&studentID); err != nil {
			t.Fatal(err)
		}
		if err := s.Pool.QueryRow(ctx, `
			INSERT INTO answers (assessment_id, student_id, problem_id)
			VALUES ($1, $2, $3) RETURNING id`, pinnedAssessmentID, studentID, problemID,
		).Scan(&answerIDs[i]); err != nil {
			t.Fatal(err)
		}
	}

	olderRecords := make([]int64, 2)
	newerRecords := make([]int64, 2)
	for i, answerID := range answerIDs {
		for _, rec := range []struct {
			runID int64
			total int
			dest  *int64
		}{{olderRunID, 3, &olderRecords[i]}, {newestRunID, 7, &newerRecords[i]}} {
			if err := s.Pool.QueryRow(ctx, `
				INSERT INTO grading_records
					(answer_id, run_id, source, model_id, method_version_id, rubric_version_id, criterion_scores, total)
				VALUES ($1, $2, 'model', 'synthetic-model', $3, $4, '[]', $5)
				RETURNING id`, answerID, rec.runID, methodVersionID, rubricID, rec.total,
			).Scan(rec.dest); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Both legacy pointers are stale on the older run. The first is unpublished
	// and must move to the backfilled run; the second is published and immutable.
	if _, err := s.Pool.Exec(ctx, `
		UPDATE answers
		SET official_record_id = CASE id WHEN $1::bigint THEN $2::bigint WHEN $3::bigint THEN $4::bigint END,
		    published_at = CASE WHEN id = $3::bigint THEN now() ELSE NULL END
		WHERE id = ANY($5::bigint[])`,
		answerIDs[0], olderRecords[0], answerIDs[1], olderRecords[1], answerIDs,
	); err != nil {
		t.Fatal(err)
	}

	if err := store.MigrateUpTo(ctx, dsn, 35); err != nil {
		t.Fatalf("migrate up to 0035: %v", err)
	}

	var gotRun pgtype.Int8
	if err := s.Pool.QueryRow(ctx,
		`SELECT final_run_id FROM assessments WHERE id = $1`, pinnedAssessmentID,
	).Scan(&gotRun); err != nil {
		t.Fatal(err)
	}
	if !gotRun.Valid || gotRun.Int64 != newestRunID {
		t.Fatalf("legacy source pinned run = %+v, want latest completed run %d (not %d)", gotRun, newestRunID, olderRunID)
	}
	for _, tc := range []struct {
		name string
		id   int64
		want int64
	}{
		{"unpublished official recomputed", answerIDs[0], newerRecords[0]},
		{"published official preserved", answerIDs[1], olderRecords[1]},
	} {
		var got int64
		if err := s.Pool.QueryRow(ctx, `SELECT official_record_id FROM answers WHERE id = $1`, tc.id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("%s: official_record_id=%d want %d", tc.name, got, tc.want)
		}
	}

	var kind pgtype.Text
	var method, run pgtype.Int8
	if err := s.Pool.QueryRow(ctx, `
		SELECT final_source_kind, final_method_id, final_run_id
		FROM assessments WHERE id = $1`, noRunAssessmentID,
	).Scan(&kind, &method, &run); err != nil {
		t.Fatal(err)
	}
	if kind.Valid || method.Valid || run.Valid {
		t.Fatalf("legacy source without completed run must be unset, got kind=%+v method=%+v run=%+v", kind, method, run)
	}

	// The deferred circular FK must not make assessment deletion impossible:
	// deleting the assessment cascades its selected run in the same transaction.
	if _, err := s.Pool.Exec(ctx, `DELETE FROM assessments WHERE id = $1`, pinnedAssessmentID); err != nil {
		t.Fatalf("delete assessment with pinned run: %v", err)
	}
}

// TestMigration0035_SecondRungVotesForOfficialRunOrFailsClosed is A6's
// RED/GREEN pin for 0035's second backfill rung. Rung 1 (above) only
// recovers "the latest completed run for the assessment's declared method";
// a legacy assessment may have moved methods, or its officials may simply
// point at a different run than the declared method's latest. Before giving
// up, rung 2 recovers the run that actually backs the assessment's current
// official model records — the most-represented grading_records.run_id
// among answers' official_record_id rows with source='model' — but only if
// that run independently passes the SAME guards SetAssessmentFinalSource
// enforces live (audit A3/A4-minimal: completed, assessment-scoped, at
// least one succeeded leaf). A candidate that fails any of those must still
// fail closed to NULL, exactly like finding no candidate at all.
func TestMigration0035_SecondRungVotesForOfficialRunOrFailsClosed(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	dsn := storetest.DSN(t)
	if err := store.MigrateDownTo(ctx, dsn, 34); err != nil {
		t.Fatalf("migrate down to 0034: %v", err)
	}

	// declaredMethod is what each assessment's final_method_id already
	// points at, and deliberately has NO completed runs — rung 1 never
	// fires in this test, so only rung 2's vote-recovery is exercised.
	var declaredMethodID int64
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO grading_methods (name) VALUES ('declared method, no runs') RETURNING id`,
	).Scan(&declaredMethodID); err != nil {
		t.Fatal(err)
	}

	// votedMethod backs the run the answers' official pointers actually
	// reference — a DIFFERENT method than declaredMethodID, proving rung 2
	// recovers the true source (and corrects final_method_id to match) even
	// when it disagrees with the stale declared method.
	var votedMethodID, votedMethodVersionID int64
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO grading_methods (name) VALUES ('voted method, has runs') RETURNING id`,
	).Scan(&votedMethodID); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx,
		`INSERT INTO grading_method_versions (method_id, version, config) VALUES ($1, 1, '{}') RETURNING id`, votedMethodID,
	).Scan(&votedMethodVersionID); err != nil {
		t.Fatal(err)
	}

	newAssessment := func(name string) int64 {
		var id int64
		if err := s.Pool.QueryRow(ctx, `
			INSERT INTO assessments (kind, name, final_source_kind, final_method_id)
			VALUES ('exam', $1, 'method', $2) RETURNING id`, name, declaredMethodID,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	recoverableID := newAssessment("recoverable via votes")
	guardFailsID := newAssessment("candidate fails guards")
	crossOwnerID := newAssessment("official votes for foreign run")
	tieBreakID := newAssessment("split vote tie-break")

	// foreignID owns the run crossOwnerID's corrupted official points at. It
	// has NO final_source_kind, so no backfill rung ever touches it.
	var foreignID int64
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO assessments (kind, name) VALUES ('exam', 'foreign run owner') RETURNING id`,
	).Scan(&foreignID); err != nil {
		t.Fatal(err)
	}

	newProblemAndRubric := func(assessmentID int64) (problemID, rubricID int64) {
		if err := s.Pool.QueryRow(ctx, `
			INSERT INTO problems (assessment_id, number, title, max_points, position)
			VALUES ($1, 1, 'Synthetic problem', 10, 1) RETURNING id`, assessmentID,
		).Scan(&problemID); err != nil {
			t.Fatal(err)
		}
		if err := s.Pool.QueryRow(ctx, `
			INSERT INTO rubric_versions (problem_id, version) VALUES ($1, 1) RETURNING id`, problemID,
		).Scan(&rubricID); err != nil {
			t.Fatal(err)
		}
		return
	}
	newStudentAndAnswer := func(assessmentID, problemID int64, tag string) int64 {
		var studentID, answerID int64
		if err := s.Pool.QueryRow(ctx, `
			INSERT INTO students (student_id, name, email)
			VALUES ($1, 'Synthetic Student', $2) RETURNING id`,
			t.Name()+"-"+tag, tag+"@example.test",
		).Scan(&studentID); err != nil {
			t.Fatal(err)
		}
		if err := s.Pool.QueryRow(ctx, `
			INSERT INTO answers (assessment_id, student_id, problem_id)
			VALUES ($1, $2, $3) RETURNING id`, assessmentID, studentID, problemID,
		).Scan(&answerID); err != nil {
			t.Fatal(err)
		}
		return answerID
	}

	// --- recoverableID: a completed, assessment-scoped run with a succeeded
	// leaf backs its official pointer. Rung 2 must pin it, and correct
	// final_method_id to the recovered run's real method.
	recoverableProblemID, recoverableRubricID := newProblemAndRubric(recoverableID)
	recoverableAnswerID := newStudentAndAnswer(recoverableID, recoverableProblemID, "recoverable")

	var recoverableRunID int64
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO grading_runs (assessment_id, scope_kind, scope_id, method_version_id, status, finished_at)
		VALUES ($1, 'assessment', $1, $2, 'completed', now()) RETURNING id`,
		recoverableID, votedMethodVersionID,
	).Scan(&recoverableRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO grading_run_items (run_id, answer_id, model_id, provider, state)
		VALUES ($1, $2, 'synthetic-model', 'synthetic-provider', 'succeeded')`,
		recoverableRunID, recoverableAnswerID,
	); err != nil {
		t.Fatal(err)
	}
	var recoverableRecordID int64
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO grading_records
			(answer_id, run_id, source, model_id, method_version_id, rubric_version_id, criterion_scores, total)
		VALUES ($1, $2, 'model', 'synthetic-model', $3, $4, '[]', 8)
		RETURNING id`, recoverableAnswerID, recoverableRunID, votedMethodVersionID, recoverableRubricID,
	).Scan(&recoverableRecordID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx,
		`UPDATE answers SET official_record_id = $2 WHERE id = $1`,
		recoverableAnswerID, recoverableRecordID,
	); err != nil {
		t.Fatal(err)
	}

	// --- guardFailsID: a completed, assessment-scoped run backs its
	// official pointer too, but has ZERO succeeded leaves (only a failed
	// one) — the same zero-leaf guard SetAssessmentFinalSource enforces
	// live (A3). Rung 2 must refuse to pin it and fall through to NULL.
	guardFailsProblemID, guardFailsRubricID := newProblemAndRubric(guardFailsID)
	guardFailsAnswerID := newStudentAndAnswer(guardFailsID, guardFailsProblemID, "guard-fails")

	var guardFailsRunID int64
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO grading_runs (assessment_id, scope_kind, scope_id, method_version_id, status, finished_at)
		VALUES ($1, 'assessment', $1, $2, 'completed', now()) RETURNING id`,
		guardFailsID, votedMethodVersionID,
	).Scan(&guardFailsRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO grading_run_items (run_id, answer_id, model_id, provider, state)
		VALUES ($1, $2, 'synthetic-model', 'synthetic-provider', 'failed')`,
		guardFailsRunID, guardFailsAnswerID,
	); err != nil {
		t.Fatal(err)
	}
	var guardFailsRecordID int64
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO grading_records
			(answer_id, run_id, source, model_id, method_version_id, rubric_version_id, criterion_scores, total)
		VALUES ($1, $2, 'model', 'synthetic-model', $3, $4, '[]', 8)
		RETURNING id`, guardFailsAnswerID, guardFailsRunID, votedMethodVersionID, guardFailsRubricID,
	).Scan(&guardFailsRecordID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx,
		`UPDATE answers SET official_record_id = $2 WHERE id = $1`,
		guardFailsAnswerID, guardFailsRecordID,
	); err != nil {
		t.Fatal(err)
	}

	// --- crossOwnerID: its official pointer references a record whose run_id
	// is ANOTHER assessment's run — completed, assessment-scoped, with a
	// succeeded leaf, so it passes every per-run guard. grading_records'
	// run_id and answer_id are independent FKs with no cross-check, so this
	// legacy/corrupted shape is representable; rung 2 must refuse to pin a
	// run the assessment does not own (the live path's ErrFinalRunInvalid
	// ownership check) and fall through to NULL.
	foreignProblemID, _ := newProblemAndRubric(foreignID)
	foreignAnswerID := newStudentAndAnswer(foreignID, foreignProblemID, "foreign-owner")

	var foreignRunID int64
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO grading_runs (assessment_id, scope_kind, scope_id, method_version_id, status, finished_at)
		VALUES ($1, 'assessment', $1, $2, 'completed', now()) RETURNING id`,
		foreignID, votedMethodVersionID,
	).Scan(&foreignRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO grading_run_items (run_id, answer_id, model_id, provider, state)
		VALUES ($1, $2, 'synthetic-model', 'synthetic-provider', 'succeeded')`,
		foreignRunID, foreignAnswerID,
	); err != nil {
		t.Fatal(err)
	}

	crossOwnerProblemID, crossOwnerRubricID := newProblemAndRubric(crossOwnerID)
	crossOwnerAnswerID := newStudentAndAnswer(crossOwnerID, crossOwnerProblemID, "cross-owner")
	var crossOwnerRecordID int64
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO grading_records
			(answer_id, run_id, source, model_id, method_version_id, rubric_version_id, criterion_scores, total)
		VALUES ($1, $2, 'model', 'synthetic-model', $3, $4, '[]', 8)
		RETURNING id`, crossOwnerAnswerID, foreignRunID, votedMethodVersionID, crossOwnerRubricID,
	).Scan(&crossOwnerRecordID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx,
		`UPDATE answers SET official_record_id = $2 WHERE id = $1`,
		crossOwnerAnswerID, crossOwnerRecordID,
	); err != nil {
		t.Fatal(err)
	}

	// --- tieBreakID: two of its own runs, both fully guard-passing, one
	// official vote each. The documented tie-break (n DESC, run_id DESC)
	// must pin the NEWER run — mirroring rung 1's "latest wins" precedent.
	tieProblemID, tieRubricID := newProblemAndRubric(tieBreakID)
	tieAnswer1ID := newStudentAndAnswer(tieBreakID, tieProblemID, "tie-1")
	tieAnswer2ID := newStudentAndAnswer(tieBreakID, tieProblemID, "tie-2")

	var tieOlderRunID, tieNewerRunID int64
	for _, dest := range []*int64{&tieOlderRunID, &tieNewerRunID} {
		if err := s.Pool.QueryRow(ctx, `
			INSERT INTO grading_runs (assessment_id, scope_kind, scope_id, method_version_id, status, finished_at)
			VALUES ($1, 'assessment', $1, $2, 'completed', now()) RETURNING id`,
			tieBreakID, votedMethodVersionID,
		).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	tieRecords := make(map[int64]int64, 2) // answer id -> record id
	for _, pair := range []struct {
		answerID, runID int64
	}{{tieAnswer1ID, tieOlderRunID}, {tieAnswer2ID, tieNewerRunID}} {
		if _, err := s.Pool.Exec(ctx, `
			INSERT INTO grading_run_items (run_id, answer_id, model_id, provider, state)
			VALUES ($1, $2, 'synthetic-model', 'synthetic-provider', 'succeeded')`,
			pair.runID, pair.answerID,
		); err != nil {
			t.Fatal(err)
		}
		var recID int64
		if err := s.Pool.QueryRow(ctx, `
			INSERT INTO grading_records
				(answer_id, run_id, source, model_id, method_version_id, rubric_version_id, criterion_scores, total)
			VALUES ($1, $2, 'model', 'synthetic-model', $3, $4, '[]', 6)
			RETURNING id`, pair.answerID, pair.runID, votedMethodVersionID, tieRubricID,
		).Scan(&recID); err != nil {
			t.Fatal(err)
		}
		tieRecords[pair.answerID] = recID
		if _, err := s.Pool.Exec(ctx,
			`UPDATE answers SET official_record_id = $2 WHERE id = $1`,
			pair.answerID, recID,
		); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.MigrateUpTo(ctx, dsn, 35); err != nil {
		t.Fatalf("migrate up to 0035: %v", err)
	}

	var gotRun, gotMethod pgtype.Int8
	if err := s.Pool.QueryRow(ctx,
		`SELECT final_run_id, final_method_id FROM assessments WHERE id = $1`, recoverableID,
	).Scan(&gotRun, &gotMethod); err != nil {
		t.Fatal(err)
	}
	if !gotRun.Valid || gotRun.Int64 != recoverableRunID {
		t.Fatalf("recoverable assessment final_run_id = %+v, want %d", gotRun, recoverableRunID)
	}
	if !gotMethod.Valid || gotMethod.Int64 != votedMethodID {
		t.Fatalf("recoverable assessment final_method_id = %+v, want %d (the recovered run's real method)", gotMethod, votedMethodID)
	}

	var gotOfficial int64
	if err := s.Pool.QueryRow(ctx,
		`SELECT official_record_id FROM answers WHERE id = $1`, recoverableAnswerID,
	).Scan(&gotOfficial); err != nil {
		t.Fatal(err)
	}
	if gotOfficial != recoverableRecordID {
		t.Fatalf("recoverable answer official_record_id = %d, want %d (re-derived from the recovered run)", gotOfficial, recoverableRecordID)
	}

	var kind pgtype.Text
	var method, run pgtype.Int8
	if err := s.Pool.QueryRow(ctx, `
		SELECT final_source_kind, final_method_id, final_run_id
		FROM assessments WHERE id = $1`, guardFailsID,
	).Scan(&kind, &method, &run); err != nil {
		t.Fatal(err)
	}
	if kind.Valid || method.Valid || run.Valid {
		t.Fatalf("guard-failing candidate must still fail closed, got kind=%+v method=%+v run=%+v", kind, method, run)
	}

	var guardFailsOfficial pgtype.Int8
	if err := s.Pool.QueryRow(ctx,
		`SELECT official_record_id FROM answers WHERE id = $1`, guardFailsAnswerID,
	).Scan(&guardFailsOfficial); err != nil {
		t.Fatal(err)
	}
	if guardFailsOfficial.Valid {
		t.Fatalf("guard-failing candidate's official must be cleared with the rest of the fail-closed branch, got %+v", guardFailsOfficial)
	}

	// Ownership guard: the foreign-owned run must NOT be pinned, no matter
	// how cleanly it passes the per-run guards — fall through to NULL.
	if err := s.Pool.QueryRow(ctx, `
		SELECT final_source_kind, final_method_id, final_run_id
		FROM assessments WHERE id = $1`, crossOwnerID,
	).Scan(&kind, &method, &run); err != nil {
		t.Fatal(err)
	}
	if kind.Valid || method.Valid || run.Valid {
		t.Fatalf("foreign-owned candidate must fail closed, got kind=%+v method=%+v run=%+v", kind, method, run)
	}
	var crossOwnerOfficial pgtype.Int8
	if err := s.Pool.QueryRow(ctx,
		`SELECT official_record_id FROM answers WHERE id = $1`, crossOwnerAnswerID,
	).Scan(&crossOwnerOfficial); err != nil {
		t.Fatal(err)
	}
	if crossOwnerOfficial.Valid {
		t.Fatalf("foreign-owned candidate's official must be cleared with the fail-closed branch, got %+v", crossOwnerOfficial)
	}

	// Tie-break: equal votes resolve to the newer run (n DESC, run_id DESC).
	var tieRun, tieMethod pgtype.Int8
	if err := s.Pool.QueryRow(ctx,
		`SELECT final_run_id, final_method_id FROM assessments WHERE id = $1`, tieBreakID,
	).Scan(&tieRun, &tieMethod); err != nil {
		t.Fatal(err)
	}
	if !tieRun.Valid || tieRun.Int64 != tieNewerRunID {
		t.Fatalf("split vote pinned run = %+v, want newer run %d (not %d)", tieRun, tieNewerRunID, tieOlderRunID)
	}
	if !tieMethod.Valid || tieMethod.Int64 != votedMethodID {
		t.Fatalf("split vote final_method_id = %+v, want %d", tieMethod, votedMethodID)
	}
	// The re-derive then keeps the winning run's official and clears the
	// loser's (no record on the pinned run, no human fallback).
	for _, tc := range []struct {
		name   string
		id     int64
		want   int64
		wantOK bool
	}{
		{"tie loser official cleared", tieAnswer1ID, 0, false},
		{"tie winner official kept", tieAnswer2ID, tieRecords[tieAnswer2ID], true},
	} {
		var got pgtype.Int8
		if err := s.Pool.QueryRow(ctx, `SELECT official_record_id FROM answers WHERE id = $1`, tc.id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got.Valid != tc.wantOK || (tc.wantOK && got.Int64 != tc.want) {
			t.Fatalf("%s: official_record_id=%+v want valid=%t id=%d", tc.name, got, tc.wantOK, tc.want)
		}
	}
}
