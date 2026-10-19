package expenses

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }
func invalid(s string) error             { return &ValidationError{Message: s} }

// ParseAmount converts decimal rubles to kopecks without floating point math.
func ParseAmount(s string) (int64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", ".")
	p := strings.Split(s, ".")
	if len(p) > 2 || p[0] == "" {
		return 0, invalid("Укажите сумму, например 350 или 350.50")
	}
	for _, c := range p[0] {
		if c < '0' || c > '9' {
			return 0, invalid("Сумма должна быть положительным числом")
		}
	}
	whole, err := strconv.ParseInt(p[0], 10, 64)
	if err != nil || whole > 999999999 {
		return 0, invalid("Сумма не должна превышать 999 999 999.99 ₽")
	}
	var cents int64
	if len(p) == 2 {
		if len(p[1]) < 1 || len(p[1]) > 2 {
			return 0, invalid("У суммы должно быть не больше двух знаков после запятой")
		}
		for _, c := range p[1] {
			if c < '0' || c > '9' {
				return 0, invalid("Некорректная сумма")
			}
		}
		frac := p[1]
		if len(frac) == 1 {
			frac += "0"
		}
		cents, _ = strconv.ParseInt(frac, 10, 64)
	}
	n := whole*100 + cents
	if n <= 0 {
		return 0, invalid("Сумма должна быть больше нуля")
	}
	return n, nil
}

func FormatAmount(n int64) string { return fmt.Sprintf("%d.%02d", n/100, n%100) }

func validCategory(category string) bool {
	for _, c := range Categories {
		if category == c {
			return true
		}
	}
	return false
}

func ValidateInput(in Input) error {
	if in.UserID <= 0 || in.UserID > 9007199254740991 {
		return invalid("Укажите положительный Telegram ID владельца")
	}
	if _, err := ParseAmount(in.Amount); err != nil {
		return err
	}
	if !validCategory(in.Category) {
		return invalid("Выберите категорию из списка")
	}
	if date, err := time.Parse(time.DateOnly, in.SpentAt); err != nil || date.Year() < 1 {
		return invalid("Дата должна быть в формате ГГГГ-ММ-ДД")
	}
	if utf8.RuneCountInString(in.Comment) > 500 {
		return invalid("Комментарий должен быть не длиннее 500 символов")
	}
	return nil
}

func ValidateFilter(f Filter) error {
	if f.UserID != nil && (*f.UserID <= 0 || *f.UserID > 9007199254740991) {
		return invalid("Некорректный Telegram ID")
	}
	if f.Category != "" && !validCategory(f.Category) {
		return invalid("Неизвестная категория")
	}
	for _, date := range []string{f.From, f.To} {
		if date != "" {
			if parsed, err := time.Parse(time.DateOnly, date); err != nil || parsed.Year() < 1 {
				return invalid("Дата фильтра должна быть в формате ГГГГ-ММ-ДД")
			}
		}
	}
	if f.From != "" && f.To != "" && f.From > f.To {
		return invalid("Начало периода не может быть позже окончания")
	}
	if f.Limit < 0 || f.Limit > 100 {
		return invalid("Размер страницы должен быть от 1 до 100")
	}
	if f.Offset < 0 {
		return invalid("Смещение не может быть отрицательным")
	}
	if utf8.RuneCountInString(f.Search) > 100 {
		return invalid("Поисковая строка слишком длинная")
	}
	return nil
}
