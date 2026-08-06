package offline

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
// reproduced verbatim: they carry the line number and the fix, and roster's own
// contract keeps cell contents other than student_id out of them (D14).
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
			fmt.Fprintf(&b, "\n  %s", pe.Error())
		}
		return nil, newRosterError(nil, "%s", b.String())
	}
	if len(rows) == 0 {
		return nil, newRosterError(nil, "roster file %s has no rows: a roster with nobody on it cannot be matched against", path)
	}
	return rows, nil
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
