package roster

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store/db"
)

// fakeQueries implements Queries in memory: enough to test diff computation and
// the cross-DB duplicate-email guard without a database.
type fakeQueries struct {
	students []db.Student
	upserts  []db.UpsertStudentParams
}

func (f *fakeQueries) ListStudents(context.Context) ([]db.Student, error) {
	return f.students, nil
}

func (f *fakeQueries) UpsertStudent(_ context.Context, arg db.UpsertStudentParams) (db.Student, error) {
	f.upserts = append(f.upserts, arg)
	return db.Student{StudentID: arg.StudentID, Name: arg.Name, Email: arg.Email}, nil
}

func active(sid, name, email string) db.Student {
	return db.Student{StudentID: sid, Name: name, Email: email}
}

func withdrawn(sid, name, email string) db.Student {
	s := active(sid, name, email)
	s.WithdrawnAt = pgtype.Timestamptz{Valid: true}
	return s
}

// Roster-lifecycle plan 2026-07-10 fix 1: the import response reports the diff
// against the current roster — sync is proposed, never automatic.
func TestImport_ComputesRosterDiff(t *testing.T) {
	q := &fakeQueries{students: []db.Student{
		active("b01", "Alice", "a@x.edu"),     // in CSV, email changes
		active("b02", "Bob", "b@x.edu"),       // absent from CSV → missing_active
		withdrawn("b03", "Carol", "c@x.edu"),  // in CSV → withdrawn_present (retaker)
		withdrawn("b04", "Dave", "d@x.edu"),   // absent + withdrawn → in neither list
		active("b05", "Erin", "e@x.edu"),      // in CSV, name changes
	}}
	rows := []Row{
		{StudentID: "b01", Name: "Alice", Email: "a2@x.edu", Line: 2},
		{StudentID: "b03", Name: "Carol", Email: "c@x.edu", Line: 3},
		{StudentID: "b05", Name: "Erin Chen", Email: "e@x.edu", Line: 4},
		{StudentID: "b06", Name: "Frank", Email: "f@x.edu", Line: 5},
	}
	rep, rowErrs, err := Import(context.Background(), q, rows)
	if err != nil || len(rowErrs) != 0 {
		t.Fatalf("unexpected: rowErrs=%v err=%v", rowErrs, err)
	}
	d := rep.RosterDiff
	if len(d.MissingActive) != 1 || d.MissingActive[0] != "b02" {
		t.Errorf("missing_active: %v", d.MissingActive)
	}
	if len(d.WithdrawnPresent) != 1 || d.WithdrawnPresent[0] != "b03" {
		t.Errorf("withdrawn_present: %v", d.WithdrawnPresent)
	}
	if d.EmailChanged != 1 {
		t.Errorf("email_changed: got %d want 1", d.EmailChanged)
	}
	if d.NameChanged != 1 {
		t.Errorf("name_changed: got %d want 1", d.NameChanged)
	}
	if rep.Added != 1 || rep.Updated != 2 || rep.Unchanged != 1 || rep.Total != 4 {
		t.Errorf("report counts: %+v", rep)
	}
	// The diff never mutates: b02 was NOT withdrawn, b03 NOT reinstated.
	if len(q.upserts) != 3 {
		t.Errorf("upserts: %+v", q.upserts)
	}
}

// Empty diff lists must be non-nil so the JSON is [] (frontend maps over them).
func TestImport_DiffListsNeverNil(t *testing.T) {
	q := &fakeQueries{}
	rep, rowErrs, err := Import(context.Background(), q, []Row{{StudentID: "b01", Name: "A", Email: "a@x.edu", Line: 2}})
	if err != nil || len(rowErrs) != 0 {
		t.Fatalf("unexpected: %v %v", rowErrs, err)
	}
	if rep.MissingActive == nil || rep.WithdrawnPresent == nil {
		t.Errorf("diff lists must be non-nil: %+v", rep.RosterDiff)
	}
}

