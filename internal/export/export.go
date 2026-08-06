// Package export packages already-transcribed answers into the professor's
// LaTeX transcription bundle (docs/superpowers/specs/2026-07-25-latex-transcription-export-design.md §3).
//
// It is a pure function from an Input value to ZIP bytes — the same shape as
// internal/report.BuildZIP. It never calls a model, never touches the database,
// and never reads a blob: the caller resolves all of that and hands over
// finished transcribe.Doc values plus masked page images. That is what makes
// re-export free and byte-identical (spec §2), and what makes the privacy
// invariant below testable as a pure property.
//
// THE INVARIANT (spec §3):
//
//	Identity lives in filenames, never in file bytes.
//
// _all.tex is pseudonymous — Student 001, Student 002, … assigned by sorted
// student id — so it is a genuinely zero-identity artifact that can be uploaded
// to a chat LLM wholesale, which is the case the professor actually cares
// about. MANIFEST.csv is the local decoder ring and the single documented
// exception; it carries student ids and says so in-band. Per-student files
// still disclose their student id through the file NAME, which is recorded
// honestly in the spec rather than papered over.
//
// See privacy_test.go: the invariant is asserted mechanically over every byte
// of every entry, not spot-checked.
package export

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/regrade"
	"github.com/HaoWen46/adagrade/internal/transcribe"
)

// MaxZipBytes bounds one built export archive, mirroring
// internal/scan.MaxZipBytes on the ingest side. A var rather than a const so
// tests can shrink it (the typstCompileTimeout precedent in
// internal/report/typst.go).
var MaxZipBytes int64 = 2 << 30 // 2 GiB

// zipEpoch is the pinned modification time for every entry. archive/zip stamps
// time.Now() into each header by default, which alone would make two builds of
// identical input differ — and "re-exporting must be free and byte-identical"
// (spec §1) is a product requirement, not a nicety. 1980-01-01 UTC is the
// MS-DOS epoch, the conventional choice for reproducible archives and the
// earliest instant the ZIP header format can represent.
var zipEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// Status distinguishes the four different facts an empty .tex could otherwise
// stand for (spec §3). Getting this wrong is not cosmetic: "wrote nothing",
// "the transcription failed", and "was never scanned" call for three different
// actions when the professor grades from this bundle.
type Status string

const (
	// StatusOK means the answer transcribed successfully. It may still be
	// empty if the student genuinely wrote nothing.
	StatusOK Status = "ok"
	// StatusIllegible means pages exist and were read, but nothing usable came
	// back — handwriting the model could not resolve.
	StatusIllegible Status = "illegible"
	// StatusFailed means the transcription attempt errored (provider failure,
	// timeout, refusal). Retrying may help; the other statuses are terminal.
	StatusFailed Status = "failed"
	// StatusAbsent means no pages were ever scanned for this student. Nothing
	// was attempted and nothing can be retried.
	StatusAbsent Status = "absent"
)

// Source records where the transcription came from, so a mixed-provenance
// export is legible rather than uniform-looking (spec §3).
type Source string

const (
	// SourceGradingCache means the text was already on hand from the grading
	// pipeline — no new spend.
	SourceGradingCache Source = "grading-cache"
	// SourceDedicated means a dedicated transcription call produced it.
	SourceDedicated Source = "dedicated"
)

// Answer is one student's answer to the exported problem.
type Answer struct {
	// Identity is the roster identity. Export writes NONE of it into file
	// bytes: StudentID reaches the archive only through entry NAMES and
	// through MANIFEST.csv, and Name/Email reach it not at all — they are
	// carried here solely as redaction needles for the scrub pass below.
	// Reusing regrade.Identity keeps one definition of "the things that tie
	// this text to a person" (D51) instead of a second, drifting copy.
	Identity regrade.Identity

	// Doc is the already-transcribed answer. Its Title is IGNORED: export sets
	// the section title itself, because a title is exactly the kind of field a
	// caller would innocently populate with a student's name.
	Doc transcribe.Doc

	// Pages are the masked page images, in page order.
	//
	// The type is the seal (spec §3, D10/D19): imaging.MaskedImage's fields are
	// unexported, so the only ways to obtain one are imaging.Mask (fresh
	// masking) and imaging.LoadMasked (the audited read-back gate that requires
	// a "/masked/" storage key). Handing this package an unmasked page is a
	// compile error, not a code-review question.
	//
	// imaging.ProviderImage was deliberately NOT used even though it is the
	// broader sealed interface: it also admits IDCrop, which is the identity
	// region itself — precisely the bytes this export exists to leave behind.
	Pages []imaging.MaskedImage

	Status Status
	Source Source

	// Confidence is a decimal string (never a float64 — the repo-wide rule for
	// numbers that must round-trip and compare exactly). Empty is allowed.
	Confidence string

	// Flags are caller-supplied notes for the manifest. Export appends its own
	// (emitter demotions, identity-redaction counts).
	Flags []string
}

