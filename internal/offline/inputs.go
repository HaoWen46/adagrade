package offline

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/HaoWen46/adagrade/internal/roster"
)

// maxReportedParseErrors caps the roster parse errors echoed into the message.
// A broken export produces one error per line; the operator needs the first
// few and the total, not a thousand lines of scrollback.
const maxReportedParseErrors = 20

// LoadRoster reads and parses the roster CSV. Parsing itself is roster.Parse —
// the same code path the server's import uses, so the offline run accepts
// exactly the files the web UI accepts (UTF-8 only, header student_id,name,email).
//
// Every failure is a *RosterError (exit 3) naming the path. Parse errors are
// NOT reproduced verbatim — see redactedRosterMessage — because five of
// roster.ParseError's variants quote the offending row's student_id, and this
// message is printed to a terminal rather than shown to the TA who owns the
// file.
//
// A roster that parses but holds zero rows is rejected. Force-matching against
// nobody cannot succeed, and failing here beats failing per page later.
func LoadRoster(path string) ([]roster.Row, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, newRosterError(err, "roster file %s does not exist: pass --roster with the path to the roster CSV", path)
		}
		if errors.Is(err, fs.ErrPermission) {
			return nil, newRosterError(err, "cannot read roster file %s: check its permissions", path)
		}
		return nil, newRosterError(err, "cannot open roster file %s", path)
	}
	defer f.Close()

	rows, parseErrs := roster.Parse(f)
	if len(parseErrs) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "roster file %s has %d problem(s); fix the CSV and re-run:", path, len(parseErrs))
		for i, pe := range parseErrs {
			if i == maxReportedParseErrors {
				fmt.Fprintf(&b, "\n  ... and %d more", len(parseErrs)-i)
				break
			}
			fmt.Fprintf(&b, "\n  %s", redactedRosterMessage(pe))
		}
		return nil, newRosterError(nil, "%s", b.String())
	}
	if len(rows) == 0 {
		return nil, newRosterError(nil, "roster file %s has no rows: a roster with nobody on it cannot be matched against", path)
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// Redacting roster parse errors.
//
// roster.ParseError's contract (internal/roster/csv.go) is "no cell contents
// BEYOND the student_id column", and the server relies on that: its import
// screen shows the messages back to the TA who just uploaded the file, and
// naming the id is what makes "line 41 collides with line 12" fixable.
//
// This mode has no such screen. The message goes to stderr, which means terminal
// scrollback, a CI log, a screenshot, or a pasted bug report — the same
// destinations run.go keeps identity out of run.log for. CLAUDE.md names student
// ids as PII outright, so the boundary between roster's contract and this one is
// exactly here, and the five variants that quote an id lose it.
//
// The rewrite is an ALLOWLIST, not a search-and-replace: a message shape that is
// not listed is WITHHELD. roster may grow a variant tomorrow that quotes a name
// or an address, and a denylist would print it until someone noticed.
// ---------------------------------------------------------------------------

// rosterContentFreeMessages are the roster.ParseError messages that carry no
// cell value at all, reproduced verbatim because their wording IS the remedy:
//
//	"cannot read file"
//	"file is not valid UTF-8 — in Excel, use Save As → 'CSV UTF-8 (Comma delimited)'"
//	"cannot read header row (empty file?)"
//	"missing required column(s): student_id, name"   (header names, not values)
//	"malformed CSV row"
//	"empty student_id"
//
// Matched by prefix so the missing-column list survives intact.
var rosterContentFreeMessages = []string{
	"cannot read file",
	"file is not valid UTF-8",
	"cannot read header row",
	"missing required column(s):",
	"malformed CSV row",
	"empty student_id",
}

// rosterIdentityBearingMessages maps each roster.ParseError variant that
// embeds a student_id onto the kind of problem alone. The prefixes are the
// literal format strings of internal/roster/csv.go:
//
//	"empty name for student_id %s"
//	"invalid email for student_id %s"
//	"duplicate student_id %s (first at line %d)"
//	"student_id %s is the same as student_id %s (line %d) after normalization (case/width/punctuation)"
//	"duplicate email: student_id %s shares an email address with student_id %s (line %d)"
//
// Order matters only in that no prefix here is a prefix of another; "empty
// student_id" (content-free, above) does not begin with "student_id".
var rosterIdentityBearingMessages = []struct{ prefix, kind string }{
	{"empty name for student_id", "empty name"},
	{"invalid email for student_id", "invalid email"},
	{"duplicate student_id", "duplicate student_id"},
	{"student_id ", "student_id collides with an earlier row after normalization (case/width/punctuation)"},
	{"duplicate email:", "duplicate email: this row shares an email address with an earlier row"},
}

// rosterCrossRefLine finds the "(first at line 12)" / "(line 12)" tail the three
// duplicate variants carry. The referenced line is the most useful part of those
// messages and is not a cell value, so it is the one thing carried across.
var rosterCrossRefLine = regexp.MustCompile(`line (\d+)\)`)

// redactedRosterMessage renders one parse error as "line N: KIND", with every
// cell value removed and the conflicting line kept when the original named one.
func redactedRosterMessage(pe roster.ParseError) string {
	for _, safe := range rosterContentFreeMessages {
		if strings.HasPrefix(pe.Msg, safe) {
			return fmt.Sprintf("line %d: %s", pe.Line, pe.Msg)
		}
	}
	for _, r := range rosterIdentityBearingMessages {
		if !strings.HasPrefix(pe.Msg, r.prefix) {
			continue
		}
		if m := rosterCrossRefLine.FindAllStringSubmatch(pe.Msg, -1); len(m) > 0 {
			return fmt.Sprintf("line %d: %s (see line %s)", pe.Line, r.kind, m[len(m)-1][1])
		}
		return fmt.Sprintf("line %d: %s", pe.Line, r.kind)
	}
	// An unrecognized shape. Naming the line is the whole remedy anyway — the
	// operator opens their own file, where the row is in front of them.
	return fmt.Sprintf("line %d: rejected (the reason is withheld here because it may quote the row; open this line in the CSV)", pe.Line)
}

// ValidateScans checks the scan inputs up front so a typo fails in the first
// second of a run instead of after the render stage. Each path must exist, be a
// regular file, be readable, and be non-empty; the same file may not be listed
// twice (duplicates are compared after filepath.Clean).
//
// It deliberately does NOT sniff file formats. Deciding whether the bytes are a
// usable PDF or image belongs to the renderer, which produces the same exit
// code (4) with a far better message than a magic-number guess could.
func ValidateScans(paths []string) error {
	if len(paths) == 0 {
		return newScanError(nil, "no scan files given: pass --scans FILE (repeatable) or list scan paths after all flags")
	}
	seen := make(map[string]string, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if first, dup := seen[clean]; dup {
			return newScanError(nil, "duplicate scan file %s (already given as %s): list each scan once", path, first)
		}
		seen[clean] = path

		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return newScanError(err, "scan file %s does not exist: check the path", path)
			}
			return newScanError(err, "cannot stat scan file %s", path)
		}
		if !info.Mode().IsRegular() {
			return newScanError(nil, "scan file %s is not a regular file: pass the scanned PDF or image itself, not a directory", path)
		}
		if info.Size() == 0 {
			return newScanError(nil, "scan file %s is empty (0 bytes): re-export it from the scanner", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return newScanError(err, "cannot read scan file %s: check its permissions", path)
		}
		_ = f.Close()
	}
	return nil
}

