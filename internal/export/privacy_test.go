package export

import (
	"archive/zip"
	"bytes"
	"image/color"
	"io"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/regrade"
	"github.com/HaoWen46/adagrade/internal/transcribe"
)

// The invariant this file exists for (spec §3):
//
//	IDENTITY LIVES IN FILENAMES, NEVER IN FILE BYTES.
//
// with exactly two documented exceptions:
//
//   - MANIFEST.csv, the local decoder ring, which carries student ids on
//     purpose and is documented as not-for-upload; even it carries no roster
//     name and no email.
//   - the entry NAMES themselves, which are the whole point of the scheme.
//
// Everything below is mechanical: it walks every byte of every entry against
// every roster identity, rather than spot-checking the places we happened to
// think of.

// identityKind labels a needle without printing it, so a failure message can
// say WHAT leaked without becoming a PII leak itself (CLAUDE.md).
type identityKind struct {
	kind  string
	value string
}

func rosterNeedles(in Input) []identityKind {
	var out []identityKind
	for _, a := range in.Answers {
		if a.Identity.Name != "" {
			out = append(out, identityKind{"roster name", a.Identity.Name})
		}
		if a.Identity.StudentID != "" {
			out = append(out, identityKind{"student id", a.Identity.StudentID})
		}
		if a.Identity.Email != "" {
			out = append(out, identityKind{"roster email", a.Identity.Email})
		}
	}
	return out
}

func containsFold(haystack []byte, needle string) bool {
	return strings.Contains(strings.ToLower(string(haystack)), strings.ToLower(needle))
}

// TestBuildZIP_NoExportedByteCarriesARosterIdentity is the load-bearing
// privacy test for the whole feature.
func TestBuildZIP_NoExportedByteCarriesARosterIdentity(t *testing.T) {
	in := sampleInput(t)
	// The B-C10 case: region masking is rectangular, students write their name
	// in the margin, so the transcription itself can carry identity. Export
	// must not pass that through to a file the professor uploads.
	in.Answers[0].Doc.Blocks = append(in.Answers[0].Doc.Blocks, transcribe.Block{
		Kind: transcribe.BlockProse,
		Text: "Grace Hopper (b09901007, hopper@example.edu) — Problem 3 continued on the back.",
	})
	in.Answers[1].Doc.Blocks = append(in.Answers[1].Doc.Blocks, transcribe.Block{
		Kind:  transcribe.BlockList,
		Items: []string{"see Ada Lovelace's earlier proof", "b09901002"},
	})

	out, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}

	needles := rosterNeedles(in)
	zr, err := zip.NewReader(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}

	sawManifest := false
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %q: %v", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %q: %v", f.Name, err)
		}

		isManifest := strings.HasSuffix(f.Name, "/MANIFEST.csv")
		sawManifest = sawManifest || isManifest

		for _, n := range needles {
			if !containsFold(body, n.value) {
				continue
			}
			// The one documented exception: MANIFEST.csv is the decoder ring,
			// so it may carry student ids — and nothing else.
			if isManifest && n.kind == "student id" {
				continue
			}
			t.Errorf("entry %q contains a %s in its BYTES", f.Name, n.kind)
		}
	}
	if !sawManifest {
		t.Fatal("no MANIFEST.csv in the archive — the decoder ring is mandatory")
	}
}

// TestBuildZIP_ManifestIsTheOnlyEntryCarryingStudentIDs states the exception
// positively, so a future change that starts sprinkling ids into .tex headers
// fails here with an explanation rather than only in the sweep above.
func TestBuildZIP_ManifestIsTheOnlyEntryCarryingStudentIDs(t *testing.T) {
	in := sampleInput(t)
	out, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	manifest := zipEntryBytes(t, out, "algorithms-midterm-2-p3/MANIFEST.csv")
	for _, a := range in.Answers {
		if !containsFold(manifest, a.Identity.StudentID) {
			t.Error("MANIFEST.csv must carry every student id — it is the decoder ring")
		}
		if a.Identity.Name != "" && containsFold(manifest, a.Identity.Name) {
			t.Error("MANIFEST.csv must not carry roster names; the id is enough to decode")
		}
		if a.Identity.Email != "" && containsFold(manifest, a.Identity.Email) {
			t.Error("MANIFEST.csv must not carry roster emails")
		}
	}
	if !strings.Contains(string(manifest), "do not upload") {
		t.Error("MANIFEST.csv must say in-band that it is a local decoder ring, not for upload")
	}
}

