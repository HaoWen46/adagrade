package roster

import (
	"strings"
	"testing"
)

func TestParse_HappyPath(t *testing.T) {
	rows, errs := Parse(strings.NewReader("student_id,name,email\nb01,Alice,a@x.edu\nb02,Bob,b@x.edu\n"))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(rows) != 2 || rows[0].StudentID != "b01" || rows[0].Name != "Alice" || rows[1].Email != "b@x.edu" {
		t.Errorf("rows: %+v", rows)
	}
}

func TestParse_ToleratesBOMHeaderCaseExtraColumnsAndOrder(t *testing.T) {
	in := "\uFEFFEmail,Extra,STUDENT_ID,Name\r\na@x.edu,zzz,b01,Alice\r\n"
	rows, errs := Parse(strings.NewReader(in))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(rows) != 1 || rows[0].StudentID != "b01" || rows[0].Name != "Alice" || rows[0].Email != "a@x.edu" {
		t.Errorf("rows: %+v", rows)
	}
}

func TestParse_MissingRequiredColumn(t *testing.T) {
	_, errs := Parse(strings.NewReader("student_id,name\nb01,Alice\n"))
	if len(errs) == 0 || !strings.Contains(errs[0].Msg, "email") {
		t.Errorf("expected missing-column error naming email, got %v", errs)
	}
}

func TestParse_DuplicateStudentIDsReportedWithLines(t *testing.T) {
	_, errs := Parse(strings.NewReader("student_id,name,email\nb01,Alice,a@x.edu\nb01,Bob,b@x.edu\n"))
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %v", errs)
	}
	if errs[0].Line != 3 || !strings.Contains(errs[0].Msg, "duplicate") {
		t.Errorf("error: %+v", errs[0])
	}
}

func TestParse_RowFieldValidation(t *testing.T) {
	in := "student_id,name,email\n,NoID,a@x.edu\nb02,,b@x.edu\nb03,Carol,not-an-email\n"
	_, errs := Parse(strings.NewReader(in))
	if len(errs) != 3 {
		t.Fatalf("want 3 errors, got %v", errs)
	}
	for i, want := range []int{2, 3, 4} {
		if errs[i].Line != want {
			t.Errorf("error %d line: got %d want %d", i, errs[i].Line, want)
		}
	}
}

func TestParse_TrimsWhitespace(t *testing.T) {
	rows, errs := Parse(strings.NewReader("student_id,name,email\n  b01 , Alice Liddell , A@X.edu \n"))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if rows[0].StudentID != "b01" || rows[0].Name != "Alice Liddell" || rows[0].Email != "a@x.edu" {
		t.Errorf("row not trimmed/normalized: %+v", rows[0])
	}
}

func TestParse_EmptyFileAndHeaderOnly(t *testing.T) {
	if _, errs := Parse(strings.NewReader("")); len(errs) == 0 {
		t.Error("empty file should error")
	}
	rows, errs := Parse(strings.NewReader("student_id,name,email\n"))
	if len(errs) != 0 || len(rows) != 0 {
		t.Errorf("header-only file: rows=%v errs=%v", rows, errs)
	}
}

// Roster-lifecycle plan 2026-07-10 fix 2: Excel "Save As CSV" on Windows emits
// Big5/CP950 for Chinese names; those bytes are not UTF-8 and previously
// imported as mojibake. The whole file is rejected with ONE actionable error.
func TestParse_RejectsNonUTF8(t *testing.T) {
	const want = "file is not valid UTF-8 — in Excel, use Save As → 'CSV UTF-8 (Comma delimited)'"

	// "中文" in Big5 on line 2.
	in := "student_id,name,email\nb01,\xa4\xa4\xa4\xe5,a@x.edu\nb02,Bob,b@x.edu\n"
	rows, errs := Parse(strings.NewReader(in))
	if len(rows) != 0 || len(errs) != 1 {
		t.Fatalf("want 0 rows + exactly 1 error, got rows=%v errs=%v", rows, errs)
	}
	if errs[0].Msg != want {
		t.Errorf("msg: got %q want %q", errs[0].Msg, want)
	}
	if errs[0].Line != 2 {
		t.Errorf("line: got %d want 2 (first offending line)", errs[0].Line)
	}

	// Invalid bytes in the header itself → line 1.
	_, errs = Parse(strings.NewReader("student_id,\xff,email\nb01,Alice,a@x.edu\n"))
	if len(errs) != 1 || errs[0].Line != 1 || errs[0].Msg != want {
		t.Errorf("header case: %v", errs)
	}
}

// Fix 3: two rows sharing an email (case-insensitive) is an error naming both
// lines and student_ids — the email itself never appears (PII, D14).
func TestParse_InFileDuplicateEmail(t *testing.T) {
	in := "student_id,name,email\nb01,Alice,shared@x.edu\nb02,Bob,SHARED@X.edu\n"
	_, errs := Parse(strings.NewReader(in))
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %v", errs)
	}
	e := errs[0]
	if e.Line != 3 {
		t.Errorf("line: got %d want 3", e.Line)
	}
	for _, needle := range []string{"b01", "b02", "line 2"} {
		if !strings.Contains(e.Msg, needle) {
			t.Errorf("msg should name %q: %q", needle, e.Msg)
		}
	}
	if strings.Contains(strings.ToLower(e.Msg), "shared@x.edu") || strings.Contains(e.Msg, "@") {
		t.Errorf("msg must never contain the email value: %q", e.Msg)
	}
}

// Fix 4: two distinct student_id strings that fold to the same key under
// studentid.Normalize (case, width, punctuation) are duplicates — filename
// ingest and scan matching would treat them as the same student anyway.
func TestParse_NormalizedDuplicateStudentID(t *testing.T) {
	for name, second := range map[string]string{
		"case+hyphen": "B-01",
		"full-width":  "ｂ０１",
	} {
		in := "student_id,name,email\nb01,Alice,a@x.edu\n" + second + ",Bob,b@x.edu\n"
		_, errs := Parse(strings.NewReader(in))
		if len(errs) != 1 {
			t.Fatalf("%s: want 1 error, got %v", name, errs)
		}
		e := errs[0]
		if e.Line != 3 {
			t.Errorf("%s: line: got %d want 3", name, e.Line)
		}
		for _, needle := range []string{"b01", second, "line 2"} {
			if !strings.Contains(e.Msg, needle) {
				t.Errorf("%s: msg should name %q: %q", name, needle, e.Msg)
			}
		}
	}

	// Exact duplicates keep their dedicated message (not the normalized one).
	_, errs := Parse(strings.NewReader("student_id,name,email\nb01,Alice,a@x.edu\nb01,Bob,b@x.edu\n"))
	if len(errs) != 1 || !strings.Contains(errs[0].Msg, "duplicate student_id") {
		t.Errorf("exact dup: %v", errs)
	}
}
