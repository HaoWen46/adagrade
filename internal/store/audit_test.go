package store_test

import (
	"context"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

func TestListAudit_FiltersAndPagination(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)

	admin, err := s.Q.CreateUser(ctx, db.CreateUserParams{Email: "admin@example.test", Role: "admin", Active: true})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := s.InsertAudit(ctx, admin.ID, "publish.create", "assessment", "1", nil); err != nil {
		t.Fatalf("InsertAudit 1: %v", err)
	}
	if err := s.InsertAudit(ctx, admin.ID, "publish.unpublish", "assessment", "1", nil); err != nil {
		t.Fatalf("InsertAudit 2: %v", err)
	}
	if err := s.InsertAudit(ctx, admin.ID, "regrade.resolve", "regrade_request", "9", nil); err != nil {
		t.Fatalf("InsertAudit 3: %v", err)
	}

	all, err := s.ListAudit(ctx, store.ListAuditParams{})
	if err != nil || len(all) != 3 {
		t.Fatalf("ListAudit (no filter): got %d err %v", len(all), err)
	}
	// Newest first.
	if all[0].Action != "regrade.resolve" {
		t.Fatalf("expected newest-first order, got %+v", all[0])
	}
	if all[0].ActorEmail.String != "admin@example.test" {
		t.Fatalf("expected actor email joined, got %+v", all[0].ActorEmail)
	}

	byTarget, err := s.ListAudit(ctx, store.ListAuditParams{TargetKind: "assessment", TargetID: "1"})
	if err != nil || len(byTarget) != 2 {
		t.Fatalf("ListAudit (target filter): got %d err %v", len(byTarget), err)
	}

	byAction, err := s.ListAudit(ctx, store.ListAuditParams{Action: "publish.create"})
	if err != nil || len(byAction) != 1 {
		t.Fatalf("ListAudit (action filter): got %d err %v", len(byAction), err)
	}

	page, err := s.ListAudit(ctx, store.ListAuditParams{Limit: 1, Offset: 1})
	if err != nil || len(page) != 1 {
		t.Fatalf("ListAudit (paginated): got %d err %v", len(page), err)
	}
	if page[0].Action != "publish.unpublish" {
		t.Fatalf("unexpected page[0]: %+v", page[0])
	}
}