// Input is everything BuildZIP needs. It is deliberately inert data: no
// context, no store, no blob client, no provider.
type Input struct {
	// AssessmentName is operator-supplied free text, slugified for the archive
	// root. It is not student data and is safe to place in the entry name.
	AssessmentName string
	// ProblemNumber is 1-based, as printed on the paper.
	ProblemNumber int
	// Answers may be supplied in any order; the archive is sorted by student id.
	Answers []Answer
	// TeX tunes the emitted LaTeX (the bundled CJK font path). It is shared by
	// every document in the bundle, which is what lets _all.tex carry a single
	// preamble.
	TeX transcribe.Options
	// TypstVerdict is the secondary compile gate's outcome for the Typst
	// mirror: "verified", "failed", or "" (rendered "unverified" — no Typst
	// binary configured). It lands in the manifest as a header comment; a
	// failed mirror never blocks the bundle (spec 2026-07-30, LaTeX primary).
	TypstVerdict string
}

// RootDir is the archive's single top-level directory, {slug}-p{n}. Exported
// because the caller naming the .zip download wants the same string. Inside a
// whole-exam bundle the same string names the problem's SUBdirectory — the two
// layouts share one namer so a per-problem download and the corresponding
// subtree of an exam download can never drift apart.
func (in Input) RootDir() string {
	return fmt.Sprintf("%s-p%d", Slug(in.AssessmentName), in.ProblemNumber)
}

// ExamInput is the whole-exam bundle: every problem's per-problem tree, side by
// side, under a single root named for the assessment.
//
//	{slug}/
//	├── {slug}-p1/…   byte-for-byte what BuildZIP writes for problem 1
//	└── {slug}-p2/…
//
// Problems with no answers have no tree; the CALLER drops them (an empty
// directory in the bundle would read as "nobody answered", which is a claim
// about the cohort that the export has no business making). Passing one through
// is an error, not a silent skip.
type ExamInput struct {
	// AssessmentName names the whole archive and, through Slug, every entry in
	// it. It overrides each Problem's own AssessmentName — one exam, one slug.
	AssessmentName string
	// Problems may be supplied in any order; the archive is sorted by problem
	// number.
	Problems []Input
}

// RootDir is the exam archive's single top-level directory, {slug}.
func (in ExamInput) RootDir() string { return Slug(in.AssessmentName) }

// manifestColumns is the documented MANIFEST.csv schema (spec §3), with flags
// added so demotions and redaction counts have somewhere to land.
var manifestColumns = []string{"student_id", "pseudonym", "pages", "status", "source", "confidence", "flags"}

// manifestNotice is written as a leading '#' comment line — the convention
// encoding/csv, pandas and R all understand as a comment. It is in-band on
// purpose: the warning has to travel with the file, because the file is the
// one artifact in the bundle that must not be uploaded.
const manifestNotice = "# ADA-Marker transcription export — LOCAL DECODER RING, do not upload this file.\n" +
	"# It maps pseudonyms back to student ids; _all.tex (authoritative) and its\n" +
	"# _all.typ mirror are the upload-safe artifacts.\n"

// BuildZIP packages the answers into the spec §3 bundle:
//
//	{assessment-slug}-p{problem}/
//	├── _all.tex                  every student, pseudonymous, ONE preamble
//	├── MANIFEST.csv              the local decoder ring
//	├── tex/{student_id}.tex      per-student, standalone, compilable
//	└── images/{student_id}.jpg   masked page image (-p1/-p2 when multi-page)
//
// It is deterministic: identical input yields byte-identical output.
func BuildZIP(in Input) ([]byte, error) {
	return buildArchive([]Input{in}, "")
}

