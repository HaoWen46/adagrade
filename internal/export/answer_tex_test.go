package export

import "testing"

// TestAnswerTeXes_MatchBundledPerStudentEntries pins the compile gate's
// attribution path to the shipped artifacts: when the gate blames a student's
// standalone document, it must be blaming the exact bytes the professor
// receives as tex/{id}.tex, or the attribution checks a fiction (the same
// invariant TestAllTeX_MatchesTheBundledEntry pins for the bundle).
func TestAnswerTeXes_MatchBundledPerStudentEntries(t *testing.T) {
	in := sampleInput(t)
	singles, err := AnswerTeXes(in)
	if err != nil {
		t.Fatalf("AnswerTeXes: %v", err)
	}
	if len(singles) == 0 {
		t.Fatal("sample input produced no standalone answers")
	}
	zipBytes, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	for _, one := range singles {
		want := zipEntryBytes(t, zipBytes, in.RootDir()+"/tex/"+one.StudentID+".tex")
		if one.TeX != string(want) {
			t.Errorf("AnswerTeXes entry for %s differs from the bundled tex/%s.tex", one.StudentID, one.StudentID)
		}
	}
}
