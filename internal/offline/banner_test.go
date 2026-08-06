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
