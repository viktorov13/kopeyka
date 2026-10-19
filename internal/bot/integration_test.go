package bot

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"familyexpenses/internal/expenses"
)

func isolatedDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("bot_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	db, err := sql.Open("postgres", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = expenses.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestIntegrationUpdateDedupAndForeignOwnership(t *testing.T) {
	db := isolatedDatabase(t)
	processor := NewProcessor(expenses.NewStore(db), time.UTC)
	update := telegramUpdate{ID: 1001, Message: &telegramMessage{ID: 1, From: &telegramUser{ID: 42}, Chat: telegramChat{ID: 42, Type: "private"}, Text: "кофе 350"}}
	first, err := processor.processUpdate(context.Background(), db, update)
	if err != nil {
		t.Fatal(err)
	}
	second, err := processor.processUpdate(context.Background(), db, update)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("dedup did not replay saved reply: %q, %q", first, second)
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM expenses").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate update created %d expenses", count)
	}
	var id int64
	if err = db.QueryRow("SELECT id FROM expenses").Scan(&id); err != nil {
		t.Fatal(err)
	}
	update.ID++
	update.Message.From.ID = 99
	update.Message.Chat.ID = 99
	update.Message.Text = fmt.Sprintf("удали #%d", id)
	got, err := processor.processUpdate(context.Background(), db, update)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "не найдена") {
		t.Fatal(got)
	}
	if err = db.QueryRow("SELECT count(*) FROM expenses").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("foreign expense was deleted")
	}
}

func TestIntegrationCommitFailureRollsBackExpense(t *testing.T) {
	db := isolatedDatabase(t)
	// Failure to persist the reply must also roll back the expense mutation.
	if _, err := db.Exec("ALTER TABLE telegram_updates ADD CONSTRAINT reject_reply CHECK (response='')"); err != nil {
		t.Fatal(err)
	}
	processor := NewProcessor(expenses.NewStore(db), time.UTC)
	update := telegramUpdate{ID: 2001, Message: &telegramMessage{From: &telegramUser{ID: 42}, Text: "кофе 350"}}
	if _, err := processor.processUpdate(context.Background(), db, update); err == nil {
		t.Fatal("expected response constraint failure")
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM expenses").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("expense committed without durable reply")
	}
	if _, err := db.Exec("ALTER TABLE telegram_updates DROP CONSTRAINT reject_reply"); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.processUpdate(context.Background(), db, update); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM expenses").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("retry created %d expenses", count)
	}
}