// BuildExamZIP packages every problem of one assessment into a single archive,
// each problem's per-problem tree side by side under one root:
//
//	{slug}/{slug}-p1/…
//	{slug}/{slug}-p2/…
//
// The inner trees are produced by the SAME writer BuildZIP uses, only under a
// path prefix — not by zipping per-problem zips, and not by a parallel
// implementation. That is what keeps a whole-exam download byte-identical to the
// per-problem downloads it replaces, and it is what keeps privacy_test.go's
// filenames-not-bytes sweep meaningful for both.
//
// MaxZipBytes bounds the WHOLE archive: the cap exists to keep one built export
// off the heap and off the wire, and an 8-problem exam is one export.
func BuildExamZIP(in ExamInput) ([]byte, error) {
	if len(in.Problems) == 0 {
		return nil, errors.New("export: no problems to export")
	}
	problems := make([]Input, len(in.Problems))
	for i, p := range in.Problems {
		// The exam's name is authoritative. A sub-Input carrying a different one
		// would name its subdirectory after a second slug, splitting the bundle.
		p.AssessmentName = in.AssessmentName
		problems[i] = p
	}
	// Sort here rather than trusting the caller: the handler assembles this list
	// from a query, and a query's row order must never be load-bearing.
	sort.Slice(problems, func(i, j int) bool { return problems[i].ProblemNumber < problems[j].ProblemNumber })
	return buildArchive(problems, in.RootDir()+"/")
}

// buildArchive is the one path both public builders take: assemble the entries,
// then seal them into a ZIP.
func buildArchive(problems []Input, prefix string) ([]byte, error) {
	entries, err := archiveEntries(problems, prefix)
	if err != nil {
		return nil, err
	}
	return writeArchive(entries)
}

// archiveEntries is the whole bundle minus the ZIP framing: validate
// everything, preflight the size, assemble each problem's entries under prefix.
// It is the single source of truth for WHAT a bundle contains — buildArchive
// zips the result, Files (dir.go) hands the same list to a caller writing a
// directory, and neither can drift from the other because there is only one
// assembler.
//
// Order matters and is preserved from the original single-problem builder:
// validation first (so an unusable student id is reported as such rather than as
// a size failure), then the preflight (so an oversized cohort fails before
// anything is materialised), then assembly.
func archiveEntries(problems []Input, prefix string) ([]entry, error) {
	valid := make([][]Answer, len(problems))
	for i, in := range problems {
		answers, err := in.validated()
		if err != nil {
			return nil, err
		}
		valid[i] = answers
	}

	// Two problems with the same number would write two trees into one
	// directory, silently interleaving two cohorts' files.
	seen := make(map[int]bool, len(problems))
	for _, in := range problems {
		if seen[in.ProblemNumber] {
			return nil, fmt.Errorf("export: problem %d appears twice; its two trees would collide on disk", in.ProblemNumber)
		}
		seen[in.ProblemNumber] = true
	}

	// Preflight the dominant term (page images) before building anything, so an
	// oversized cohort fails fast instead of after materialising a 2 GiB buffer.
	var rawBytes int64
	for _, answers := range valid {
		for _, a := range answers {
			for _, p := range a.Pages {
				rawBytes += int64(len(p.JPEG()))
			}
		}
	}
	if rawBytes > MaxZipBytes {
		return nil, fmt.Errorf("export: page images total %d bytes, over the %d-byte archive cap", rawBytes, MaxZipBytes)
	}

	var entries []entry
	for i, in := range problems {
		es, err := problemEntries(in, valid[i], prefix)
		if err != nil {
			return nil, err
		}
		entries = append(entries, es...)
	}
	return entries, nil
}

// AllTeX returns the exact _all.tex source BuildZIP would place in this
// problem's tree. Exported for the compile gate (spec §2 stage 4): _all.tex
// concatenates every student's body under the one shared preamble, so ONE
// compile of it verifies every body in the bundle — the validator guarantees
// each body is brace- and environment-balanced, so a body cannot swallow its
// successor and mask a breakage. Compiling this beats compiling N per-student
// documents at N× the subprocess cost.
func AllTeX(in Input) (string, error) {
	answers, err := in.validated()
	if err != nil {
		return "", err
	}
	entries, err := problemEntries(in, answers, "")
	if err != nil {
		return "", err
	}
	want := in.RootDir() + "/_all.tex"
	for _, e := range entries {
		if e.name == want {
			return string(e.body), nil
		}
	}
	// Structurally unreachable: problemEntries always writes _all.tex.
	return "", fmt.Errorf("export: no _all.tex entry produced")
}

