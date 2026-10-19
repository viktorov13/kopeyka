// Package bot implements the Telegram adapter with configurable LLM and
// deterministic expense processors.
package bot

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"familyexpenses/internal/expenses"
)

const help = `Я записываю расходы в рублях. Примеры:
• кофе 350
• такси 1200 вчера
• продукты 1500 16.10.2026
• покажи траты за неделю
• сколько потратил на еду в этом месяце
• последнее было не 350, а 450
• измени #12 сумма 450
• измени #12 категория еда
• измени #12 дата 2026-10-15
• измени #12 комментарий обед
• удали такси
• удали #12

Понимаю ограниченный набор фраз, без ИИ. Категории: еда, транспорт, жильё, развлечения, здоровье, покупки, другое. Даты: сегодня, вчера, позавчера, ДД.ММ.ГГГГ, ГГГГ-ММ-ДД. Для списка и суммы: за неделю (7 дней), в этом месяце, за всё время, с 2026-10-01 по 2026-10-15. По умолчанию показываю все расходы. «Отмена» отменяет уточнение. Изменение по номеру доступно только для ваших расходов.`

var (
	isoDateRE    = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	dotDateRE    = regexp.MustCompile(`\b\d{2}\.\d{2}\.\d{4}\b`)
	amountRE     = regexp.MustCompile(`[-+]?\d+(?:[.,]\d+)?`)
	idRE         = regexp.MustCompile(`#(\d+)`)
	correctionRE = regexp.MustCompile(`не\s+[-+]?\d+(?:[.,]\d+)?\s*,?\s*а\s+([-+]?\d+(?:[.,]\d+)?)\s*(?:рублей|рубля|рубль|руб\.?|₽)?[.!]?$`)
)

type pending struct {
	kind    string
	input   expenses.Input
	ids     map[int64]bool
	expires time.Time
}

type Processor struct {
	ledger    expenses.Ledger
	location  *time.Location
	now       func() time.Time
	mu        sync.Mutex
	pending   map[int64]pending
	assistant Assistant
}

func NewProcessor(ledger expenses.Ledger, location *time.Location) *Processor {
	if location == nil {
		location = time.UTC
	}
	return &Processor{ledger: ledger, location: location, now: time.Now, pending: make(map[int64]pending)}
}

func (p *Processor) Reply(ctx context.Context, userID int64, text string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.assistant != nil {
		return "", fmt.Errorf("LLM replies require transactional Telegram processing")
	}
	return p.reply(ctx, p.ledger, userID, text)
}

func (p *Processor) reply(ctx context.Context, ledger expenses.Ledger, userID int64, text string) (string, error) {
	if userID <= 0 {
		return "", fmt.Errorf("invalid Telegram sender")
	}
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > 2000 {
		return "Сообщение слишком длинное. Сократите его до 2000 символов.", nil
	}
	lower := strings.ToLower(text)
	if lower == "отмена" || lower == "/cancel" {
		delete(p.pending, userID)
		return "Уточнение отменено.", nil
	}
	if lower == "помощь" || lower == "/start" || lower == "/help" || lower == "" {
		return help, nil
	}
	if state, ok := p.pending[userID]; ok {
		if p.now().After(state.expires) {
			delete(p.pending, userID)
		} else if !isNewRequest(lower) {
			return p.continuePending(ctx, ledger, userID, text, state)
		} else {
			delete(p.pending, userID)
		}
	}
	if strings.HasPrefix(lower, "удали") || strings.HasPrefix(lower, "удалить") {
		return p.remove(ctx, ledger, userID, lower)
	}
	if strings.HasPrefix(lower, "измени") || strings.HasPrefix(lower, "исправь") || (strings.Contains(lower, "последн") && (strings.Contains(lower, "было") || strings.Contains(lower, "сумм"))) {
		return p.update(ctx, ledger, userID, text)
	}
	if strings.Contains(lower, "сколько") || strings.HasPrefix(lower, "сумма") || strings.HasPrefix(lower, "итого") {
		filter, err := p.filter(userID, lower)
		if err != nil {
			return err.Error(), nil
		}
		page, err := ledger.List(ctx, filter)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Всего: %s ₽. Расходов: %d.%s", page.TotalAmount, page.Total, periodLabel(filter)), nil
	}
	if strings.HasPrefix(lower, "покажи") || strings.HasPrefix(lower, "список") || strings.HasPrefix(lower, "траты") || strings.HasPrefix(lower, "расходы") {
		filter, err := p.filter(userID, lower)
		if err != nil {
			return err.Error(), nil
		}
		page, err := ledger.List(ctx, filter)
		if err != nil {
			return "", err
		}
		if page.Total == 0 {
			return "За выбранный период расходов не найдено." + periodLabel(filter), nil
		}
		return formatPage(page) + periodLabel(filter), nil
	}
	return p.create(ctx, ledger, userID, text)
}

