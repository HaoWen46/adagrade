// Package storetest gives other packages' integration tests a migrated, clean
// database. Tests skip automatically when ADAMARKER_TEST_DATABASE_URL is unset, so
// plain `make test` never needs Postgres.
//
// Isolation model: migrations run ONCE into a template database
// (adamarker_test_template); each test then gets its own throwaway database
// cloned from the template (CREATE DATABASE ... TEMPLATE, ~100ms) and dropped in
// cleanup. Tests never share state — across packages too — so DB-backed test
// binaries no longer serialize behind a global advisory lock, and per-test setup
// no longer pays the full migration stack. The template is rebuilt (under an
// advisory lock on the base database) whenever the embedded migrations or the
// River module version change. Requires the test-DB user to have CREATEDB
// (the compose/CI user is the container superuser).
package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/migrations"
)

const (
	templateName = "adamarker_test_template"
	// testDBPrefix embeds the creating process's pid so orphans left by killed
	// test binaries can be detected (pid no longer alive) and swept on the next
	// template rebuild.
	testDBPrefix = "adamarker_test_p"
	// lockKey serializes template rebuilds against per-test clones: clones take
	// the lock shared (they can run concurrently with each other), a rebuild
	// takes it exclusive.
	lockKey = "adamarker-storetest-template"
)

var (
	mu      sync.Mutex
	perTest = map[*testing.T]string{} // test -> its clone's DSN (memoized so DSN and Fresh agree)
	nameSeq atomic.Int64

	setupOnce sync.Once
	setupErr  error
	adminPool *pgxpool.Pool // connected to the base (env DSN) database for CREATE/DROP DATABASE
	baseURL   *url.URL
)

// DSN returns the connection URL of this test's own throwaway database, creating
// it from the migrated template on first call (or skips the test when
// ADAMARKER_TEST_DATABASE_URL is unset). Repeated calls from the same test — and
// Fresh — return the same database, so goose helpers taking a DSN operate on the
// exact database the test's Store is connected to.
func DSN(t *testing.T) string {
	t.Helper()
	base := os.Getenv("ADAMARKER_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("ADAMARKER_TEST_DATABASE_URL not set (use `make test-integration`)")
	}
	ctx := context.Background()

	setupOnce.Do(func() { setupErr = ensureTemplate(ctx, base) })
	if setupErr != nil {
		t.Fatalf("storetest: template setup: %v", setupErr)
	}

	mu.Lock()
	defer mu.Unlock()
	if dsn, ok := perTest[t]; ok {
		return dsn
	}

	name := fmt.Sprintf("%s%d_n%d", testDBPrefix, os.Getpid(), nameSeq.Add(1))
	if err := createFromTemplate(ctx, name); err != nil {
		t.Fatalf("storetest: create test database %s: %v", name, err)
	}
	t.Cleanup(func() {
		mu.Lock()
		delete(perTest, t)
		mu.Unlock()
		// Runs after the test's own cleanups (LIFO), i.e. after Fresh's pool
		// closed; FORCE covers any connection a test left behind.
		_, _ = adminPool.Exec(context.Background(),
			fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
	})

	// NOTE: the DSN must stay valid for plain pgx / database/sql connections
	// (goose, AcquireWorkerLock), so no pgxpool-only params like pool_max_conns
	// here — Postgres rejects them as unrecognized runtime parameters.
	u := *baseURL
	u.Path = "/" + name
	dsn := u.String()
	perTest[t] = dsn
	return dsn
}

// Fresh returns a connected Store on this test's own fully-migrated throwaway
// database (see DSN); the connection closes with the test.
func Fresh(t *testing.T) *store.Store {
	t.Helper()
	dsn := DSN(t)
	s, err := store.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("storetest: connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// createFromTemplate clones the template under the shared advisory lock, so
// clones never interleave with a template rebuild (which holds it exclusively).
func createFromTemplate(ctx context.Context, name string) error {
	conn, err := adminPool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock_shared(hashtext($1))", lockKey); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock_shared(hashtext($1))", lockKey)
	}()
	// A recycled pid could collide with an orphan from a killed run; replace it.
	if _, err := conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name)); err != nil {
		return err
	}
	_, err = conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q TEMPLATE %q`, name, templateName))
	return err
}

// ensureTemplate connects the admin pool and, under the exclusive advisory lock,
// (re)builds the template database unless its recorded fingerprint already
// matches the current migration set.
func ensureTemplate(ctx context.Context, base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return fmt.Errorf("ADAMARKER_TEST_DATABASE_URL must be a postgres:// URL, got %q: %v", base, err)
	}
	baseURL = u

	cfg, err := pgxpool.ParseConfig(base)
	if err != nil {
		return err
	}
	cfg.MaxConns = 3
	adminPool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	if err := adminPool.Ping(ctx); err != nil {
		return fmt.Errorf("ping %s: %w", u.Host, err)
	}

	fp, err := fingerprint()
	if err != nil {
		return err
	}

	conn, err := adminPool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", lockKey); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtext($1))", lockKey)
	}()

	// Fingerprint bookkeeping lives in the base database, which nothing else
	// writes to anymore.
	if _, err := conn.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS storetest_template_meta (id int PRIMARY KEY DEFAULT 1, fingerprint text NOT NULL)`); err != nil {
		return err
	}
	var recorded string
	_ = conn.QueryRow(ctx, `SELECT fingerprint FROM storetest_template_meta WHERE id = 1`).Scan(&recorded)
	var templateExists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, templateName).Scan(&templateExists); err != nil {
		return err
	}
	if recorded == fp && templateExists {
		return nil
	}

	sweepOrphans(ctx, conn)

	if _, err := conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, templateName)); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, templateName)); err != nil {
		return fmt.Errorf("create template (the test-DB user needs CREATEDB): %w", err)
	}

	tu := *u
	tu.Path = "/" + templateName
	if err := migrateTemplate(ctx, tu.String()); err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}

	if _, err := conn.Exec(ctx, `INSERT INTO storetest_template_meta (id, fingerprint) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET fingerprint = EXCLUDED.fingerprint`, fp); err != nil {
		return err
	}
	return nil
}

