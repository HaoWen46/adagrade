package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestOpsStatus_RequiresAdmin: lecturers/TAs get 403; a signed-out caller gets 401.
func TestOpsStatus_RequiresAdmin(t *testing.T) {
	ts, c, st := harness(t)

	resp, err := c.Get(ts.URL + "/api/ops/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed out: got %d want 401", resp.StatusCode)
	}

	ta := loginAs(t, ts, st, "ta@ntu.edu.tw", "ta")
	resp, err = ta.Get(ts.URL + "/api/ops/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ta: got %d want 403", resp.StatusCode)
	}

	lect := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")
	resp, err = lect.Get(ts.URL + "/api/ops/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("lecturer: got %d want 403", resp.StatusCode)
	}
}

// TestOpsStatus_AdminShapeNoJobsNoBackup exercises the zero-state: no river jobs,
// no backups/last-backup-ok in the CWD — every optional field must degrade to a
// present-but-empty/null value rather than erroring the whole request.
func TestOpsStatus_AdminShapeNoJobsNoBackup(t *testing.T) {
	ts, c, st := harness(t)
	admin := loginAs(t, ts, st, "boss@ntu.edu.tw", "admin")
	_ = c

	// Guard against a stray backups/last-backup-ok in the test binary's CWD from a
	// previous manual run leaking into this assertion.
	if _, err := os.Stat(backupStampPath); err == nil {
		t.Skipf("a %s exists in the test CWD; remove it to run this zero-state test", backupStampPath)
	}

	var status opsStatusJSON
	resp, err := admin.Get(ts.URL + "/api/ops/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin: got %d want 200", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if status.JobCounts == nil {
		t.Errorf("job_counts: got nil, want empty slice (not null) with zero river jobs")
	}
	if len(status.JobCounts) != 0 {
		t.Errorf("job_counts: got %+v, want empty with no jobs enqueued", status.JobCounts)
	}
	if status.OldestRunningAgeSecs != nil {
		t.Errorf("oldest_running_age_secs: got %v, want nil with nothing running", *status.OldestRunningAgeSecs)
	}
	if status.LastBackupAt != nil {
		t.Errorf("last_backup_at: got %v, want nil with no stamp file", *status.LastBackupAt)
	}
	// DB size should be reported (real Postgres in this test env).
	if status.DBSizeBytes == nil || *status.DBSizeBytes <= 0 {
		t.Errorf("db_size_bytes: got %v, want a positive size", status.DBSizeBytes)
	}
}

// TestOpsStatus_JobCountsAndBackupStamp seeds a river_job row directly (bypassing
// the queue client, which is simplest for asserting the raw count/age shape) and
// writes a real backups/last-backup-ok stamp, then checks both surface correctly.
func TestOpsStatus_JobCountsAndBackupStamp(t *testing.T) {
	ts, c, st := harness(t)
	admin := loginAs(t, ts, st, "boss@ntu.edu.tw", "admin")
	_ = c

	ctx := t.Context()
	// Insert one 'running' river_job row with attempted_at 90s in the past, and one
	// 'available' row — enough to exercise both the per-state counts and the
	// oldest-running-age computation without depending on real River workers.
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO river_job (state, queue, kind, args, attempted_at, attempt, max_attempts, priority, created_at, scheduled_at)
		VALUES
			('running', 'llm', 'run.leaf', '{}', now() - interval '90 seconds', 1, 3, 1, now(), now()),
			('available', 'llm', 'run.leaf', '{}', NULL, 0, 3, 1, now(), now())
	`); err != nil {
		t.Fatalf("seed river_job rows: %v", err)
	}

	// Stamp a backup success marker in the test binary's CWD (internal/httpapi/),
	// matching backup.sh's relative BACKUP_DIR default; clean it up after.
	if err := os.MkdirAll(filepath.Dir(backupStampPath), 0o755); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}
	if err := os.WriteFile(backupStampPath, []byte("2026-07-03T02:15:00Z\n"), 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(backupStampPath)) })

	var status opsStatusJSON
	resp, err := admin.Get(ts.URL + "/api/ops/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin: got %d want 200", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}

	counts := map[string]int64{}
	for _, c := range status.JobCounts {
		counts[c.State] = c.Count
	}
	if counts["running"] != 1 {
		t.Errorf("running count: got %d want 1 (counts=%+v)", counts["running"], counts)
	}
	if counts["available"] != 1 {
		t.Errorf("available count: got %d want 1 (counts=%+v)", counts["available"], counts)
	}
	if status.OldestRunningAgeSecs == nil || *status.OldestRunningAgeSecs < 60 {
		t.Errorf("oldest_running_age_secs: got %v, want >= 60 (seeded 90s ago)", status.OldestRunningAgeSecs)
	}
	if status.LastBackupAt == nil {
		t.Fatalf("last_backup_at: got nil, want the stamp file's mtime")
	}
}