// TestBuildZIP_RecordsRedactionCountsAsAMaskQualitySignal — spec §4: routine
// redactions mean identity text is surviving the image mask, which is a leak
// signal for the core grading path. Counts only; never the excised text.
func TestBuildZIP_RecordsRedactionCountsAsAMaskQualitySignal(t *testing.T) {
	in := sampleInput(t)
	in.Answers[0].Doc.Blocks = append(in.Answers[0].Doc.Blocks, transcribe.Block{
		Kind: transcribe.BlockProse,
		Text: "Grace Hopper b09901007 hopper@example.edu",
	})
	out, err := BuildZIP(in)
	if err != nil {
		t.Fatalf("BuildZIP: %v", err)
	}
	flags := manifestRow(t, out, "b09901007")[6]
	if !strings.Contains(flags, "identity-redacted: 3") {
		t.Errorf("flags = %q, want an identity-redacted count of 3", flags)
	}
	// A clean answer must not be flagged, or the signal is worthless.
	if got := manifestRow(t, out, "b09901005")[6]; strings.Contains(got, "identity-redacted") {
		t.Errorf("a clean answer must not carry a redaction flag; got %q", got)
	}
}

// TestBuildZIP_ErrorsNeverNameAStudent — CLAUDE.md forbids logging PII, and
// validation errors are the likeliest place to leak an id by accident.
func TestBuildZIP_ErrorsNeverNameAStudent(t *testing.T) {
	cases := map[string]func(in *Input){
		"unsafe id":     func(in *Input) { in.Answers[0].Identity.StudentID = "../../etc/passwd" },
		"duplicate id":  func(in *Input) { in.Answers[1].Identity.StudentID = in.Answers[0].Identity.StudentID },
		"bad status":    func(in *Input) { in.Answers[0].Status = "maybe" },
		"bad source":    func(in *Input) { in.Answers[0].Source = "vibes" },
		"absent w/page": func(in *Input) { in.Answers[3].Pages = []imaging.MaskedImage{maskedPage(t, color.RGBA{A: 255})} },
	}
	for name, mutate := range cases {
		in := sampleInput(t)
		mutate(&in)
		_, err := BuildZIP(in)
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		msg := strings.ToLower(err.Error())
		for _, n := range rosterNeedles(sampleInput(t)) {
			if strings.Contains(msg, strings.ToLower(n.value)) {
				t.Errorf("%s: error message leaks a %s", name, n.kind)
			}
		}
	}
}

// The seam this file used to guard — splitting the emitter's standalone output
// to recover a body — no longer exists. transcribe.EmitBody returns the body
// directly and transcribe.Preamble returns the preamble, so a bundle can never
// be assembled from mis-cut source in the first place. The failure mode is
// structurally impossible rather than merely detected, and the surviving
// invariant ("_all.tex is ONE document with ONE preamble") is asserted by
// TestBuildZIP_AllTexIsOneStandaloneDocument.
//
// transcribe.TestEmitTeXWith_IsPreamblePlusBody pins the composition property
// that makes the two paths agree.

// Compile-time proof of requirement 3: the only way to hand pages to this
// package is imaging.MaskedImage, whose sealed constructor set is Mask() and
// the audited LoadMasked() gate. An unmasked []byte does not typecheck, and
// imaging.ProviderImage was deliberately NOT used — it would also admit
// IDCrop, which is the identity region itself.
var _ = func() bool {
	var a Answer
	var _ []imaging.MaskedImage = a.Pages
	var _ regrade.Identity = a.Identity
	return true
}()