func isNewRequest(text string) bool {
	for _, prefix := range []string{"покажи", "список", "сколько", "удали", "измени", "исправь", "сумма", "итого"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func (p *Processor) create(ctx context.Context, ledger expenses.Ledger, userID int64, text string) (string, error) {
	date, rest, err := p.extractDate(text)
	if err != nil {
		return err.Error(), nil
	}
	amounts := amountRE.FindAllString(rest, -1)
	if len(amounts) > 1 {
		return "Укажите одну трату и одну сумму, например: «кофе 350 вчера».", nil
	}
	comment := strings.TrimSpace(rest)
	amount := ""
	if len(amounts) == 1 {
		amount, err = normalizedAmount(amounts[0])
		if err != nil {
			return err.Error(), nil
		}
		comment = strings.TrimSpace(amountRE.ReplaceAllString(rest, ""))
	}
	comment = cleanComment(comment)
	input := expenses.Input{UserID: userID, Amount: amount, Category: detectCategory(strings.ToLower(comment)), SpentAt: date, Comment: comment}
	if input.Category == "" {
		input.Category = "другое"
	}
	if amount == "" {
		p.pending[userID] = pending{kind: "amount", input: input, expires: p.now().Add(10 * time.Minute)}
		return "Какую сумму записать в рублях? Например: 350 или 350,50. Для отмены напишите «отмена».", nil
	}
	item, err := ledger.Create(ctx, input)
	if err != nil {
		return "", err
	}
	return "Записано: " + formatExpense(item), nil
}

func (p *Processor) continuePending(ctx context.Context, ledger expenses.Ledger, userID int64, text string, state pending) (string, error) {
	if state.kind == "amount" {
		amount, err := normalizedAmount(strings.TrimSpace(text))
		if err != nil {
			return "Нужна только сумма в рублях, например 350,50. Для отмены напишите «отмена».", nil
		}
		state.input.Amount = amount
		state.input.UserID = userID
		item, err := ledger.Create(ctx, state.input)
		if err != nil {
			return "", err
		}
		delete(p.pending, userID)
		return "Записано: " + formatExpense(item), nil
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(text), "#"), 10, 64)
	if err != nil || !state.ids[id] {
		return "Укажите номер из списка, например #12, или напишите «отмена».", nil
	}
	if err := ledger.Delete(ctx, id, &userID); err != nil {
		return "", err
	}
	delete(p.pending, userID)
	return fmt.Sprintf("Трата #%d удалена.", id), nil
}

func (p *Processor) remove(ctx context.Context, ledger expenses.Ledger, userID int64, text string) (string, error) {
	if len(idRE.FindAllString(text, -1)) > 1 {
		return "Укажите одну трату для удаления, например: «удали #12».", nil
	}
	if match := idRE.FindStringSubmatch(text); len(match) > 0 {
		id, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return "Некорректный номер траты.", nil
		}
		if err := ledger.Delete(ctx, id, &userID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Трата #%d удалена.", id), nil
	}
	filter := expenses.Filter{UserID: &userID, Limit: 10}
	if strings.Contains(text, "последн") {
		if amountRE.MatchString(text) || strings.Contains(text, "последние") || strings.Contains(text, "последних") {
			return "Можно удалить одну трату: «удали последнее» или «удали #12».", nil
		}
		filter.Latest = true
		filter.Limit = 1
	} else {
		query := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "удалить"), "удали"))
		query = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(query, "трату"), "расход"))
		if query == "" || query == "всё" || query == "все" {
			return "Укажите номер или описание одной траты: «удали #12» или «удали такси».", nil
		}
		filter.Search = query
	}
	page, err := ledger.List(ctx, filter)
	if err != nil {
		return "", err
	}
	if len(page.Items) == 0 {
		return "Подходящих расходов не найдено.", nil
	}
	if page.Total == 1 || filter.Latest {
		id := page.Items[0].ID
		if err := ledger.Delete(ctx, id, &userID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Трата #%d удалена.", id), nil
	}
	ids := make(map[int64]bool, len(page.Items))
	for _, item := range page.Items {
		ids[item.ID] = true
	}
	p.pending[userID] = pending{kind: "delete", ids: ids, expires: p.now().Add(10 * time.Minute)}
	return "Нашёл несколько трат. Какую удалить? Ответьте номером с # или напишите «отмена».\n" + formatPage(page), nil
}

