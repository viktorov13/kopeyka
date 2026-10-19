package bot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"familyexpenses/internal/expenses"
)

type telegramUser struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}
type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type telegramMessage struct {
	ID   int64         `json:"message_id"`
	From *telegramUser `json:"from"`
	Chat telegramChat  `json:"chat"`
	Text string        `json:"text"`
}
type telegramUpdate struct {
	ID      int64            `json:"update_id"`
	Message *telegramMessage `json:"message"`
}

type telegramClient struct {
	http  *http.Client
	base  string
	token string
}

// apiError deliberately excludes raw URLs and underlying HTTP errors, because
// Telegram embeds the bot token in the request path.
type apiError struct {
	method     string
	status     int
	retryAfter time.Duration
}

func (e *apiError) Error() string {
	return fmt.Sprintf("Telegram %s failed (status %d)", e.method, e.status)
}

func (c *telegramClient) call(ctx context.Context, method string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode Telegram request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return &apiError{method: method}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &apiError{method: method}
	}
	defer resp.Body.Close()
	var envelope struct {
		OK         bool            `json:"ok"`
		Result     json.RawMessage `json:"result"`
		Code       int             `json:"error_code"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&envelope); err != nil {
		return &apiError{method: method, status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !envelope.OK {
		status := envelope.Code
		if status == 0 {
			status = resp.StatusCode
		}
		delay := time.Duration(envelope.Parameters.RetryAfter) * time.Second
		return &apiError{method: method, status: status, retryAfter: delay}
	}
	if output != nil {
		if err := json.Unmarshal(envelope.Result, output); err != nil {
			return fmt.Errorf("decode Telegram %s response", method)
		}
	}
	return nil
}

func (c *telegramClient) send(ctx context.Context, chatID int64, reply string) error {
	if len([]rune(reply)) > 4000 {
		reply = string([]rune(reply)[:3997]) + "..."
	}
	return c.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": reply}, nil)
}

type transactionalLedger interface{ WithTx(*sql.Tx) expenses.Ledger }

// Run processes private messages serially. The persisted response is committed
// in the same transaction as the expense mutation, so retries never create a
// second expense. A reply itself may be delivered twice after a process crash.
func Run(ctx context.Context, token string, processor *Processor, db *sql.DB, logger *slog.Logger) error {
	if token == "" {
		return fmt.Errorf("Telegram token is empty")
	}
	if strings.ContainsAny(token, "/?#\r\n ") {
		return fmt.Errorf("Telegram token has invalid characters")
	}
	if db == nil {
		return fmt.Errorf("Telegram requires a database for durable update deduplication")
	}
	if _, ok := processor.ledger.(transactionalLedger); !ok {
		return fmt.Errorf("Telegram ledger must support WithTx")
	}
	if logger == nil {
		logger = slog.Default()
	}
	client := &telegramClient{http: &http.Client{Timeout: 45 * time.Second}, base: "https://api.telegram.org", token: token}
	var offset int64
	delay := time.Second
	for {
		var updates []telegramUpdate
		err := client.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 30, "limit": 20, "allowed_updates": []string{"message"}}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var api *apiError
			if errors.As(err, &api) && (api.status == 401 || api.status == 404 || api.status == 409) {
				return err
			}
			logger.Warn("Telegram polling failed", "error", err)
			if !pause(ctx, retryDelay(err, delay)) {
				return nil
			}
			delay = min(delay*2, 30*time.Second)
			continue
		}
		delay = time.Second
		for _, update := range updates {
			if update.ID < offset {
				continue
			}
			message := update.Message
			if message == nil || message.From == nil || message.From.IsBot || message.Chat.Type != "private" || message.Chat.ID != message.From.ID {
				offset = update.ID + 1
				continue
			}
			// Keep this update in hand until its database commit and reply finish.
			// The next getUpdates offset acknowledges it to Telegram.
			for {
				reply, err := processor.processUpdate(ctx, db, update)
				if err == nil && reply != "" {
					err = client.send(ctx, message.Chat.ID, reply)
				}
				if err == nil {
					break
				}
				if ctx.Err() != nil {
					return nil
				}
				var api *apiError
				if errors.As(err, &api) {
					if api.status == 401 || api.status == 404 {
						return err
					}
					// A user who blocked the bot must not stall everyone else's updates.
					if api.status == 403 || api.status == 400 {
						logger.Warn("Telegram reply cannot be delivered", "update_id", update.ID, "status", api.status)
						break
					}
				}
				logger.Warn("Telegram update will be retried", "update_id", update.ID, "error", err)
				if !pause(ctx, retryDelay(err, delay)) {
					return nil
				}
				delay = min(delay*2, 30*time.Second)
			}
			offset = update.ID + 1
			delay = time.Second
		}
	}
}

func (p *Processor) processUpdate(ctx context.Context, db *sql.DB, update telegramUpdate) (reply string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	timeout := 15 * time.Second
	if p.assistant != nil {
		// The assistant has its own (at most 60s) deadline. Leave time to
		// roll back staged writes and persist a safe error reply afterwards.
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "INSERT INTO telegram_updates (update_id) VALUES ($1) ON CONFLICT DO NOTHING", update.ID)
	if err != nil {
		return "", err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if inserted == 0 {
		if err := tx.QueryRowContext(ctx, "SELECT response FROM telegram_updates WHERE update_id=$1", update.ID).Scan(&reply); err != nil {
			return "", err
		}
		return reply, tx.Commit()
	}
	userID := update.Message.From.ID
	previous, hadPending := p.pending[userID]
	defer func() {
		if err != nil {
			if hadPending {
				p.pending[userID] = previous
			} else {
				delete(p.pending, userID)
			}
		}
	}()
	if update.Message.Text == "" {
		reply = "Пришлите расход текстом, например: кофе 350."
	} else {
		ledger := p.ledger.(transactionalLedger).WithTx(tx)
		if p.assistant != nil {
			reply, err = p.replyWithAssistant(ctx, tx, ledger, userID, update.Message.Text)
		} else {
			reply, err = p.reply(ctx, ledger, userID, update.Message.Text)
		}
		if err != nil {
			var validation *expenses.ValidationError
			switch {
			case errors.Is(err, expenses.ErrNotFound):
				reply, err = "Трата не найдена среди ваших расходов.", nil
			case errors.As(err, &validation):
				reply, err = validation.Error(), nil
			default:
				return "", err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE telegram_updates SET response=$2 WHERE update_id=$1", update.ID, reply); err != nil {
		return "", err
	}
	err = tx.Commit()
	return reply, err
}

func retryDelay(err error, fallback time.Duration) time.Duration {
	var api *apiError
	if errors.As(err, &api) && api.retryAfter > fallback {
		return api.retryAfter
	}
	return fallback
}

func pause(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
