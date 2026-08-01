package export

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// Typst mirror bundle wiring (spec 2026-07-30): typ/ beside tex/, LaTeX first.

func zipNames(t *testing.T, zipBytes []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

func TestBuildZIP_ContainsTypMirror(t *testing.T) {
	in := sampleInput(t)
	zipBytes, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	names := zipNames(t, zipBytes)
	root := in.RootDir()

	byName := map[string]bool{}
	lastTex, firstTyp := -1, len(names)
	for i, n := range names {
		byName[n] = true
		if strings.HasPrefix(n, root+"/tex/") && i > lastTex {
			lastTex = i
		}
		if (strings.HasPrefix(n, root+"/typ/") || n == root+"/_all.typ") && i < firstTyp {
			firstTyp = i
		}
	}
	if !byName[root+"/_all.typ"] {
		t.Fatalf("bundle must contain _all.typ, got %v", names)
	}
	for _, id := range []string{"b09901002", "b09901005", "b09901007", "b09901009"} {
		if !byName[root+"/typ/"+id+".typ"] {
			t.Errorf("bundle must contain typ/%s.typ", id)
		}
	}
	if lastTex > firstTyp {
		t.Errorf("tex/ entries must precede typ entries (LaTeX primary), got order %v", names)
	}

	// Per-student mirror is a standalone document with the app-controlled title.
	one := string(zipEntryBytes(t, zipBytes, root+"/typ/b09901002.typ"))
	if !strings.Contains(one, `#import "@preview/mitex:`) {
		t.Errorf("per-student .typ must carry the preamble, got %q", one[:120])
	}
	if !strings.Contains(one, `#heading(level: 2, "Problem 3")`) {
		t.Errorf("per-student .typ must be titled by problem, never by student, got %q", one)
	}
	// The illegible answer's status must surface as a Typst comment.
	ill := string(zipEntryBytes(t, zipBytes, root+"/typ/b09901005.typ"))
	if !strings.Contains(ill, "// status: illegible") {
		t.Errorf("non-ok status must surface in the .typ, got %q", ill)
	}
}

func TestAllTyp_MatchesTheBundledEntry(t *testing.T) {
	// Same invariant as TestAllTeX_MatchesTheBundledEntry: what the secondary
	// gate compiles must be the exact bytes the professor receives.
	in := sampleInput(t)
	typ, err := AllTyp(in)
	if err != nil {
		t.Fatalf("AllTyp: %v", err)
	}
	zipBytes, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	bundled := zipEntryBytes(t, zipBytes, in.RootDir()+"/_all.typ")
	if typ != string(bundled) {
		t.Error("AllTyp must be byte-identical to the bundled _all.typ")
	}
	if !strings.Contains(typ, "authoritative") {
		t.Error("_all.typ must document that the LaTeX bundle is authoritative")
	}
}

func TestManifest_RecordsTypstVerdict(t *testing.T) {
	cases := map[string]string{ // Input.TypstVerdict -> manifest line
		"":         "# typst: unverified",
		"verified": "# typst: verified",
		"failed":   "# typst: failed",
	}
	for verdict, want := range cases {
		in := sampleInput(t)
		in.TypstVerdict = verdict
		zipBytes, err := BuildZIP(in)
		if err != nil {
			t.Fatalf("BuildZIP: %v", err)
		}
		manifest := string(zipEntryBytes(t, zipBytes, in.RootDir()+"/MANIFEST.csv"))
		if !strings.Contains(manifest, want+"\n") {
			t.Errorf("verdict %q: manifest must contain %q, got header %q", verdict, want, manifest[:200])
		}
	}
}

func TestValidated_RejectsForgedTypstVerdict(t *testing.T) {
	// TypstVerdict is interpolated into the manifest ahead of the CSV header;
	// an unvalidated value with a newline could forge a manifest record.
	in := sampleInput(t)
	in.TypstVerdict = "failed\nb09901002,Student 001,1,ok,dedicated,1.0,"
	if _, err := BuildZIP(in); err == nil {
		t.Fatal("a verdict outside the closed set must refuse the build")
	}
	in.TypstVerdict = "verified"
	if _, err := BuildZIP(in); err != nil {
		t.Fatalf("a legal verdict must build, got %v", err)
	}
}