func (p *Processor) update(ctx context.Context, ledger expenses.Ledger, userID int64, text string) (string, error) {
	lower := strings.ToLower(text)
	if len(idRE.FindAllString(lower, -1)) > 1 {
		return "Укажите одну трату для изменения, например: «измени #12 сумма 450».", nil
	}
	var id int64
	if match := idRE.FindStringSubmatch(lower); len(match) > 0 {
		parsed, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return "Некорректный номер траты.", nil
		}
		id = parsed
		text = idRE.ReplaceAllString(text, "")
		lower = strings.ToLower(text)
	} else if strings.Contains(lower, "последн") {
		page, err := ledger.List(ctx, expenses.Filter{UserID: &userID, Limit: 1, Latest: true})
		if err != nil {
			return "", err
		}
		if len(page.Items) == 0 {
			return "У вас пока нет расходов.", nil
		}
		id = page.Items[0].ID
	} else {
		return "Укажите номер: «измени #12 сумма 450» или «последнее было не 350, а 450».", nil
	}
	var patch expenses.Patch
	if index := strings.Index(lower, "категория "); index >= 0 {
		value := strings.TrimSpace(lower[index+len("категория "):])
		if value == "жилье" {
			value = "жильё"
		}
		patch.Category = &value
	} else if index := strings.Index(lower, "дата "); index >= 0 {
		date, _, err := p.extractDate(strings.TrimSpace(lower[index+len("дата "):]))
		if err != nil {
			return err.Error(), nil
		}
		if !hasDate(lower[index+len("дата "):]) {
			return "Укажите дату: «измени #12 дата 2026-10-15».", nil
		}
		patch.SpentAt = &date
	} else if index := strings.Index(lower, "комментарий "); index >= 0 {
		value := strings.TrimSpace(text[index+len("комментарий "):])
		patch.Comment = &value
	} else {
		if !strings.Contains(lower, "сумм") && !strings.Contains(lower, "было") {
			return "Можно изменить сумму, категорию, дату или комментарий. Например: «измени #12 сумма 450».", nil
		}
		amounts := amountRE.FindAllString(lower, -1)
		if len(amounts) == 0 {
			return "Укажите новое значение: сумма, категория, дата или комментарий. Например: «измени #12 сумма 450».", nil
		}
		amount := amounts[0]
		if len(amounts) > 1 {
			correction := correctionRE.FindStringSubmatch(lower)
			if len(amounts) != 2 || len(correction) != 2 {
				return "Не удалось однозначно определить сумму. Напишите без разделителей тысяч: «измени #12 сумма 1200».", nil
			}
			amount = correction[1]
		}
		value, err := normalizedAmount(amount)
		if err != nil {
			return err.Error(), nil
		}
		patch.Amount = &value
	}
	item, err := ledger.Update(ctx, id, &userID, patch)
	if err != nil {
		return "", err
	}
	return "Обновлено: " + formatExpense(item), nil
}

