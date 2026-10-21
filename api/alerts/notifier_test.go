package alerts

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/centrys-labs/stagezero/telemetry"
)

func TestSendDiscord(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	err := Send(context.Background(), Channel{Type: "discord", Enabled: true, Config: map[string]interface{}{"url": server.URL}}, Message{Title: "[StageZero] CPU high", Body: "api is above 85%", StartedAt: time.Now()})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if !strings.Contains(body, "CPU high") || !strings.Contains(body, "api is above") {
		t.Fatalf("payload = %s", body)
	}
}

func TestValidateChannelConfig(t *testing.T) {
	if err := ValidateChannelConfig("telegram", map[string]interface{}{"bot_token": "token"}); err == nil {
		t.Fatal("telegram without chat ID should be rejected")
	}
	if err := ValidateChannelConfig("webhook", map[string]interface{}{"url": "https://example.test/hook"}); err != nil {
		t.Fatalf("valid webhook rejected: %v", err)
	}
}

func TestSendDoesNotExposeCredentialInProviderError(t *testing.T) {
	secret := "bot-credential-should-not-be-logged"
	channel := Channel{Type: "telegram", Enabled: true, Config: map[string]interface{}{
		"bot_token": secret, "chat_id": "123", "api_url": "http://127.0.0.1:1/" + secret,
	}}
	err := Send(context.Background(), channel, Message{Body: "test"})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("provider error exposed credential: %v", err)
	}
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, nil)))
	defer slog.SetDefault(previous)
	telemetry.Printf("alert notification: channel=123: %v", err)
	if strings.Contains(log.String(), secret) {
		t.Fatalf("logged provider error exposed credential: %s", log.String())
	}
}

func TestEmailFailureDoesNotExposeCredential(t *testing.T) {
	secret := "smtp-secret-should-not-be-logged"
	err := Send(context.Background(), Channel{Type: "email", Enabled: true, Config: map[string]interface{}{
		"smtp_host": "127.0.0.1", "smtp_port": 1, "from": "from@example.test", "to": "to@example.test", "username": "login", "password": secret,
	}}, Message{Body: "test"})
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("SMTP failure exposed connection detail or credential: %v", err)
	}
}
