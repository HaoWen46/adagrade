package offline

import (
	"io"
	"strings"
)

// bannerTemplate is the warning printed before every offline run. It is
// deliberately long and deliberately blunt: this mode ships pages to an LLM
// with no human in the loop, and the operator has to be told what they gave up
// BEFORE the run, not after they trust a wrong grade.
//
// banner_test holds a second, independent copy of this text and compares the
// two line by line, so softening a line here fails the build until someone
// edits the warning deliberately in both places. "<out>" is the only
// substitution; both artifact lines pad to the same column, so the description
// columns stay aligned for any --out path.
const bannerTemplate = `================================================================================
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

// PrintBanner writes the fallback-mode warning, with outDir filled in so the
// two artifacts the operator must check are named by their real paths.
func PrintBanner(w io.Writer, outDir string) {
	_, _ = io.WriteString(w, strings.ReplaceAll(bannerTemplate, "<out>", outDir))
}
