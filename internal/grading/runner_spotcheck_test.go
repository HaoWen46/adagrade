package grading

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// spotCheckHarness wires a real Runner (fake provider, local-disk blobstore) against
// a fresh migrated DB — the internal/grading-package equivalent of httpapi's
// phase4Setup/driveRun, since those helpers live in internal/httpapi and this fix
// must stay scoped to internal/grading.
type spotCheckHarness struct {
	st        *store.Store
	blobs     blobstore.Store
	runner    *Runner
	fakeProv  *fake.Provider
	checkerID int64
}

func newSpotCheckHarness(t *testing.T) *spotCheckHarness {
	t.Helper()
	ctx := context.Background()
	st := storetest.Fresh(t)
	blobs, err := blobstore.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatalf("blobstore: %v", err)
	}
	fakeProv := &fake.Provider{}
	runner := &Runner{
		Store:     st,
		Blobs:     blobs,
		Providers: llm.StaticSource{"fake": fakeProv},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	checker, err := st.Q.CreateUser(ctx, db.CreateUserParams{
		Email: "checker@x.edu", DisplayName: "Checker", Role: "ta", Active: true,
	})
	if err != nil {
		t.Fatalf("create checker user: %v", err)
	}
	return &spotCheckHarness{st: st, blobs: blobs, runner: runner, fakeProv: fakeProv, checkerID: checker.ID}
}

