package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/centrys-labs/stagezero/db"
)

func TestNotificationChannelResponseDoesNotExposeCredentials(t *testing.T) {
	cases := []struct {
		kind   string
		config map[string]interface{}
		secret string
	}{
		{"discord", map[string]interface{}{"url": "https://discord.test/hook/a-private-token"}, "a-private-token"},
		{"telegram", map[string]interface{}{"bot_token": "bot-private-token", "api_url": "https://private-token@telegram.test/secret", "chat_id": "123"}, "private-token"},
		{"email", map[string]interface{}{"smtp_host": "smtp.test", "password": "mail-private-token", "username": "login@example.test", "to": "team@example.test"}, "mail-private-token"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			response := notificationChannelResponse(db.NotificationChannel{Type: tc.kind, Config: tc.config})
			for _, value := range response.Config {
				if strings.Contains(strings.ToLower(fmt.Sprint(value)), "private-token") || value == "***" {
					t.Fatalf("credential or placeholder in response: %#v", response.Config)
				}
			}
			if _, exists := response.Config["url"]; exists {
				t.Fatal("credential-bearing URL in response")
			}
			if _, exists := response.Config["username"]; exists {
				t.Fatal("SMTP login in response")
			}
			if len(response.ConfiguredCredentials) == 0 {
				t.Fatal("configured credential metadata missing")
			}
		})
	}
}

func TestNotificationChannelsRequireSession(t *testing.T) {
	server := &Server{}
	for _, call := range []struct {
		name string
		run  func(http.ResponseWriter, *http.Request)
	}{
		{"list", server.ListNotificationChannels},
		{"create", server.CreateNotificationChannel},
		{"update", func(w http.ResponseWriter, r *http.Request) { server.UpdateNotificationChannel(w, r, "chn-1") }},
		{"delete", func(w http.ResponseWriter, r *http.Request) { server.DeleteNotificationChannel(w, r, "chn-1") }},
		{"test", func(w http.ResponseWriter, r *http.Request) { server.TestNotificationChannel(w, r, "chn-1") }},
	} {
		t.Run(call.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			call.run(rec, httptest.NewRequest(http.MethodGet, "/api/notification-channels", nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