// migrateTemplate runs the full migration stack (goose + River) into the
// template, then disconnects so the template has no sessions and can be cloned.
func migrateTemplate(ctx context.Context, dsn string) error {
	if err := store.RunMigrations(ctx, dsn); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return err
	}
	_, err = migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	return err
}

// sweepOrphans drops per-test databases left behind by killed test binaries
// (their embedded pid is no longer alive). Called only while holding the
// exclusive lock, so no live clone/create is in flight. Best-effort: a database
// belonging to a still-running binary is left alone.
func sweepOrphans(ctx context.Context, conn *pgxpool.Conn) {
	rows, err := conn.Query(ctx,
		`SELECT datname FROM pg_database WHERE datname LIKE $1`, testDBPrefix+"%")
	if err != nil {
		return
	}
	var names []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			names = append(names, name)
		}
	}
	rows.Close()
	for _, name := range names {
		rest, ok := strings.CutPrefix(name, testDBPrefix)
		if !ok {
			continue
		}
		pidStr, _, _ := strings.Cut(rest, "_")
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pidAlive(pid) {
			continue
		}
		_, _ = conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
	}
}

// fingerprint hashes the embedded goose migrations plus the River module
// versions: any change to either invalidates the template.
func fingerprint() (string, error) {
	h := sha256.New()
	var names []string
	if err := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			names = append(names, path)
		}
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(names)
	for _, name := range names {
		b, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\n%d\n", name, len(b))
		h.Write(b)
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			if strings.HasPrefix(dep.Path, "github.com/riverqueue/") {
				fmt.Fprintf(h, "%s@%s\n", dep.Path, dep.Version)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// pidAlive reports whether a process with the given pid exists on this host.
// All test binaries sharing one Postgres run on the same host, so a dead pid
// means its per-test databases are orphans.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || err == syscall.EPERM
}
