package offline

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HaoWen46/adagrade/internal/export"
	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/regrade"
	"github.com/HaoWen46/adagrade/internal/roster"
	"github.com/HaoWen46/adagrade/internal/transcribe"
)

// WriteBundle writes the professor's transcription bundle into outDir, one
// tree per problem:
//
//	outDir/{slug}-p1/…
//	outDir/{slug}-p2/…
//
// outDir is the destination itself (the orchestrator passes <out>/bundle), NOT
// a parent to nest a root under: export.File.Name already carries the
// "{slug}-p{n}/" root, so joining it onto outDir is the whole path.
//
// The trees are export's own — the same assembler the web download uses
// (export.Files over export.archiveEntries), so the directory a professor gets
// here is the directory they would have got from the app, with ONE deliberate
// deviation:
//
//	images/ holds the ORIGINAL page, not the masked one.
//
// The Answer handed to export still carries the MASKED image, which is what
// keeps export's internal privacy sweep meaningful (it is the type system's
// only proof that nothing unmasked reached the assembler), and then every
// images/ entry is overwritten with the original page bytes on the way to disk.
// The justification is the mode itself: this bundle never leaves the machine,
// and a professor grading a paper needs to see whose paper it is. That
// substitution is the one place these two paths differ, and it is a single
// explicit filter in writeProblem below.
//
// A cell whose transcription FAILED still becomes an answer — status "failed",
// its real pages attached, flagged "transcription failed" — exactly as the web
// export does (internal/httpapi/transcription.go). Dropping it would make the
// student vanish from the bundle, and "no row" is indistinguishable from "never
// sat the exam"; the professor needs to see the row and grade the pages by
// hand. A problem with no cells AT ALL still gets no directory: an empty
// problem tree reads as "nobody answered", which is a claim about the cohort
// this run has no business making.
//
// cjkFontPath is the bundled Traditional Chinese face (Task 9 passes
// ADAMARKER_REPORT_FONT). When set, the LaTeX preamble loads the font BY PATH
// and compilation does not depend on the host having one installed — the same
// wiring the web export uses. When empty, the preamble falls back to the family
// name "Noto Sans TC", which MUST then be installed system-wide: without it
// every Chinese glyph is dropped silently, producing a clean-looking .tex whose
// PDF is missing the answer. The Typst mirror has no equivalent knob —
// transcribe.TypstPreamble takes no options and resolves CJK by family name in
// both paths — so there is nothing to mirror there.
//
// Identity is NOT scrubbed here. export.problemEntries — which Files reaches —
// already runs scrubDoc over every answer, and a second pass would find nothing
// left to remove and report zero redactions, destroying the mask-quality signal
// that count exists to carry (export spec §4).
func WriteBundle(outDir string, examName string, cells []CellDoc, rows []roster.Row, problems int, cjkFontPath string) error {
	if err := mkdirPrivate(outDir); err != nil {
		return newOutDirError(err, "cannot create bundle directory %s", outDir)
	}
	identities := make(map[string]regrade.Identity, len(rows))
	for _, row := range rows {
		identities[row.StudentID] = regrade.Identity{Name: row.Name, StudentID: row.StudentID, Email: row.Email}
	}
	texOptions := transcribe.Options{CJKFontFile: absoluteFontPath(cjkFontPath)}

	for q := 1; q <= problems; q++ {
		answers, originals, err := problemAnswers(cells, q, identities)
		if err != nil {
			return err
		}
		if len(answers) == 0 {
			continue
		}
		in := export.Input{AssessmentName: examName, ProblemNumber: q, Answers: answers, TeX: texOptions}
		if err := writeProblem(outDir, in, originals); err != nil {
			return err
		}
	}
	return nil
}

