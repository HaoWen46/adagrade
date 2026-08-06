package offline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/roster"
)

// Synthetic identities only — never real roster data (D14).
const goodRosterCSV = "student_id,name,email\n" +
	"B11902001,丁一心,b11902001@example.edu\n" +
	"B11902002,王二明,b11902002@example.edu\n"

// writeFile creates a file under t.TempDir() with the given contents.
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// makeUnreadable chmods path to 000, skipping the test when that cannot deny
// access (root ignores the mode bits).
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 000 does not deny reads")
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
}

// assertErrorType fails unless err is of type E and its message mentions each
// of the wanted substrings.
func assertErrorType[E error](t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %T", *new(E))
	}
	var target E
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T (%v), want %T", err, err, target)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("message %q does not mention %q", err.Error(), w)
		}
	}
}

func TestLoadRosterGood(t *testing.T) {
	path := writeFile(t, t.TempDir(), "roster.csv", goodRosterCSV)
	rows, err := LoadRoster(path)
	if err != nil {
		t.Fatalf("LoadRoster error = %v, want nil", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].StudentID != "B11902001" {
		t.Errorf("rows[0].StudentID = %q, want %q", rows[0].StudentID, "B11902001")
	}
}

func TestLoadRosterFailures(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.csv")

	unreadable := writeFile(t, dir, "locked.csv", goodRosterCSV)
	makeUnreadable(t, unreadable)

	empty := writeFile(t, dir, "empty.csv", "")
	headerOnly := writeFile(t, dir, "header.csv", "student_id,name,email\n")
	dupes := writeFile(t, dir, "dupes.csv", goodRosterCSV+"B11902001,張三,other@example.edu\n")
	noHeader := writeFile(t, dir, "nohdr.csv", "id,who\nB11902001,x\n")

	tests := []struct {
		name string
		path string
		want []string
	}{
		{"missing file", missing, []string{missing}},
		{"unreadable file", unreadable, []string{unreadable}},
		{"empty file", empty, []string{empty}},
		{"no rows", headerOnly, []string{headerOnly, "no rows"}},
		{"duplicate student_id", dupes, []string{dupes, "duplicate student_id", "line 4"}},
		{"missing columns", noHeader, []string{noHeader, "student_id"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := LoadRoster(tc.path)
			if rows != nil {
				t.Errorf("rows = %v, want nil on failure", rows)
			}
			assertErrorType[*RosterError](t, err, tc.want...)
			if got := ExitCode(err); got != ExitRoster {
				t.Errorf("ExitCode = %d, want %d", got, ExitRoster)
			}
		})
	}
}

// rosterWithEveryIdentityBearingErrorCSV trips every roster.ParseError variant
// that embeds a cell value. Line numbers are 1-based INCLUDING the header, the
// way roster.Parse counts them:
//
//	line 3: duplicate student_id (first at line 2)
//	line 4: full-width id colliding with line 2 under studentid.Normalize
//	line 5: shares line 2's email address
//	line 6: empty name
//	line 7: invalid email (no @)
//
// Synthetic identities only (D14).
const rosterWithEveryIdentityBearingErrorCSV = "student_id,name,email\n" +
	"B11902001,丁一心,b11902001@example.edu\n" +
	"B11902001,王二明,b11902099@example.edu\n" +
	"Ｂ１１９０２００１,王二明,b11902098@example.edu\n" +
	"B11902002,李三多,b11902001@example.edu\n" +
	"B11902003,,b11902003@example.edu\n" +
	"B11902004,趙四維,b11902004.example.edu\n"

// TestLoadRosterErrorsNameLinesNotStudents — LoadRoster's message goes to a
// terminal, into scrollback and into pasted bug reports, and CLAUDE.md forbids
// student ids there. roster.ParseError's own contract allows the id (D14: the
// server shows those messages back to the importing TA), so the redaction has
// to happen HERE, at the boundary where the message becomes console output.
func TestLoadRosterErrorsNameLinesNotStudents(t *testing.T) {
	path := writeFile(t, t.TempDir(), "mistakes.csv", rosterWithEveryIdentityBearingErrorCSV)

	rows, err := LoadRoster(path)
	if rows != nil {
		t.Errorf("rows = %v, want nil on failure", rows)
	}
	assertErrorType[*RosterError](t, err)
	msg := err.Error()

	// Not one cell value from the file, in any spelling that appears in it.
	for _, secret := range []string{
		"B11902001", "B11902002", "B11902003", "B11902004",
		"Ｂ１１９０２００１",
		"丁一心", "王二明", "李三多", "趙四維",
		"b11902001@example.edu", "b11902099@example.edu",
	} {
		if strings.Contains(msg, secret) {
			t.Errorf("the roster error quotes %q from the CSV:\n%s", secret, msg)
		}
	}

	// Still actionable: every bad line is named, with the KIND of problem and
	// the earlier line it conflicts with.
	for _, want := range []string{
		"line 3: duplicate student_id (see line 2)",
		// Whole line, cross-reference included: the conflicting line number is
		// the actionable half of a redacted message, so it has to survive
		// attached to the right line rather than merely appearing somewhere.
		"line 4: student_id collides with an earlier row after normalization (case/width/punctuation) (see line 2)",
		"line 5: duplicate email",
		"line 6: empty name",
		"line 7: invalid email",
		path,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the roster error does not mention %q:\n%s", want, msg)
		}
	}
}

