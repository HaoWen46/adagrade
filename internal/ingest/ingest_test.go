package ingest

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// fixture: assessment with 2 problems, roster of b01/b02, fake 2-page renderer,
// real local-disk blobstore in a temp dir.
type fx struct {
	svc *Service
	st  *store.Store
	aid int64
	ctx context.Context
}

func setup(t *testing.T) fx {
	t.Helper()
	ctx := context.Background()
	st := storetest.Fresh(t)

	blobs, err := blobstore.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	a, err := st.Q.CreateAssessment(ctx, db.CreateAssessmentParams{Kind: "exam", Name: "Midterm"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		mp, _ := store.Num("10")
		if _, err := st.Q.CreateProblem(ctx, db.CreateProblemParams{
			AssessmentID: a.ID, Number: int32(i), MaxPoints: mp, Position: int32(i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, sid := range []string{"b01", "b02"} {
		if _, err := st.Q.UpsertStudent(ctx, db.UpsertStudentParams{StudentID: sid, Name: "Student " + sid, Email: sid + "@x.edu"}); err != nil {
			t.Fatal(err)
		}
	}

	return fx{
		svc: &Service{Store: st, Blobs: blobs, Renderer: render.NewFake(2)},
		st:  st, aid: a.ID, ctx: ctx,
	}
}

func TestIngest_HappyPathMaterializesAndMaps(t *testing.T) {
	f := setup(t)

	res := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-fake"), 0, false)
	if res.Status != "ingested" || res.MappedPages != 2 {
		t.Fatalf("ingest: %+v", res)
	}

	// Answers pre-materialized for the whole roster (2 students x 2 problems).
	answers, err := f.st.Q.ListAnswersForAssessment(f.ctx, f.aid)
	if err != nil || len(answers) != 4 {
		t.Fatalf("answers: got %d want 4 (%v)", len(answers), err)
	}

	// b01's two answers each carry one page; b02's carry none.
	pages, err := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	if err != nil || len(pages) != 2 {
		t.Fatalf("pages: got %d want 2 (%v)", len(pages), err)
	}
	for _, pg := range pages {
		ok, err := f.svc.Blobs.Exists(f.ctx, pg.ImageRef)
		if err != nil || !ok {
			t.Errorf("page image %s missing from blobstore", pg.ImageRef)
		}
		if pg.ImageSha256 == "" || pg.ImageWidth == 0 {
			t.Errorf("page metadata incomplete: %+v", pg)
		}
	}
}

func TestIngest_UnknownFilenameQuarantined(t *testing.T) {
	f := setup(t)
	res := f.svc.IngestFile(f.ctx, f.aid, "mystery.pdf", []byte("%PDF-fake"), 0, false)
	if res.Status != "quarantined" || res.Reason != "unknown_student" {
		t.Fatalf("quarantine: %+v", res)
	}
	open, err := f.st.Q.ListOpenQuarantine(f.ctx, f.aid)
	if err != nil || len(open) != 1 {
		t.Fatalf("open quarantine: %d %v", len(open), err)
	}

	// Assigning it to a student ingests it and resolves the entry.
	ares, err := f.svc.AssignQuarantine(f.ctx, open[0].ID, "b02", 0, false)
	if err != nil || ares.Status != "ingested" {
		t.Fatalf("assign: %+v %v", ares, err)
	}
	open, _ = f.st.Q.ListOpenQuarantine(f.ctx, f.aid)
	if len(open) != 0 {
		t.Errorf("quarantine should be resolved, still open: %d", len(open))
	}
}

// A malformed image reaches the same durable quarantine path as a malformed PDF.
// This is deliberately service-level (not just NormalizeImage's unit test): it
// exercises the migrated upload_quarantine reason CHECK that must admit
// "invalid_image".
func TestIngest_InvalidImageQuarantined(t *testing.T) {
	f := setup(t)
	res := f.svc.Ingest(f.ctx, f.aid, IngestInput{
		Filename: "b01.png",
		Data:     []byte("not an image"),
		Kind:     "image",
	}, 0, false)
	if res.Status != "quarantined" || res.Reason != "invalid_image" {
		t.Fatalf("invalid image quarantine: %+v", res)
	}
	open, err := f.st.Q.ListOpenQuarantine(f.ctx, f.aid)
	if err != nil || len(open) != 1 || open[0].Reason != "invalid_image" {
		t.Fatalf("open invalid-image quarantine: %+v %v", open, err)
	}
}

// Unreadable bytes cannot become readable merely by attaching a roster id. The
// assign path must reject before reading/re-ingesting them; the original row stays
// open and no duplicate quarantine row is created.
func TestAssignQuarantine_RejectsUnreadableWithoutDuplicating(t *testing.T) {
	f := setup(t)
	const ref = "assessments/1/quarantine/unreadable.pdf"
	if _, _, err := f.svc.Blobs.Put(f.ctx, ref, strings.NewReader("not a pdf")); err != nil {
		t.Fatal(err)
	}
	q, err := f.st.Q.CreateQuarantine(f.ctx, db.CreateQuarantineParams{
		AssessmentID: f.aid, OriginalFilename: "b01.pdf", PdfRef: ref,
		PdfSha256: "unreadable", Reason: "invalid_pdf",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.svc.AssignQuarantine(f.ctx, q.ID, "b01", 0, false)
	if !errors.Is(err, ErrQuarantineNotAssignable) {
		t.Fatalf("assign unreadable: got %v", err)
	}
	open, listErr := f.st.Q.ListOpenQuarantine(f.ctx, f.aid)
	if listErr != nil || len(open) != 1 || open[0].ID != q.ID {
		t.Fatalf("assignment must leave exactly the original row open: %+v %v", open, listErr)
	}
}

func TestAssignQuarantine_ImagePreservesDecodeKind(t *testing.T) {
	f := setup(t)
	res := f.svc.Ingest(f.ctx, f.aid, IngestInput{
		Filename: "mystery.png", Data: pngBytes(t, 200, 150), Kind: "image",
	}, 0, false)
	if res.Status != "quarantined" || res.Reason != "unknown_student" {
		t.Fatalf("initial image quarantine: %+v", res)
	}
	open, err := f.st.Q.ListOpenQuarantine(f.ctx, f.aid)
	if err != nil || len(open) != 1 {
		t.Fatalf("open image quarantine: %+v %v", open, err)
	}

	assigned, err := f.svc.AssignQuarantine(f.ctx, open[0].ID, "b02", 0, false)
	if err != nil || assigned.Status != "ingested" {
		t.Fatalf("assign image quarantine: %+v %v", assigned, err)
	}
	sub, err := f.st.Q.GetSubmission(f.ctx, assigned.SubmissionID)
	if err != nil || sub.SourceKind != "image" {
		t.Fatalf("assigned source kind = %q, want image (%v)", sub.SourceKind, err)
	}
}

// A filename mismatch wins before decoding during initial ingest. If the bytes
// later prove unreadable when a TA assigns that filename, reclassify the original
// row in place instead of appending a second quarantine entry.
func TestAssignQuarantine_UnreadableUnknownImageReclassifiesOriginal(t *testing.T) {
	f := setup(t)
	res := f.svc.Ingest(f.ctx, f.aid, IngestInput{
		Filename: "mystery.png", Data: []byte("not an image"), Kind: "image",
	}, 0, false)
	if res.Status != "quarantined" || res.Reason != "unknown_student" {
		t.Fatalf("initial unknown image quarantine: %+v", res)
	}
	open, err := f.st.Q.ListOpenQuarantine(f.ctx, f.aid)
	if err != nil || len(open) != 1 {
		t.Fatalf("initial open quarantine: %+v %v", open, err)
	}
	originalID := open[0].ID

	assigned, err := f.svc.AssignQuarantine(f.ctx, originalID, "b02", 0, false)
	if err != nil || assigned.Status != "quarantined" || assigned.Reason != "invalid_image" {
		t.Fatalf("assign unreadable unknown image: %+v %v", assigned, err)
	}
	open, err = f.st.Q.ListOpenQuarantine(f.ctx, f.aid)
	if err != nil || len(open) != 1 || open[0].ID != originalID || open[0].Reason != "invalid_image" {
		t.Fatalf("unreadable assignment must reclassify one row in place: %+v %v", open, err)
	}
}

// seedRosterID adds one roster row beyond the fixture's b01/b02 and returns it
// (roster-lifecycle tests need extra ids and the row's DB id for withdrawal).
func seedRosterID(t *testing.T, f fx, sid string) db.Student {
	t.Helper()
	st, err := f.st.Q.UpsertStudent(f.ctx, db.UpsertStudentParams{StudentID: sid, Name: "Student " + sid, Email: sid + "@x.edu"})
	if err != nil {
		t.Fatalf("seed roster id: %v", err)
	}
	return st
}

// Roster-lifecycle fix 4: when the filename's id has no exact roster match but
// exactly ONE active roster id is equal under studentid.Normalize (lowercase
// filename vs uppercase roster id), ingest proceeds with that student instead
// of quarantining.
func TestIngest_FilenameNormalizedFallbackMatches(t *testing.T) {
	f := setup(t)
	seedRosterID(t, f, "B11902066")

	res := f.svc.IngestFile(f.ctx, f.aid, "b11902066.pdf", []byte("%PDF-fake"), 0, false)
	if res.Status != "ingested" || res.StudentID != "B11902066" {
		t.Fatalf("normalized fallback ingest: %+v", res)
	}
	open, err := f.st.Q.ListOpenQuarantine(f.ctx, f.aid)
	if err != nil || len(open) != 0 {
		t.Fatalf("open quarantine after fallback match: %d %v", len(open), err)
	}
}

// Two roster ids that collide under studentid.Normalize make the fallback
// ambiguous: the file quarantines exactly as an unknown id does today.
func TestIngest_FilenameNormalizedCollisionQuarantines(t *testing.T) {
	f := setup(t)
	seedRosterID(t, f, "B66")
	seedRosterID(t, f, "b66")

	res := f.svc.IngestFile(f.ctx, f.aid, "b-66.pdf", []byte("%PDF-fake"), 0, false)
	if res.Status != "quarantined" || res.Reason != "unknown_student" {
		t.Fatalf("ambiguous fallback: %+v", res)
	}
}

// The fallback scans ACTIVE ids only: a withdrawn student's id never resolves
// via normalization (the file quarantines), while an EXACT match keeps the
// existing withdraw rejection message.
func TestIngest_NormalizedFallbackSkipsWithdrawn(t *testing.T) {
	f := setup(t)
	row := seedRosterID(t, f, "B77")
	if _, err := f.st.Q.SetStudentWithdrawn(f.ctx, db.SetStudentWithdrawnParams{ID: row.ID, Withdrawn: true}); err != nil {
		t.Fatal(err)
	}

	res := f.svc.IngestFile(f.ctx, f.aid, "b77.pdf", []byte("%PDF-fake"), 0, false)
	if res.Status != "quarantined" || res.Reason != "unknown_student" {
		t.Fatalf("withdrawn student via fallback: %+v", res)
	}

	exact := f.svc.IngestFile(f.ctx, f.aid, "B77.pdf", []byte("%PDF-fake"), 0, false)
	if exact.Status != "rejected" || exact.Reason != "student is withdrawn; reinstate before uploading" {
		t.Fatalf("withdrawn student via exact match: %+v", exact)
	}
}

// Quarantine resolve uses the same exact-then-normalized lookup: a case-variant
// id resolves to the roster's canonical student, and a normalization-ambiguous
// id errors with "ambiguous student id" leaving the entry open.
func TestAssignQuarantine_NormalizedLookupAndAmbiguity(t *testing.T) {
	f := setup(t)
	seedRosterID(t, f, "B11902066")
	seedRosterID(t, f, "B66")
	seedRosterID(t, f, "b66")

	res := f.svc.IngestFile(f.ctx, f.aid, "mystery.pdf", []byte("%PDF-fake"), 0, false)
	if res.Status != "quarantined" {
		t.Fatalf("quarantine: %+v", res)
	}
	open, err := f.st.Q.ListOpenQuarantine(f.ctx, f.aid)
	if err != nil || len(open) != 1 {
		t.Fatalf("open quarantine: %d %v", len(open), err)
	}
	qid := open[0].ID

	if _, err := f.svc.AssignQuarantine(f.ctx, qid, "b-66", 0, false); err == nil || err.Error() != "ambiguous student id" {
		t.Fatalf("ambiguous assign: err = %v, want \"ambiguous student id\"", err)
	}
	open, _ = f.st.Q.ListOpenQuarantine(f.ctx, f.aid)
	if len(open) != 1 {
		t.Fatalf("ambiguous assign must leave the entry open: %d", len(open))
	}

	ares, err := f.svc.AssignQuarantine(f.ctx, qid, "b11902066", 0, false)
	if err != nil || ares.Status != "ingested" || ares.StudentID != "B11902066" {
		t.Fatalf("normalized assign: %+v %v", ares, err)
	}
	open, _ = f.st.Q.ListOpenQuarantine(f.ctx, f.aid)
	if len(open) != 0 {
		t.Errorf("quarantine should be resolved, still open: %d", len(open))
	}
}

func TestIngest_ReuploadSupersedesAndGuards(t *testing.T) {
	f := setup(t)

	first := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-one"), 0, false)
	if first.Status != "ingested" {
		t.Fatal(first)
	}

	// Plain re-upload (ungraded): supersedes, keeps exactly 2 live pages.
	second := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-two"), 0, false)
	if second.Status != "ingested" {
		t.Fatalf("re-upload: %+v", second)
	}
	prev, err := f.st.Q.GetSubmission(f.ctx, first.SubmissionID)
	if err != nil || !prev.SupersededBy.Valid || prev.SupersededBy.Int64 != second.SubmissionID {
		t.Errorf("first submission should be superseded by second: %+v %v", prev, err)
	}
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	if len(pages) != 2 {
		t.Errorf("pages after re-upload: got %d want 2", len(pages))
	}

	// Grade one answer (minimal rubric + record), then re-upload must be blocked...
	answers, _ := f.st.Q.ListAnswersForAssessment(f.ctx, f.aid)
	var b01Answer db.Answer
	for _, a := range answers {
		if len(pages) > 0 && a.ID == pages[0].AnswerID {
			b01Answer = a
		}
	}
	inc, _ := store.Num("0.5")
	rv, err := f.st.Q.CreateRubricVersion(f.ctx, db.CreateRubricVersionParams{ProblemID: b01Answer.ProblemID, ScoreIncrement: inc})
	if err != nil {
		t.Fatal(err)
	}
	total, _ := store.Num("7")
	_, err = f.st.Pool.Exec(f.ctx, `INSERT INTO grading_records (answer_id, source, rubric_version_id, criterion_scores, total, created_by)
		VALUES ($1, 'human', $2, '[]', $3, NULL)`, b01Answer.ID, rv.ID, total)
	if err == nil {
		t.Fatal("human record without created_by must violate the check constraint")
	}
	u, err := f.st.Q.CreateUser(f.ctx, db.CreateUserParams{Email: "ta@x.edu", Role: "ta", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.st.Pool.Exec(f.ctx, `INSERT INTO grading_records (answer_id, source, rubric_version_id, criterion_scores, total, created_by)
		VALUES ($1, 'human', $2, '[]', $3, $4)`, b01Answer.ID, rv.ID, total, u.ID); err != nil {
		t.Fatal(err)
	}

	blocked := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-three"), 0, false)
	if blocked.Status != "rejected" || !strings.Contains(blocked.Reason, "force") {
		t.Fatalf("graded re-upload without force: %+v", blocked)
	}

	// ...and forced re-upload flags the answers image_superseded (D1).
	forced := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-three"), 0, true)
	if forced.Status != "ingested" {
		t.Fatalf("forced re-upload: %+v", forced)
	}
	got, _ := f.st.Q.GetAnswer(f.ctx, b01Answer.ID)
	found := false
	for _, fl := range got.Flags {
		if fl == FlagImageSuperseded {
			found = true
		}
	}
	if !found {
		t.Errorf("answer should be flagged %s: %v", FlagImageSuperseded, got.Flags)
	}

	// Published answers can never be replaced.
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE answers SET published_at = now() WHERE id = $1`, b01Answer.ID); err != nil {
		t.Fatal(err)
	}
	pub := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-four"), 0, true)
	if pub.Status != "rejected" || !strings.Contains(pub.Reason, "published") {
		t.Fatalf("published re-upload: %+v", pub)
	}
}

func TestIngest_ImageSubmissionSinglePage(t *testing.T) {
	f := setup(t)
	png := pngBytes(t, 800, 600)

	res := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: png, Kind: "image"}, 0, false)
	if res.Status != "ingested" || res.Pages != 1 || res.MappedPages != 1 {
		t.Fatalf("image ingest: %+v", res)
	}

	sub, err := f.st.Q.GetSubmission(f.ctx, res.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.SourceKind != "image" {
		t.Errorf("source_kind: got %q want image", sub.SourceKind)
	}
	if sub.PageCount != 1 {
		t.Errorf("page_count: got %d want 1", sub.PageCount)
	}
	if !strings.HasSuffix(sub.SourceRef, ".png") {
		t.Errorf("source_ref should keep .png extension: %s", sub.SourceRef)
	}
	ok, err := f.svc.Blobs.Exists(f.ctx, sub.SourceRef)
	if err != nil || !ok {
		t.Errorf("stored source missing: %s (%v)", sub.SourceRef, err)
	}

	pages, err := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	if err != nil || len(pages) != 1 {
		t.Fatalf("pages: got %d want 1 (%v)", len(pages), err)
	}
}

func TestIngest_PerProblemScopingAllowsMultipleLiveSubmissions(t *testing.T) {
	f := setup(t)
	problems, err := f.st.Q.ListProblems(f.ctx, f.aid)
	if err != nil || len(problems) != 2 {
		t.Fatalf("problems: %v %v", problems, err)
	}
	p1, p2 := problems[0], problems[1]

	res1 := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: pngBytes(t, 400, 300), Kind: "image", TargetProblemID: p1.ID}, 0, false)
	if res1.Status != "ingested" {
		t.Fatalf("problem 1 ingest: %+v", res1)
	}
	res2 := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: pngBytes(t, 400, 300), Kind: "image", TargetProblemID: p2.ID}, 0, false)
	if res2.Status != "ingested" {
		t.Fatalf("problem 2 ingest: %+v", res2)
	}

	// Both submissions are live (per-problem scope allows coexistence, D22).
	sub1, err := f.st.Q.GetSubmission(f.ctx, res1.SubmissionID)
	if err != nil || sub1.SupersededBy.Valid {
		t.Errorf("submission 1 should remain live: %+v %v", sub1, err)
	}
	sub2, err := f.st.Q.GetSubmission(f.ctx, res2.SubmissionID)
	if err != nil || sub2.SupersededBy.Valid {
		t.Errorf("submission 2 should remain live: %+v %v", sub2, err)
	}
	if !sub1.ProblemID.Valid || sub1.ProblemID.Int64 != p1.ID {
		t.Errorf("submission 1 problem_id: %+v want %d", sub1.ProblemID, p1.ID)
	}
	if !sub2.ProblemID.Valid || sub2.ProblemID.Int64 != p2.ID {
		t.Errorf("submission 2 problem_id: %+v want %d", sub2.ProblemID, p2.ID)
	}

	// Re-uploading for problem 1 supersedes only that problem's submission.
	res3 := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: pngBytes(t, 500, 300), Kind: "image", TargetProblemID: p1.ID}, 0, false)
	if res3.Status != "ingested" {
		t.Fatalf("re-upload problem 1: %+v", res3)
	}
	sub1Again, err := f.st.Q.GetSubmission(f.ctx, res1.SubmissionID)
	if err != nil || !sub1Again.SupersededBy.Valid || sub1Again.SupersededBy.Int64 != res3.SubmissionID {
		t.Errorf("submission 1 should be superseded by re-upload: %+v %v", sub1Again, err)
	}
	sub2Still, err := f.st.Q.GetSubmission(f.ctx, res2.SubmissionID)
	if err != nil || sub2Still.SupersededBy.Valid {
		t.Errorf("submission 2 should be untouched by problem-1 re-upload: %+v %v", sub2Still, err)
	}

	// Grading records on problem 1 block a plain re-upload but not problem 2.
	answers, _ := f.st.Q.ListAnswersForAssessment(f.ctx, f.aid)
	var p1Answer db.Answer
	for _, a := range answers {
		if a.ProblemID == p1.ID && a.StudentID == sub1.StudentID {
			p1Answer = a
		}
	}
	rv, err := f.st.Q.CreateRubricVersion(f.ctx, db.CreateRubricVersionParams{ProblemID: p1.ID, ScoreIncrement: mustNum(t, "0.5")})
	if err != nil {
		t.Fatal(err)
	}
	u, err := f.st.Q.CreateUser(f.ctx, db.CreateUserParams{Email: "ta2@x.edu", Role: "ta", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(f.ctx, `INSERT INTO grading_records (answer_id, source, rubric_version_id, criterion_scores, total, created_by)
		VALUES ($1, 'human', $2, '[]', $3, $4)`, p1Answer.ID, rv.ID, mustNum(t, "7"), u.ID); err != nil {
		t.Fatal(err)
	}

	blocked := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: pngBytes(t, 400, 300), Kind: "image", TargetProblemID: p1.ID}, 0, false)
	if blocked.Status != "rejected" || !strings.Contains(blocked.Reason, "force") {
		t.Fatalf("graded problem-1 re-upload without force: %+v", blocked)
	}
	// Problem 2 is a different scope: still ungraded, so it's unaffected.
	stillOK := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: pngBytes(t, 400, 300), Kind: "image", TargetProblemID: p2.ID}, 0, false)
	if stillOK.Status != "ingested" {
		t.Fatalf("problem-2 re-upload should be unaffected by problem-1 grading: %+v", stillOK)
	}
}

func TestIngest_PerProblemMultiPagePDFAppendsOntoSameAnswer(t *testing.T) {
	f := setup(t)
	f.svc.Renderer = render.NewFake(3) // 3-page PDF
	problems, _ := f.st.Q.ListProblems(f.ctx, f.aid)
	p1 := problems[0]

	res := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.pdf", Data: []byte("%PDF-multi"), Kind: "pdf", TargetProblemID: p1.ID}, 0, false)
	if res.Status != "ingested" || res.MappedPages != 3 {
		t.Fatalf("multi-page per-problem ingest: %+v", res)
	}

	answer, err := f.st.Q.GetAnswerByKey(f.ctx, db.GetAnswerByKeyParams{AssessmentID: f.aid, StudentID: mustStudentID(f, t, "b01"), ProblemID: p1.ID})
	if err != nil {
		t.Fatal(err)
	}
	pages, err := f.st.Q.ListAnswerPages(f.ctx, answer.ID)
	if err != nil || len(pages) != 3 {
		t.Fatalf("pages on one answer: got %d want 3 (%v)", len(pages), err)
	}
	for i, pg := range pages {
		if pg.PageIndex != int32(i) {
			t.Errorf("page %d: page_index got %d want %d", i, pg.PageIndex, i)
		}
		if pg.PdfPageIndex != int32(i) {
			t.Errorf("page %d: pdf_page_index got %d want %d", i, pg.PdfPageIndex, i)
		}
	}
}

func mustStudentID(f fx, t *testing.T, externalID string) int64 {
	t.Helper()
	st, err := f.st.Q.GetStudentByExternalID(f.ctx, externalID)
	if err != nil {
		t.Fatal(err)
	}
	return st.ID
}

func mustNum(t *testing.T, s string) pgtype.Numeric {
	t.Helper()
	n, err := store.Num(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRetractSubmission_WholeAssessmentScope(t *testing.T) {
	f := setup(t)
	res := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-fake"), 0, false)
	if res.Status != "ingested" {
		t.Fatal(res)
	}

	// Ungraded: retract without force succeeds, deletes pages, sets retracted_at.
	if err := f.svc.RetractSubmission(f.ctx, res.SubmissionID, 0, false); err != nil {
		t.Fatalf("retract: %v", err)
	}
	sub, err := f.st.Q.GetSubmission(f.ctx, res.SubmissionID)
	if err != nil || !sub.RetractedAt.Valid {
		t.Fatalf("retracted_at should be set: %+v %v", sub, err)
	}
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	if len(pages) != 0 {
		t.Errorf("pages should be deleted: got %d", len(pages))
	}

	// Re-upload after retraction succeeds (the active-unique index frees the slot).
	second := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-two"), 0, false)
	if second.Status != "ingested" {
		t.Fatalf("re-upload after retraction: %+v", second)
	}

	// Grade it, then retraction without force is blocked; with force it flags.
	answers, _ := f.st.Q.ListAnswersForAssessment(f.ctx, f.aid)
	var b01Answer db.Answer
	pages, _ = f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	for _, a := range answers {
		for _, pg := range pages {
			if pg.AnswerID == a.ID {
				b01Answer = a
			}
		}
	}
	rv, err := f.st.Q.CreateRubricVersion(f.ctx, db.CreateRubricVersionParams{ProblemID: b01Answer.ProblemID, ScoreIncrement: mustNum(t, "0.5")})
	if err != nil {
		t.Fatal(err)
	}
	u, err := f.st.Q.CreateUser(f.ctx, db.CreateUserParams{Email: "ta3@x.edu", Role: "ta", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(f.ctx, `INSERT INTO grading_records (answer_id, source, rubric_version_id, criterion_scores, total, created_by)
		VALUES ($1, 'human', $2, '[]', $3, $4)`, b01Answer.ID, rv.ID, mustNum(t, "7"), u.ID); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.RetractSubmission(f.ctx, second.SubmissionID, 0, false); err == nil || !strings.Contains(err.Error(), "force") {
		t.Fatalf("retract graded without force should be blocked: %v", err)
	}

	// Force-reupload (not retract) to exercise the forced re-upload's own flagging,
	// leaving a fresh live submission (`third`) for the published-guard check below.
	third := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-three"), 0, true)
	if third.Status != "ingested" {
		t.Fatalf("forced re-upload: %+v", third)
	}
	got, _ := f.st.Q.GetAnswer(f.ctx, b01Answer.ID)
	found := false
	for _, fl := range got.Flags {
		if fl == FlagImageSuperseded {
			found = true
		}
	}
	if !found {
		t.Errorf("answer should be flagged %s after forced re-upload: %v", FlagImageSuperseded, got.Flags)
	}

	// Published answers can never be retracted, even with force.
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE answers SET published_at = now() WHERE id = $1`, b01Answer.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RetractSubmission(f.ctx, third.SubmissionID, 0, true); err == nil || !strings.Contains(err.Error(), "published") {
		t.Fatalf("retract of published-answer submission should always be blocked: %v", err)
	}
}

func TestRetractSubmission_PerProblemScope(t *testing.T) {
	f := setup(t)
	problems, _ := f.st.Q.ListProblems(f.ctx, f.aid)
	p1 := problems[0]

	res := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: pngBytes(t, 400, 300), Kind: "image", TargetProblemID: p1.ID}, 0, false)
	if res.Status != "ingested" {
		t.Fatal(res)
	}
	if err := f.svc.RetractSubmission(f.ctx, res.SubmissionID, 0, false); err != nil {
		t.Fatalf("retract per-problem submission: %v", err)
	}
	sub, err := f.st.Q.GetSubmission(f.ctx, res.SubmissionID)
	if err != nil || !sub.RetractedAt.Valid {
		t.Fatalf("retracted_at should be set: %+v %v", sub, err)
	}
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	if len(pages) != 0 {
		t.Errorf("pages should be deleted: got %d", len(pages))
	}

	// Slot is free again: a new per-problem submission for the same student+problem
	// can be created (the partial unique index only blocks live rows).
	res2 := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: pngBytes(t, 400, 300), Kind: "image", TargetProblemID: p1.ID}, 0, false)
	if res2.Status != "ingested" {
		t.Fatalf("re-ingest after retraction: %+v", res2)
	}
}

func TestIngest_WithdrawnStudentExcludedFromMaterializeAndReport(t *testing.T) {
	f := setup(t)
	b02, err := f.st.Q.GetStudentByExternalID(f.ctx, "b02")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Q.SetStudentWithdrawn(f.ctx, db.SetStudentWithdrawnParams{ID: b02.ID, Withdrawn: true}); err != nil {
		t.Fatal(err)
	}

	res := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-fake"), 0, false)
	if res.Status != "ingested" {
		t.Fatal(res)
	}

	// MaterializeAnswers should only have created answers for b01 (2 problems), not
	// the withdrawn b02.
	answers, err := f.st.Q.ListAnswersForAssessment(f.ctx, f.aid)
	if err != nil || len(answers) != 2 {
		t.Fatalf("answers: got %d want 2 (withdrawn student excluded) (%v)", len(answers), err)
	}
	for _, a := range answers {
		if a.StudentID == b02.ID {
			t.Errorf("withdrawn student b02 should have no materialized answers: %+v", a)
		}
	}

	// IngestReportRows should exclude the withdrawn student from the expected list.
	rows, err := f.st.Q.IngestReportRows(f.ctx, f.aid)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.StudentID == "b02" {
			t.Errorf("withdrawn student b02 should be excluded from ingest report: %+v", r)
		}
	}
	if len(rows) != 1 {
		t.Errorf("ingest report rows: got %d want 1 (only active b01)", len(rows))
	}
}

// gradeAnswer inserts one human grading record on the given answer, returning
// the created TA user id (helper for the mixed-mode tests below).
func gradeAnswer(f fx, t *testing.T, answerID int64, taEmail string) {
	t.Helper()
	answer, err := f.st.Q.GetAnswer(f.ctx, answerID)
	if err != nil {
		t.Fatal(err)
	}
	rv, err := f.st.Q.CreateRubricVersion(f.ctx, db.CreateRubricVersionParams{ProblemID: answer.ProblemID, ScoreIncrement: mustNum(t, "0.5")})
	if err != nil {
		t.Fatal(err)
	}
	u, err := f.st.Q.CreateUser(f.ctx, db.CreateUserParams{Email: taEmail, Role: "ta", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(f.ctx, `INSERT INTO grading_records (answer_id, source, rubric_version_id, criterion_scores, total, created_by)
		VALUES ($1, 'human', $2, '[]', $3, $4)`, answerID, rv.ID, mustNum(t, "7"), u.ID); err != nil {
		t.Fatal(err)
	}
}

func hasFlag(flags []string, want string) bool {
	for _, fl := range flags {
		if fl == want {
			return true
		}
	}
	return false
}

// TestIngest_WholeThenPerProblem_Ungraded: a live whole-assessment submission
// already covers a problem positionally; a per-problem re-upload for that problem
// must succeed, replace only that answer's pages, leave other answers untouched,
// and leave the whole submission row live (it still owns the other problems).
func TestIngest_WholeThenPerProblem_Ungraded(t *testing.T) {
	f := setup(t)
	problems, _ := f.st.Q.ListProblems(f.ctx, f.aid)
	p1, p2 := problems[0], problems[1]
	sid := mustStudentID(f, t, "b01")

	whole := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-whole"), 0, false)
	if whole.Status != "ingested" || whole.MappedPages != 2 {
		t.Fatalf("whole ingest: %+v", whole)
	}

	// Per-problem upload for p1 (positionally page 0 of the whole submission).
	perP := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: pngBytes(t, 500, 400), Kind: "image", TargetProblemID: p1.ID}, 0, false)
	if perP.Status != "ingested" {
		t.Fatalf("per-problem ingest over live whole submission: %+v", perP)
	}

	// The whole submission row stays live.
	wsub, err := f.st.Q.GetSubmission(f.ctx, whole.SubmissionID)
	if err != nil || wsub.SupersededBy.Valid || wsub.RetractedAt.Valid {
		t.Fatalf("whole submission should remain live: %+v %v", wsub, err)
	}

	// p1's answer now has exactly one page, owned by the new per-problem submission.
	p1ans, err := f.st.Q.GetAnswerByKey(f.ctx, db.GetAnswerByKeyParams{AssessmentID: f.aid, StudentID: sid, ProblemID: p1.ID})
	if err != nil {
		t.Fatal(err)
	}
	p1pages, err := f.st.Q.ListAnswerPages(f.ctx, p1ans.ID)
	if err != nil || len(p1pages) != 1 {
		t.Fatalf("p1 pages: got %d want 1 (%v)", len(p1pages), err)
	}
	if p1pages[0].SubmissionID != perP.SubmissionID {
		t.Errorf("p1 page should belong to per-problem submission %d, got %d", perP.SubmissionID, p1pages[0].SubmissionID)
	}
	if p1pages[0].PageIndex != 0 {
		t.Errorf("p1 page_index: got %d want 0", p1pages[0].PageIndex)
	}

	// p2's answer is untouched: still one page owned by the whole submission.
	p2ans, err := f.st.Q.GetAnswerByKey(f.ctx, db.GetAnswerByKeyParams{AssessmentID: f.aid, StudentID: sid, ProblemID: p2.ID})
	if err != nil {
		t.Fatal(err)
	}
	p2pages, err := f.st.Q.ListAnswerPages(f.ctx, p2ans.ID)
	if err != nil || len(p2pages) != 1 {
		t.Fatalf("p2 pages: got %d want 1 (%v)", len(p2pages), err)
	}
	if p2pages[0].SubmissionID != whole.SubmissionID {
		t.Errorf("p2 page should still belong to whole submission %d, got %d", whole.SubmissionID, p2pages[0].SubmissionID)
	}
}

// TestIngest_WholeThenPerProblem_Graded: whole submission live and p1 graded;
// per-problem re-upload for p1 is rejected without force, succeeds with force,
// and flags image_superseded on p1's answer ONLY (not p2's).
func TestIngest_WholeThenPerProblem_Graded(t *testing.T) {
	f := setup(t)
	problems, _ := f.st.Q.ListProblems(f.ctx, f.aid)
	p1, p2 := problems[0], problems[1]
	sid := mustStudentID(f, t, "b01")

	whole := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-whole"), 0, false)
	if whole.Status != "ingested" {
		t.Fatal(whole)
	}
	p1ans, _ := f.st.Q.GetAnswerByKey(f.ctx, db.GetAnswerByKeyParams{AssessmentID: f.aid, StudentID: sid, ProblemID: p1.ID})
	gradeAnswer(f, t, p1ans.ID, "ta-wtp@x.edu")

	blocked := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: pngBytes(t, 400, 300), Kind: "image", TargetProblemID: p1.ID}, 0, false)
	if blocked.Status != "rejected" || !strings.Contains(blocked.Reason, "force") {
		t.Fatalf("graded per-problem re-upload without force should be blocked: %+v", blocked)
	}

	forced := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: pngBytes(t, 400, 300), Kind: "image", TargetProblemID: p1.ID}, 0, true)
	if forced.Status != "ingested" {
		t.Fatalf("forced per-problem re-upload over graded whole coverage: %+v", forced)
	}

	got1, _ := f.st.Q.GetAnswer(f.ctx, p1ans.ID)
	if !hasFlag(got1.Flags, FlagImageSuperseded) {
		t.Errorf("p1 answer should be flagged %s: %v", FlagImageSuperseded, got1.Flags)
	}
	p2ans, _ := f.st.Q.GetAnswerByKey(f.ctx, db.GetAnswerByKeyParams{AssessmentID: f.aid, StudentID: sid, ProblemID: p2.ID})
	got2, _ := f.st.Q.GetAnswer(f.ctx, p2ans.ID)
	if hasFlag(got2.Flags, FlagImageSuperseded) {
		t.Errorf("p2 answer should NOT be flagged (different scope): %v", got2.Flags)
	}
}

