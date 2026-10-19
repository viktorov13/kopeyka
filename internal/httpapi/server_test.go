package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"familyexpenses/internal/expenses"

	"github.com/lib/pq"
)

var httpSchemaCounter atomic.Uint64

func TestAPIAndStaticFrontendShareRouter(t *testing.T) {
	const html = "<!doctype html><title>Expenses</title><main>Расходы</main>"
	static := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte(html)}}
	s := &Server{Location: time.UTC}
	h := s.Handler(static)
	w := apiRequest(h, http.MethodGet, "/", "", nil)
	if w.Code != http.StatusOK || w.Body.String() != html || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("static frontend was not served: status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
	config := apiJSON[map[string]any](t, apiRequest(h, http.MethodGet, "/api/config", "", nil), http.StatusOK)
	if config["mode"] != "rules" || config["timezone"] != "UTC" {
		t.Fatalf("API route was shadowed by frontend: %#v", config)
	}
	apiError(t, apiRequest(h, http.MethodGet, "/api/unknown-route", "", nil), http.StatusNotFound)
}

func TestConfigReportsLLMModeWithoutProviderSettings(t *testing.T) {
	s := &Server{Location: time.UTC, BotEnabled: true, Mode: "llm"}
	config := apiJSON[map[string]any](t, apiRequest(s.Handler(nil), http.MethodGet, "/api/config", "", nil), http.StatusOK)
	if config["mode"] != "llm" || config["bot_enabled"] != true {
		t.Fatalf("incorrect bot state in config: %#v", config)
	}
	allowed := map[string]bool{"categories": true, "timezone": true, "today": true, "bot_enabled": true, "mode": true}
	for key := range config {
		if !allowed[key] {
			t.Fatalf("public configuration exposes an unexpected field: %s", key)
		}
	}
}

// Every fixture has a separate schema so the HTTP and store test packages may
// run against the same dedicated test database without touching each other's data.
func apiFixture(t *testing.T) http.Handler {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL integration test skipped")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("TEST_DATABASE_URL must be a PostgreSQL URL")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	schema := fmt.Sprintf("http_test_%d_%d", time.Now().UnixNano(), httpSchemaCounter.Add(1))
	quotedSchema := pq.QuoteIdentifier(schema)
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("create isolated test schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("clean up test schema: %v", err)
		}
	})
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := expenses.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate isolated test schema: %v", err)
	}
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: expenses.NewStore(db), DB: db, Location: loc, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return s.Handler(nil)
}

func apiRequest(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func apiJSON[T any](t *testing.T, w *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, status, w.Body.String())
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("expected JSON response, Content-Type = %q", w.Header().Get("Content-Type"))
	}
	var result T
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, w.Body.String())
	}
	return result
}

func apiError(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	body := apiJSON[map[string]any](t, w, status)
	message, ok := body["error"].(string)
	if !ok || strings.TrimSpace(message) == "" || len(body) != 1 {
		t.Fatalf("expected a single non-empty error field, got %#v", body)
	}
}

func apiCreate(t *testing.T, h http.Handler, owner int64, amount, category, date, comment string) expenses.Expense {
	t.Helper()
	body, err := json.Marshal(expenses.Input{UserID: owner, Amount: amount, Category: category, SpentAt: date, Comment: comment})
	if err != nil {
		t.Fatal(err)
	}
	w := apiRequest(h, http.MethodPost, "/api/expenses", string(body), nil)
	e := apiJSON[expenses.Expense](t, w, http.StatusCreated)
	if e.ID <= 0 || w.Header().Get("Location") != "/api/expenses/"+strconv.FormatInt(e.ID, 10) {
		t.Fatalf("invalid created expense ID or Location: %#v, %q", e, w.Header().Get("Location"))
	}
	return e
}

