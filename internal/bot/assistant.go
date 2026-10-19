package bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"familyexpenses/internal/expenses"
)

// Assistant only receives the authenticated Telegram sender and a ledger bound
// to the update's transaction. It must return an error if a turn is incomplete.
type Assistant interface {
	Respond(context.Context, expenses.Ledger, int64, string, json.RawMessage) (string, json.RawMessage, error)
}

func NewLLMProcessor(ledger expenses.Ledger, location *time.Location, assistant Assistant) *Processor {
	p := NewProcessor(ledger, location)
	p.assistant = assistant
	return p
}

const assistantHelp = `Я помогаю учитывать ваши расходы в рублях. Пишите обычным текстом:
• кофе 350
• такси 1200 вчера
• сколько потратил на еду в этом месяце
• покажи траты за неделю
• последнее было не 350, а 450
• удали такси

Если данных не хватает или подходят несколько трат, я попрошу уточнение. За одно сообщение изменяю одну трату. «Отмена» очищает контекст диалога. Доступны только ваши расходы.`

const assistantFailure = "Не удалось обработать запрос. Изменения не сохранены. Попробуйте ещё раз чуть позже."

func (p *Processor) replyWithAssistant(ctx context.Context, tx *sql.Tx, ledger expenses.Ledger, userID int64, text string) (string, error) {
	if userID <= 0 || userID > 9007199254740991 {
		return "", fmt.Errorf("invalid Telegram sender")
	}
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > 2000 {
		return "Сообщение слишком длинное. Сократите его до 2000 символов.", nil
	}
	switch strings.ToLower(text) {
	case "отмена", "/cancel":
		_, err := tx.ExecContext(ctx, "DELETE FROM telegram_conversations WHERE user_id=$1", userID)
		return "Контекст диалога очищен. Расходы сохранены.", err
	case "", "помощь", "/help", "/start":
		return assistantHelp, nil
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO telegram_conversations(user_id) VALUES($1) ON CONFLICT DO NOTHING", userID); err != nil {
		return "", err
	}
	var history json.RawMessage
	var updated time.Time
	if err := tx.QueryRowContext(ctx, "SELECT history,updated_at FROM telegram_conversations WHERE user_id=$1 FOR UPDATE", userID).Scan(&history, &updated); err != nil {
		return "", err
	}
	if p.now().Sub(updated) >= 24*time.Hour {
		history = json.RawMessage("[]")
	}
	if _, err := tx.ExecContext(ctx, "SAVEPOINT assistant_turn"); err != nil {
		return "", err
	}
	reply, nextHistory, err := p.assistant.Respond(ctx, ledger, userID, text, history)
	if err != nil {
		var safe interface{ UserMessage() string }
		if !errors.As(err, &safe) {
			return "", err // Database failures retry the entire update.
		}
		if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT assistant_turn"); rollbackErr != nil {
			return "", rollbackErr
		}
		return safe.UserMessage(), nil
	}
	var turns []json.RawMessage
	if strings.TrimSpace(reply) == "" || len(nextHistory) > 64<<10 || json.Unmarshal(nextHistory, &turns) != nil || turns == nil {
		if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT assistant_turn"); rollbackErr != nil {
			return "", rollbackErr
		}
		return assistantFailure, nil
	}
	if _, err = tx.ExecContext(ctx, "UPDATE telegram_conversations SET history=$2::jsonb,updated_at=clock_timestamp() WHERE user_id=$1", userID, string(nextHistory)); err != nil {
		return "", err
	}
	return reply, nil
}