// TestLoadRosterKeepsContentFreeMessagesVerbatim — the messages that carry no
// cell value at all ARE the remedy (the Excel "CSV UTF-8" instruction, the list
// of missing columns), so redaction must not blunt them.
func TestLoadRosterKeepsContentFreeMessagesVerbatim(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name, body string
		want       []string
	}{
		{"missing columns", "id,who\nB11902001,x\n", []string{"line 1: missing required column(s): student_id, name, email"}},
		{"empty student_id", "student_id,name,email\n,丁一心,a@example.edu\n", []string{"line 2: empty student_id"}},
		{"not utf-8", "student_id,name,email\nB11902001,\xb1\xda,a@example.edu\n", []string{"line 2: file is not valid UTF-8", "CSV UTF-8"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, dir, tc.name+".csv", tc.body)
			_, err := LoadRoster(path)
			assertErrorType[*RosterError](t, err, tc.want...)
		})
	}
}

// TestRedactedRosterMessageWithholdsUnknownShapes — the redaction is an
// ALLOWLIST, so a message roster grows tomorrow is withheld rather than printed
// on the assumption that it carries nothing identifying.
func TestRedactedRosterMessageWithholdsUnknownShapes(t *testing.T) {
	got := redactedRosterMessage(roster.ParseError{Line: 9, Msg: "student_id B11902001 has a suspicious email domain example.edu"})
	for _, secret := range []string{"B11902001", "example.edu", "suspicious"} {
		if strings.Contains(got, secret) {
			t.Errorf("an unrecognized roster message was echoed (%q leaked): %q", secret, got)
		}
	}
	if !strings.Contains(got, "line 9") {
		t.Errorf("a withheld message must still name its line: %q", got)
	}
}

func TestValidateScansGood(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a.pdf", "not really a pdf, but non-empty")
	b := writeFile(t, dir, "b.jpg", "bytes")
	if err := ValidateScans([]string{a, b}); err != nil {
		t.Fatalf("ValidateScans error = %v, want nil", err)
	}
}

func TestValidateScansFailures(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, dir, "good.pdf", "bytes")
	empty := writeFile(t, dir, "empty.pdf", "")
	locked := writeFile(t, dir, "locked.pdf", "bytes")
	makeUnreadable(t, locked)
	subdir := filepath.Join(dir, "adir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	missing := filepath.Join(dir, "gone.pdf")

	tests := []struct {
		name  string
		paths []string
		want  []string
	}{
		{"no paths", nil, []string{"--scans"}},
		{"missing", []string{good, missing}, []string{missing, "does not exist"}},
		{"directory", []string{subdir}, []string{subdir, "not a regular file"}},
		{"empty", []string{empty}, []string{empty, "empty"}},
		{"unreadable", []string{locked}, []string{locked}},
		{"duplicate", []string{good, good}, []string{good, "duplicate"}},
		// Same file reached by a different spelling of the path.
		{"duplicate after clean", []string{good, filepath.Join(dir, ".", "sub", "..", "good.pdf")}, []string{"duplicate"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateScans(tc.paths)
			assertErrorType[*ScanError](t, err, tc.want...)
			if got := ExitCode(err); got != ExitScan {
				t.Errorf("ExitCode = %d, want %d", got, ExitScan)
			}
		})
	}
}

func TestPrepareOutDirCreatesMissingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "run-1")
	if err := PrepareOutDir(path, false); err != nil {
		t.Fatalf("PrepareOutDir error = %v, want nil", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after PrepareOutDir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory", path)
	}
}

func TestPrepareOutDirExistingEmpty(t *testing.T) {
	if err := PrepareOutDir(t.TempDir(), false); err != nil {
		t.Fatalf("PrepareOutDir on an existing empty dir error = %v, want nil", err)
	}
}

func TestPrepareOutDirNonEmpty(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "leftover.csv", "old run")

	err := PrepareOutDir(dir, false)
	assertErrorType[*OutDirError](t, err, dir, "not empty", "--force")
	if got := ExitCode(err); got != ExitOutDir {
		t.Errorf("ExitCode = %d, want %d", got, ExitOutDir)
	}

	if err := PrepareOutDir(dir, true); err != nil {
		t.Errorf("PrepareOutDir(dir, force=true) error = %v, want nil", err)
	}
}

func TestPrepareOutDirNotADirectory(t *testing.T) {
	path := writeFile(t, t.TempDir(), "a-file", "content")
	err := PrepareOutDir(path, false)
	assertErrorType[*OutDirError](t, err, path, "not a directory")
	// --force must not turn a regular file into an output directory.
	err = PrepareOutDir(path, true)
	assertErrorType[*OutDirError](t, err, path, "not a directory")
}