// AllTyp returns the exact _all.typ source BuildZIP would place in this
// problem's tree — the secondary gate compiles these bytes, so they must be
// the bytes the professor receives (pinned by test, same invariant as AllTeX).
func AllTyp(in Input) (string, error) {
	answers, err := in.validated()
	if err != nil {
		return "", err
	}
	entries, err := problemEntries(in, answers, "")
	if err != nil {
		return "", err
	}
	want := in.RootDir() + "/_all.typ"
	for _, e := range entries {
		if e.name == want {
			return string(e.body), nil
		}
	}
	// Structurally unreachable: problemEntries always writes _all.typ.
	return "", fmt.Errorf("export: no _all.typ entry produced")
}

// AnswerTeX pairs one answer's standalone document with the student id that
// names it in the archive. Status lets the compile gate skip attribution
// work that cannot matter (an absent answer carries no student content).
type AnswerTeX struct {
	StudentID string
	TeX       string
	Status    Status
}

// AnswerTeXes returns each validated answer's STANDALONE .tex — byte-identical
// to the tex/{id}.tex entries BuildZIP writes (pinned by test) — in bundle
// (id-sorted) order. The compile gate uses it to attribute a bundle failure to
// the specific answer(s) that cannot compile, instead of refusing the whole
// cohort with an anonymous error (2026-07-30 audit).
func AnswerTeXes(in Input) ([]AnswerTeX, error) {
	answers, err := in.validated()
	if err != nil {
		return nil, err
	}
	preamble := transcribe.Preamble(in.TeX)
	problemTitle := fmt.Sprintf("Problem %d", in.ProblemNumber)
	out := make([]AnswerTeX, 0, len(answers))
	for _, a := range answers {
		scrubbed, _ := scrubDoc(a.Doc, a.Identity)
		body, _, err := emitSection(scrubbed, problemTitle, a.Status, in.TeX)
		if err != nil {
			return nil, err
		}
		out = append(out, AnswerTeX{
			StudentID: a.Identity.StudentID,
			TeX:       preamble + beginDocument + body + endDocument,
			Status:    a.Status,
		})
	}
	return out, nil
}

// entry is one file in the archive, already named and already assembled. The
// list is built in full before a single byte is compressed so the entry ORDER —
// the thing determinism depends on — is visible in one place rather than spread
// across the writer.
type entry struct {
	name   string
	method uint16
	body   []byte
}

