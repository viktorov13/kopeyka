package bot

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"familyexpenses/internal/expenses"
)

type memoryLedger struct {
	items       map[int64]expenses.Expense
	nextID      int64
	lastFilter  expenses.Filter
	lastOwner   *int64
	createError error
}

func newMemoryLedger() *memoryLedger { return &memoryLedger{items: make(map[int64]expenses.Expense)} }
func (m *memoryLedger) Create(_ context.Context, in expenses.Input) (expenses.Expense, error) {
	if m.createError != nil {
		return expenses.Expense{}, m.createError
	}
	if err := expenses.ValidateInput(in); err != nil {
		return expenses.Expense{}, err
	}
	m.nextID++
	e := expenses.Expense{ID: m.nextID, UserID: in.UserID, Amount: in.Amount, Category: in.Category, SpentAt: in.SpentAt, Comment: in.Comment}
	m.items[e.ID] = e
	return e, nil
}
func (m *memoryLedger) List(_ context.Context, f expenses.Filter) (expenses.Page, error) {
	m.lastFilter = f
	p := expenses.Page{Items: []expenses.Expense{}}
	var sum int64
	for _, item := range m.items {
		if f.UserID != nil && item.UserID != *f.UserID {
			continue
		}
		if f.Category != "" && item.Category != f.Category {
			continue
		}
		if f.From != "" && item.SpentAt < f.From {
			continue
		}
		if f.To != "" && item.SpentAt > f.To {
			continue
		}
		if f.Search != "" && !strings.Contains(strings.ToLower(item.Comment), f.Search) && !strings.Contains(item.Category, f.Search) {
			continue
		}
		p.Items = append(p.Items, item)
		n, _ := expenses.ParseAmount(item.Amount)
		sum += n
	}
	sort.Slice(p.Items, func(i, j int) bool { return p.Items[i].ID > p.Items[j].ID })
	p.Total = len(p.Items)
	p.TotalAmount = expenses.FormatAmount(sum)
	if f.Limit > 0 && len(p.Items) > f.Limit {
		p.Items = p.Items[:f.Limit]
	}
	return p, nil
}
func (m *memoryLedger) Update(_ context.Context, id int64, owner *int64, patch expenses.Patch) (expenses.Expense, error) {
	m.lastOwner = owner
	e, ok := m.items[id]
	if !ok || owner != nil && e.UserID != *owner {
		return expenses.Expense{}, expenses.ErrNotFound
	}
	if patch.UserID != nil {
		e.UserID = *patch.UserID
	}
	if patch.Amount != nil {
		e.Amount = *patch.Amount
	}
	if patch.Category != nil {
		e.Category = *patch.Category
	}
	if patch.SpentAt != nil {
		e.SpentAt = *patch.SpentAt
	}
	if patch.Comment != nil {
		e.Comment = *patch.Comment
	}
	m.items[id] = e
	return e, nil
}
func (m *memoryLedger) Delete(_ context.Context, id int64, owner *int64) error {
	m.lastOwner = owner
	e, ok := m.items[id]
	if !ok || owner != nil && e.UserID != *owner {
		return expenses.ErrNotFound
	}
	delete(m.items, id)
	return nil
}
func fixedProcessor() (*Processor, *memoryLedger) {
	ledger := newMemoryLedger()
	location, _ := time.LoadLocation("Europe/Moscow")
	p := NewProcessor(ledger, location)
	p.now = func() time.Time { return time.Date(2026, 10, 16, 0, 30, 0, 0, location) }
	return p, ledger
}
func reply(t *testing.T, p *Processor, user int64, text string) string {
	t.Helper()
	result, err := p.Reply(context.Background(), user, text)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCreateAmountsAndDates(t *testing.T) {
	for _, tt := range []struct{ text, amount, category, date, comment string }{
		{"кофе350", "350.00", "еда", "2026-10-16", "кофе"},
		{"такси 1200 вчера", "1200.00", "транспорт", "2026-10-15", "такси"},
		{"кофе 350,50 позавчера", "350.50", "еда", "2026-10-14", "кофе"},
		{"лекарства 123.45 01.10.2026", "123.45", "здоровье", "2026-10-01", "лекарства"},
		{"аренда 50000 2026-09-30", "50000.00", "жильё", "2026-09-30", "аренда"},
		{"запиши билеты 700 ₽", "700.00", "другое", "2026-10-16", "билеты"},
	} {
		t.Run(tt.text, func(t *testing.T) {
			p, ledger := fixedProcessor()
			reply(t, p, 100, tt.text)
			item := ledger.items[1]
			if item.UserID != 100 || item.Amount != tt.amount || item.Category != tt.category || item.SpentAt != tt.date || item.Comment != tt.comment {
				t.Fatalf("unexpected expense: %#v", item)
			}
		})
	}
}

func TestInvalidInputDoesNotMutate(t *testing.T) {
	for _, text := range []string{"кофе -350", "кофе 0", "кофе 350.123", "кофе 350 31.02.2026", "кофе 350 и такси 200", "кофе 350 2026-10-15 2026-10-16", "кофе 350 0000-01-01", "кофе 350 вчера сегодня", "кофе 350 завтра"} {
		t.Run(text, func(t *testing.T) {
			p, ledger := fixedProcessor()
			reply(t, p, 1, text)
			if len(ledger.items) != 0 {
				t.Fatal("invalid input created expense")
			}
		})
	}
}

func TestAmbiguousMutationDoesNotChangeData(t *testing.T) {
	for _, text := range []string{
		"удали #1 и #2", "удали последние 5", "измени #1 и #2 сумма 500",
		"измени #1 сумма 1 200", "последнее было 350 или 450",
		"последнее было не 350, а 450 или 500",
	} {
		t.Run(text, func(t *testing.T) {
			p, ledger := fixedProcessor()
			reply(t, p, 42, "кофе 350")
			reply(t, p, 42, "такси 1200")
			answer := reply(t, p, 42, text)
			if len(ledger.items) != 2 || ledger.items[1].Amount != "350.00" || ledger.items[2].Amount != "1200.00" {
				t.Fatalf("ambiguous request mutated expenses: %q: %#v", answer, ledger.items)
			}
		})
	}
}

func TestAmbiguousPeriodDoesNotQueryLedger(t *testing.T) {
	for _, text := range []string{"покажи траты за 2 недели", "сколько потратил с 2026-10-01 по вчера", "покажи траты за неделю 2026-10-01"} {
		p, ledger := fixedProcessor()
		reply(t, p, 42, text)
		if ledger.lastFilter.UserID != nil {
			t.Errorf("ambiguous period queried ledger: %q, %+v", text, ledger.lastFilter)
		}
	}
}

func TestMissingAmountAndPerUserContext(t *testing.T) {
	p, ledger := fixedProcessor()
	if got := reply(t, p, 1, "кофе вчера"); !strings.Contains(got, "сумму") {
		t.Fatal(got)
	}
	reply(t, p, 2, "такси 800")
	reply(t, p, 1, "не знаю")
	if len(ledger.items) != 1 {
		t.Fatal("invalid clarification changed data")
	}
	reply(t, p, 1, "350,50")
	item := ledger.items[2]
	if item.UserID != 1 || item.Amount != "350.50" || item.SpentAt != "2026-10-15" || item.Comment != "кофе" {
		t.Fatalf("lost pending input: %#v", item)
	}
	if _, ok := p.pending[1]; ok {
		t.Fatal("pending was not consumed")
	}
}

func TestPendingCancelAndExpiration(t *testing.T) {
	p, ledger := fixedProcessor()
	reply(t, p, 1, "кофе")
	reply(t, p, 1, "отмена")
	if _, ok := p.pending[1]; ok {
		t.Fatal("cancel failed")
	}
	reply(t, p, 1, "такси")
	now := p.now()
	p.now = func() time.Time { return now.Add(11 * time.Minute) }
	reply(t, p, 1, "350")
	if ledger.items[1].Comment != "Расход" {
		t.Fatal("expired context was applied")
	}
}

func TestDeleteAmbiguityAndOwnership(t *testing.T) {
	p, ledger := fixedProcessor()
	reply(t, p, 1, "такси 100")
	reply(t, p, 1, "такси 200")
	reply(t, p, 2, "такси 300")
	got := reply(t, p, 1, "удали такси")
	if !strings.Contains(got, "несколько") || strings.Contains(got, "#3 ·") || len(ledger.items) != 3 {
		t.Fatalf("unexpected delete response: %s", got)
	}
	reply(t, p, 1, "#3")
	if len(ledger.items) != 3 {
		t.Fatal("selection outside own candidate set was deleted")
	}
	reply(t, p, 1, "#2")
	if len(ledger.items) != 2 || ledger.lastOwner == nil || *ledger.lastOwner != 1 {
		t.Fatal("delete not scoped")
	}
	_, err := p.Reply(context.Background(), 1, "удали #3")
	if !errors.Is(err, expenses.ErrNotFound) {
		t.Fatalf("foreign delete error: %v", err)
	}
	if _, ok := ledger.items[3]; !ok {
		t.Fatal("foreign expense deleted")
	}
}

func TestLatestCorrectionAndExplicitCRUD(t *testing.T) {
	p, ledger := fixedProcessor()
	reply(t, p, 1, "кофе 350")
	reply(t, p, 2, "такси 200")
	reply(t, p, 1, "последнее было не 350, а 450")
	if ledger.items[1].Amount != "450.00" || ledger.items[2].Amount != "200.00" || !ledger.lastFilter.Latest || ledger.lastFilter.UserID == nil || *ledger.lastFilter.UserID != 1 {
		t.Fatal("latest lookup is not scoped")
	}
	for _, text := range []string{"измени #1 сумма 500,25", "измени #1 категория покупки", "измени #1 дата 2026-10-01", "измени #1 комментарий Обед с другом"} {
		reply(t, p, 1, text)
	}
	item := ledger.items[1]
	if item.Amount != "500.25" || item.Category != "покупки" || item.SpentAt != "2026-10-01" || item.Comment != "Обед с другом" {
		t.Fatalf("bad edit: %#v", item)
	}
	_, err := p.Reply(context.Background(), 1, "измени #2 сумма 999")
	if !errors.Is(err, expenses.ErrNotFound) {
		t.Fatal("foreign edit allowed")
	}
	reply(t, p, 1, "измени #1 user_id 2")
	if ledger.items[1].UserID != 1 || ledger.items[1].Amount != "500.25" {
		t.Fatal("unsupported edit mutated expense")
	}
	reply(t, p, 1, "удали последнее")
	if _, ok := ledger.items[1]; ok {
		t.Fatal("latest delete failed")
	}
}

func TestFiltersAndSumsAreScoped(t *testing.T) {
	p, ledger := fixedProcessor()
	reply(t, p, 1, "кофе 350")
	reply(t, p, 1, "кофе 150 вчера")
	reply(t, p, 2, "кофе 900")
	got := reply(t, p, 1, "сколько потратил на еду в этом месяце")
	if !strings.Contains(got, "500.00") || ledger.lastFilter.From != "2026-10-01" || ledger.lastFilter.To != "2026-10-16" || ledger.lastFilter.Category != "еда" {
		t.Fatalf("unexpected sum: %s, %#v", got, ledger.lastFilter)
	}
	reply(t, p, 1, "покажи траты за неделю")
	if ledger.lastFilter.From != "2026-10-10" || ledger.lastFilter.To != "2026-10-16" || ledger.lastFilter.UserID == nil || *ledger.lastFilter.UserID != 1 {
		t.Fatalf("bad weekly filter: %#v", ledger.lastFilter)
	}
	reply(t, p, 1, "покажи траты с 2026-09-01 по 2026-09-30")
	if ledger.lastFilter.From != "2026-09-01" || ledger.lastFilter.To != "2026-09-30" {
		t.Fatal("bad explicit range")
	}
}

func TestPendingSurvivesFailedWrite(t *testing.T) {
	p, ledger := fixedProcessor()
	reply(t, p, 1, "кофе")
	ledger.createError = errors.New("database unavailable")
	if _, err := p.Reply(context.Background(), 1, "350"); err == nil {
		t.Fatal("expected error")
	}
	ledger.createError = nil
	reply(t, p, 1, "350")
	if ledger.items[1].Comment != "кофе" {
		t.Fatal("write failure consumed pending context")
	}
}