// Fix 3 (cross-DB half): a CSV row whose email already belongs to a DIFFERENT
// existing student is a row error; the import is all-or-nothing so nothing is
// upserted. The message carries the owner's student_id, never the email (D14).
func TestImport_CrossDBDuplicateEmail(t *testing.T) {
	q := &fakeQueries{students: []db.Student{
		active("b01", "Alice", "Taken@X.edu"), // DB casing differs — match is case-insensitive
	}}
	rows := []Row{
		{StudentID: "b01", Name: "Alice", Email: "taken@x.edu", Line: 2}, // same student: fine
		{StudentID: "b07", Name: "Grace", Email: "taken@x.edu", Line: 3}, // different student: error
	}
	rep, rowErrs, err := Import(context.Background(), q, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowErrs) != 1 {
		t.Fatalf("want 1 row error, got %v", rowErrs)
	}
	e := rowErrs[0]
	if e.Line != 3 {
		t.Errorf("line: got %d want 3", e.Line)
	}
	if want := "email already belongs to student b01"; e.Msg != want {
		t.Errorf("msg: got %q want %q", e.Msg, want)
	}
	if strings.Contains(e.Msg, "@") {
		t.Errorf("msg must not contain the email: %q", e.Msg)
	}
	if len(q.upserts) != 0 {
		t.Errorf("all-or-nothing: no upserts on row errors, got %+v", q.upserts)
	}
	if rep.Total != 0 {
		t.Errorf("report should be zero-valued on rejection: %+v", rep)
	}
}

// Cross-DB normalized-id guard: a CSV row whose student_id differs from every existing
// student's EXACT id but collides with one under studentid.Normalize would insert a
// second roster row for the same person (students.student_id is UNIQUE on the exact
// string only) — and scan matching, which resolves OCR ids through Normalize, would
// then resolve to whichever row it saw first. The csv.go seenNorm guard only covers
// collisions WITHIN one file; this covers a later CSV against the existing roster.
func TestImport_CrossDBNormalizedIDCollision(t *testing.T) {
	q := &fakeQueries{students: []db.Student{
		active("B10902066", "Ming", "ming@x.edu"),
	}}
	rows := []Row{
		{StudentID: "b10902066", Name: "Ming", Email: "ming2@x.edu", Line: 2}, // same id after normalization: error
		{StudentID: "B99999999", Name: "Frank", Email: "f@x.edu", Line: 3},   // genuinely new: fine
	}
	rep, rowErrs, err := Import(context.Background(), q, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowErrs) != 1 {
		t.Fatalf("want 1 row error, got %v", rowErrs)
	}
	e := rowErrs[0]
	if e.Line != 2 {
		t.Errorf("line: got %d want 2", e.Line)
	}
	if want := "student_id b10902066 duplicates existing student B10902066 after normalization"; e.Msg != want {
		t.Errorf("msg: got %q want %q", e.Msg, want)
	}
	if len(q.upserts) != 0 {
		t.Errorf("all-or-nothing: no upserts on row errors, got %+v", q.upserts)
	}
	if rep.Total != 0 {
		t.Errorf("report should be zero-valued on rejection: %+v", rep)
	}
}

// A row that matches an existing student EXACTLY is an update, never a normalization
// collision — the guard must only fire when the exact ids differ.
func TestImport_ExactIDMatchIsUpdateNotNormalizedCollision(t *testing.T) {
	q := &fakeQueries{students: []db.Student{
		active("B10902066", "Ming", "ming@x.edu"),
	}}
	rows := []Row{
		{StudentID: "B10902066", Name: "Ming Chen", Email: "ming@x.edu", Line: 2}, // exact id: plain update
	}
	rep, rowErrs, err := Import(context.Background(), q, rows)
	if err != nil || len(rowErrs) != 0 {
		t.Fatalf("exact-id update must not trip the normalization guard: rowErrs=%v err=%v", rowErrs, err)
	}
	if rep.Updated != 1 || len(q.upserts) != 1 {
		t.Errorf("expected 1 update, got rep=%+v upserts=%+v", rep, q.upserts)
	}
}

// Normalization strips punctuation/width noise too, not just case — a hyphenated or
// full-width variant of an existing id is the same collision.
func TestImport_CrossDBNormalizedIDCollision_PunctuationVariant(t *testing.T) {
	q := &fakeQueries{students: []db.Student{
		active("B10902066", "Ming", "ming@x.edu"),
	}}
	rows := []Row{
		{StudentID: "B109-02066", Name: "Ming", Email: "ming2@x.edu", Line: 2},
	}
	_, rowErrs, err := Import(context.Background(), q, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowErrs) != 1 {
		t.Fatalf("want 1 row error for the hyphenated variant, got %v", rowErrs)
	}
	if want := "student_id B109-02066 duplicates existing student B10902066 after normalization"; rowErrs[0].Msg != want {
		t.Errorf("msg: got %q want %q", rowErrs[0].Msg, want)
	}
	if len(q.upserts) != 0 {
		t.Errorf("all-or-nothing: no upserts on row errors, got %+v", q.upserts)
	}
}
