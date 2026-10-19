package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"familyexpenses/internal/expenses"
)

type Server struct {
	Store      *expenses.Store
	DB         *sql.DB
	Location   *time.Location
	BotEnabled bool
	Mode       string
	Logger     *slog.Logger
}

func (s *Server) Handler(static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/users", s.users)
	mux.HandleFunc("GET /api/expenses", s.list)
	mux.HandleFunc("POST /api/expenses", s.create)
	mux.HandleFunc("PATCH /api/expenses/{id}", s.update)
	mux.HandleFunc("DELETE /api/expenses/{id}", s.delete)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Маршрут не найден"})
	})
	if static != nil {
		files := http.FileServer(http.FS(static))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Метод не поддерживается"})
				return
			}
			files.ServeHTTP(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
			// The assignment intentionally has no login. Block browser requests
			// from other origins without turning it into an authentication system.
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				writeJSON(w, 403, map[string]string{"error": "Запрос с другого сайта запрещён"})
				return
			}
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log().Error("http panic", "error", recovered)
				writeJSON(w, 500, map[string]string{"error": "Внутренняя ошибка сервера"})
			}
		}()
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) fail(w http.ResponseWriter, err error) {
	var validation *expenses.ValidationError
	switch {
	case errors.As(err, &validation):
		writeJSON(w, 400, map[string]string{"error": validation.Error()})
	case errors.Is(err, expenses.ErrNotFound):
		writeJSON(w, 404, map[string]string{"error": err.Error()})
	case errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, 504, map[string]string{"error": "Превышено время ожидания. Попробуйте ещё раз"})
	default:
		s.log().Error("request failed", "error", err)
		writeJSON(w, 500, map[string]string{"error": "Не удалось выполнить запрос. Попробуйте ещё раз"})
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return &expenses.ValidationError{Message: "Ожидается Content-Type: application/json"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return &expenses.ValidationError{Message: "Некорректный JSON: сумма передаётся строкой, Telegram ID — числом"}
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return &expenses.ValidationError{Message: "Ожидается один JSON-объект"}
	}
	return nil
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.DB.PingContext(ctx); err != nil {
		writeJSON(w, 503, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	mode := s.Mode
	if mode == "" {
		mode = "rules"
	}
	writeJSON(w, 200, map[string]any{"categories": expenses.Categories, "timezone": s.Location.String(), "today": time.Now().In(s.Location).Format(time.DateOnly), "bot_enabled": s.BotEnabled, "mode": mode})
}
func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.Users(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func parseFilter(r *http.Request) (expenses.Filter, error) {
	q := r.URL.Query()
	f := expenses.Filter{From: q.Get("from"), To: q.Get("to"), Category: q.Get("category"), Search: strings.TrimSpace(q.Get("search")), Limit: 20}
	if u := q.Get("user_id"); u != "" {
		id, err := strconv.ParseInt(u, 10, 64)
		if err != nil {
			return f, &expenses.ValidationError{Message: "Некорректный Telegram ID"}
		}
		f.UserID = &id
	}
	for name, dest := range map[string]*int{"limit": &f.Limit, "offset": &f.Offset} {
		if value := q.Get(name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil {
				return f, &expenses.ValidationError{Message: "Параметры limit и offset должны быть целыми числами"}
			}
			*dest = n
		}
	}
	if f.Limit < 1 {
		return f, &expenses.ValidationError{Message: "Размер страницы должен быть от 1 до 100"}
	}
	return f, expenses.ValidateFilter(f)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilter(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	page, err := s.Store.List(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, page)
}
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var input expenses.Input
	if err := decode(w, r, &input); err != nil {
		s.fail(w, err)
		return
	}
	e, err := s.Store.Create(r.Context(), input)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Location", "/api/expenses/"+strconv.FormatInt(e.ID, 10))
	writeJSON(w, 201, e)
}
func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, &expenses.ValidationError{Message: "Некорректный ID траты"}
	}
	return id, nil
}
func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var p expenses.Patch
	if err = decode(w, r, &p); err != nil {
		s.fail(w, err)
		return
	}
	if p.UserID == nil && p.Amount == nil && p.Category == nil && p.SpentAt == nil && p.Comment == nil {
		s.fail(w, &expenses.ValidationError{Message: "Укажите хотя бы одно поле для изменения"})
		return
	}
	e, err := s.Store.Update(r.Context(), id, nil, p)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, e)
}
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.Store.Delete(r.Context(), id, nil)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