func TestAPIFullCRUD(t *testing.T) {
	h := apiFixture(t)
	created := apiCreate(t, h, 111, "350.10", "еда", "2026-10-16", "Кофе")
	if created.Amount != "350.10" || created.UserID != 111 || created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("incorrect creation result: %#v", created)
	}
	path := "/api/expenses/" + strconv.FormatInt(created.ID, 10)
	page := apiJSON[expenses.Page](t, apiRequest(h, http.MethodGet, "/api/expenses", "", nil), http.StatusOK)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != created.ID || page.TotalAmount != "350.10" {
		t.Fatalf("created record missing from list: %#v", page)
	}
	updated := apiJSON[expenses.Expense](t, apiRequest(h, http.MethodPatch, path, `{"amount":"450.30"}`, nil), http.StatusOK)
	if updated.Amount != "450.30" || updated.Category != created.Category || updated.Comment != created.Comment || updated.UserID != created.UserID || updated.SpentAt != created.SpentAt {
		t.Fatalf("partial update overwrote unrelated fields: %#v", updated)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) || updated.UpdatedAt.Before(created.UpdatedAt) {
		t.Fatalf("incorrect timestamps after patch: created=%#v updated=%#v", created, updated)
	}
	transferred := apiJSON[expenses.Expense](t, apiRequest(h, http.MethodPatch, path, `{"user_id":222,"category":"транспорт","spent_at":"2026-10-15","comment":""}`, nil), http.StatusOK)
	if transferred.UserID != 222 || transferred.Amount != "450.30" || transferred.Category != "транспорт" || transferred.SpentAt != "2026-10-15" || transferred.Comment != "" {
		t.Fatalf("admin edit and ownership transfer failed: %#v", transferred)
	}
	users := apiJSON[struct {
		Items []expenses.User `json:"items"`
	}](t, apiRequest(h, http.MethodGet, "/api/users", "", nil), http.StatusOK)
	if len(users.Items) != 1 || users.Items[0].UserID != 222 || users.Items[0].Count != 1 || users.Items[0].TotalAmount != "450.30" {
		t.Fatalf("incorrect users summary after ownership transfer: %#v", users)
	}
	w := apiRequest(h, http.MethodDelete, path, "", nil)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("delete should return empty 204; status=%d body=%s", w.Code, w.Body.String())
	}
	page = apiJSON[expenses.Page](t, apiRequest(h, http.MethodGet, "/api/expenses", "", nil), http.StatusOK)
	if page.Total != 0 || page.TotalAmount != "0.00" || page.Items == nil || len(page.Items) != 0 || page.CategoryTotals == nil || len(page.CategoryTotals) != 0 {
		t.Fatalf("deleted record or non-array empty collections in list: %#v", page)
	}
	apiError(t, apiRequest(h, http.MethodPatch, path, `{"amount":"1"}`, nil), http.StatusNotFound)
	apiError(t, apiRequest(h, http.MethodDelete, path, "", nil), http.StatusNotFound)
}

