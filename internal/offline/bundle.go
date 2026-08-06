package offline

import (
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
// Only cells that transcribed successfully become answers. A problem with none
// gets no directory at all: an empty problem tree reads as "nobody answered",
// which is a claim about the cohort this run has no business making.
//
// Identity is NOT scrubbed here. export.problemEntries — which Files reaches —
// already runs scrubDoc over every answer, and a second pass would find nothing
// left to remove and report zero redactions, destroying the mask-quality signal
// that count exists to carry (export spec §4).
func WriteBundle(outDir string, examName string, cells []CellDoc, rows []roster.Row, problems int) error {
	if err := mkdirPrivate(outDir); err != nil {
		return newOutDirError(err, "cannot create bundle directory %s", outDir)
	}
	identities := make(map[string]regrade.Identity, len(rows))
	for _, row := range rows {
		identities[row.StudentID] = regrade.Identity{Name: row.Name, StudentID: row.StudentID, Email: row.Email}
	}

	for q := 1; q <= problems; q++ {
		answers, originals, err := problemAnswers(cells, q, identities)
		if err != nil {
			return err
		}
		if len(answers) == 0 {
			continue
		}
		in := export.Input{AssessmentName: examName, ProblemNumber: q, Answers: answers}
		if err := writeProblem(outDir, in, originals); err != nil {
			return err
		}
	}
	return nil
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
		// An unmatched page names nobody, and a failed cell has no transcription:
		// neither can be filed under a student.
		if c.Err != nil || c.Result.Problem != problem || c.Result.Status == StatusUnmatched || c.Result.StudentID == "" {
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

		pages := make([]imaging.MaskedImage, 0, len(pageCells))
		blocks := make([]transcribe.Block, 0, len(pageCells))
		confidence := ""
		for _, c := range pageCells {
			pages = append(pages, c.Masked)
			blocks = append(blocks, c.Doc.Blocks...)
			confidence = worseConfidence(confidence, c.Confidence)
		}

		status := export.StatusOK
		// The web export's rule, applied to the assembled answer: an empty
		// transcription or an "illegible" verdict is not a student who wrote
		// nothing, and the professor must be able to tell those apart.
		if confidence == "illegible" || len(blocks) == 0 {
			status = export.StatusIllegible
		}
		answers = append(answers, export.Answer{
			Identity:   identity,
			Doc:        transcribe.Doc{Blocks: blocks},
			Pages:      pages,
			Status:     status,
			Source:     export.SourceDedicated,
			Confidence: confidence,
		})

		for i, c := range pageCells {
			originals[export.ImageName(studentID, i, len(pageCells))] = c.Result.Page.JPEG
		}
	}
	return answers, originals, nil
}

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
		// export's messages withhold the offending value and name the answer by
		// index; the likeliest cause is a roster id that cannot be a filename.
		return newRosterError(err, "cannot assemble the bundle for problem %d", in.ProblemNumber)
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
				return newOutDirError(nil,
					"bundle image %s has no page to substitute: export's image naming and this stage's have diverged", f.Name)
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
