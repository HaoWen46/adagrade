package publish

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// fake invented data only (CLAUDE.md): no real student names/emails/answers.

// numText is a tiny helper so tests can build pgtype.Numeric fixtures ergonomically.
// Test-only (only snapshot_test.go's fixtures use it — moved out of snapshot.go, which
// otherwise has no runtime use for it).
func numText(s string) pgtype.Numeric {
	n, _ := store.Num(s)
	return n
}

func snapInputRow(sid int64, ext, name, email string, pnum int32, ptitle, pmax string, noSub bool, recordID int64, total, comment string, scoresJSON string) db.PublishSnapshotInputsRow {
	return snapInputRowWithAnswer(sid, ext, name, email, 0, pnum, ptitle, pmax, noSub, recordID, total, comment, scoresJSON)
}

// snapInputRowWithAnswer is snapInputRow plus an explicit answer_id. The row still
// carries answer_id (PublishSnapshotInputsRow reflects the DB join), but buildSnapshots
// must NOT copy it onto the persisted SnapProblem (finding 1) — the send job resolves
// answer ids live instead, so the stored snapshot shape must not depend on it.
func snapInputRowWithAnswer(sid int64, ext, name, email string, answerID int64, pnum int32, ptitle, pmax string, noSub bool, recordID int64, total, comment string, scoresJSON string) db.PublishSnapshotInputsRow {
	r := db.PublishSnapshotInputsRow{
		StudentID: sid, StudentExternalID: ext, StudentName: name, StudentEmail: email,
		ProblemID: int64(pnum), ProblemNumber: pnum, ProblemTitle: ptitle,
		MaxPoints:    numText(pmax),
		AnswerID:     answerID,
		NoSubmission: noSub,
	}
	if recordID != 0 {
		r.RecordID = pgtype.Int8{Int64: recordID, Valid: true}
		r.Total = numText(total)
		r.Comment = pgtype.Text{String: comment, Valid: true}
		r.CriterionScores = []byte(scoresJSON)
	}
	return r
}

func TestBuildSnapshots_HandBuiltFixture(t *testing.T) {
	// Assessment "Midterm" with two problems (P1 max 10, P2 max 5). One student b01,
	// P1 graded (total 8.5, two criteria), P2 no submission.
	criteria := []db.PublishCriteriaRow{
		{CriterionID: 1, RubricVersionID: 100, Position: 1, Description: "Correctness", Points: numText("6")},
		{CriterionID: 2, RubricVersionID: 100, Position: 2, Description: "Clarity", Points: numText("4")},
	}
	rows := []db.PublishSnapshotInputsRow{
		snapInputRow(11, "b01", "Ada Fake", "ada@example.edu", 1, "Greedy", "10", false, 500, "8.5", "good work",
			`[{"criterion_id":1,"score":"5"},{"criterion_id":2,"score":"3.5"}]`),
		snapInputRow(11, "b01", "Ada Fake", "ada@example.edu", 2, "DP", "5", true, 0, "", "", ""),
	}

	snaps, emails, names, ext := buildSnapshots("Midterm", rows, criteria)

	got, ok := snaps[11]
	if !ok {
		t.Fatal("no snapshot for student 11")
	}
	if emails[11] != "ada@example.edu" || names[11] != "Ada Fake" || ext[11] != "b01" {
		t.Errorf("side maps wrong: email=%q name=%q ext=%q", emails[11], names[11], ext[11])
	}

	want := Snapshot{
		AssessmentName:    "Midterm",
		StudentExternalID: "b01",
		StudentName:       "Ada Fake",
		Total:             "8.5",
		Max:               "15",
		AllNoSubmission:   false,
		Problems: []SnapProblem{
			{
				Number: 1, Title: "Greedy", Max: "10", NoSubmission: false, Total: "8.5", Comment: "good work",
				Criteria: []SnapCriterion{
					{Name: "Clarity", Score: "3.5", Max: "4"},
					{Name: "Correctness", Score: "5", Max: "6"},
				},
			},
			{Number: 2, Title: "DP", Max: "5", NoSubmission: true, Total: "", Comment: "", Criteria: []SnapCriterion{}},
		},
	}

	wb, _ := json.Marshal(want)
	gb, _ := json.Marshal(got)
	if !bytes.Equal(wb, gb) {
		t.Errorf("snapshot mismatch:\n want %s\n  got %s", wb, gb)
	}
}

