package httpapi

import (
	"net/http"
	"os"
	"syscall"
	"time"
)

// backupStampPath is the success stamp deploy/backup.sh writes as its LAST step,
// only after blobs+secret+db all archived cleanly (docs/OPERATIONS.md §4). It is a
// relative path resolved against the process's working directory, matching
// backup.sh's own BACKUP_DIR default (./backups) and systemd's
// WorkingDirectory=/opt/adamarker in production.
const backupStampPath = "backups/last-backup-ok"

// opsJobCount is one river_job state's row count.
type opsJobCount struct {
	State string `json:"state"`
	Count int64  `json:"count"`
}

// opsStatusJSON is the GET /api/ops/status response shape (Task U3). Every field
// degrades gracefully (zero/null) rather than failing the whole request — this
// endpoint exists to be polled by monitoring, so a partial answer beats a 500.
type opsStatusJSON struct {
	JobCounts            []opsJobCount `json:"job_counts"`
	OldestRunningAgeSecs *int64        `json:"oldest_running_age_secs"`
	BlobDirFreeBytes     *uint64       `json:"blob_dir_free_bytes"`
	DBSizeBytes          *int64        `json:"db_size_bytes"`
	LastBackupAt         *time.Time    `json:"last_backup_at"`
}

// handleOpsStatus reports operational health for monitoring (Task U3,
// docs/OPERATIONS.md §4): River queue depth by state, how long the oldest running
// job has been running, free space on the blob volume, on-disk DB size, and the
// last successful backup's timestamp. Admin-only — this can reveal infrastructure
// shape (paths, sizes) that isn't appropriate for lecturers/TAs.
func (s *Server) handleOpsStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := opsStatusJSON{JobCounts: []opsJobCount{}}

	// River job counts by state — river_job is River's own table (not sqlc-managed;
	// see internal/queue/river.go), so this is raw SQL against the shared pool.
	rows, err := s.store.Pool.Query(ctx, `SELECT state, count(*) FROM river_job GROUP BY state`)
	if err != nil {
		s.log.Error("ops status: river_job count query failed", "err", err)
	} else {
		for rows.Next() {
			var c opsJobCount
			if err := rows.Scan(&c.State, &c.Count); err != nil {
				s.log.Error("ops status: river_job count scan failed", "err", err)
				continue
			}
			out.JobCounts = append(out.JobCounts, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			s.log.Error("ops status: river_job count rows failed", "err", err)
		}
	}

	// Oldest running job's age, in seconds. NULL (nothing running) stays null.
	var ageSecs *float64
	if err := s.store.Pool.QueryRow(ctx,
		`SELECT extract(epoch FROM now() - min(attempted_at)) FROM river_job WHERE state = 'running'`,
	).Scan(&ageSecs); err != nil {
		s.log.Error("ops status: oldest running job query failed", "err", err)
	} else if ageSecs != nil {
		secs := int64(*ageSecs)
		out.OldestRunningAgeSecs = &secs
	}

	// Blob-dir free bytes via syscall.Statfs. A missing/unreadable dir is reported
	// as null, not a request failure (F5/F15 house style: degrade, don't 500).
	var stat syscall.Statfs_t
	if err := syscall.Statfs(s.cfg.BlobDir, &stat); err != nil {
		s.log.Warn("ops status: statfs blob dir failed", "dir", s.cfg.BlobDir, "err", err)
	} else {
		free := uint64(stat.Bavail) * uint64(stat.Bsize) //nolint:unconvert // Bsize's width differs by GOOS
		out.BlobDirFreeBytes = &free
	}

	// DB size in bytes (current database).
	var dbSize int64
	if err := s.store.Pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&dbSize); err != nil {
		s.log.Error("ops status: db size query failed", "err", err)
	} else {
		out.DBSizeBytes = &dbSize
	}

	// Last successful backup's timestamp = the stamp file's mtime, if present.
	if fi, err := os.Stat(backupStampPath); err == nil {
		t := fi.ModTime()
		out.LastBackupAt = &t
	} else if !os.IsNotExist(err) {
		s.log.Warn("ops status: stat backup stamp failed", "path", backupStampPath, "err", err)
	}

	writeJSON(w, http.StatusOK, out)
}
