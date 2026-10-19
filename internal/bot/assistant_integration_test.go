package bot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"familyexpenses/internal/expenses"
)

type assistantFunc func(context.Context, expenses.Ledger, int64, string, json.RawMessage) (string, json.RawMessage, error)

func (f assistantFunc) Respond(ctx context.Context, ledger expenses.Ledger, user int64, text string, history json.RawMessage) (string, json.RawMessage, error) {
	return f(ctx, ledger, user, text, history)
}

type providerFailure struct{}

func (providerFailure) Error() string       { return "provider unavailable" }
func (providerFailure) UserMessage() string { return assistantFailure }

func conversationFor(text string) json.RawMessage {
	raw, _ := json.Marshal([]any{[]any{
		map[string]string{"role": "user", "content": text},
		map[string]string{"role": "assistant", "content": "Уточните сумму"},
	}})
	return raw
}

func messageUpdate(id, user int64, text string) telegramUpdate {
	return telegramUpdate{ID: id, Message: &telegramMessage{From: &telegramUser{ID: user}, Chat: telegramChat{ID: user, Type: "private"}, Text: text}}
}

func TestIntegrationAssistantHistoryDedupIsolationAndRestart(t *testing.T) {
	db := isolatedDatabase(t)
	store := expenses.NewStore(db)
	calls := 0
	assistant := assistantFunc(func(ctx context.Context, ledger expenses.Ledger, user int64, text string, history json.RawMessage) (string, json.RawMessage, error) {
		calls++
		if text == "350" {
			if user != 42 || !strings.Contains(string(history), "кофе") {
				t.Fatalf("lost clarification context: user=%d history=%s", user, history)
			}
			if _, err := ledger.Create(ctx, expenses.Input{UserID: user, Amount: "350", Category: "еда", SpentAt: "2026-10-19", Comment: "кофе"}); err != nil {
				return "", nil, err
			}
		} else if string(history) != "[]" {
			t.Fatalf("new user received history: %s", history)
		}
		return "Готово: " + text, conversationFor(text), nil
	})
	p := NewLLMProcessor(store, time.UTC, assistant)
	first := messageUpdate(3001, 42, "кофе")
	reply, err := p.processUpdate(context.Background(), db, first)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := p.processUpdate(context.Background(), db, first); err != nil || got != reply || calls != 1 {
		t.Fatalf("duplicate called provider: calls=%d reply=%q err=%v", calls, got, err)
	}
	// A fresh processor reads durable context; another sender starts empty.
	p = NewLLMProcessor(store, time.UTC, assistant)
	if _, err := p.processUpdate(context.Background(), db, messageUpdate(3002, 99, "такси")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.processUpdate(context.Background(), db, messageUpdate(3003, 42, "350")); err != nil {
		t.Fatal(err)
	}
	page, err := store.List(context.Background(), expenses.Filter{})
	if err != nil || page.Total != 1 || page.Items[0].UserID != 42 || page.TotalAmount != "350.00" {
		t.Fatalf("clarification expense=%+v err=%v", page, err)
	}
}

func TestIntegrationAssistantContextExpiryAndCancellation(t *testing.T) {
	db := isolatedDatabase(t)
	calls := 0
	assistant := assistantFunc(func(_ context.Context, _ expenses.Ledger, _ int64, text string, history json.RawMessage) (string, json.RawMessage, error) {
		calls++
		if string(history) != "[]" {
			t.Fatalf("expired or cancelled history reused: %s", history)
		}
		return "Уточните сумму", conversationFor(text), nil
	})
	p := NewLLMProcessor(expenses.NewStore(db), time.UTC, assistant)
	if _, err := p.processUpdate(context.Background(), db, messageUpdate(3101, 42, "кофе")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE telegram_conversations SET updated_at=now()-interval '25 hours'"); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"такси", "отмена", "/help", "продукты"} {
		if _, err := p.processUpdate(context.Background(), db, messageUpdate(3102+int64(i), 42, text)); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatalf("cancel/help must not invoke LLM: calls=%d", calls)
	}
}

func TestIntegrationAssistantFailureRollsBackMutationAndHistory(t *testing.T) {
	for _, failure := range []string{"provider", "invalid_history", "database", "reply_commit"} {
		t.Run(failure, func(t *testing.T) {
			db := isolatedDatabase(t)
			oldHistory := conversationFor("кофе")
			if _, err := db.Exec("INSERT INTO telegram_conversations(user_id,history) VALUES(42,$1::jsonb)", string(oldHistory)); err != nil {
				t.Fatal(err)
			}
			if failure == "reply_commit" {
				if _, err := db.Exec("ALTER TABLE telegram_updates ADD CONSTRAINT reject_reply CHECK (response='')"); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			assistant := assistantFunc(func(ctx context.Context, ledger expenses.Ledger, user int64, text string, history json.RawMessage) (string, json.RawMessage, error) {
				calls++
				if _, err := ledger.Create(ctx, expenses.Input{UserID: user, Amount: "350", Category: "еда", SpentAt: "2026-10-19"}); err != nil {
					return "", nil, err
				}
				switch failure {
				case "provider":
					return "", nil, providerFailure{}
				case "database":
					return "", nil, errors.New("database failed")
				case "invalid_history":
					return "Готово", json.RawMessage(`{"invalid":true}`), nil
				default:
					return "Готово", conversationFor(text), nil
				}
			})
			p := NewLLMProcessor(expenses.NewStore(db), time.UTC, assistant)
			update := messageUpdate(3201, 42, "350")
			reply, err := p.processUpdate(context.Background(), db, update)
			if failure == "provider" || failure == "invalid_history" {
				if err != nil || reply != assistantFailure {
					t.Fatalf("expected durable safe reply: %q %v", reply, err)
				}
				if second, err := p.processUpdate(context.Background(), db, update); err != nil || second != reply || calls != 1 {
					t.Fatalf("failed provider turn retried: %q %v calls=%d", second, err, calls)
				}
			} else if err == nil {
				t.Fatal("expected database error")
			}
			var count int
			if err := db.QueryRow("SELECT count(*) FROM expenses").Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial expense was committed: count=%d err=%v", count, err)
			}
			var equal bool
			if err := db.QueryRow("SELECT history=$1::jsonb FROM telegram_conversations WHERE user_id=42", string(oldHistory)).Scan(&equal); err != nil || !equal {
				t.Fatalf("history changed after failure: equal=%v err=%v", equal, err)
			}
		})
	}
}