// absoluteFontPath mirrors the web export's absFontPath: the preamble embeds
// the font's DIRECTORY in a fontspec Path= key, and the professor compiles the
// bundle from wherever they unpacked it, so a relative path would resolve
// against the wrong working directory and drop every Chinese glyph. An empty
// path stays empty (the family-name fallback).
func absoluteFontPath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// problemAnswers assembles one problem's export answers, and alongside them the
// ORIGINAL page bytes keyed by the images/ FILENAME export will give each page
// — computed with export.ImageName, the same namer the entries use, so a
// predicted name and the written entry cannot disagree (and writeProblem can
// refuse rather than guess if they ever do).
//
// Cells are grouped by student because two pages of one answer are one
// export.Answer with two images, not two answers colliding on one filename.
func problemAnswers(cells []CellDoc, problem int, identities map[string]regrade.Identity) ([]export.Answer, map[string][]byte, error) {
	groups := make(map[string][]CellDoc)
	for _, c := range cells {
		// An unmatched page names nobody, so there is no student to file it
		// under; it lives in the match report and nowhere else. A FAILED cell is
		// kept: it becomes a "failed" row with its pages, not a disappearance.
		if c.Result.Problem != problem || c.Result.Status == StatusUnmatched || c.Result.StudentID == "" {
			continue
		}
		groups[c.Result.StudentID] = append(groups[c.Result.StudentID], c)
	}
	if len(groups) == 0 {
		return nil, nil, nil
	}
	// export sorts the answers itself; sorting here too means the bundle does
	// not depend on map iteration or on the order the pages were scanned in.
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	answers := make([]export.Answer, 0, len(ids))
	originals := make(map[string][]byte)
	for _, studentID := range ids {
		pageCells := groups[studentID]
		sort.Slice(pageCells, func(i, j int) bool { return pageCells[i].Result.Page.Index < pageCells[j].Result.Page.Index })

		identity, ok := identities[studentID]
		if !ok {
			// Structurally unreachable: the matcher only assigns roster rows. If
			// it happens anyway the answer would ship with no redaction needles,
			// so it stops the run instead. The page index is enough to find it;
			// the id is deliberately not in the message (PII rule).
			return nil, nil, newRosterError(nil,
				"page %d was matched to a student that is not in the roster: re-run with the roster the match used",
				pageCells[0].Result.Page.Index)
		}

		// pages and originalPages are built in ONE pass so they cannot desync:
		// export names each image by its position in Pages, and this stage
		// predicts those names from originalPages to substitute the scan.
		pages := make([]imaging.MaskedImage, 0, len(pageCells))
		originalPages := make([][]byte, 0, len(pageCells))
		blocks := make([]transcribe.Block, 0, len(pageCells))
		confidence := ""
		failed := false
		for _, c := range pageCells {
			// A cell with no masked image never reached the provider and has no
			// bytes export would accept; it is still a failure to report, just
			// one with no page to show.
			if !c.Masked.IsZero() {
				pages = append(pages, c.Masked)
				originalPages = append(originalPages, c.Result.Page.JPEG)
			}
			if c.Err != nil {
				failed = true
				continue
			}
			blocks = append(blocks, c.Doc.Blocks...)
			confidence = worseConfidence(confidence, c.Confidence)
		}

		status := export.StatusOK
		var flags []string
		switch {
		case failed:
			// One failed page fails the answer even when its siblings
			// transcribed: a partially transcribed answer graded as complete is
			// the worse error, and the flag plus the real pages tell the
			// professor exactly what to do. Confidence is dropped for the same
			// reason the web path never sets one on a failure — the number would
			// describe only the pages that worked.
			status, confidence = export.StatusFailed, ""
			flags = []string{transcriptionFailedFlag}
		case confidence == "illegible" || len(blocks) == 0:
			// The web export's rule, applied to the assembled answer: an empty
			// transcription or an "illegible" verdict is not a student who wrote
			// nothing, and the professor must be able to tell those apart.
			status = export.StatusIllegible
		}
		answers = append(answers, export.Answer{
			Identity:   identity,
			Doc:        transcribe.Doc{Blocks: blocks},
			Pages:      pages,
			Status:     status,
			Source:     export.SourceDedicated,
			Confidence: confidence,
			Flags:      flags,
		})

		for i, original := range originalPages {
			originals[export.ImageName(studentID, i, len(originalPages))] = original
		}
	}
	return answers, originals, nil
}

// transcriptionFailedFlag is the manifest note a failed answer carries, worded
// exactly as the web export words it (internal/httpapi/transcription.go) so one
// cohort's bundle reads the same however it was produced.
const transcriptionFailedFlag = "transcription failed"

// confidenceRank orders the model's verdicts worst-first. A multi-page answer
// is transcribed one page per call in this mode (the web path sends a whole
// answer at once), so the pages' verdicts have to be combined — and the WORST
// one wins: "one of these pages could not be read" is the fact the professor
// needs, and averaging it away would hide it.
var confidenceRank = map[string]int{"illegible": 0, "low": 1, "medium": 2, "high": 3}

func worseConfidence(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	ra, oka := confidenceRank[a]
	rb, okb := confidenceRank[b]
	switch {
	case !oka:
		return b
	case !okb:
		return a
	case rb < ra:
		return b
	}
	return a
}

// writeProblem materialises one problem's tree, substituting the original page
// for every images/ entry.
func writeProblem(outDir string, in export.Input, originals map[string][]byte) error {
	files, err := export.Files(in)
	if err != nil {
		// Deliberately NOT a typed error. export rejects several unrelated
		// things here — an unusable student id, a duplicate, a status/source
		// combination this stage assembled wrong — and only the first is the
		// operator's roster to fix. Claiming exit 3 for all of them would send
		// someone to edit a CSV over a bug in this file. export's own message
		// (which withholds the offending value and names the answer by index)
		// carries the detail; the exit code stays the honest "unclassified" 1.
		return fmt.Errorf("offline: cannot assemble the bundle for problem %d: %w", in.ProblemNumber, err)
	}

	root := in.RootDir()
	made := make(map[string]bool)
	for _, f := range files {
		body := f.Body // aliases the caller's bytes; read-only

		// THE SUBSTITUTION. Everything under the tree's images/ is replaced by
		// the page as it was scanned, identity and all.
		if name, ok := strings.CutPrefix(f.Name, root+"/images/"); ok {
			original, found := originals[name]
			if !found {
				// The predicted name and the entry name disagree, which means
				// export's naming changed under us. Writing the masked bytes
				// instead would silently hand the professor a redacted paper.
				// An internal invariant break, not an --out the operator can
				// fix, so it is a plain error (exit 1) rather than exit 5.
				return fmt.Errorf(
					"offline: bundle image %s has no page to substitute: export.ImageName and this stage's prediction have diverged", f.Name)
			}
			body = original
		}

		path := filepath.Join(outDir, filepath.FromSlash(f.Name))
		if dir := filepath.Dir(path); !made[dir] {
			if err := mkdirPrivate(dir); err != nil {
				return newOutDirError(err, "cannot create bundle directory %s", dir)
			}
			made[dir] = true
		}
		if err := writePrivate(path, body); err != nil {
			return newOutDirError(err, "cannot write bundle file %s", path)
		}
	}
	return nil
}
