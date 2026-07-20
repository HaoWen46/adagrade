package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

func TestMigrations_UpDownUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s := storetest.Fresh(t)
	dsn := storetest.DSN(t)

	// Every down migration must actually work (DECISIONS D15).
	if err := store.MigrateDownTo(ctx, dsn, 0); err != nil {
		t.Fatalf("migrate down to 0: %v", err)
	}
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("re-migrate up: %v", err)
	}

	var n int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
		t.Fatalf("users table should exist after re-up: %v", err)
	}
}

func TestUserQueries_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)

	u, err := s.Q.CreateUser(ctx, db.CreateUserParams{Email: "ta@ntu.edu.tw", Role: "ta", Active: true})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	got, err := s.Q.GetUserByEmail(ctx, "ta@ntu.edu.tw")
	if err != nil || got.ID != u.ID || got.Role != "ta" || !got.Active {
		t.Fatalf("GetUserByEmail: got %+v err %v", got, err)
	}

	admins, err := s.Q.CountActiveAdmins(ctx)
	if err != nil || admins != 0 {
		t.Fatalf("CountActiveAdmins: got %d err %v", admins, err)
	}
	if _, err := s.Q.UpsertActiveAdmin(ctx, "boss@ntu.edu.tw"); err != nil {
		t.Fatalf("UpsertActiveAdmin: %v", err)
	}
	// Upsert is idempotent and promotes an existing row.
	if _, err := s.Q.UpsertActiveAdmin(ctx, "boss@ntu.edu.tw"); err != nil {
		t.Fatalf("UpsertActiveAdmin twice: %v", err)
	}
	admins, err = s.Q.CountActiveAdmins(ctx)
	if err != nil || admins != 1 {
		t.Fatalf("CountActiveAdmins after upsert: got %d err %v", admins, err)
	}
}