// problemEntries writes one problem's tree under prefix ("" for the per-problem
// archive, "{slug}/" for a whole-exam bundle). answers must already be the
// validated, id-sorted slice.
func problemEntries(in Input, answers []Answer, prefix string) ([]entry, error) {
	// One preamble for the whole bundle. Taken from the emitter (rather than
	// duplicated here) so the per-student files and _all.tex can never drift
	// apart, and an emitter improvement lands in both at once.
	preamble := transcribe.Preamble(in.TeX)

	root := prefix + in.RootDir()
	width := pseudonymWidth(len(answers))
	problemTitle := fmt.Sprintf("Problem %d", in.ProblemNumber)

	var all strings.Builder
	fmt.Fprintf(&all, "%% ADA-Marker transcription export — problem %d, %d students.\n", in.ProblemNumber, len(answers))
	all.WriteString("% Sections are pseudonymous and ordered by student id; this file carries no identity.\n")
	all.WriteString("% MANIFEST.csv holds the pseudonym -> student mapping and must stay local.\n")
	all.WriteString(preamble)
	all.WriteString(beginDocument)

	// The Typst mirror aggregate (spec 2026-07-30): same sections, same
	// pseudonyms, one Typst preamble. LaTeX stays authoritative.
	typPreamble := transcribe.TypstPreamble()
	var allTyp strings.Builder
	fmt.Fprintf(&allTyp, "// ADA-Marker transcription export — problem %d, %d students (Typst mirror).\n", in.ProblemNumber, len(answers))
	allTyp.WriteString("// The LaTeX bundle (tex/, _all.tex) is authoritative; this mirror is best-effort.\n")
	allTyp.WriteString("// Sections are pseudonymous and ordered by student id; this file carries no identity.\n")
	allTyp.WriteString(typPreamble)

	rows := make([][]string, 0, len(answers))
	texFiles := make([]entry, 0, len(answers))
	typFiles := make([]entry, 0, len(answers))
	imageFiles := make([]entry, 0, len(answers))

	for i, a := range answers {
		alias := pseudonym(i, width)
		scrubbed, counts := scrubDoc(a.Doc, a.Identity)

		// Per-student file: titled by problem, never by student.
		perStudent, flags, err := emitSection(scrubbed, problemTitle, a.Status, in.TeX)
		if err != nil {
			return nil, err
		}
		texFiles = append(texFiles, entry{
			name:   root + "/tex/" + a.Identity.StudentID + ".tex",
			method: zip.Deflate,
			body:   []byte(preamble + beginDocument + perStudent + endDocument),
		})

		// _all.tex section: same content, pseudonymous heading.
		section, _, err := emitSection(scrubbed, alias, a.Status, in.TeX)
		if err != nil {
			return nil, err
		}
		all.WriteString(section)

		// Typst mirror: per-student standalone plus the aggregate section.
		// Flags are deliberately discarded — the emitter parity invariant
		// makes them identical to emitSection's, which the manifest carries.
		typFiles = append(typFiles, entry{
			name:   root + "/typ/" + a.Identity.StudentID + ".typ",
			method: zip.Deflate,
			body:   []byte(typPreamble + typstSection(scrubbed, problemTitle, a.Status)),
		})
		allTyp.WriteString(typstSection(scrubbed, alias, a.Status))

		for pageIdx, p := range a.Pages {
			imageFiles = append(imageFiles, entry{
				name: root + "/images/" + imageName(a.Identity.StudentID, pageIdx, len(a.Pages)),
				// Stored, not deflated: JPEG is already compressed, so deflating
				// it costs CPU to grow the archive.
				method: zip.Store,
				body:   p.JPEG(),
			})
		}

		allFlags := dedupe(append(append(append([]string(nil), a.Flags...), flags...), redactionFlag(counts)...))
		rows = append(rows, []string{
			a.Identity.StudentID,
			alias,
			strconv.Itoa(len(a.Pages)),
			string(a.Status),
			string(a.Source),
			a.Confidence,
			strings.Join(allFlags, "; "),
		})
	}
	all.WriteString(endDocument)

	typstVerdict := in.TypstVerdict
	if typstVerdict == "" {
		typstVerdict = "unverified"
	}
	manifest, err := renderManifest(rows, typstVerdict)
	if err != nil {
		return nil, err
	}

	// Fixed entry order, so the archive bytes do not depend on map iteration or
	// on the caller's slice order. tex/ precedes the Typst mirror: LaTeX is
	// the primary format and the layout says so.
	out := make([]entry, 0, 3+len(texFiles)+len(typFiles)+len(imageFiles))
	out = append(out,
		entry{name: root + "/_all.tex", method: zip.Deflate, body: []byte(all.String())},
		entry{name: root + "/MANIFEST.csv", method: zip.Deflate, body: manifest},
	)
	out = append(out, texFiles...)
	out = append(out, entry{name: root + "/_all.typ", method: zip.Deflate, body: []byte(allTyp.String())})
	out = append(out, typFiles...)
	out = append(out, imageFiles...)
	return out, nil
}

func writeArchive(entries []entry) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		if err := addEntry(zw, e.name, e.method, e.body); err != nil {
			_ = zw.Close()
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("export: finalize zip: %w", err)
	}
	if int64(buf.Len()) > MaxZipBytes {
		return nil, fmt.Errorf("export: archive is %d bytes, over the %d-byte cap", buf.Len(), MaxZipBytes)
	}
	return buf.Bytes(), nil
}

func addEntry(zw *zip.Writer, name string, method uint16, body []byte) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: zipEpoch})
	if err != nil {
		return fmt.Errorf("export: create zip entry: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("export: write zip entry: %w", err)
	}
	return nil
}

// ---- validation ----------------------------------------------------------
//
// Every message below identifies an answer by its INDEX, never by student id or
// name: validation errors are the likeliest place for PII to escape into a log
// line (CLAUDE.md), and an index is enough for a caller to find the row.

