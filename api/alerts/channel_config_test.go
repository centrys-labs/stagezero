package alerts

import (
	"reflect"
	"testing"
)

func TestMergeChannelConfigCredentialLifecycle(t *testing.T) {
	initial := map[string]interface{}{"smtp_host": "mail.example.test", "from": "a@example.test", "to": "b@example.test", "username": "login", "password": "first"}
	for name, tc := range map[string]struct {
		patch map[string]interface{}
		want  string
		err   bool
	}{
		"preserve": {map[string]interface{}{"smtp_host": "next.example.test"}, "first", false},
		"replace":  {map[string]interface{}{"password": "second"}, "second", false},
		"remove":   {map[string]interface{}{"password": nil}, "", false},
		"empty":    {map[string]interface{}{"password": ""}, "", true},
		"bad type": {map[string]interface{}{"password": 5}, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := MergeChannelConfig("email", "email", initial, tc.patch)
			if (err != nil) != tc.err {
				t.Fatalf("error = %v, want error = %t", err, tc.err)
			}
			if !tc.err {
				if value, _ := result["password"].(string); value != tc.want {
					t.Fatalf("password = %q, want %q", value, tc.want)
				}
				if _, ok := initial["password"]; !ok {
					t.Fatal("mutated original stored config")
				}
			}
		})
	}
	for _, kind := range []string{"discord", "slack", "webhook"} {
		for _, patch := range []map[string]interface{}{{"url": nil}, {"url": ""}} {
			if _, err := MergeChannelConfig(kind, kind, map[string]interface{}{"url": "https://example.test/token"}, patch); err == nil {
				t.Fatalf("%s accepted required credential removal: %#v", kind, patch)
			}
		}
	}
	if _, err := MergeChannelConfig("discord", "telegram", map[string]interface{}{"url": "https://example.test/token"}, map[string]interface{}{"chat_id": "123"}); err == nil {
		t.Fatal("type switch reused an unrelated credential")
	}
	telegram, err := MergeChannelConfig("telegram", "telegram", map[string]interface{}{"bot_token": "token", "chat_id": "123", "api_url": "https://api.test"}, map[string]interface{}{"api_url": nil})
	if err != nil || telegram["api_url"] != nil || telegram["bot_token"] != "token" {
		t.Fatalf("optional Telegram URL removal: %#v, %v", telegram, err)
	}
	if _, err := MergeChannelConfig("telegram", "telegram", telegram, map[string]interface{}{"bot_token": nil}); err == nil {
		t.Fatal("removed required Telegram bot token")
	}
	if _, err := MergeChannelConfig("email", "email", initial, map[string]interface{}{"unexpected_url": "sensitive"}); err == nil {
		t.Fatal("accepted unclassified field")
	}
}

func TestPublicChannelConfigOnlyExposesKnownSafeFields(t *testing.T) {
	config := map[string]interface{}{"chat_id": "123", "api_url": "https://secret@api.example.test", "bot_token": "secret", "future_secret": "unclassified"}
	public, names := PublicChannelConfig("telegram", config)
	if !reflect.DeepEqual(public, map[string]interface{}{"chat_id": "123"}) || !reflect.DeepEqual(names, []string{"api_url", "bot_token"}) {
		t.Fatalf("public=%#v configured=%#v", public, names)
	}
}
