package store_test

import (
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

func loginTokenUser(t *testing.T, s *store.Store) int64 {
	t.Helper()
	u, err := s.Q.CreateUser(t.Context(), db.CreateUserParams{Email: "ta@ntu.edu.tw", Role: "ta", Active: true})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u.ID
}

func randomTokenHash(t *testing.T) []byte {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return buf
}

func mustCreateLoginToken(t *testing.T, s *store.Store, userID int64, expiresAt time.Time) []byte {
	t.Helper()
	hash := randomTokenHash(t)
	created, err := s.CreateLoginToken(t.Context(), userID, hash, expiresAt)
	if err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}
	if !created {
		t.Fatal("CreateLoginToken: created = false, want true")
	}
	return hash
}

func countLoginTokens(t *testing.T, s *store.Store, userID int64) int {
	t.Helper()
	var n int
	err := s.Pool.QueryRow(t.Context(), "SELECT count(*) FROM login_tokens WHERE user_id = $1", userID).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateLoginToken_CapsActiveTokensAtThree(t *testing.T) {
	s := storetest.Fresh(t)
	userID := loginTokenUser(t, s)
	expiresAt := time.Now().Add(15 * time.Minute)

	for i := 0; i < 3; i++ {
		mustCreateLoginToken(t, s, userID, expiresAt)
	}

	created, err := s.CreateLoginToken(t.Context(), userID, randomTokenHash(t), expiresAt)
	if err != nil {
		t.Fatalf("CreateLoginToken over cap: %v", err)
	}
	if created {
		t.Fatal("4th CreateLoginToken: created = true, want false")
	}
	if n := countLoginTokens(t, s, userID); n != 3 {
		t.Fatalf("login_tokens rows = %d, want 3", n)
	}
}

// A concurrent burst must not race past the cap: without per-user
// serialization, N READ COMMITTED inserts all see the pre-burst count.
func TestCreateLoginToken_ConcurrentBurstRespectsCap(t *testing.T) {
	s := storetest.Fresh(t)
	userID := loginTokenUser(t, s)
	expiresAt := time.Now().Add(15 * time.Minute)
	ctx := t.Context()

	const burst = 8
	hashes := make([][]byte, burst)
	for i := range hashes {
		hashes[i] = randomTokenHash(t)
	}
	created := make([]bool, burst)
	errs := make([]error, burst)
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			created[i], errs[i] = s.CreateLoginToken(ctx, userID, hashes[i], expiresAt)
		}(i)
	}
	wg.Wait()

	got := 0
	for i := 0; i < burst; i++ {
		if errs[i] != nil {
			t.Fatalf("CreateLoginToken %d: %v", i, errs[i])
		}
		if created[i] {
			got++
		}
	}
	if got != 3 {
		t.Fatalf("concurrent burst: created = %d, want 3", got)
	}
	if n := countLoginTokens(t, s, userID); n != 3 {
		t.Fatalf("login_tokens rows = %d, want 3", n)
	}
}

func TestDeleteLoginToken_FreesCapSlotAndInvalidatesToken(t *testing.T) {
	s := storetest.Fresh(t)
	userID := loginTokenUser(t, s)
	expiresAt := time.Now().Add(15 * time.Minute)

	hash := mustCreateLoginToken(t, s, userID, expiresAt)
	mustCreateLoginToken(t, s, userID, expiresAt)
	mustCreateLoginToken(t, s, userID, expiresAt)

	if err := s.DeleteLoginToken(t.Context(), hash); err != nil {
		t.Fatalf("DeleteLoginToken: %v", err)
	}
	if _, err := s.ConsumeLoginToken(t.Context(), hash); !errors.Is(err, store.ErrLoginTokenInvalid) {
		t.Fatalf("consume deleted token: err = %v, want ErrLoginTokenInvalid", err)
	}

	// The slot is freed: a 4th create succeeds where the cap would have blocked it.
	mustCreateLoginToken(t, s, userID, expiresAt)
	if n := countLoginTokens(t, s, userID); n != 3 {
		t.Fatalf("login_tokens rows = %d, want 3", n)
	}

	// Deleting a token that no longer exists is a silent no-op.
	if err := s.DeleteLoginToken(t.Context(), randomTokenHash(t)); err != nil {
		t.Fatalf("DeleteLoginToken (missing row): %v", err)
	}
}

func TestCreateLoginToken_ConsumedTokenFreesSlot(t *testing.T) {
	s := storetest.Fresh(t)
	userID := loginTokenUser(t, s)
	expiresAt := time.Now().Add(15 * time.Minute)

	first := mustCreateLoginToken(t, s, userID, expiresAt)
	mustCreateLoginToken(t, s, userID, expiresAt)
	mustCreateLoginToken(t, s, userID, expiresAt)

	if _, err := s.ConsumeLoginToken(t.Context(), first); err != nil {
		t.Fatalf("ConsumeLoginToken: %v", err)
	}

	mustCreateLoginToken(t, s, userID, expiresAt)
	if n := countLoginTokens(t, s, userID); n != 4 {
		t.Fatalf("login_tokens rows = %d, want 4", n)
	}
}

func TestCreateLoginToken_ExpiredTokensDoNotCount(t *testing.T) {
	s := storetest.Fresh(t)
	userID := loginTokenUser(t, s)
	expiresAt := time.Now().Add(15 * time.Minute)

	first := mustCreateLoginToken(t, s, userID, expiresAt)
	mustCreateLoginToken(t, s, userID, expiresAt)
	mustCreateLoginToken(t, s, userID, expiresAt)

	// Recently expired: does not count as active, but is inside the cleanup grace window.
	backdateLoginToken(t, s, first, time.Now().Add(-time.Minute))

	mustCreateLoginToken(t, s, userID, expiresAt)
	if n := countLoginTokens(t, s, userID); n != 4 {
		t.Fatalf("login_tokens rows = %d, want 4", n)
	}
}

func TestCreateLoginToken_CleansUpRowsExpiredOverAnHourAgo(t *testing.T) {
	s := storetest.Fresh(t)
	userID := loginTokenUser(t, s)
	expiresAt := time.Now().Add(15 * time.Minute)

	old := mustCreateLoginToken(t, s, userID, expiresAt)
	recent := mustCreateLoginToken(t, s, userID, expiresAt)
	backdateLoginToken(t, s, old, time.Now().Add(-2*time.Hour))
	backdateLoginToken(t, s, recent, time.Now().Add(-30*time.Minute))

	mustCreateLoginToken(t, s, userID, expiresAt)

	if got := loginTokenExists(t, s, old); got {
		t.Fatal("row expired 2h ago should have been deleted")
	}
	if got := loginTokenExists(t, s, recent); !got {
		t.Fatal("row expired 30m ago should have survived cleanup")
	}
}

func backdateLoginToken(t *testing.T, s *store.Store, tokenHash []byte, expiresAt time.Time) {
	t.Helper()
	tag, err := s.Pool.Exec(t.Context(), "UPDATE login_tokens SET expires_at = $2 WHERE token_hash = $1", tokenHash, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("backdate affected %d rows, want 1", tag.RowsAffected())
	}
}

func loginTokenExists(t *testing.T, s *store.Store, tokenHash []byte) bool {
	t.Helper()
	var exists bool
	err := s.Pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM login_tokens WHERE token_hash = $1)", tokenHash).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}
