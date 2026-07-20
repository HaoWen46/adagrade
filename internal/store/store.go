// Package store owns database access: the pgx pool, in-process goose migrations, and
// the sqlc-generated typed queries (spec §2 — query/schema drift is a build error).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/migrations"
)

// Store bundles the connection pool with the generated query API.
type Store struct {
	Pool *pgxpool.Pool
	Q    *db.Queries
}

// New connects, pings, and returns a ready Store. Callers own Close.
func New(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("store: database URL is empty")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{Pool: pool, Q: db.New(pool)}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// WithTx runs fn inside a transaction with a Queries bound to it, committing on nil.
func (s *Store) WithTx(ctx context.Context, fn func(q *db.Queries) error) error {
	return s.WithTxPgx(ctx, func(_ pgx.Tx, q *db.Queries) error { return fn(q) })
}

// WithTxPgx additionally exposes the raw pgx.Tx — needed by callers that must
// enqueue River jobs in the same transaction as their writes (spec §6.1).
func (s *Store) WithTxPgx(ctx context.Context, fn func(tx pgx.Tx, q *db.Queries) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx, s.Q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// InsertAudit writes one append-only audit row (plan §10). detail may be nil.
func (s *Store) InsertAudit(ctx context.Context, actorID int64, action, kind, targetID string, detail map[string]any) error {
	b := []byte("{}")
	if detail != nil {
		var err error
		if b, err = json.Marshal(detail); err != nil {
			return fmt.Errorf("store: audit detail: %w", err)
		}
	}
	return s.Q.InsertAudit(ctx, db.InsertAuditParams{
		ActorUserID: pgtype.Int8{Int64: actorID, Valid: actorID != 0},
		Action:      action,
		TargetKind:  kind,
		TargetID:    targetID,
		Detail:      b,
	})
}

// AuditRow is one row of the admin audit-log read path (trust spec §6, D39),
// including the actor's email for display.
type AuditRow = db.ListAuditRow

// ListAuditParams filters the audit log (GET /api/audit?target_kind=&target_id=
// &action=&actor=&limit=&offset=, trust spec §6). Zero values mean "no filter";
// Limit<=0 defaults to 50 (the UI's page size) so callers can't accidentally request
// an unbounded scan of an append-only, ever-growing table.
type ListAuditParams struct {
	TargetKind string
	TargetID   string
	Action     string
	ActorID    int64
	Limit      int
	Offset     int
}

// ListAudit is the admin audit-log read path: newest-first, filterable, paginated.
func (s *Store) ListAudit(ctx context.Context, p ListAuditParams) ([]AuditRow, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	return s.Q.ListAudit(ctx, db.ListAuditParams{
		TargetKind:  pgtype.Text{String: p.TargetKind, Valid: p.TargetKind != ""},
		TargetID:    pgtype.Text{String: p.TargetID, Valid: p.TargetID != ""},
		Action:      pgtype.Text{String: p.Action, Valid: p.Action != ""},
		ActorUserID: pgtype.Int8{Int64: p.ActorID, Valid: p.ActorID != 0},
		RowLimit:    int32(limit),
		RowOffset:   int32(p.Offset),
	})
}

// RunMigrations applies all pending goose migrations. goose needs database/sql, so a
// short-lived stdlib connection is opened just for this (closed before returning).
func RunMigrations(ctx context.Context, dsn string) error {
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("store: open for migrations: %w", err)
	}
	defer func() { _ = sqldb.Close() }()

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if err := goose.UpContext(ctx, sqldb, "."); err != nil {
		return fmt.Errorf("store: migrate up: %w", err)
	}
	return nil
}

// MigrateUpTo applies migrations up to (and including) the given version only —
// exposed for tests that need to seed data at an intermediate schema version before
// applying a later migration's backfill (e.g. 0023's turn backfill), never called in
// the normal startup path (which always wants RunMigrations' "up to latest").
func MigrateUpTo(ctx context.Context, dsn string, version int64) error {
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = sqldb.Close() }()

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.UpToContext(ctx, sqldb, ".", version)
}

// MigrateDownTo rolls back to the given version — exposed for tests and the D15
// rollback procedure, never called in the normal startup path.
func MigrateDownTo(ctx context.Context, dsn string, version int64) error {
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = sqldb.Close() }()

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.DownToContext(ctx, sqldb, ".", version)
}

// Keep the stdlib driver import (registers "pgx" with database/sql).
var _ = stdlib.GetDefaultDriver
