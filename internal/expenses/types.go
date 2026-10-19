package expenses

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("Трата не найдена")

var Categories = []string{"еда", "транспорт", "жильё", "развлечения", "здоровье", "покупки", "другое"}

type Expense struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Amount    string    `json:"amount"`
	Category  string    `json:"category"`
	SpentAt   string    `json:"spent_at"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Input struct {
	UserID   int64  `json:"user_id"`
	Amount   string `json:"amount"`
	Category string `json:"category"`
	SpentAt  string `json:"spent_at"`
	Comment  string `json:"comment"`
}

type Patch struct {
	UserID   *int64  `json:"user_id,omitempty"`
	Amount   *string `json:"amount,omitempty"`
	Category *string `json:"category,omitempty"`
	SpentAt  *string `json:"spent_at,omitempty"`
	Comment  *string `json:"comment,omitempty"`
}

type Filter struct {
	UserID   *int64
	From     string
	To       string
	Category string
	Search   string
	Limit    int
	Offset   int
	// Latest selects creation order for phrases such as “последняя трата”.
	Latest bool
}

type CategoryTotal struct {
	Category string `json:"category"`
	Amount   string `json:"amount"`
	Count    int    `json:"count"`
}

type Page struct {
	Items          []Expense       `json:"items"`
	Total          int             `json:"total"`
	TotalAmount    string          `json:"total_amount"`
	CategoryTotals []CategoryTotal `json:"category_totals"`
}

type User struct {
	UserID      int64  `json:"user_id"`
	Count       int    `json:"count"`
	TotalAmount string `json:"total_amount"`
}

// Ledger is shared by the HTTP adapter and the rule-based Telegram processor.
// A non-nil owner always restricts update/delete to that Telegram user.
type Ledger interface {
	Create(context.Context, Input) (Expense, error)
	List(context.Context, Filter) (Page, error)
	Update(context.Context, int64, *int64, Patch) (Expense, error)
	Delete(context.Context, int64, *int64) error
}
