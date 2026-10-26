package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
)

func TestClawNewWebLoginTicket(t *testing.T) {
	ticket, err := clawNewWebLoginTicket()
	if err != nil {
		t.Fatalf("生成票据失败：%v", err)
	}
	// 32 字节 base64url 无填充 → 43 字符（与 Python token_urlsafe(32) 同长）。
	if len(ticket) != 43 {
		t.Fatalf("票据长度 = %d，期望 43（32 字节 base64url 无填充）", len(ticket))
	}
	// 必须是 URL-safe 且无填充：这张票会被塞进二维码 JSON、再拼进
	// /sessions/{ticket} 的路径段，带 + / = 会需要额外转义。
	for _, r := range ticket {
		ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			t.Fatalf("票据含非 base64url 字符 %q：%q", r, ticket)
		}
	}

	second, err := clawNewWebLoginTicket()
	if err != nil {
		t.Fatalf("第二次生成失败：%v", err)
	}
	if ticket == second {
		t.Fatal("两次生成的票据相同，随机源有问题")
	}
}

func TestClawWebLoginExpiresAt(t *testing.T) {
	at := time.Date(2026, 10, 26, 12, 34, 56, 0, time.UTC)
	got := clawWebLoginExpiresAt(at)

	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("%q 不是合法的 RFC3339：%v", got, err)
	}
	if !parsed.Equal(at) {
		t.Fatalf("往返后 != 原值：got %s want %s", parsed, at)
	}
	// 前端两处都用 Date.parse 解析（AuthScreen 倒计时、小程序 confirmWebLogin
	// 判断过期），RFC3339 的 Z 后缀它认；带毫秒也能认，但没必要多三个字符。
	if strings.Contains(got, ".") {
		t.Fatalf("expiresAt 不该带小数秒（旧 Python 也不带）：%q", got)
	}
}

func TestClawWebLoginQRContent(t *testing.T) {
	expiresAt := time.Date(2026, 10, 26, 12, 34, 56, 0, time.UTC)
	content := clawWebLoginQRContent("ticket-xyz", expiresAt)

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatalf("qrContent 不是合法 JSON：%v（%q）", err, content)
	}

	// 这些键名是跨端契约：小程序 parseScanResult 按 data.type 分流，再读
	// data.ticket / data.expiresAt（pages/conversation/index.vue:248-249、
	// search-user/index.vue:502）。改名字会让扫码「识别失败」，且两端各自
	// 都觉得自己没错。
	if payload["type"] != "web_login" {
		t.Fatalf("type = %v, want web_login", payload["type"])
	}
	if version, ok := payload["version"].(float64); !ok || version != 1 {
		t.Fatalf("version = %v, want 1（数字，不是字符串）", payload["version"])
	}
	if payload["ticket"] != "ticket-xyz" {
		t.Fatalf("ticket = %v, want ticket-xyz", payload["ticket"])
	}
	if payload["expiresAt"] != clawWebLoginExpiresAt(expiresAt) {
		t.Fatalf("expiresAt = %v, want %s", payload["expiresAt"], clawWebLoginExpiresAt(expiresAt))
	}
}

func TestClawWebLoginTicketHashMatchesCommandSide(t *testing.T) {
	// handler 侧（签发/查询/兑换）与 rongcloud 侧（移动端确认）必须算出同一个值。
	// 两边算法一旦漂移，确认接口会稳定 404「登录二维码无效」，而 Web 端看起来
	// 一切正常（二维码画得出来、状态一直是 pending），极难定位。
	for _, ticket := range []string{"ticket-xyz", "", "带中文的票据", strings.Repeat("a", 64)} {
		if got, want := clawWebLoginTicketHash(ticket), rongcloud.WebLoginTicketHash(ticket); got != want {
			t.Fatalf("票据 %q 两侧哈希不一致：handler=%q rongcloud=%q", ticket, got, want)
		}
	}
}
