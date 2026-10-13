package handler

import (
	"errors"
	"testing"
)

// 登录必须复用库内存量 token：rongcloud_token 同时是 /api/claw/* 的 Bearer
// 会话凭据，换发会踢掉其它已登录端，圆桌检测随之失败。这里锁死「有存量就不
// 申请新 token」这一行为，防止回归成「每次登录都换发」。
func TestClawResolveLoginTokenReusesStored(t *testing.T) {
	mintCalls := 0
	token, minted := clawResolveLoginToken("stored-token", func() (string, error) {
		mintCalls++
		return "fresh-token", nil
	})

	if token != "stored-token" {
		t.Fatalf("want stored-token, got %q", token)
	}
	if minted {
		t.Fatal("reusing a stored token must not flag a write-back")
	}
	if mintCalls != 0 {
		t.Fatalf("stored token must skip the mint call, got %d calls", mintCalls)
	}
}

func TestClawResolveLoginTokenTrimsStored(t *testing.T) {
	token, minted := clawResolveLoginToken("  padded-token  ", func() (string, error) {
		t.Fatal("a whitespace-padded stored token still counts as present")
		return "", nil
	})

	if token != "padded-token" || minted {
		t.Fatalf("want trimmed stored token without mint, got %q/%t", token, minted)
	}
}

func TestClawResolveLoginTokenMintsWhenMissing(t *testing.T) {
	for _, stored := range []string{"", "   "} {
		token, minted := clawResolveLoginToken(stored, func() (string, error) {
			return "fresh-token", nil
		})
		if token != "fresh-token" || !minted {
			t.Fatalf("stored=%q: want fresh-token and write-back, got %q/%t", stored, token, minted)
		}
	}
}

func TestClawResolveLoginTokenMintFailure(t *testing.T) {
	token, minted := clawResolveLoginToken("", func() (string, error) {
		return "", errors.New("rongcloud unreachable")
	})

	if token != "" || minted {
		t.Fatalf("failed mint must yield empty/unminted, got %q/%t", token, minted)
	}
}

func TestClawResolveLoginTokenBlankMintResult(t *testing.T) {
	token, minted := clawResolveLoginToken("", func() (string, error) {
		return "   ", nil
	})

	if token != "" || minted {
		t.Fatalf("blank mint result must not be written back, got %q/%t", token, minted)
	}
}