func (in Input) validated() ([]Answer, error) {
	if in.ProblemNumber < 1 {
		return nil, fmt.Errorf("export: problem number must be 1-based, got %d", in.ProblemNumber)
	}
	if len(in.Answers) == 0 {
		return nil, fmt.Errorf("export: no answers to export")
	}
	switch in.TypstVerdict {
	case "", "verified", "failed":
	default:
		// The verdict is interpolated into the manifest ahead of the CSV
		// header; anything outside the closed set could forge a record.
		return nil, fmt.Errorf("export: typst verdict %q is not one of \"\"|verified|failed", in.TypstVerdict)
	}

	seen := make(map[string]int, len(in.Answers))
	for i, a := range in.Answers {
		if !safeIDForFilename(a.Identity.StudentID) {
			return nil, fmt.Errorf("export: answer %d: student id is unusable as a filename (want 1-64 characters, letters/digits/dash/underscore only; value withheld per the PII rule)", i)
		}
		key := strings.ToLower(a.Identity.StudentID)
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("export: answers %d and %d share a student id (case-insensitively), which would collide on disk", prev, i)
		}
		seen[key] = i

		switch a.Status {
		case StatusOK, StatusIllegible, StatusFailed, StatusAbsent:
		default:
			return nil, fmt.Errorf("export: answer %d: unknown status (want %q|%q|%q|%q)", i, StatusOK, StatusIllegible, StatusFailed, StatusAbsent)
		}

		if a.Status == StatusAbsent {
			// "Absent" means never scanned. If pages exist, the right status is
			// illegible or failed, and letting the two blur destroys the whole
			// point of having four statuses.
			if len(a.Pages) > 0 {
				return nil, fmt.Errorf("export: answer %d: status %q but %d page image(s) supplied — a scanned answer is not absent", i, StatusAbsent, len(a.Pages))
			}
			if a.Source != "" {
				return nil, fmt.Errorf("export: answer %d: status %q must have an empty source — nothing was transcribed", i, StatusAbsent)
			}
			continue
		}

		switch a.Source {
		case SourceGradingCache, SourceDedicated:
		default:
			return nil, fmt.Errorf("export: answer %d: unknown source (want %q|%q)", i, SourceGradingCache, SourceDedicated)
		}
		for j, p := range a.Pages {
			if p.IsZero() {
				return nil, fmt.Errorf("export: answer %d: page %d has no masked bytes", i, j)
			}
		}
	}

	// Sort by student id. This single line is what makes the pseudonym mapping
	// deterministic and the archive independent of the caller's ordering.
	out := append([]Answer(nil), in.Answers...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Identity.StudentID < out[j].Identity.StudentID
	})
	return out, nil
}

