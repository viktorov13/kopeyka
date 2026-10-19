package main

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"familyexpenses/internal/agent"
	"familyexpenses/internal/bot"
	"familyexpenses/internal/config"
	"familyexpenses/internal/expenses"
	"familyexpenses/internal/httpapi"
	"familyexpenses/web"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	settings, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	loc, err := time.LoadLocation(env("APP_TIMEZONE", "Europe/Moscow"))
	if err != nil {
		return fmt.Errorf("APP_TIMEZONE: %w", err)
	}
	var assistant *agent.Agent
	if settings.BotMode == config.ModeLLM {
		assistant, err = agent.New(agent.Config{BaseURL: settings.LLMBaseURL, APIKey: settings.LLMAPIKey, Model: settings.LLMModel, ReasoningEffort: settings.LLMReasoningEffort, Timeout: settings.LLMTimeout, MaxSteps: settings.LLMMaxSteps}, loc)
		if err != nil {
			return fmt.Errorf("LLM configuration: %w", err)
		}
	}
	db, err := sql.Open("postgres", env("DATABASE_URL", "postgres://expenses:expenses@localhost:5432/expenses?sslmode=disable"))
	if err != nil {
		return fmt.Errorf("invalid database configuration")
	}
	defer db.Close()
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = db.PingContext(startup); err != nil {
		return fmt.Errorf("database unavailable: %w", err)
	}
	if err = expenses.Migrate(startup, db); err != nil {
		return fmt.Errorf("database migration: %w", err)
	}
	store := expenses.NewStore(db)
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	static, err := fs.Sub(web.FS, "static")
	if err != nil {
		return err
	}
	api := &httpapi.Server{Store: store, DB: db, Location: loc, BotEnabled: token != "", Mode: settings.BotMode, Logger: logger}
	server := &http.Server{Addr: env("HTTP_ADDR", ":8080"), Handler: api.Handler(static), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second}
	errs := make(chan error, 2)
	go func() {
		logger.Info("HTTP server ready", "address", server.Addr, "timezone", loc.String(), "bot_enabled", token != "", "bot_mode", settings.BotMode)
		errs <- server.ListenAndServe()
	}()
	if token != "" {
		processor := bot.NewProcessor(store, loc)
		if assistant != nil {
			processor = bot.NewLLMProcessor(store, loc, assistant)
		}
		go func() { errs <- bot.Run(ctx, token, processor, db, logger) }()
	} else {
		logger.Info("Telegram disabled; set TELEGRAM_BOT_TOKEN to connect")
	}
	select {
	case <-ctx.Done():
	case err = <-errs:
		if err != nil && err != http.ErrServerClosed {
			stop()
		} else {
			err = nil
		}
	}
	shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	if shutdownErr := server.Shutdown(shutdown); shutdownErr != nil {
		logger.Error("HTTP shutdown", "error", shutdownErr)
	}
	return err
}
