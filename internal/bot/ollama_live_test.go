package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode"

	"familyexpenses/internal/agent"
	"familyexpenses/internal/expenses"
)

// This opt-in acceptance test calls a real local model against an isolated
// PostgreSQL schema. Regular unit/integration runs never download or invoke it.
func TestLiveOllamaExpenses(t *testing.T) {
	baseURL := os.Getenv("TEST_OLLAMA_URL")
	if baseURL == "" {
		t.Skip("TEST_OLLAMA_URL is not set; opt-in live local-model acceptance test")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Fatal("TEST_DATABASE_URL must point to a dedicated test database")
	}
	if os.Getenv("TEST_OLLAMA_TRACE") == "1" {
		provider := strings.TrimRight(baseURL, "/")
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, provider+r.URL.Path, r.Body)
			if err != nil {
				http.Error(w, "invalid local request", 500)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			started := time.Now()
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Logf("local HTTP request ended after %s", time.Since(started).Round(time.Millisecond))
				http.Error(w, "local request failed", 502)
				return
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(io.LimitReader(response.Body, 256<<10))
			if err != nil {
				http.Error(w, "local response failed", 502)
				return
			}
			var metadata struct {
				Choices []struct {
					FinishReason string `json:"finish_reason"`
					Message      struct {
						ToolCalls []struct {
							Function struct{ Name string } `json:"function"`
						} `json:"tool_calls"`
					} `json:"message"`
				} `json:"choices"`
				Usage struct {
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(raw, &metadata) == nil && len(metadata.Choices) == 1 {
				names := []string{}
				for _, call := range metadata.Choices[0].Message.ToolCalls {
					names = append(names, call.Function.Name)
				}
				t.Logf("local HTTP: %s; finish=%s; tools=%v; generated_tokens=%d", time.Since(started).Round(time.Millisecond), metadata.Choices[0].FinishReason, names, metadata.Usage.CompletionTokens)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(response.StatusCode)
			w.Write(raw)
		}))
		defer proxy.Close()
		baseURL = proxy.URL
	}
	model := os.Getenv("TEST_OLLAMA_MODEL")
	if model == "" {
		model = "qwen3:4b-instruct-2507-q4_K_M"
	}
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := agent.New(agent.Config{BaseURL: baseURL, Model: model, Timeout: 60 * time.Second, MaxSteps: 6, ReasoningEffort: "none"}, loc)
	if err != nil {
		t.Fatal(err)
	}
	db := isolatedDatabase(t)
	store := expenses.NewStore(db)
	traced := assistantFunc(func(ctx context.Context, ledger expenses.Ledger, user int64, text string, history json.RawMessage) (string, json.RawMessage, error) {
		reply, next, err := assistant.Respond(ctx, ledger, user, text, history)
		if err != nil {
			t.Logf("local model diagnostic: %v", err)
		}
		return reply, next, err
	})
	p := NewLLMProcessor(store, loc, traced)
	ctx := context.Background()
	today := time.Now().In(loc)
	var updateID int64 = 50000
	userID := int64(42)
	send := func(text string) string {
		t.Helper()
		updateID++
		started := time.Now()
		reply, err := p.processUpdate(ctx, db, messageUpdate(updateID, userID, text))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s → %s (%s)", text, reply, time.Since(started).Round(time.Millisecond))
		if reply == "" || reply == assistantFailure {
			t.Fatalf("local model could not process %q: %s", text, reply)
		}
		return reply
	}
	page := func(owner int64) expenses.Page {
		t.Helper()
		result, err := store.List(ctx, expenses.Filter{UserID: &owner, Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	find := func(category string) expenses.Expense {
		t.Helper()
		for _, item := range page(42).Items {
			if item.Category == category {
				return item
			}
		}
		t.Fatalf("no %s expense", category)
		return expenses.Expense{}
	}

	send("кофе 350")
	coffee := find("еда")
	if got := page(42); got.Total != 1 || coffee.Amount != "350.00" || coffee.SpentAt != today.Format(time.DateOnly) {
		t.Fatalf("coffee parsing: %+v", got)
	}
	send("последнее было не 350, а 450")
	if got := find("еда"); got.ID != coffee.ID || got.Amount != "450.00" {
		t.Fatalf("latest correction: %+v", got)
	}
	send("такси 1200 вчера")
	taxi := find("транспорт")
	if got := page(42); got.Total != 2 || taxi.Amount != "1200.00" || taxi.SpentAt != today.AddDate(0, 0, -1).Format(time.DateOnly) {
		t.Fatalf("taxi/date parsing: %+v", got)
	}
	if reply := send("сколько потратил на еду в этом месяце"); !strings.Contains(reply, "450") {
		t.Fatalf("incorrect food total: %s", reply)
	}
	if reply := send("покажи траты за неделю"); !strings.Contains(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, reply), "1200") {
		t.Fatalf("weekly list did not include taxi: %s", reply)
	}
	// A fresh processor must continue using the persisted dialogue.
	p = NewLLMProcessor(store, loc, traced)
	send("удали такси")
	if got := page(42); got.Total != 1 || got.Items[0].ID != coffee.ID {
		t.Fatalf("delete by description: %+v", got)
	}
	userID = 99
	send(fmt.Sprintf("измени #%d сумму на 999", coffee.ID))
	if got := find("еда"); got.Amount != "450.00" || page(99).Total != 0 {
		t.Fatalf("foreign sender affected expense: %+v", got)
	}
	userID = 77
	send("кофе")
	if got := page(77); got.Total != 0 {
		t.Fatalf("missing amount created expense: %+v", got)
	}
	p = NewLLMProcessor(store, loc, traced)
	send("250")
	if got := page(77); got.Total != 1 || got.Items[0].Amount != "250.00" || got.Items[0].Category != "еда" {
		t.Fatalf("clarification after restart: %+v", got)
	}
}

// Check semantic category selection with the real model, not a scripted tool
// response. The test writes only into its temporary PostgreSQL schema.
func TestLiveOllamaExpenseCategories(t *testing.T) {
	baseURL := os.Getenv("TEST_OLLAMA_URL")
	if baseURL == "" {
		t.Skip("TEST_OLLAMA_URL is not set; opt-in live local-model acceptance test")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Fatal("TEST_DATABASE_URL must point to a dedicated test database")
	}
	model := os.Getenv("TEST_OLLAMA_MODEL")
	if model == "" {
		model = "qwen3:4b-instruct-2507-q4_K_M"
	}
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := agent.New(agent.Config{BaseURL: baseURL, Model: model, Timeout: 60 * time.Second, MaxSteps: 6, ReasoningEffort: "none"}, loc)
	if err != nil {
		t.Fatal(err)
	}
	db := isolatedDatabase(t)
	store := expenses.NewStore(db)
	p := NewLLMProcessor(store, loc, assistant)
	ctx := context.Background()
	for i, tc := range []struct {
		text, category string
	}{
		{"энергетик burn сочная энергия 150", "еда"},
		{"энергетик Monster 200", "еда"},
		{"баночка тонизирующего напитка со вкусом манго 180", "еда"},
		{"корм для кота 500", "покупки"},
		{"корм для собаки 800", "покупки"},
		{"сухой рацион для домашнего питомца 900", "покупки"},
		{"наполнитель для кошачьего лотка 600", "покупки"},
		{"приём у ветеринара для кота 1500", "здоровье"},
		{"лекарства для кота 700", "здоровье"},
		{"билет на спектакль 1200", "развлечения"},
		{"счёт за электричество 850", "жильё"},
		{"проезд на трамвае 60", "транспорт"},
		{"пожертвование приюту 500", "другое"},
		{"энергетик 150, запиши в категорию покупки", "покупки"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			// Separate owners keep earlier descriptions/categories from becoming
			// conversation hints for the next independent purchase.
			owner := int64(1000 + i)
			started := time.Now()
			reply, err := p.processUpdate(ctx, db, messageUpdate(int64(60000+i), owner, tc.text))
			if err != nil {
				t.Fatal(err)
			}
			if reply == "" || reply == assistantFailure {
				t.Fatalf("category request failed: %s", reply)
			}
			page, err := store.List(ctx, expenses.Filter{UserID: &owner, Limit: 20})
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != 1 || page.Items[0].Category != tc.category {
				t.Fatalf("expected one %s expense; got %+v; reply=%s", tc.category, page, reply)
			}
			t.Logf("%s → %s (%s)", tc.text, page.Items[0].Category, time.Since(started).Round(time.Millisecond))
		})
	}
}