// safeIDForFilename accepts only characters that are unambiguous in a ZIP entry
// name on every extraction target. Student ids are alphanumeric by
// construction, so this rejects rather than sanitises: silently rewriting an id
// would break the filename↔manifest mapping the whole bundle depends on.
func safeIDForFilename(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// ---- LaTeX assembly ------------------------------------------------------

const (
	beginDocument = "\\begin{document}\n"
	endDocument   = "\\end{document}\n"
)

// emitSection renders one already-scrubbed answer as a document BODY (no
// preamble, no document environment) headed by the given title, plus a status
// comment when the status is anything other than ok. The comment is drawn from
// a closed enum, so it can never carry content.
//
// transcribe.EmitBody gives us the body directly, so this no longer has to cut
// a standalone document back apart — the emitter's document shape is no longer
// load-bearing for the bundle.
func emitSection(d transcribe.Doc, title string, status Status, _ transcribe.Options) (string, []string, error) {
	d.Title = title // never the caller's Doc.Title: see Answer.Doc.
	body, flags := transcribe.EmitBody(d)
	if status != StatusOK {
		// An empty section is otherwise indistinguishable from "wrote nothing"
		// when reading the .tex alone, and the professor reads the .tex alone.
		body = fmt.Sprintf("%% status: %s\n%s", status, body)
	}
	return body, flags, nil
}

// typstSection mirrors emitSection for the Typst mirror. Flags are not
// returned: the emitter parity invariant (transcribe/typst.go) makes them
// identical to emitSection's, and the manifest takes them from the LaTeX
// pass. The status comment uses Typst's line-comment form; the status is a
// closed enum, so it can never carry content.
func typstSection(d transcribe.Doc, title string, status Status) string {
	d.Title = title
	body, _ := transcribe.EmitTypstBody(d)
	if status != StatusOK {
		body = fmt.Sprintf("// status: %s\n%s", status, body)
	}
	return body
}

func pseudonymWidth(n int) int {
	if w := len(strconv.Itoa(n)); w > 3 {
		return w
	}
	return 3
}

func pseudonym(i, width int) string {
	return fmt.Sprintf("Student %0*d", width, i+1)
}

func imageName(studentID string, pageIdx, pageCount int) string {
	if pageCount == 1 {
		return studentID + ".jpg"
	}
	return fmt.Sprintf("%s-p%d.jpg", studentID, pageIdx+1)
}

// ---- privacy scrub -------------------------------------------------------

// scrubDoc removes roster identity from the transcription before it is
// rendered. This is DEFENCE IN DEPTH, not the fix for PLAN_GAPS B-C10: the
// prescribed fix is a regrade.Redact pass before the transcription is
// persisted, because scrubbing only on the way out leaves the PII permanently
// in the immutable store (spec §4).
//
// It is here as well because this package makes a testable promise — no
// exported byte carries a roster identity — and a promise that depends on an
// upstream writer having been correct is not a promise. Masking is
// region-based; students write their names in margins the mask does not cover;
// so the transcription genuinely can carry identity, including rows written
// before the write-time fix landed.
//
// The returned counts are content-free and become a mask-quality signal in the
// manifest (spec §4): routine redactions mean identity text is surviving the
// image mask, which is a leak signal for the core grading path.
func scrubDoc(d transcribe.Doc, id regrade.Identity) (transcribe.Doc, regrade.RedactionCounts) {
	var total regrade.RedactionCounts
	add := func(c regrade.RedactionCounts) {
		total.Name += c.Name
		total.StudentID += c.StudentID
		total.Email += c.Email
		total.Token += c.Token
	}

	out := transcribe.Doc{}
	if len(d.Blocks) == 0 {
		return out, total
	}
	out.Blocks = make([]transcribe.Block, len(d.Blocks))
	for i, b := range d.Blocks {
		nb := transcribe.Block{Kind: b.Kind}
		text, c := regrade.Redact(b.Text, id)
		nb.Text = text
		add(c)
		if len(b.Items) > 0 {
			nb.Items = make([]string, len(b.Items))
			for j, item := range b.Items {
				it, ci := regrade.Redact(item, id)
				nb.Items[j] = it
				add(ci)
			}
		}
		out.Blocks[i] = nb
	}
	return out, total
}

// redactionFlag reports the count and nothing else — never the excised text,
// never which field it came from beyond the aggregate (CLAUDE.md).
func redactionFlag(c regrade.RedactionCounts) []string {
	if c.Total() == 0 {
		return nil
	}
	return []string{fmt.Sprintf("identity-redacted: %d", c.Total())}
}

// ---- manifest ------------------------------------------------------------

func renderManifest(rows [][]string, typstVerdict string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(manifestNotice)
	// The secondary gate's bundle-level verdict (spec 2026-07-30): a failed
	// Typst mirror ships anyway, so the manifest must say so somewhere.
	buf.WriteString("# typst: " + typstVerdict + "\n")

	w := csv.NewWriter(&buf) // '\n' line endings by default: deterministic.
	if err := w.Write(manifestColumns); err != nil {
		return nil, fmt.Errorf("export: write manifest header: %w", err)
	}
	for _, r := range rows {
		safe := make([]string, len(r))
		for i, field := range r {
			safe[i] = csvSafe(field)
		}
		if err := w.Write(safe); err != nil {
			return nil, fmt.Errorf("export: write manifest row: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("export: flush manifest: %w", err)
	}
	return buf.Bytes(), nil
}

// csvSafe neutralises spreadsheet formula injection (CWE-1236). The manifest is
// opened in Excel or Numbers by a human, and a leading =, +, -, @ or control
// character turns a cell into a formula the spreadsheet then evaluates. The
// flags column is the live path: it can carry caller-supplied text derived from
// model output, and this feature's whole threat model assumes that text is
// hostile. Prefixing with an apostrophe is the standard mitigation; it fires
// only on the trigger characters, so ordinary values (a "0.91" confidence, a
// "demoted display math" flag) are untouched.
func csvSafe(field string) string {
	if field == "" {
		return field
	}
	switch field[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + field
	}
	return field
}

// dedupe removes duplicates while preserving first-seen order, and flattens any
// embedded newline so one flag can never forge extra CSV records.
func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