// PrepareOutDir makes path usable as the run's output directory, creating it
// (with parents) when it does not exist.
//
// A non-empty existing directory is refused unless force is set: artifacts are
// the only audit trail this mode produces, and silently mixing two runs'
// match-reports in one directory would make that trail unreadable. An existing
// non-directory is refused outright — --force widens "may overwrite files", not
// "may delete your file".
func PrepareOutDir(path string, force bool) error {
	if strings.TrimSpace(path) == "" {
		return newOutDirError(nil, "--out is required: pass the directory to write artifacts into")
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// 0700 (mask.go's PII modes): everything inside is a student's page, a
		// crop of their name, or a transcription of their answer. Only a
		// directory this function CREATES is set — an --out that already exists
		// is the operator's own, and every file written into it is 0600 whether
		// or not the directory itself is narrowed.
		if err := mkdirPrivate(path); err != nil {
			return newOutDirError(err, "cannot create output directory %s", path)
		}
		return nil
	case err != nil:
		return newOutDirError(err, "cannot inspect output directory %s", path)
	case !info.IsDir():
		return newOutDirError(nil, "output path %s exists and is not a directory: pass --out with a directory path", path)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return newOutDirError(err, "cannot read output directory %s: check its permissions", path)
	}
	if len(entries) > 0 && !force {
		return newOutDirError(nil, "output directory %s is not empty (%d entries): pick an empty directory, or pass --force to write into it anyway (existing artifacts may be overwritten)", path, len(entries))
	}
	return nil
}