// buildRun creates one assessment, one problem (2 rubric criteria), nStudents
// answers each with one accepted-masked page, a fake-provider method, and a
// pending grading_runs row scoped to the whole assessment. Returns the run id.
func (h *spotCheckHarness) buildRun(t *testing.T, nStudents int) int64 {
	t.Helper()
	ctx := context.Background()
	q := h.st.Q

	asm, err := q.CreateAssessment(ctx, db.CreateAssessmentParams{Kind: "exam", Name: "T"})
	if err != nil {
		t.Fatalf("create assessment: %v", err)
	}
	tenPts, _ := store.Num("10")
	prob, err := q.CreateProblem(ctx, db.CreateProblemParams{
		AssessmentID: asm.ID, Number: 1, Title: "P1", MaxPoints: tenPts, Position: 1,
	})
	if err != nil {
		t.Fatalf("create problem: %v", err)
	}
	half, _ := store.Num("0.5")
	rv, err := q.CreateRubricVersion(ctx, db.CreateRubricVersionParams{ProblemID: prob.ID, ScoreIncrement: half})
	if err != nil {
		t.Fatalf("create rubric version: %v", err)
	}
	sixPts, _ := store.Num("6")
	fourPts, _ := store.Num("4")
	if _, err := q.CreateRubricCriterion(ctx, db.CreateRubricCriterionParams{
		RubricVersionID: rv.ID, Position: 1, Description: "A", Points: sixPts,
	}); err != nil {
		t.Fatalf("create criterion: %v", err)
	}
	if _, err := q.CreateRubricCriterion(ctx, db.CreateRubricCriterionParams{
		RubricVersionID: rv.ID, Position: 2, Description: "B", Points: fourPts,
	}); err != nil {
		t.Fatalf("create criterion: %v", err)
	}

	tpl, err := q.CreatePromptTemplateVersion(ctx, db.CreatePromptTemplateVersionParams{
		Name: "t", SystemTemplate: "sys {{.ProblemNumber}}", UserTemplate: "usr {{.MaxPoints}}",
	})
	if err != nil {
		t.Fatalf("create prompt template: %v", err)
	}
	method, err := q.CreateGradingMethod(ctx, "fake method")
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
		sid := "s" + string(rune('a'+i))
		student, err := q.UpsertStudent(ctx, db.UpsertStudentParams{StudentID: sid, Name: sid, Email: sid + "@x.edu"})
		if err != nil {
			t.Fatalf("upsert student: %v", err)
		}
		sub, err := q.CreateSubmission(ctx, db.CreateSubmissionParams{
			AssessmentID: asm.ID, StudentID: student.ID, OriginalFilename: sid + ".pdf",
			SourceRef: "blob/" + sid, SourceSha256: "sha-" + sid, SourceKind: "pdf", PageCount: 1,
		})
		if err != nil {
			t.Fatalf("create submission: %v", err)
		}
		answer, err := q.EnsureAnswer(ctx, db.EnsureAnswerParams{AssessmentID: asm.ID, StudentID: student.ID, ProblemID: prob.ID})
		if err != nil {
			t.Fatalf("ensure answer: %v", err)
		}
		page, err := q.CreateAnswerPage(ctx, db.CreateAnswerPageParams{
			AnswerID: answer.ID, PageIndex: 0, SubmissionID: sub.ID, PdfPageIndex: 0,
			ImageRef: "raw/" + sid, ImageSha256: "img-sha-" + sid, ImageWidth: 100, ImageHeight: 100,
		})
		if err != nil {
			t.Fatalf("create answer page: %v", err)
		}
		maskedKey := "assessments/masked/" + sid + ".jpg"
		if _, _, err := h.blobs.Put(ctx, maskedKey, bytesReader([]byte("fake-jpeg-"+sid))); err != nil {
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

	run, err := q.CreateRun(ctx, db.CreateRunParams{
		AssessmentID: asm.ID, ScopeKind: "assessment", ScopeID: asm.ID,
		MethodVersionID: mv.ID, ExecutionMode: "sync",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run.ID
}

// bytesReader avoids importing bytes just for one helper call site.
func bytesReader(b []byte) *bytesReaderT { return &bytesReaderT{b: b} }

type bytesReaderT struct {
	b   []byte
	pos int
}

func (r *bytesReaderT) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

// driveRun plans and executes every pending/running leaf of a run to completion,
// mirroring httpapi's driveRun test helper (which this package cannot import).
func driveSpotCheckRun(t *testing.T, h *spotCheckHarness, runID int64, finalAttempt bool) {
	t.Helper()
	ctx := context.Background()
	if err := h.runner.Plan(ctx, runID); err != nil {
		t.Fatalf("plan: %v", err)
	}
	items, err := h.st.Q.ListRunItems(ctx, db.ListRunItemsParams{RunID: runID, ItemLimit: 100000})
	if err != nil {
		t.Fatalf("list run items: %v", err)
	}
	for _, it := range items {
		if it.State == "pending" || it.State == "running" {
			_ = h.runner.ExecuteLeaf(ctx, it.ID, finalAttempt) // leaf errors land on the item row
		}
	}
}

// TestCreateSpotCheckSample_StableAcrossRetryRecompletion is the regression test for
// the finding at runner.go's createSpotCheckSample: a run that completes, gets its
// spot-check sample verdicted (gate open), then has a leaf fail and get
// retry-failed'd — causing the run to re-complete — must NOT get a second, larger
// sample inserted. A second insertion re-blocks a gate the checker already cleared,
// because InsertSpotChecks only dedupes rows already present; it doesn't stop new
// ones from being APPENDED once the pool (and therefore the run-id-seeded selection)
// has grown.
func TestCreateSpotCheckSample_StableAcrossRetryRecompletion(t *testing.T) {
	h := newSpotCheckHarness(t)
	ctx := context.Background()

	// Enough students that the sample is a strict subset of the pool (floor 5,
	// 5% of 30 = 1 -> floored to 5), so a bigger pool on re-completion would change
	// which records get selected if the bug were present.
	const nStudents = 30
	runID := h.buildRun(t, nStudents)

	// Script the fake provider to terminally fail exactly ONE leaf on this first
	// pass. That excludes it from the succeeded pool the FIRST createSpotCheckSample
	// call draws from — the run still completes (MaybeCompleteRun only waits for no
	// pending/running items, not zero failures), just with a smaller graded pool.
	h.fakeProv.Script = []fake.Step{{Err: errors.New("simulated transient failure")}}
	driveSpotCheckRun(t, h, runID, true) // finalAttempt: convert the scripted error to a terminal failed item

	run, err := h.st.Q.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "completed" {
		t.Fatalf("run status = %q, want completed", run.Status)
	}
	counts, err := h.st.Q.RunItemStateCounts(ctx, runID)
	if err != nil {
		t.Fatalf("run item state counts: %v", err)
	}
	var nFailed, nSucceeded int64
	for _, c := range counts {
		switch c.State {
		case "failed":
			nFailed = c.N
		case "succeeded":
			nSucceeded = c.N
		}
	}
	if nFailed != 1 {
		t.Fatalf("expected exactly 1 failed item after first pass, got %d (counts=%v)", nFailed, counts)
	}
	if nSucceeded != nStudents-1 {
		t.Fatalf("expected %d succeeded items after first pass, got %d", nStudents-1, nSucceeded)
	}

	totalBefore, doneBefore, waivedBefore, err := h.st.SpotCheckState(ctx, runID)
	if err != nil {
		t.Fatalf("spot check state: %v", err)
	}
	if totalBefore == 0 {
		t.Fatalf("expected a spot-check sample to have been created, got total=0")
	}
	if waivedBefore {
		t.Fatalf("run should not start waived")
	}

	sampleBefore, err := h.st.ListSpotChecks(ctx, runID)
	if err != nil {
		t.Fatalf("list spot checks: %v", err)
	}
	idsBefore := spotCheckRecordIDs(sampleBefore)

	// Verdict every sampled record: the gate opens.
	for _, sc := range sampleBefore {
		if _, err := h.st.SetSpotCheckVerdict(ctx, sc.ID, "agree", "", h.checkerID); err != nil {
			t.Fatalf("set verdict: %v", err)
		}
	}
	_, doneAfterVerdicts, _, err := h.st.SpotCheckState(ctx, runID)
	if err != nil {
		t.Fatalf("spot check state: %v", err)
	}
	if doneAfterVerdicts != totalBefore {
		t.Fatalf("done=%d after verdicting all, want %d (gate should be open)", doneAfterVerdicts, totalBefore)
	}
	_ = doneBefore

	// Simulate the retry-failed HTTP handler's cycle (internal/httpapi/runs.go
	// handleRetryFailed): reset the failed item to pending via the same store method
	// it uses, flip the run back to running, then drive it to completion again. This
	// is the pending->running->completed transition a second time — exactly the
	// MaybeCompleteRun edge that re-triggers createSpotCheckSample — and this time
	// the fake provider has no more scripted errors, so the previously-failed leaf
	// succeeds, GROWING the succeeded pool from nStudents-1 to nStudents.
	resetIDs, err := h.st.Q.ResetFailedItems(ctx, runID)
	if err != nil {
		t.Fatalf("reset failed items: %v", err)
	}
	if len(resetIDs) != 1 {
		t.Fatalf("expected 1 reset item, got %d", len(resetIDs))
	}
	if _, err := h.st.Q.SetRunStatus(ctx, db.SetRunStatusParams{ID: runID, Status: "running"}); err != nil {
		t.Fatalf("set run running: %v", err)
	}

	driveSpotCheckRun(t, h, runID, false)

	run, err = h.st.Q.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run (after retry): %v", err)
	}
	if run.Status != "completed" {
		t.Fatalf("run status after retry = %q, want completed", run.Status)
	}

	// The regression check: sample must be UNCHANGED (same ids, same total), and the
	// gate must still read open (done == total, no fresh unchecked rows appended).
	totalAfter, doneAfter, waivedAfter, err := h.st.SpotCheckState(ctx, runID)
	if err != nil {
		t.Fatalf("spot check state (after retry): %v", err)
	}
	if totalAfter != totalBefore {
		t.Fatalf("spot-check total changed across retry re-completion: before=%d after=%d", totalBefore, totalAfter)
	}
	if !waivedAfter && doneAfter != totalAfter {
		t.Fatalf("gate re-blocked after retry re-completion: done=%d total=%d waived=%v", doneAfter, totalAfter, waivedAfter)
	}

	sampleAfter, err := h.st.ListSpotChecks(ctx, runID)
	if err != nil {
		t.Fatalf("list spot checks (after retry): %v", err)
	}
	idsAfter := spotCheckRecordIDs(sampleAfter)
	if len(idsAfter) != len(idsBefore) {
		t.Fatalf("sample size changed: before=%d after=%d", len(idsBefore), len(idsAfter))
	}
	for id := range idsBefore {
		if !idsAfter[id] {
			t.Fatalf("sample record set changed: record %d present before retry, missing after", id)
		}
	}
	for id := range idsAfter {
		if !idsBefore[id] {
			t.Fatalf("sample record set changed: record %d appeared after retry re-completion (not in original sample)", id)
		}
	}
}

func spotCheckRecordIDs(rows []db.ListSpotChecksRow) map[int64]bool {
	out := make(map[int64]bool, len(rows))
	for _, r := range rows {
		out[r.GradingRecordID] = true
	}
	return out
}