func TestAPICombinedFiltersAndTotalsAcrossPages(t *testing.T) {
	h := apiFixture(t)
	apiCreate(t, h, 11, "10.10", "еда", "2026-10-01", "кофе утром")
	middle := apiCreate(t, h, 11, "20.20", "еда", "2026-10-16", "кофе днём")
	apiCreate(t, h, 11, "40.40", "еда", "2026-10-31", "кофе вечером")
	apiCreate(t, h, 11, "50.50", "еда", "2026-09-30", "кофе до периода")
	apiCreate(t, h, 11, "80.80", "еда", "2026-11-01", "кофе после периода")
	apiCreate(t, h, 11, "30.30", "транспорт", "2026-10-16", "кофе доставка")
	apiCreate(t, h, 22, "60.60", "еда", "2026-10-16", "кофе другого владельца")
	apiCreate(t, h, 11, "70.70", "еда", "2026-10-16", "чай")
	q := url.Values{"user_id": {"11"}, "from": {"2026-10-01"}, "to": {"2026-10-31"}, "category": {"еда"}, "search": {"КОФЕ"}, "limit": {"1"}, "offset": {"1"}}
	page := apiJSON[expenses.Page](t, apiRequest(h, http.MethodGet, "/api/expenses?"+q.Encode(), "", nil), http.StatusOK)
	if page.Total != 3 || page.TotalAmount != "70.70" || len(page.Items) != 1 || page.Items[0].ID != middle.ID {
		t.Fatalf("incorrect combined filters, inclusive dates or pagination: %#v", page)
	}
	if len(page.CategoryTotals) != 1 || page.CategoryTotals[0].Category != "еда" || page.CategoryTotals[0].Count != 3 || page.CategoryTotals[0].Amount != "70.70" {
		t.Fatalf("category totals should cover all filtered rows: %#v", page.CategoryTotals)
	}
	q.Set("offset", "99")
	empty := apiJSON[expenses.Page](t, apiRequest(h, http.MethodGet, "/api/expenses?"+q.Encode(), "", nil), http.StatusOK)
	if len(empty.Items) != 0 || empty.Items == nil || empty.Total != 3 || empty.TotalAmount != "70.70" {
		t.Fatalf("out-of-range page lost aggregate data: %#v", empty)
	}
	q.Del("category")
	q.Del("search")
	q.Set("offset", "0")
	allCategories := apiJSON[expenses.Page](t, apiRequest(h, http.MethodGet, "/api/expenses?"+q.Encode(), "", nil), http.StatusOK)
	if allCategories.Total != 5 || allCategories.TotalAmount != "171.70" || len(allCategories.Items) != 1 || len(allCategories.CategoryTotals) != 2 {
		t.Fatalf("incorrect aggregates over multiple categories: %#v", allCategories)
	}
	totals := map[string]string{}
	for _, category := range allCategories.CategoryTotals {
		totals[category.Category] = category.Amount
	}
	if totals["еда"] != "141.40" || totals["транспорт"] != "30.30" {
		t.Fatalf("incorrect category amounts: %#v", totals)
	}
}

func TestAPIRejectsInvalidRequestsWithoutWriting(t *testing.T) {
	h := apiFixture(t)
	valid := `{"user_id":111,"amount":"350.00","category":"еда","spent_at":"2026-10-16","comment":"Кофе"}`
	cases := []struct {
		name, body string
		headers    map[string]string
	}{
		{"missing body", "", nil},
		{"wrong content type", valid, map[string]string{"Content-Type": "text/plain"}},
		{"malformed JSON", `{"user_id":`, nil},
		{"multiple JSON values", valid + valid, nil},
		{"unknown field", strings.Replace(valid, `"comment":"Кофе"`, `"unexpected":true`, 1), nil},
		{"numeric amount", strings.Replace(valid, `"350.00"`, `350.00`, 1), nil},
		{"string owner", strings.Replace(valid, `111`, `"111"`, 1), nil},
		{"zero amount", strings.Replace(valid, `"350.00"`, `"0"`, 1), nil},
		{"fractional kopecks", strings.Replace(valid, `"350.00"`, `"350.001"`, 1), nil},
		{"negative owner", strings.Replace(valid, `111`, `-111`, 1), nil},
		{"unknown category", strings.Replace(valid, `еда`, `неизвестная`, 1), nil},
		{"impossible date", strings.Replace(valid, `2026-10-16`, `2026-02-30`, 1), nil},
		{"array instead of object", "[" + valid + "]", nil},
		{"oversized body", strings.Replace(valid, `Кофе`, strings.Repeat("x", 17000), 1), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiError(t, apiRequest(h, http.MethodPost, "/api/expenses", tc.body, tc.headers), http.StatusBadRequest)
		})
	}
	page := apiJSON[expenses.Page](t, apiRequest(h, http.MethodGet, "/api/expenses", "", nil), http.StatusOK)
	if page.Total != 0 {
		t.Fatalf("invalid requests wrote records: %#v", page)
	}
}

