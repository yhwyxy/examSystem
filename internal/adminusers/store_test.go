package adminusers

import (
	"context"
	"errors"
	"testing"

	"github.com/yhwyxy/examSystem/internal/db"
	"github.com/yhwyxy/examSystem/internal/testutil"
)

func TestBootstrapCreateAuthenticateAndAudit(t *testing.T) {
	pool, _ := testutil.NewSchema(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewStore(pool)
	if err := store.EnsureBootstrapAdmin(ctx, "first-password", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := store.EnsureBootstrapAdmin(ctx, "replacement-password", ""); err != nil {
		t.Fatalf("repeat bootstrap: %v", err)
	}
	if role, err := store.Authenticate(ctx, "admin", "first-password"); err != nil || role != RoleAdmin {
		t.Fatalf("bootstrap authentication role=%q err=%v", role, err)
	}
	var bootstrapAuditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_user_audit WHERE actor='bootstrap' AND username='admin' AND action='created'`).Scan(&bootstrapAuditCount); err != nil {
		t.Fatal(err)
	}
	if bootstrapAuditCount != 1 {
		t.Fatalf("bootstrap audit rows=%d, want 1", bootstrapAuditCount)
	}
	if _, err := store.Authenticate(ctx, "admin", "replacement-password"); err == nil {
		t.Fatal("repeat bootstrap replaced an existing account password")
	}

	user, err := store.Create(ctx, CreateInput{
		Username: "reviewer01", Password: "review-password", Role: RoleReviewer, Actor: "admin",
	})
	if err != nil {
		t.Fatalf("create reviewer: %v", err)
	}
	if user.Username != "reviewer01" || user.Role != RoleReviewer || !user.Active {
		t.Fatalf("unexpected user response: %+v", user)
	}
	if role, err := store.Authenticate(ctx, user.Username, "review-password"); err != nil || role != RoleReviewer {
		t.Fatalf("reviewer authentication role=%q err=%v", role, err)
	}
	if _, err := store.Authenticate(ctx, user.Username, "wrong-password"); err == nil {
		t.Fatal("wrong password authenticated")
	}
	if _, err := store.Create(ctx, CreateInput{
		Username: "reviewer01", Password: "another-password", Role: RoleReviewer, Actor: "admin",
	}); !errors.Is(err, ErrUserExists) {
		t.Fatalf("duplicate create err=%v, want ErrUserExists", err)
	}

	if _, err := store.Update(ctx, UpdateInput{Username: "reviewer01", Role: RolePublisher, Actor: "admin"}); err != nil {
		t.Fatalf("update role: %v", err)
	}
	if role, err := store.Authenticate(ctx, "reviewer01", "review-password"); err != nil || role != RolePublisher {
		t.Fatalf("updated authentication role=%q err=%v", role, err)
	}
	users, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("listed %d users, want 2", len(users))
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_user_audit WHERE username='reviewer01'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 2 {
		t.Fatalf("audit rows=%d, want create and update", auditCount)
	}
	if err := store.SetAdminPassword(ctx, "changed-admin-password"); err != nil {
		t.Fatalf("change bootstrap admin password: %v", err)
	}
	if role, err := store.Authenticate(ctx, "admin", "changed-admin-password"); err != nil || role != RoleAdmin {
		t.Fatalf("changed admin password role=%q err=%v", role, err)
	}
	var passwordAuditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_user_audit WHERE username='admin' AND password_changed`).Scan(&passwordAuditCount); err != nil {
		t.Fatal(err)
	}
	if passwordAuditCount != 2 {
		t.Fatalf("admin password audit rows=%d, want bootstrap and password-change entries", passwordAuditCount)
	}
	var leaked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_user_audit WHERE actor LIKE '%password%' OR username LIKE '%password%'`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("audit unexpectedly contains password text")
	}
}

func TestUpdateCannotRemoveLastAdminOrDemoteBootstrapAdmin(t *testing.T) {
	pool, _ := testutil.NewSchema(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewStore(pool)
	if err := store.EnsureBootstrapAdmin(ctx, "admin-password", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	active := false
	if _, err := store.Update(ctx, UpdateInput{Username: "admin", Role: RoleReviewer, Active: &active, Actor: "admin"}); !errors.Is(err, ErrSelfDemotion) {
		t.Fatalf("bootstrap admin demotion err=%v, want ErrSelfDemotion", err)
	}

	// Exercise the last-admin invariant independently of the protected bootstrap identity.
	otherPool, _ := testutil.NewSchema(t)
	if _, err := db.Migrate(ctx, otherPool); err != nil {
		t.Fatalf("migrate second schema: %v", err)
	}
	otherStore := NewStore(otherPool)
	if _, err := otherStore.Create(ctx, CreateInput{Username: "rootadmin", Password: "admin-password", Role: RoleAdmin, Actor: "bootstrap"}); err != nil {
		t.Fatalf("create sole admin: %v", err)
	}
	if _, err := otherStore.Update(ctx, UpdateInput{Username: "rootadmin", Role: RolePublisher, Actor: "bootstrap"}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("sole admin demotion err=%v, want ErrLastAdmin", err)
	}
}
