package rongcloud

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func nullString(v string) sql.NullString {
	return sql.NullString{String: v, Valid: v != ""}
}

func TestWebLoginTicketHash(t *testing.T) {
	t.Run("稳定且是 64 位小写 hex", func(t *testing.T) {
		got := WebLoginTicketHash("ticket-abc")
		if got != WebLoginTicketHash("ticket-abc") {
			t.Fatalf("同一输入两次哈希不一致：%q", got)
		}
		if len(got) != 64 {
			t.Fatalf("sha256 hex 应为 64 字符，实际 %d（%q）", len(got), got)
		}
		for _, r := range got {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("哈希含非 hex 字符 %q：%q", r, got)
			}
		}
	})

	t.Run("不同票据不碰撞（含只差一个字符）", func(t *testing.T) {
		a := WebLoginTicketHash("ticket-abc")
		b := WebLoginTicketHash("ticket-abd")
		if a == b {
			t.Fatal("只差一个字符的两张票据哈希相同")
		}
	})

	t.Run("空串也有稳定哈希（调用方应先拒绝空票据，这里只保证不 panic）", func(t *testing.T) {
		if got := WebLoginTicketHash(""); len(got) != 64 {
			t.Fatalf("空票据哈希长度异常：%d", len(got))
		}
	})
}

func TestWebLoginSessionTTL(t *testing.T) {
	// 与 internal/handler 的 clawWebLoginSessionTTL 必须同值。两边漂移会让
	// 「Web 端显示已过期、移动端仍能确认」同时出现，很难查。
	if WebLoginSessionTTL != 180*time.Second {
		t.Fatalf("TTL 必须是 180s（旧 Python SESSION_TTL_SECONDS），实际 %s", WebLoginSessionTTL)
	}
}

func TestDecideWebLoginConfirmation(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	past := now.Add(-time.Second)

	tests := []struct {
		name      string
		casWon    bool
		row       *webLoginSessionRow
		userID    string
		wantCode  int
		wantOK    bool
		wantState string
	}{
		{
			name:      "CAS 命中即成功，不再看行",
			casWon:    true,
			row:       nil,
			userID:    "u1",
			wantCode:  200,
			wantOK:    true,
			wantState: "confirmed",
		},
		{
			name:     "票据不存在 → 404",
			casWon:   false,
			row:      nil,
			userID:   "u1",
			wantCode: 404,
			wantOK:   false,
		},
		{
			name:     "已过期 → 410（即便状态还是 pending）",
			casWon:   false,
			row:      &webLoginSessionRow{Status: "pending", ExpiresAt: past},
			userID:   "u1",
			wantCode: 410,
			wantOK:   false,
		},
		{
			name:     "过期边界：expires_at 恰好等于 now 也算过期",
			casWon:   false,
			row:      &webLoginSessionRow{Status: "confirmed", UserID: nullString("u1"), ExpiresAt: now},
			userID:   "u1",
			wantCode: 410,
			wantOK:   false,
		},
		{
			name:      "同一账号重复确认 → 幂等成功",
			casWon:    false,
			row:       &webLoginSessionRow{Status: "confirmed", UserID: nullString("u1"), ExpiresAt: future},
			userID:    "u1",
			wantCode:  200,
			wantOK:    true,
			wantState: "confirmed",
		},
		{
			name:     "另一个账号来确认已被确认的票 → 409",
			casWon:   false,
			row:      &webLoginSessionRow{Status: "confirmed", UserID: nullString("u1"), ExpiresAt: future},
			userID:   "u2",
			wantCode: 409,
			wantOK:   false,
		},
		{
			name:     "confirmed 但 user_id 为空（脏数据）→ 409，不给任何账号开口子",
			casWon:   false,
			row:      &webLoginSessionRow{Status: "confirmed", UserID: sql.NullString{}, ExpiresAt: future},
			userID:   "u1",
			wantCode: 409,
			wantOK:   false,
		},
		{
			name:     "已被 Web 兑换（consumed）→ 409",
			casWon:   false,
			row:      &webLoginSessionRow{Status: "consumed", UserID: nullString("u1"), ExpiresAt: future},
			userID:   "u1",
			wantCode: 409,
			wantOK:   false,
		},
		{
			name:     "已标 expired → 409（未过期的时间里出现这种状态说明被别处标过）",
			casWon:   false,
			row:      &webLoginSessionRow{Status: "expired", ExpiresAt: future},
			userID:   "u1",
			wantCode: 409,
			wantOK:   false,
		},
		{
			name:     "CAS 输了但行仍是 pending → 409（被并发的另一次确认抢先）",
			casWon:   false,
			row:      &webLoginSessionRow{Status: "pending", ExpiresAt: future},
			userID:   "u1",
			wantCode: 409,
			wantOK:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideWebLoginConfirmation(tc.casWon, tc.row, tc.userID, now)
			if got.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d（message=%q）", got.Code, tc.wantCode, got.Message)
			}
			if got.OK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", got.OK, tc.wantOK)
			}
			if tc.wantOK {
				if got.Status != tc.wantState {
					t.Fatalf("status = %q, want %q", got.Status, tc.wantState)
				}
				if got.Message == "" {
					t.Fatal("成功路径必须有可读 message")
				}
			} else if got.Message == "" {
				t.Fatal("失败路径必须有可读 message（客户端会直接 toast 它）")
			}
		})
	}
}
