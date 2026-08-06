package offline

import (
	"bytes"
	"strings"
	"testing"
)

func bannerText(t *testing.T, outDir string) string {
	t.Helper()
	var buf bytes.Buffer
	PrintBanner(&buf, outDir)
	return buf.String()
}

// goldenBanner is an independent copy of the whole warning. It is duplicated
// from banner.go on purpose: editing the banner has to mean editing this too,
// which is what makes "soften a line and the build fails" true rather than
// aspirational. The hard-line assertions below stay as well — they say WHICH
// sentences must not be watered down when someone updates both copies.
const goldenBanner = `================================================================================
  ADAMARKER OFFLINE-GRADE — FALLBACK MODE. NO HUMAN REVIEW.
================================================================================
  This command exists for when the web server is unavailable. It gives up the
  safeguards the normal pipeline is built around. Specifically:

  * Every page is FORCE-MATCHED to a student. There is no orphan queue and no
    TA confirmation step. Pages WILL be assigned to the wrong student.
  * Masking is best-effort and fully automatic. It covers the identity regions
    you configured and nothing else. A name written in a margin is NOT covered.
  * The masked page images ARE SENT to the configured LLM API for
    transcription. Nothing else leaves this machine.

  Before you trust a single line of the output, check:
    <out>/match-report.csv    who each page was assigned to, and how confident
    <out>/masked-preview.jpg  what the model actually saw where identity was

  Pages that could not be matched are left in <out>/unmatched/ with a reason.
================================================================================
`

func TestBannerMatchesGolden(t *testing.T) {
	got := bannerText(t, "<out>")
	if got == goldenBanner {
		return
	}
	gotLines := strings.Split(got, "\n")
	wantLines := strings.Split(goldenBanner, "\n")
	for i := 0; i < max(len(gotLines), len(wantLines)); i++ {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			t.Errorf("banner line %d:\n got: %q\nwant: %q", i+1, g, w)
		}
	}
}

// TestBannerHardLines pins the two sentences the banner exists for. They are
// verbatim assertions on purpose: softening "FORCE-MATCHED" or "ARE SENT" would
// quietly change what the operator was told about wrong assignments and about
// page images leaving the machine, so it has to break a test.
func TestBannerHardLines(t *testing.T) {
	got := bannerText(t, "/tmp/run")
	hard := []string{
		"  * Every page is FORCE-MATCHED to a student. There is no orphan queue and no\n" +
			"    TA confirmation step. Pages WILL be assigned to the wrong student.",
		"  * The masked page images ARE SENT to the configured LLM API for\n" +
			"    transcription. Nothing else leaves this machine.",
	}
	for _, want := range hard {
		if !strings.Contains(got, want) {
			t.Errorf("banner is missing this text verbatim:\n%s\n\ngot:\n%s", want, got)
		}
	}
}

func TestBannerNamesArtifactsAndOutDir(t *testing.T) {
	const outDir = "/var/folders/run-42"
	got := bannerText(t, outDir)
	for _, want := range []string{
		outDir + "/match-report.csv",
		outDir + "/masked-preview.jpg",
		outDir + "/unmatched/",
		"NO HUMAN REVIEW",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("banner does not mention %q\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<out>") {
		t.Errorf("banner still contains the <out> placeholder:\n%s", got)
	}
}

func TestBannerLayout(t *testing.T) {
	got := bannerText(t, "out")
	if !strings.HasSuffix(got, "\n") {
		t.Error("banner does not end with a newline")
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("banner has %d lines, want a rule, a title and a body", len(lines))
	}
	rule := strings.Repeat("=", 80)
	for _, i := range []int{0, 2, len(lines) - 1} {
		if lines[i] != rule {
			t.Errorf("line %d = %q, want an 80-character rule", i+1, lines[i])
		}
	}
	for i, line := range lines {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line %d has trailing whitespace: %q", i+1, line)
		}
	}
}

func TestBannerIsStableAcrossCalls(t *testing.T) {
	if a, b := bannerText(t, "out"), bannerText(t, "out"); a != b {
		t.Error("PrintBanner is not deterministic for the same outDir")
	}
}