func (p *Processor) extractDate(text string) (string, string, error) {
	now := p.now().In(p.location)
	lower := strings.ToLower(text)
	if unsupportedPeriod(lower) {
		return "", "", fmt.Errorf("Не понял дату. Укажите сегодня, вчера, позавчера или точную дату, например 2026-10-15.")
	}
	iso := isoDateRE.FindAllString(text, -1)
	dot := dotDateRE.FindAllString(text, -1)
	relativeCount := 0
	remainder := lower
	for _, word := range []string{"позавчера", "вчера", "сегодня"} {
		relativeCount += strings.Count(remainder, word)
		remainder = strings.ReplaceAll(remainder, word, "")
	}
	if len(iso)+len(dot)+relativeCount > 1 {
		return "", "", fmt.Errorf("Для одной траты укажите одну дату.")
	}
	if len(iso) == 1 {
		date, err := time.Parse("2006-01-02", iso[0])
		if err != nil || date.Year() < 1 {
			return "", "", fmt.Errorf("Некорректная дата. Пример: 2026-10-15.")
		}
		return date.Format("2006-01-02"), strings.Replace(text, iso[0], "", 1), nil
	}
	if len(dot) == 1 {
		date, err := time.Parse("02.01.2006", dot[0])
		if err != nil || date.Year() < 1 {
			return "", "", fmt.Errorf("Некорректная дата. Пример: 15.10.2026.")
		}
		return date.Format("2006-01-02"), strings.Replace(text, dot[0], "", 1), nil
	}
	for _, item := range []struct {
		word string
		days int
	}{{"позавчера", -2}, {"вчера", -1}, {"сегодня", 0}} {
		if index := strings.Index(lower, item.word); index >= 0 {
			return now.AddDate(0, 0, item.days).Format("2006-01-02"), text[:index] + text[index+len(item.word):], nil
		}
	}
	return now.Format("2006-01-02"), text, nil
}

func hasDate(text string) bool {
	return isoDateRE.MatchString(text) || dotDateRE.MatchString(text) || strings.Contains(text, "сегодня") || strings.Contains(text, "вчера")
}

func unsupportedPeriod(text string) bool {
	for _, word := range []string{"завтра", "прошл", "следующ", "дней назад", "дня назад", "за год", "в году", "в этом году", "год назад"} {
		if strings.Contains(text, word) {
			return true
		}
	}
	return false
}

func (p *Processor) filter(userID int64, text string) (expenses.Filter, error) {
	filter := expenses.Filter{UserID: &userID, Limit: 10, Category: detectCategory(text)}
	if unsupportedPeriod(text) {
		return filter, fmt.Errorf("Не понял период. Укажите «за неделю», «в этом месяце» или точные даты: «с 2026-10-01 по 2026-10-15».")
	}
	now := p.now().In(p.location)
	today := now.Format("2006-01-02")
	iso, dot := isoDateRE.FindAllString(text, -1), dotDateRE.FindAllString(text, -1)
	withoutDates := dotDateRE.ReplaceAllString(isoDateRE.ReplaceAllString(text, ""), "")
	if amountRE.MatchString(withoutDates) {
		return filter, fmt.Errorf("Укажите период словами «за неделю», «в этом месяце» или двумя точными датами.")
	}
	if len(iso)+len(dot) > 0 && (hasDate(withoutDates) || strings.Contains(withoutDates, "недел") || strings.Contains(withoutDates, "месяц")) {
		return filter, fmt.Errorf("Укажите один период: точные даты или относительный период.")
	}
	if len(iso)+len(dot) > 2 || (len(iso) > 0 && len(dot) > 0) {
		return filter, fmt.Errorf("Укажите период двумя датами в одном формате.")
	}
	dates, layout := iso, "2006-01-02"
	if len(dot) > 0 {
		dates, layout = dot, "02.01.2006"
	}
	if len(dates) > 0 {
		for _, date := range dates {
			if parsed, err := time.Parse(layout, date); err != nil || parsed.Year() < 1 {
				return filter, fmt.Errorf("Некорректная дата: %s.", date)
			}
		}
		from, _ := time.Parse(layout, dates[0])
		filter.From = from.Format("2006-01-02")
		filter.To = filter.From
		if len(dates) == 2 {
			to, _ := time.Parse(layout, dates[1])
			filter.To = to.Format("2006-01-02")
		}
		if filter.From > filter.To {
			return filter, fmt.Errorf("Начало периода должно быть не позже окончания.")
		}
	} else if strings.Contains(text, "недел") {
		filter.From, filter.To = now.AddDate(0, 0, -6).Format("2006-01-02"), today
	} else if strings.Contains(text, "месяц") {
		filter.From, filter.To = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, p.location).Format("2006-01-02"), today
	} else if hasDate(text) {
		date, _, err := p.extractDate(text)
		if err != nil {
			return filter, err
		}
		filter.From, filter.To = date, date
	}
	return filter, nil
}

