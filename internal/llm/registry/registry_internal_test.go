package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/secrets"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// White-box tests for DBSource.Provider's locking: a slow/stalled DB fetch for one
// provider must not serialize a concurrent lookup of an already-cached provider
// behind it (audit finding: the whole worker fleet stalls behind any one refresh).

func newTestProvider(t *testing.T, st *db.Queries, ctx context.Context, key [32]byte, name string) {
	t.Helper()
	sealed, err := secrets.Seal(key, []byte("sk-"+name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProvider(ctx, db.CreateProviderParams{
		Name: name, Kind: "anthropic-compat", BaseUrl: "https://example.test",
		ApiKeyCiphertext: sealed, ApiKeyHint: "…" + name,
		Models: []string{"m1"}, RequestsPerSecond: 1, Burst: 2,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestProvider_CachedLookupNotBlockedBySlowFetch proves that a concurrent Provider()
// call for a name that is already warm in the cache returns immediately, even while
// another goroutine is stuck inside a slow fetch+BuildClient for a DIFFERENT
// (cold/expiring) name. Before the fix, both calls serialized behind the single
// mutex held across the DB round trip.
func TestProvider_CachedLookupNotBlockedBySlowFetch(t *testing.T) {
	ctx := context.Background()
	st := storetest.Fresh(t)
	key, err := secrets.LoadOrCreateKey(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	newTestProvider(t, st.Q, ctx, key, "slow")
	newTestProvider(t, st.Q, ctx, key, "fast")

	src := NewDBSource(st, key)

	// Warm "fast" in the cache so its lookup is a pure cache hit.
	if _, _, err := src.Provider(ctx, "fast"); err != nil {
		t.Fatalf("warm fast: %v", err)
	}

	// Inject a slow fetch hook for "slow" (test-only seam); it blocks until released.
	release := make(chan struct{})
	entered := make(chan struct{})
	src.testFetchHook = func(name string) {
		if name == "slow" {
			close(entered)
			<-release
		}
	}
	defer func() { src.testFetchHook = nil }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, err := src.Provider(ctx, "slow"); err != nil {
			t.Error(err)
		}
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("slow fetch never started")
	}

	// While "slow" is stuck mid-fetch, "fast" (cached) must resolve immediately.
	fastDone := make(chan struct{})
	go func() {
		defer close(fastDone)
		if _, _, err := src.Provider(ctx, "fast"); err != nil {
			t.Error(err)
		}
	}()

	select {
	case <-fastDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cached \"fast\" lookup serialized behind the stalled \"slow\" fetch")
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("slow fetch never completed")
	}
}

// TestProvider_LastWriterWinsOnDuplicateRefresh documents the accepted benign race:
// two goroutines racing to refresh the same expired/missing entry both fetch and
// build a client outside the lock; whichever installs last under the re-acquired
// lock wins, and neither call errors.
func TestProvider_LastWriterWinsOnDuplicateRefresh(t *testing.T) {
	ctx := context.Background()
	st := storetest.Fresh(t)
	key, err := secrets.LoadOrCreateKey(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	newTestProvider(t, st.Q, ctx, key, "dup")
	src := NewDBSource(st, key)

	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, _, err := src.Provider(ctx, "dup")
			errs <- err
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent refresh: %v", err)
		}
	}

	src.mu.Lock()
	e, ok := src.cache["dup"]
	src.mu.Unlock()
	if !ok || e.provider == nil {
		t.Fatalf("cache not installed after concurrent refresh: %+v", e)
	}
}
