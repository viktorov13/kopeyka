package expenses

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"
)

func testStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; PostgreSQL integration test")
	}
	base, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("expense_test_%d", time.Now().UnixNano())
	if _, err = base.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); base.Exec("DROP SCHEMA " + schema + " CASCADE"); base.Close() })
	if err = Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(context.Background(), db); err != nil {
		t.Fatalf("migration must be repeatable: %v", err)
	}
	return NewStore(db), db
}

func TestStoreIsolationAndCRUD(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	alice, bob := int64(1001), int64(1002)
	a, err := s.Create(ctx, Input{UserID: alice, Amount: "350.10", Category: "еда", SpentAt: "2026-10-15", Comment: "кофе"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, Input{UserID: bob, Amount: "1200", Category: "транспорт", SpentAt: "2026-10-16", Comment: "такси"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.List(ctx, Filter{UserID: &alice})
	if err != nil || p.Total != 1 || len(p.Items) != 1 || p.Items[0].ID != a.ID || p.TotalAmount != "350.10" {
		t.Fatalf("alice list=%+v, err=%v", p, err)
	}
	amount := "450.25"
	if _, err = s.Update(ctx, b.ID, &alice, Patch{Amount: &amount}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user update = %v", err)
	}
	if err = s.Delete(ctx, b.ID, &alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user delete = %v", err)
	}
	if _, err = s.Update(ctx, a.ID, &alice, Patch{UserID: &bob}); err == nil {
		t.Fatal("bot changed owner")
	}
	e, err := s.Update(ctx, a.ID, &alice, Patch{Amount: &amount})
	if err != nil || e.Amount != "450.25" || e.Comment != "кофе" || e.UpdatedAt.Before(e.CreatedAt) {
		t.Fatalf("patch=%+v err=%v", e, err)
	}
	all, err := s.List(ctx, Filter{})
	if err != nil || all.Total != 2 || all.TotalAmount != "1650.25" {
		t.Fatalf("admin all=%+v err=%v", all, err)
	}
	if _, err = s.Update(ctx, a.ID, nil, Patch{UserID: &bob}); err != nil {
		t.Fatalf("admin transfer: %v", err)
	}
	if err = s.Delete(ctx, a.ID, &alice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("former owner delete=%v", err)
	}
	if err = s.Delete(ctx, b.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.Delete(ctx, b.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeat delete = %v", err)
	}
}

func TestStoreFiltersTotalsAndTransaction(t *testing.T) {
	s, db := testStore(t)
	ctx := context.Background()
	user := int64(42)
	for _, in := range []Input{
		{UserID: 42, Amount: "0.10", Category: "еда", SpentAt: "2026-10-01", Comment: "кофе 100%"},
		{UserID: 42, Amount: "0.20", Category: "еда", SpentAt: "2026-10-31", Comment: "чай"},
		{UserID: 42, Amount: "100", Category: "еда", SpentAt: "2026-11-01", Comment: "обед"},
		{UserID: 43, Amount: "300", Category: "еда", SpentAt: "2026-10-10", Comment: "кофе"},
		{UserID: 42, Amount: "200", Category: "транспорт", SpentAt: "2026-10-10", Comment: "метро"},
	} {
		if _, err := s.Create(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	f := Filter{UserID: &user, Category: "еда", From: "2026-10-01", To: "2026-10-31", Limit: 1}
	p, err := s.List(ctx, f)
	if err != nil || p.Total != 2 || p.TotalAmount != "0.30" || len(p.Items) != 1 || p.Items[0].Comment != "чай" || len(p.CategoryTotals) != 1 {
		t.Fatalf("filtered page=%+v err=%v", p, err)
	}
	f.Offset = 10
	p, err = s.List(ctx, f)
	if err != nil || len(p.Items) != 0 || p.Total != 2 {
		t.Fatalf("out of range page=%+v err=%v", p, err)
	}
	p, err = s.List(ctx, Filter{Search: "%"})
	if err != nil || p.Total != 1 {
		t.Fatalf("literal wildcard search=%+v err=%v", p, err)
	}
	p, err = s.List(ctx, Filter{Search: "' OR 1=1 --"})
	if err != nil || p.Total != 0 {
		t.Fatalf("literal search=%+v err=%v", p, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.WithTx(tx).Create(ctx, Input{UserID: 42, Amount: "1", Category: "другое", SpentAt: "2026-10-01"}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	p, err = s.List(ctx, Filter{})
	if err != nil || p.Total != 5 {
		t.Fatalf("rollback did not undo write: %+v %v", p, err)
	}
	users, err := s.Users(ctx)
	if err != nil || len(users) != 2 || users[0].Count != 4 || users[0].TotalAmount != "300.30" {
		t.Fatalf("users=%+v err=%v", users, err)
	}
}

func TestMigrationUpgradePreservesExistingExpenses(t *testing.T) {
	s, db := testStore(t)
	ctx := context.Background()
	e, err := s.Create(ctx, Input{UserID: 42, Amount: "350", Category: "еда", SpentAt: "2026-10-19"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an installation that has only the original schema version.
	if _, err = db.Exec("DROP TABLE telegram_conversations; DELETE FROM schema_migrations WHERE version=2"); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatalf("repeated upgrade: %v", err)
	}
	page, err := s.List(ctx, Filter{})
	if err != nil || page.Total != 1 || page.Items[0].ID != e.ID || page.Items[0].Amount != "350.00" {
		t.Fatalf("upgrade changed expenses: %+v %v", page, err)
	}
	if _, err = db.Exec("INSERT INTO telegram_conversations(user_id) VALUES(42)"); err != nil {
		t.Fatalf("conversation migration not applied: %v", err)
	}
}