func normalizedAmount(value string) (string, error) {
	cents, err := expenses.ParseAmount(value)
	if err != nil {
		return "", fmt.Errorf("Укажите положительную сумму с точностью до копеек, например 350,50.")
	}
	return expenses.FormatAmount(cents), nil
}

func detectCategory(text string) string {
	groups := []struct {
		category string
		words    []string
	}{
		{"еда", []string{"еда", "еду", "еды", "питан", "кофе", "чай", "продукт", "обед", "завтрак", "ужин", "ресторан", "кафе", "пицц", "хлеб", "молок"}},
		{"транспорт", []string{"транспорт", "такси", "метро", "автобус", "бензин", "проезд", "парковк", "поезд", "каршер"}},
		{"жильё", []string{"жиль", "аренд", "квартир", "коммунал", "ипотек", "интернет"}},
		{"развлечения", []string{"развлеч", "кино", "театр", "концерт", "игр", "подписк"}},
		{"здоровье", []string{"здоров", "аптек", "лекарств", "врач", "стомат", "анализ"}},
		{"покупки", []string{"покупк", "одежд", "обув", "куртк", "магазин", "техник"}},
		{"другое", []string{"другое", "прочее"}},
	}
	for _, group := range groups {
		for _, word := range group.words {
			if strings.Contains(text, word) {
				return group.category
			}
		}
	}
	return ""
}

func cleanComment(text string) string {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{"добавь ", "запиши ", "потратил ", "потратила "} {
		if strings.HasPrefix(strings.ToLower(text), prefix) {
			text = strings.TrimSpace(text[len(prefix):])
		}
	}
	for _, suffix := range []string{"рублей", "рубля", "рубль", "руб.", "руб", "₽"} {
		text = strings.TrimSpace(strings.TrimSuffix(text, suffix))
	}
	text = strings.Trim(text, " ,;.-")
	if text == "" {
		return "Расход"
	}
	return text
}

func formatExpense(item expenses.Expense) string {
	comment := []rune(item.Comment)
	if len(comment) > 100 {
		comment = append(comment[:97], '.', '.', '.')
	}
	return fmt.Sprintf("#%d · %s ₽ · %s · %s · %s", item.ID, item.Amount, item.Category, item.SpentAt, string(comment))
}

func formatPage(page expenses.Page) string {
	lines := make([]string, 0, len(page.Items)+2)
	for _, item := range page.Items {
		lines = append(lines, formatExpense(item))
	}
	if page.Total > len(page.Items) {
		lines = append(lines, fmt.Sprintf("Показано %d из %d. Уточните период или категорию.", len(page.Items), page.Total))
	}
	lines = append(lines, "Итого: "+page.TotalAmount+" ₽.")
	return strings.Join(lines, "\n")
}

func periodLabel(filter expenses.Filter) string {
	label := ""
	if filter.From != "" {
		label += "\nПериод: " + filter.From + " — " + filter.To + "."
	}
	if filter.Category != "" {
		label += "\nКатегория: " + filter.Category + "."
	}
	return label
}
