package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrLoginTokenInvalid is returned when a login token is missing, expired, or
// already consumed. The caller deliberately cannot distinguish those cases.
var ErrLoginTokenInvalid = errors.New("store: invalid login token")

// CreateLoginToken stores the hash of a one-time login token for an allowlisted
// user. Each user may hold at most 3 active (unconsumed, unexpired) tokens; at
// the cap nothing is inserted and created is false. It also deletes tokens
// expired more than an hour ago — the grace period guarantees cleanup never
// removes a row that still counts toward the cap.
//
// The count-then-insert runs inside one transaction holding a row lock on the
// user (the row always exists — login_tokens has an FK to users), so concurrent
// calls for the same user serialize instead of all reading the pre-burst count
// under READ COMMITTED and racing past the cap.
func (s *Store) CreateLoginToken(ctx context.Context, userID int64, tokenHash []byte, expiresAt time.Time) (created bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var lockedID int64
	if err := tx.QueryRow(ctx, `
		SELECT id FROM users WHERE id = $1 FOR UPDATE
	`, userID).Scan(&lockedID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM login_tokens WHERE expires_at < now() - interval '1 hour'
	`); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO login_tokens (user_id, token_hash, expires_at)
		SELECT $1, $2, $3
		WHERE (
			SELECT count(*) FROM login_tokens
			WHERE user_id = $1 AND consumed_at IS NULL AND expires_at > now()
		) < 3
	`, userID, tokenHash, expiresAt)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteLoginToken removes a token row by hash, freeing its slot under the
// cap. Callers use it when the login email could not be delivered — a token
// the user never received must not count toward the 3-active limit. Deleting
// a row that no longer exists is a no-op.
func (s *Store) DeleteLoginToken(ctx context.Context, tokenHash []byte) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM login_tokens WHERE token_hash = $1`, tokenHash)
	return err
}

// ConsumeLoginToken atomically marks a valid token consumed and returns its user id.
func (s *Store) ConsumeLoginToken(ctx context.Context, tokenHash []byte) (int64, error) {
	var userID int64
	err := s.Pool.QueryRow(ctx, `
		UPDATE login_tokens
		SET consumed_at = now()
		WHERE token_hash = $1
		  AND consumed_at IS NULL
		  AND expires_at > now()
		RETURNING user_id
	`, tokenHash).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrLoginTokenInvalid
	}
	return userID, err
}
