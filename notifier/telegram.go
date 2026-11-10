package notifier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"
)

type Notifier interface {
	SendAlert(name, host string, port int, lastError string, duration time.Duration) error
	SendRecovery(name, host string, port int, downtime time.Duration) error
	SendTest(token, chatID string) error
}

type TelegramNotifier struct {
	tokenGetter  func() string
	chatIDGetter func() string
	client       *http.Client
}

func NewTelegramNotifier(tokenGetter, chatIDGetter func() string) *TelegramNotifier {
	return &TelegramNotifier{
		tokenGetter:  tokenGetter,
		chatIDGetter: chatIDGetter,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type telegramPayload struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

type telegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

func (t *TelegramNotifier) send(token, chatID, text string) error {
	token = strings.TrimSpace(token)
	chatID = strings.TrimSpace(chatID)

	if token == "" || chatID == "" {
		return fmt.Errorf("telegram token or chat ID is empty")
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	payload := telegramPayload{
		ChatID:    chatID,
		Text:      text,
		ParseMode: "HTML",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	resp, err := t.client.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to send telegram request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read telegram response: %w", err)
	}

	var tgResp telegramResponse
	if err := json.Unmarshal(respBytes, &tgResp); err != nil {
		return fmt.Errorf("failed to parse telegram response: %s", string(respBytes))
	}

	if !tgResp.OK {
		return fmt.Errorf("telegram API error: %s", tgResp.Description)
	}

	return nil
}

func (t *TelegramNotifier) SendAlert(name, host string, port int, lastError string, duration time.Duration) error {
	token := t.tokenGetter()
	chatID := t.chatIDGetter()

	now := time.Now().Format("2006-01-02 15:04:05")
	var nameStr string
	if name != "" {
		nameStr = fmt.Sprintf("<b>Название:</b> %s\n", html.EscapeString(name))
	}

	addrDisplay := host
	if port > 0 {
		addrDisplay = fmt.Sprintf("%s:%d (TCP)", host, port)
	} else {
		addrDisplay = fmt.Sprintf("%s (ICMP Ping)", host)
	}

	text := fmt.Sprintf(
		"🚨 <b>Внимание! Потеряно соединение с хостом</b>\n\n"+
			"%s"+
			"<b>Адрес:</b> <code>%s</code>\n"+
			"<b>Статус:</b> 🔴 НЕДОСТУПЕН (DOWN)\n"+
			"<b>Время:</b> %s\n"+
			"<b>Ошибка:</b> <code>%s</code>\n",
		nameStr,
		html.EscapeString(addrDisplay),
		now,
		html.EscapeString(lastError),
	)

	return t.send(token, chatID, text)
}

func (t *TelegramNotifier) SendRecovery(name, host string, port int, downtime time.Duration) error {
	token := t.tokenGetter()
	chatID := t.chatIDGetter()

	now := time.Now().Format("2006-01-02 15:04:05")
	var nameStr string
	if name != "" {
		nameStr = fmt.Sprintf("<b>Название:</b> %s\n", html.EscapeString(name))
	}

	addrDisplay := host
	if port > 0 {
		addrDisplay = fmt.Sprintf("%s:%d (TCP)", host, port)
	} else {
		addrDisplay = fmt.Sprintf("%s (ICMP Ping)", host)
	}

	downtimeStr := ""
	if downtime > 0 {
		downtimeStr = fmt.Sprintf("<b>Время простоя:</b> %s\n", downtime.Round(time.Second).String())
	}

	text := fmt.Sprintf(
		"✅ <b>Связь восстановлена!</b>\n\n"+
			"%s"+
			"<b>Адрес:</b> <code>%s</code>\n"+
			"<b>Статус:</b> 🟢 ДОСТУПЕН (UP)\n"+
			"<b>Время:</b> %s\n"+
			"%s",
		nameStr,
		html.EscapeString(addrDisplay),
		now,
		downtimeStr,
	)

	return t.send(token, chatID, text)
}

func (t *TelegramNotifier) SendTest(token, chatID string) error {
	now := time.Now().Format("2006-01-02 15:04:05")
	text := fmt.Sprintf(
		"🔔 <b>Тестовое уведомление NetCheck</b>\n\n"+
			"Интеграция с Telegram настроена успешно!\n"+
			"<b>Время отправки:</b> %s\n",
		now,
	)
	return t.send(token, chatID, text)
}
