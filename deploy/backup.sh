#!/usr/bin/env bash
# ADA-Marker nightly backup: blobs-then-DB, per docs/DECISIONS.md D15.
#
# Order matters. Blobs are backed up FIRST, then Postgres is dumped. That way a
# restore always has blob files that are at least as fresh as the DB refs — any
# grading_records/submissions/answers row created after the blob tarball closed
# is either absent from the DB dump too (dump comes after) or, in the rare case
# a row landed between the two steps, its blob file is already safely captured.
# The failure mode this avoids is a DB dump referencing a blob that doesn't
# exist yet in the tarball. See docs/DECISIONS.md D15 and docs/PLAN_GAPS.md B-C1.
#
# Also backs up ADAMARKER_SECRET_KEY_FILE (default ./data/secret.key) — D16:
# losing it doesn't lose data, but it does lose every stored provider API key.
#
# Usage: deploy/backup.sh
# Run as the adamarker service user via deploy/adamarker-backup.timer, or by
# hand for an ad hoc backup.
#
# Env (all optional):
#   ADAMARKER_BLOB_DIR         blob root to archive (default ./data/blobs)
#   ADAMARKER_SECRET_KEY_FILE  master key file to archive (default ./data/secret.key)
#   BACKUP_DIR                 where backup files land (default ./backups)
#   BACKUP_KEEP                how many nightly backups to retain (default 14)
#   BACKUP_RSYNC_TARGET        if set, rsync'd to off-host after a successful
#                               local backup, e.g. user@backup-host:/srv/adamarker-backups/
#   PGHOST, PGPORT, PGUSER, PGDATABASE, PGPASSWORD (or PGPASSFILE / .pgpass)
#                               passed straight to pg_dump; PGUSER/PGDATABASE
#                               default to "adamarker" (docker-compose.yml's
#                               dev user/db name) and PGHOST/PGPORT default to
#                               the real Postgres default (localhost:5432) —
#                               NOT the dev-compose host mapping of 5433. Set
#                               these explicitly in backup.env for production
#                               (see docs/OPERATIONS.md section 4), and to
#                               5433 if backing up the docker-compose dev DB.

set -euo pipefail

BLOB_DIR="${ADAMARKER_BLOB_DIR:-./data/blobs}"
SECRET_KEY_FILE="${ADAMARKER_SECRET_KEY_FILE:-./data/secret.key}"
BACKUP_DIR="${BACKUP_DIR:-./backups}"
BACKUP_KEEP="${BACKUP_KEEP:-14}"
BACKUP_RSYNC_TARGET="${BACKUP_RSYNC_TARGET:-}"

PGDATABASE="${PGDATABASE:-adamarker}"
PGUSER="${PGUSER:-adamarker}"
PGHOST="${PGHOST:-127.0.0.1}"
PGPORT="${PGPORT:-5432}"
export PGDATABASE PGUSER PGHOST PGPORT

ts="$(date -u +%Y%m%d-%H%M%S)"
blobs_tgz="${BACKUP_DIR}/blobs-${ts}.tgz"
secret_tgz="${BACKUP_DIR}/secret-${ts}.tgz"
db_dump="${BACKUP_DIR}/db-${ts}.sql"
stamp_file="${BACKUP_DIR}/last-backup-ok"

log() {
	printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2
}

fail() {
	log "FAILED: $*"
	exit 1
}

mkdir -p "$BACKUP_DIR"

# --- 1. Blobs first -------------------------------------------------------
if [ ! -d "$BLOB_DIR" ]; then
	fail "blob dir $BLOB_DIR does not exist — refusing to write an empty backup"
fi

log "archiving blobs from $BLOB_DIR -> $blobs_tgz"
if ! tar -czf "${blobs_tgz}.partial" -C "$(dirname "$BLOB_DIR")" "$(basename "$BLOB_DIR")"; then
	rm -f "${blobs_tgz}.partial"
	fail "tar blobs failed"
fi
mv "${blobs_tgz}.partial" "$blobs_tgz"

# --- 2. Secret key alongside the blobs (D16) -------------------------------
if [ -f "$SECRET_KEY_FILE" ]; then
	log "archiving secret key from $SECRET_KEY_FILE -> $secret_tgz"
	if ! tar -czf "${secret_tgz}.partial" -C "$(dirname "$SECRET_KEY_FILE")" "$(basename "$SECRET_KEY_FILE")"; then
		rm -f "${secret_tgz}.partial"
		fail "tar secret key failed"
	fi
	mv "${secret_tgz}.partial" "$secret_tgz"
else
	log "warning: secret key file $SECRET_KEY_FILE not found, skipping (fine on a fresh install with no providers configured yet)"
fi

# --- 3. Then the DB dump ---------------------------------------------------
log "dumping database $PGDATABASE@$PGHOST:$PGPORT -> $db_dump"
if ! pg_dump --no-owner --no-privileges -f "${db_dump}.partial"; then
	rm -f "${db_dump}.partial"
	fail "pg_dump failed"
fi
mv "${db_dump}.partial" "$db_dump"

# --- 4. Optional off-host copy ---------------------------------------------
if [ -n "$BACKUP_RSYNC_TARGET" ]; then
	log "rsyncing tonight's backup files to $BACKUP_RSYNC_TARGET"
	rsync_srcs=("$blobs_tgz" "$db_dump")
	if [ -f "$secret_tgz" ]; then
		rsync_srcs+=("$secret_tgz")
	fi
	if ! rsync -av --relative "${rsync_srcs[@]}" "$BACKUP_RSYNC_TARGET"; then
		fail "rsync to $BACKUP_RSYNC_TARGET failed (local backup files are still on disk under $BACKUP_DIR)"
	fi
fi

# --- 5. Retention prune ------------------------------------------------------
# Keep the newest BACKUP_KEEP db-*.sql dumps (and their same-timestamped blobs/
# secret tarballs); delete anything older. BACKUP_KEEP=0 disables pruning.
if [ "$BACKUP_KEEP" -gt 0 ] 2>/dev/null; then
	log "pruning backups, keeping newest $BACKUP_KEEP"
	mapfile -t old_dumps < <(find "$BACKUP_DIR" -maxdepth 1 -name 'db-*.sql' -printf '%f\n' | sort -r | tail -n +$((BACKUP_KEEP + 1)))
	for dump_name in "${old_dumps[@]:-}"; do
		[ -z "$dump_name" ] && continue
		old_ts="${dump_name#db-}"
		old_ts="${old_ts%.sql}"
		log "pruning backup set for $old_ts"
		rm -f "${BACKUP_DIR}/db-${old_ts}.sql" "${BACKUP_DIR}/blobs-${old_ts}.tgz" "${BACKUP_DIR}/secret-${old_ts}.tgz"
	done
fi

# --- 6. Success stamp --------------------------------------------------------
# adamarker -verify-blobs / GET /api/ops/status (Task U3) reads this file to
# report backup freshness — see docs/OPERATIONS.md.
date -u +%Y-%m-%dT%H:%M:%SZ > "$stamp_file"
log "backup ok: $blobs_tgz, $db_dump"
