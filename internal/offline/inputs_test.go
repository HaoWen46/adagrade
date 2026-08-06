package offline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