// TestIngest_WholeThenPerProblem_Published: whole submission live and p1
// published; a per-problem re-upload for p1 is rejected even with force.
func TestIngest_WholeThenPerProblem_Published(t *testing.T) {
	f := setup(t)
	problems, _ := f.st.Q.ListProblems(f.ctx, f.aid)
	p1 := problems[0]
	sid := mustStudentID(f, t, "b01")

	whole := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-whole"), 0, false)
	if whole.Status != "ingested" {
		t.Fatal(whole)
	}
	p1ans, _ := f.st.Q.GetAnswerByKey(f.ctx, db.GetAnswerByKeyParams{AssessmentID: f.aid, StudentID: sid, ProblemID: p1.ID})
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE answers SET published_at = now() WHERE id = $1`, p1ans.ID); err != nil {
		t.Fatal(err)
	}

	pub := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: pngBytes(t, 400, 300), Kind: "image", TargetProblemID: p1.ID}, 0, true)
	if pub.Status != "rejected" || !strings.Contains(pub.Reason, "published") {
		t.Fatalf("per-problem re-upload over a published problem must be rejected even with force: %+v", pub)
	}
}

// TestIngest_PerProblemThenWhole: per-problem submissions live for p1 and p2,
// plus (optionally) an old whole one; a whole-assessment upload supersedes ALL
// of them and rebuilds every page from the new PDF, leaving no orphan pages.
func TestIngest_PerProblemThenWhole(t *testing.T) {
	f := setup(t)
	problems, _ := f.st.Q.ListProblems(f.ctx, f.aid)
	p1, p2 := problems[0], problems[1]

	// An old whole submission first, then per-problem ones layered on top.
	oldWhole := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-old-whole"), 0, false)
	if oldWhole.Status != "ingested" {
		t.Fatal(oldWhole)
	}
	pp1 := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: pngBytes(t, 400, 300), Kind: "image", TargetProblemID: p1.ID}, 0, false)
	if pp1.Status != "ingested" {
		t.Fatal(pp1)
	}
	pp2 := f.svc.Ingest(f.ctx, f.aid, IngestInput{Filename: "b01.png", Data: pngBytes(t, 400, 300), Kind: "image", TargetProblemID: p2.ID}, 0, false)
	if pp2.Status != "ingested" {
		t.Fatal(pp2)
	}

	// New whole-assessment upload: supersedes the old whole AND both per-problem.
	newWhole := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-new-whole"), 0, false)
	if newWhole.Status != "ingested" || newWhole.MappedPages != 2 {
		t.Fatalf("new whole ingest: %+v", newWhole)
	}

	for name, id := range map[string]int64{"old whole": oldWhole.SubmissionID, "per-problem p1": pp1.SubmissionID, "per-problem p2": pp2.SubmissionID} {
		s, err := f.st.Q.GetSubmission(f.ctx, id)
		if err != nil {
			t.Fatalf("%s lookup: %v", name, err)
		}
		if !s.SupersededBy.Valid || s.SupersededBy.Int64 != newWhole.SubmissionID {
			t.Errorf("%s submission should be superseded by the new whole one: %+v", name, s)
		}
	}

	// Every live page belongs to the new whole submission; no orphans.
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	if len(pages) != 2 {
		t.Fatalf("live pages after whole rebuild: got %d want 2", len(pages))
	}
	for _, pg := range pages {
		if pg.SubmissionID != newWhole.SubmissionID {
			t.Errorf("page %d should belong to new whole submission %d, got %d", pg.ID, newWhole.SubmissionID, pg.SubmissionID)
		}
	}
}

// TestIngest_WithdrawnStudentDirectUploadRejected: a roster-matched but withdrawn
// student's direct upload is rejected with the exact reason, before any writes.
func TestIngest_WithdrawnStudentDirectUploadRejected(t *testing.T) {
	f := setup(t)
	b01, err := f.st.Q.GetStudentByExternalID(f.ctx, "b01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Q.SetStudentWithdrawn(f.ctx, db.SetStudentWithdrawnParams{ID: b01.ID, Withdrawn: true}); err != nil {
		t.Fatal(err)
	}

	res := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-fake"), 0, false)
	if res.Status != "rejected" {
		t.Fatalf("withdrawn student upload should be rejected: %+v", res)
	}
	if res.Reason != "student is withdrawn; reinstate before uploading" {
		t.Fatalf("unexpected rejection reason: %q", res.Reason)
	}
	// No rows written: no submissions, no answers for this student.
	if _, err := f.st.Q.GetActiveSubmission(f.ctx, db.GetActiveSubmissionParams{AssessmentID: f.aid, StudentID: b01.ID}); err == nil {
		t.Errorf("no submission should have been created for a withdrawn student")
	}
	answers, _ := f.st.Q.ListAnswersForAssessment(f.ctx, f.aid)
	for _, a := range answers {
		if a.StudentID == b01.ID {
			t.Errorf("no answers should have been materialized for a withdrawn student: %+v", a)
		}
	}
}

func TestApplyMasks_DerivesArtifactsAndResetsReview(t *testing.T) {
	f := setup(t)
	f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-fake"), 0, false)

	// No regions yet → error.
	if _, err := f.svc.ApplyMasks(f.ctx, f.aid); err == nil {
		t.Fatal("ApplyMasks without regions should error")
	}

	if _, err := f.st.Q.CreateMaskRegion(f.ctx, db.CreateMaskRegionParams{
		AssessmentID: f.aid, PageScope: "first",
		X: 0.05, Y: 0.02, W: 0.4, H: 0.08, Color: "#4a4a4a", Padding: 0.01,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := f.svc.ApplyMasks(f.ctx, f.aid)
	if err != nil || n != 2 {
		t.Fatalf("ApplyMasks: n=%d err=%v", n, err)
	}
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	for _, pg := range pages {
		if !pg.MaskedImageRef.Valid || !strings.Contains(pg.MaskedImageRef.String, "/masked/") {
			t.Errorf("page %d missing masked ref: %+v", pg.ID, pg.MaskedImageRef)
		}
		if pg.MaskReviewStatus != "pending" {
			t.Errorf("mask review should reset to pending, got %s", pg.MaskReviewStatus)
		}
		ok, _ := f.svc.Blobs.Exists(f.ctx, pg.MaskedImageRef.String)
		if !ok {
			t.Errorf("masked blob missing: %s", pg.MaskedImageRef.String)
		}
	}

	// Accept one page; the launch gate counts remaining blockers.
	if _, err := f.st.Q.SetMaskReview(f.ctx, db.SetMaskReviewParams{
		ID: pages[0].ID, MaskReviewStatus: "accepted",
		MaskReviewedBy: pgtype.Int8{},
	}); err != nil {
		t.Fatal(err)
	}
	blockers, err := f.st.Q.CountMaskBlockers(f.ctx, f.aid)
	if err != nil || blockers != 1 {
		t.Errorf("mask blockers: got %d want 1 (%v)", blockers, err)
	}
}

// ---- F15: blob I/O out of the transaction ----

// hookBlobStore wraps a real blobstore.Store and invokes onPut(key) before each
// Put, letting a test observe when (relative to DB state) page blobs are written and
// optionally inject a Put failure. It forwards everything else verbatim.
type hookBlobStore struct {
	blobstore.Store
	onPut func(key string) error
}

func (h hookBlobStore) Put(ctx context.Context, key string, r io.Reader) (string, int64, error) {
	if h.onPut != nil {
		if err := h.onPut(key); err != nil {
			return "", 0, err
		}
	}
	return h.Store.Put(ctx, key, r)
}

// TestIngest_PageBlobsStoredBeforeTx proves the F15 property directly: every page
// image is Put BEFORE the submission row exists (no Blobs.Put inside the open tx).
// At each page Put we assert the student still has zero live submissions — which
// holds only if the entire page-store loop runs before the tx creates the row.
func TestIngest_PageBlobsStoredBeforeTx(t *testing.T) {
	f := setup(t)
	student, err := f.st.Q.GetStudentByExternalID(f.ctx, "b01")
	if err != nil {
		t.Fatal(err)
	}

	var pagePutsSeen int
	f.svc.Blobs = hookBlobStore{
		Store: f.svc.Blobs,
		onPut: func(key string) error {
			// Only observe page-image Puts (the source blob is stored earlier too).
			if !strings.Contains(key, "/pages/") {
				return nil
			}
			pagePutsSeen++
			live, err := f.st.Q.ListLiveSubmissionsForStudent(f.ctx, db.ListLiveSubmissionsForStudentParams{
				AssessmentID: f.aid, StudentID: student.ID,
			})
			if err != nil {
				return err
			}
			if len(live) != 0 {
				t.Errorf("page Put happened while a live submission already exists (%d) — Put is inside the tx", len(live))
			}
			return nil
		},
	}

	res := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-fake"), 0, false)
	if res.Status != "ingested" {
		t.Fatalf("ingest: %+v", res)
	}
	if pagePutsSeen != 2 {
		t.Fatalf("page Puts observed: got %d want 2 (both before the tx)", pagePutsSeen)
	}
	// The row work still ran and produced the pages.
	if pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid); len(pages) != 2 {
		t.Fatalf("pages after ingest: got %d want 2", len(pages))
	}
}

// TestIngest_PageStoreFailureLeavesNoRows proves the failure semantics F15 must
// preserve: a page-store failure aborts BEFORE any row is written (previous state
// stays intact — here, nothing), and a clean re-run succeeds despite any orphan
// blob the failed attempt may have left behind (page blobs are content-addressed,
// so the re-run re-Puts identical bytes idempotently).
func TestIngest_PageStoreFailureLeavesNoRows(t *testing.T) {
	f := setup(t)
	student, _ := f.st.Q.GetStudentByExternalID(f.ctx, "b01")

	// Fail the FIRST page-image Put; source blob and everything else are fine.
	failNext := true
	f.svc.Blobs = hookBlobStore{
		Store: f.svc.Blobs,
		onPut: func(key string) error {
			if strings.Contains(key, "/pages/") && failNext {
				failNext = false
				return errors.New("injected page-store failure")
			}
			return nil
		},
	}

	res := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-fake"), 0, false)
	if res.Status == "ingested" {
		t.Fatalf("expected rejection on page-store failure, got: %+v", res)
	}
	if res.Reason != "storing page image failed" {
		t.Errorf("reason: got %q want %q", res.Reason, "storing page image failed")
	}
	// NO rows: no submission, no answer pages.
	if live, _ := f.st.Q.ListLiveSubmissionsForStudent(f.ctx, db.ListLiveSubmissionsForStudentParams{
		AssessmentID: f.aid, StudentID: student.ID,
	}); len(live) != 0 {
		t.Fatalf("a submission row exists after a failed page store: %d", len(live))
	}
	if pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid); len(pages) != 0 {
		t.Fatalf("answer_page rows exist after a failed page store: %d", len(pages))
	}

	// Re-run with the store healthy (failNext already consumed): succeeds cleanly.
	res2 := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-fake"), 0, false)
	if res2.Status != "ingested" {
		t.Fatalf("clean re-run after failure: %+v", res2)
	}
	if pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid); len(pages) != 2 {
		t.Fatalf("pages after clean re-run: got %d want 2", len(pages))
	}
}
