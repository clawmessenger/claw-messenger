package rongcloud

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestGenerateUserCodeShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code, err := generateUserCode()
		if err != nil {
			t.Fatalf("generateUserCode: %v", err)
		}
		if len(code) != userCodeLen {
			t.Fatalf("length = %d, want %d (%q)", len(code), userCodeLen, code)
		}
		for _, r := range code {
			if !containsRune(userCodeAlphabet, r) {
				t.Fatalf("code %q contains %q outside the user-code alphabet", code, r)
			}
		}
		seen[code] = true
	}
	// 200 draws from a 31^8 space: a duplicate would mean the generator is
	// not actually random (a constant or a broken index would collide).
	if len(seen) != 200 {
		t.Fatalf("only %d distinct codes out of 200 draws", len(seen))
	}
}

func containsRune(alphabet string, r rune) bool {
	for _, a := range alphabet {
		if a == r {
			return true
		}
	}
	return false
}

func TestSessionEffectiveStatus(t *testing.T) {
	now := time.Now()
	past := pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true}
	future := pgtype.Timestamptz{Time: now.Add(time.Minute), Valid: true}
	cases := []struct {
		name    string
		session db.RongcloudPairingSession
		want    string
	}{
		{"pending and live stays pending", db.RongcloudPairingSession{Status: "pending", ExpiresAt: future}, "pending"},
		{"pending and past expires at read time", db.RongcloudPairingSession{Status: "pending", ExpiresAt: past}, "expired"},
		// claimed still honours the window: the device already holds its
		// credentials, but the browser link must die with the code.
		{"claimed and past expires the link", db.RongcloudPairingSession{Status: "claimed", ExpiresAt: past}, "expired"},
		{"claimed and live stays claimed", db.RongcloudPairingSession{Status: "claimed", ExpiresAt: future}, "claimed"},
		{"cancelled is untouched", db.RongcloudPairingSession{Status: "cancelled", ExpiresAt: future}, "cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SessionEffectiveStatus(tc.session, now); got != tc.want {
				t.Fatalf("SessionEffectiveStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDevicePairLimiterWindow(t *testing.T) {
	limiter := NewDevicePairLimiter(3, 50*time.Millisecond)
	for i := 0; i < 3; i++ {
		if !limiter.Allow("machine-a") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if limiter.Allow("machine-a") {
		t.Fatal("4th attempt within the window must be rejected")
	}
	// A different machine has its own budget: the cap is per device, not global.
	if !limiter.Allow("machine-b") {
		t.Fatal("another machine must not inherit the first machine's budget")
	}
	// Once the window slides past the recorded hits, the machine is allowed again.
	time.Sleep(60 * time.Millisecond)
	if !limiter.Allow("machine-a") {
		t.Fatal("budget should recover after the window")
	}
}

func TestDevicePairLimiterPrunesMap(t *testing.T) {
	limiter := NewDevicePairLimiter(1, 20*time.Millisecond)
	// Cross the pruning threshold so the sweep branch actually runs; without
	// it the map would grow one key per anonymous request forever.
	for i := 0; i < 501; i++ {
		limiter.Allow(string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}
	time.Sleep(30 * time.Millisecond)
	limiter.Allow("fresh")
	limiter.mu.Lock()
	size := len(limiter.hits)
	limiter.mu.Unlock()
	if size > 500 {
		t.Fatalf("hits map grew to %d entries, pruning did not run", size)
	}
}