func TestAPIPatchValidationAndInvalidFilters(t *testing.T) {
	h := apiFixture(t)
	created := apiCreate(t, h, 111, "350", "еда", "2026-10-16", "Кофе")
	path := "/api/expenses/" + strconv.FormatInt(created.ID, 10)
	for _, body := range []string{`{}`, `{"amount":350}`, `{"amount":"-1"}`, `{"user_id":0}`, `{"category":"нет"}`, `{"spent_at":"2026-02-30"}`, `{"bogus":true}`} {
		t.Run("patch "+body, func(t *testing.T) {
			apiError(t, apiRequest(h, http.MethodPatch, path, body, nil), http.StatusBadRequest)
		})
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=x", "offset=-1", "offset=x", "user_id=0", "user_id=wrong", "from=wrong", "from=2026-10-16&to=2026-10-01", "category=unknown"} {
		t.Run("filter "+query, func(t *testing.T) {
			apiError(t, apiRequest(h, http.MethodGet, "/api/expenses?"+query, "", nil), http.StatusBadRequest)
		})
	}
	apiError(t, apiRequest(h, http.MethodPatch, "/api/expenses/999999999", `{"amount":"1"}`, nil), http.StatusNotFound)
	apiError(t, apiRequest(h, http.MethodPatch, "/api/expenses/not-an-id", `{"amount":"1"}`, nil), http.StatusBadRequest)
	apiError(t, apiRequest(h, http.MethodDelete, "/api/expenses/0", "", nil), http.StatusBadRequest)
	page := apiJSON[expenses.Page](t, apiRequest(h, http.MethodGet, "/api/expenses", "", nil), http.StatusOK)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Amount != "350.00" || page.Items[0].UserID != 111 {
		t.Fatalf("invalid patch changed the original record: %#v", page)
	}
}

func TestAPICrossSiteMutationsAreRejected(t *testing.T) {
	h := apiFixture(t)
	created := apiCreate(t, h, 111, "350", "еда", "2026-10-16", "Кофе")
	path := "/api/expenses/" + strconv.FormatInt(created.ID, 10)
	headers := map[string]string{"Sec-Fetch-Site": "cross-site"}
	apiError(t, apiRequest(h, http.MethodPost, "/api/expenses", `{"user_id":111,"amount":"50","category":"еда","spent_at":"2026-10-16"}`, headers), http.StatusForbidden)
	apiError(t, apiRequest(h, http.MethodPatch, path, `{"amount":"1"}`, headers), http.StatusForbidden)
	apiError(t, apiRequest(h, http.MethodDelete, path, "", headers), http.StatusForbidden)
	page := apiJSON[expenses.Page](t, apiRequest(h, http.MethodGet, "/api/expenses", "", nil), http.StatusOK)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != created.ID || page.TotalAmount != "350.00" {
		t.Fatalf("cross-site mutation affected data: %#v", page)
	}
	updated := apiJSON[expenses.Expense](t, apiRequest(h, http.MethodPatch, path, `{"amount":"400"}`, map[string]string{"Sec-Fetch-Site": "same-origin"}), http.StatusOK)
	if updated.Amount != "400.00" {
		t.Fatalf("same-origin edit should be accepted: %#v", updated)
	}
}

func TestAPIConfigurationAndHealth(t *testing.T) {
	h := apiFixture(t)
	health := apiJSON[map[string]string](t, apiRequest(h, http.MethodGet, "/healthz", "", nil), http.StatusOK)
	if health["status"] != "ok" {
		t.Fatalf("database health is not reported as healthy: %#v", health)
	}
	w := apiRequest(h, http.MethodGet, "/api/config", "", nil)
	config := apiJSON[struct {
		Categories []string `json:"categories"`
		Timezone   string   `json:"timezone"`
		Today      string   `json:"today"`
		BotEnabled bool     `json:"bot_enabled"`
		Mode       string   `json:"mode"`
	}](t, w, http.StatusOK)
	if len(config.Categories) != 7 || config.Timezone != "Europe/Moscow" || config.BotEnabled || config.Mode != "rules" {
		t.Fatalf("unexpected public config: %#v", config)
	}
	if _, err := time.Parse(time.DateOnly, config.Today); err != nil {
		t.Fatalf("config date is invalid: %q", config.Today)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("API response headers are missing: %#v", w.Header())
	}
}
