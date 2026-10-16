package auth

import (
	"testing"
	"time"
)

func TestGenerateOAuthStateCleansUpExpiredStates(t *testing.T) {
	oauthStatesMu.Lock()
	original := oauthStates
	oauthStates = map[string]time.Time{
		"expired": time.Now().Add(-time.Minute),
		"valid":   time.Now().Add(time.Minute),
	}
	oauthStatesMu.Unlock()
	t.Cleanup(func() {
		oauthStatesMu.Lock()
		oauthStates = original
		oauthStatesMu.Unlock()
	})

	state, err := GenerateOAuthState()
	if err != nil {
		t.Fatal(err)
	}
	oauthStatesMu.Lock()
	_, expired := oauthStates["expired"]
	_, valid := oauthStates["valid"]
	_, generated := oauthStates[state]
	oauthStatesMu.Unlock()
	if expired || !valid || !generated {
		t.Fatalf("state cleanup: expired=%t valid=%t generated=%t", expired, valid, generated)
	}
	if !ValidateOAuthState(state) || ValidateOAuthState(state) {
		t.Fatal("generated state must be accepted exactly once")
	}
}
