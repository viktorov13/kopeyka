package bot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientParsesUpdates(t *testing.T) {
	client := &telegramClient{token: "secret", base: "https://api.telegram.org", http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != "/botsecret/getUpdates" || req.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":[{"update_id":19,"message":{"message_id":8,"from":{"id":42},"chat":{"id":42,"type":"private"},"text":"кофе 350"}}]}`))}, nil
	})}}
	var updates []telegramUpdate
	if err := client.call(context.Background(), "getUpdates", map[string]any{"offset": 18}, &updates); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].ID != 19 || updates[0].Message.From.ID != 42 {
		t.Fatalf("unexpected updates: %#v", updates)
	}
}

func TestClientNeverExposesTokenInNetworkErrors(t *testing.T) {
	client := &telegramClient{token: "DO_NOT_LOG_ME", base: "https://api.telegram.org", http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("request failed: " + req.URL.String())
	})}}
	err := client.call(context.Background(), "sendMessage", map[string]string{"text": "hello"}, nil)
	if err == nil || strings.Contains(err.Error(), client.token) {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestRateLimitAndCancellation(t *testing.T) {
	client := &telegramClient{token: "secret", base: "https://api.telegram.org", http: &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":429,"parameters":{"retry_after":12}}`))}, nil
	})}}
	err := client.send(context.Background(), 1, "hello")
	if retryDelay(err, time.Second) != 12*time.Second {
		t.Fatal("retry_after was not respected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if pause(ctx, time.Hour) {
		t.Fatal("cancelled retry should stop")
	}
}

func TestTelegramMessageLimit(t *testing.T) {
	client := &telegramClient{token: "secret", base: "https://api.telegram.org", http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		if strings.Count(string(body), "я") != 3997 {
			t.Fatal("long Cyrillic reply was not truncated by runes")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`))}, nil
	})}}
	if err := client.send(context.Background(), 1, strings.Repeat("я", 5000)); err != nil {
		t.Fatal(err)
	}
}