// TestBuildSnapshots_DoesNotPersistAnswerID (finding 1, CRITICAL): SnapProblem must
// NOT carry answer_id into the persisted snapshot. Persisting it broke the
// changed-only republish diff — snapshots stored before the field existed decode with
// AnswerID:0 and re-marshal as "answer_id":0, while freshly built snapshots carry the
// real id, so every student would spuriously diff as "changed" and the whole cohort
// gets re-emailed (violates D30). The send job now resolves each problem's answer id
// LIVE from (student_id, problem) instead — grade content still comes from the
// snapshot; only the image-ref lookup goes live (see sender.go). Asserting the
// canonical JSON has no "answer_id" key at all (not just that a Go field is absent)
// pins the wire format, since that's what the changed-only diff actually compares.
func TestBuildSnapshots_DoesNotPersistAnswerID(t *testing.T) {
	rows := []db.PublishSnapshotInputsRow{
		snapInputRowWithAnswer(11, "b01", "Ada Fake", "ada@example.edu", 777, 1, "Greedy", "10", false, 500, "8.5", "good", `[]`),
	}
	snaps, _, _, _ := buildSnapshots("Midterm", rows, nil)
	got := snaps[11]

	b, err := canonicalJSON(got)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("unmarshal canonical snapshot: %v", err)
	}
	problems, _ := generic["problems"].([]any)
	if len(problems) != 1 {
		t.Fatalf("expected 1 problem, got %d", len(problems))
	}
	p0, _ := problems[0].(map[string]any)
	if _, present := p0["answer_id"]; present {
		t.Errorf("canonical snapshot problem has an %q key, want it entirely absent: %s", "answer_id", b)
	}
}

func TestBuildSnapshots_AllNoSubmission(t *testing.T) {
	rows := []db.PublishSnapshotInputsRow{
		snapInputRow(22, "b02", "Bo Fake", "bo@example.edu", 1, "P1", "10", true, 0, "", "", ""),
		snapInputRow(22, "b02", "Bo Fake", "bo@example.edu", 2, "P2", "5", true, 0, "", "", ""),
	}
	snaps, _, _, _ := buildSnapshots("Q", rows, nil)
	s := snaps[22]
	if !s.AllNoSubmission {
		t.Error("expected AllNoSubmission=true for a student with only no_submission answers")
	}
	if s.Total != "" {
		t.Errorf("ungraded student total should be empty, got %q", s.Total)
	}
}

func TestCanonicalJSON_StableAcrossRebuild(t *testing.T) {
	criteria := []db.PublishCriteriaRow{
		{CriterionID: 1, RubricVersionID: 1, Position: 1, Description: "A", Points: numText("5")},
		{CriterionID: 2, RubricVersionID: 1, Position: 2, Description: "B", Points: numText("5")},
	}
	rows := []db.PublishSnapshotInputsRow{
		snapInputRow(1, "s1", "S One", "s1@example.edu", 1, "P1", "10", false, 9, "7", "c",
			`[{"criterion_id":2,"score":"4"},{"criterion_id":1,"score":"3"}]`), // scores in reversed order
	}
	a, _, _, _ := buildSnapshots("X", rows, criteria)
	b, _, _, _ := buildSnapshots("X", rows, criteria)
	ab, _ := canonicalJSON(a[1])
	bb, _ := canonicalJSON(b[1])
	if !bytes.Equal(ab, bb) {
		t.Errorf("canonical JSON not stable:\n a=%s\n b=%s", ab, bb)
	}
}

func TestChangedOnly_DiffSelectsOnlyChanged(t *testing.T) {
	criteria := []db.PublishCriteriaRow{
		{CriterionID: 1, RubricVersionID: 1, Position: 1, Description: "A", Points: numText("10")},
	}
	base := []db.PublishSnapshotInputsRow{
		snapInputRow(1, "s1", "S1", "s1@example.edu", 1, "P1", "10", false, 9, "8", "ok", `[{"criterion_id":1,"score":"8"}]`),
		snapInputRow(2, "s2", "S2", "s2@example.edu", 1, "P1", "10", false, 10, "6", "ok", `[{"criterion_id":1,"score":"6"}]`),
	}
	prevSnaps, _, _, _ := buildSnapshots("X", base, criteria)

	// Rebuild with student 2's score changed 6 -> 7; student 1 unchanged.
	next := []db.PublishSnapshotInputsRow{
		snapInputRow(1, "s1", "S1", "s1@example.edu", 1, "P1", "10", false, 9, "8", "ok", `[{"criterion_id":1,"score":"8"}]`),
		snapInputRow(2, "s2", "S2", "s2@example.edu", 1, "P1", "10", false, 10, "7", "ok", `[{"criterion_id":1,"score":"7"}]`),
	}
	nextSnaps, _, _, _ := buildSnapshots("X", next, criteria)

	prevBytes := map[int64][]byte{}
	for sid, s := range prevSnaps {
		b, _ := canonicalJSON(s)
		prevBytes[sid] = b
	}
	var changed []int64
	for sid, s := range nextSnaps {
		b, _ := canonicalJSON(s)
		if !bytes.Equal(prevBytes[sid], b) {
			changed = append(changed, sid)
		}
	}
	if len(changed) != 1 || changed[0] != 2 {
		t.Errorf("changed-only diff = %v, want exactly [2]", changed)
	}
}
