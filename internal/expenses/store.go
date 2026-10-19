package expenses

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	_ "github.com/lib/pq"
)

//go:embed migrations/001_initial.sql
var migration string

//go:embed migrations/002_conversations.sql
var conversationMigration string

type querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Store struct{ db querier }

func NewStore(db *sql.DB) *Store          { return &Store{db: db} }
func (s *Store) WithTx(tx *sql.Tx) Ledger { return &Store{db: tx} }

func Migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize startup migrations across simultaneous app instances.
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(64829410)"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())"); err != nil {
		return err
	}
	for index, source := range []string{migration, conversationMigration} {
		version := index + 1
		var exists bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", version).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err = tx.ExecContext(ctx, source); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version) VALUES($1)", version); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const columns = "id,user_id,amount_minor,category,to_char(spent_at,'YYYY-MM-DD'),comment,created_at,updated_at"

type scanner interface{ Scan(...any) error }

func scanExpense(row scanner) (Expense, error) {
	var e Expense
	var minor int64
	err := row.Scan(&e.ID, &e.UserID, &minor, &e.Category, &e.SpentAt, &e.Comment, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	e.Amount = FormatAmount(minor)
	return e, err
}

func (s *Store) Create(ctx context.Context, in Input) (Expense, error) {
	if err := ValidateInput(in); err != nil {
		return Expense{}, err
	}
	minor, _ := ParseAmount(in.Amount)
	return scanExpense(s.db.QueryRowContext(ctx, "INSERT INTO expenses (user_id,amount_minor,category,spent_at,comment) VALUES ($1,$2,$3,$4,$5) RETURNING "+columns, in.UserID, minor, in.Category, in.SpentAt, strings.TrimSpace(in.Comment)))
}

func where(f Filter) (string, []any) {
	var conditions []string
	var args []any
	add := func(expr string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(expr, len(args)))
	}
	if f.UserID != nil {
		add("user_id=$%d", *f.UserID)
	}
	if f.From != "" {
		add("spent_at >= $%d::date", f.From)
	}
	if f.To != "" {
		add("spent_at <= $%d::date", f.To)
	}
	if f.Category != "" {
		add("category=$%d", f.Category)
	}
	if f.Search != "" {
		// Treat user input as literal text, including SQL LIKE wildcards.
		q := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(f.Search)
		args = append(args, "%"+q+"%")
		n := len(args)
		// PostgreSQL 18's built-in Unicode collation keeps Cyrillic search
		// case-insensitive even when the cluster was initialized with locale C.
		conditions = append(conditions, fmt.Sprintf("(comment COLLATE \"pg_unicode_fast\" ILIKE $%d OR category COLLATE \"pg_unicode_fast\" ILIKE $%d)", n, n))
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func (s *Store) List(ctx context.Context, f Filter) (Page, error) {
	page := Page{Items: []Expense{}, TotalAmount: "0.00", CategoryTotals: []CategoryTotal{}}
	if err := ValidateFilter(f); err != nil {
		return page, err
	}
	// Keep rows, counts and sums consistent even while another request edits data.
	if db, ok := s.db.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if err != nil {
			return page, err
		}
		defer tx.Rollback()
		page, err = (&Store{db: tx}).List(ctx, f)
		if err != nil {
			return page, err
		}
		return page, tx.Commit()
	}
	w, args := where(f)
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT count(*),COALESCE(sum(amount_minor),0) FROM expenses"+w, args...).Scan(&page.Total, &total); err != nil {
		return page, err
	}
	page.TotalAmount = FormatAmount(total)
	rows, err := s.db.QueryContext(ctx, "SELECT category,count(*),sum(amount_minor) FROM expenses"+w+" GROUP BY category ORDER BY sum(amount_minor) DESC,category", args...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var c CategoryTotal
		var n int64
		if err = rows.Scan(&c.Category, &c.Count, &n); err != nil {
			rows.Close()
			return page, err
		}
		c.Amount = FormatAmount(n)
		page.CategoryTotals = append(page.CategoryTotals, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	limit := f.Limit
	if limit == 0 {
		limit = 20
	}
	args = append(args, limit, f.Offset)
	order := "spent_at DESC,id DESC"
	if f.Latest {
		order = "created_at DESC,id DESC"
	}
	query := "SELECT " + columns + " FROM expenses" + w + " ORDER BY " + order + fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	rows, err = s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanExpense(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, e)
	}
	return page, rows.Err()
}

func (s *Store) Update(ctx context.Context, id int64, owner *int64, p Patch) (Expense, error) {
	if id <= 0 {
		return Expense{}, invalid("Некорректный ID траты")
	}
	// Lock the row before merging a partial update, so concurrent PATCH requests
	// don't silently overwrite unrelated fields.
	if db, ok := s.db.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return Expense{}, err
		}
		defer tx.Rollback()
		e, err := (&Store{db: tx}).Update(ctx, id, owner, p)
		if err != nil {
			return Expense{}, err
		}
		return e, tx.Commit()
	}
	w := " WHERE id=$1"
	args := []any{id}
	if owner != nil {
		w += " AND user_id=$2"
		args = append(args, *owner)
	}
	e, err := scanExpense(s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM expenses"+w+" FOR UPDATE", args...))
	if err != nil {
		return Expense{}, err
	}
	if owner != nil && p.UserID != nil && *p.UserID != *owner {
		return Expense{}, invalid("Нельзя менять владельца через бота")
	}
	in := Input{UserID: e.UserID, Amount: e.Amount, Category: e.Category, SpentAt: e.SpentAt, Comment: e.Comment}
	if p.UserID != nil {
		in.UserID = *p.UserID
	}
	if p.Amount != nil {
		in.Amount = *p.Amount
	}
	if p.Category != nil {
		in.Category = *p.Category
	}
	if p.SpentAt != nil {
		in.SpentAt = *p.SpentAt
	}
	if p.Comment != nil {
		in.Comment = *p.Comment
	}
	if err = ValidateInput(in); err != nil {
		return Expense{}, err
	}
	minor, _ := ParseAmount(in.Amount)
	return scanExpense(s.db.QueryRowContext(ctx, "UPDATE expenses SET user_id=$1,amount_minor=$2,category=$3,spent_at=$4,comment=$5,updated_at=clock_timestamp() WHERE id=$6 RETURNING "+columns, in.UserID, minor, in.Category, in.SpentAt, strings.TrimSpace(in.Comment), id))
}

func (s *Store) Delete(ctx context.Context, id int64, owner *int64) error {
	if id <= 0 {
		return invalid("Некорректный ID траты")
	}
	q := "DELETE FROM expenses WHERE id=$1"
	args := []any{id}
	if owner != nil {
		q += " AND user_id=$2"
		args = append(args, *owner)
	}
	r, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Users(ctx context.Context) ([]User, error) {
	result := []User{}
	rows, err := s.db.QueryContext(ctx, "SELECT user_id,count(*),sum(amount_minor) FROM expenses GROUP BY user_id ORDER BY user_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u User
		var n int64
		if err := rows.Scan(&u.UserID, &u.Count, &n); err != nil {
			return nil, err
		}
		u.TotalAmount = FormatAmount(n)
		result = append(result, u)
	}
	return result, rows.Err()
}
