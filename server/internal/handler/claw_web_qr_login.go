// Package handler —— Web 扫码登录（移植自旧 Python web_qr_login.py + app.py:1133-1210）。
//
// 三条路由，缺一条整条链路就断：
//
//	POST /api/auth/web-qr/sessions                    创建会话（Web 端画二维码）
//	GET  /api/auth/web-qr/sessions/{ticket}            轮询状态
//	POST /api/auth/web-qr/sessions/{ticket}/exchange   兑换登录态
//
// 移动端确认走 IM 命令 `user/confirmWebLogin`（见
// internal/integrations/rongcloud/web_login_command.go），不在这三条里。
//
// **为什么这三个接口曾经 404**：Go 迁移时 `/api/login`、`/api/register` 等
// claw_compat 契约都搬过来了，但这组前缀停在 `/api/auth/`（不是 `/api/` 直挂），
// 扫路由时被漏掉。前端 AuthScreen 一直在调，线上表现为二维码区「加载失败」+
// 底下小字 `Not Found` —— Go 默认 404 的裸文本，人眼很容易当成网络问题。
//
// 安全约定（与 Python 一致，不要放宽）：
//   - 二维码里只有一次性 ticket，**不含用户信息**；票据 180 秒过期；
//   - 库里只存 ticket 的 sha256，不存原文；
//   - 兑换前必须由移动端把状态置成 confirmed，且兑换成功即 consumed（一次性）；
//   - 兑换接口本身不校验 Bearer（它正是用来换取凭据的那一步）。
package handler

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
)

// clawWebLoginSessionTTL 与旧 Python 的 SESSION_TTL_SECONDS 保持一致。
// 二维码生命周期短，用户扫完就走；调大只会放大「扫到了但已经过期」的概率。
const clawWebLoginSessionTTL = 180 * time.Second

// clawWebLoginTimeLayout 是接口里 expiresAt 的序列化格式。
// 前端两处都用 Date.parse 解析（AuthScreen.tsx 的倒计时、小程序 confirmWebLogin
// 的过期判断），RFC3339 两边都认；旧 Python 输出的 +00:00 也在这个解析范围内。
func clawWebLoginExpiresAt(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// clawWebLoginTicketHash 只在这一处转发：真正的算法在 rongcloud 包，
// 因为「确认」比「签发」多一个调用方（IM 命令），两份实现迟早会漂。
func clawWebLoginTicketHash(ticket string) string {
	return rongcloud.WebLoginTicketHash(ticket)
}

// clawNewWebLoginTicket 生成 32 字节随机票据，base64url 无填充
// （等价于 Python 的 secrets.token_urlsafe(32)）。
func clawNewWebLoginTicket() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// clawWebLoginQRContent 拼二维码里真正写入的字符串。
//
// 形状必须与移动端 parseScanResult 的期望一致：
// {type:'web_login', version:1, ticket, expiresAt}。version 预留给以后改载荷。
func clawWebLoginQRContent(ticket string, expiresAt time.Time) string {
	payload, err := json.Marshal(map[string]interface{}{
		"type":      "web_login",
		"version":   1,
		"ticket":    ticket,
		"expiresAt": clawWebLoginExpiresAt(expiresAt),
	})
	if err != nil {
		// 结构全是标量，实际不可能失败；真失败也不该把 ticket 漏出去。
		return ""
	}
	return string(payload)
}

// ClawCreateWebQrLoginSession POST /api/auth/web-qr/sessions
func (h *Handler) ClawCreateWebQrLoginSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ticket, err := clawNewWebLoginTicket()
	if err != nil {
		slog.Error("claw: web qr login generate ticket", "error", err)
		clawJSON(w, 500, 500, "创建登录二维码失败", nil)
		return
	}
	expiresAt := time.Now().UTC().Add(clawWebLoginSessionTTL)

	// 顺手清掉一天前的记录。这张表只服务 180 秒的票据，不清理会无限增长；
	// 清理失败不影响本次创建（宁可留垃圾，也不要让登录流程卡住）。
	if _, err := h.DB.Exec(ctx,
		"DELETE FROM im_web_login_sessions WHERE expires_at < $1",
		time.Now().UTC().Add(-24*time.Hour),
	); err != nil {
		slog.Warn("claw: web qr login cleanup failed", "error", err)
	}

	if _, err := h.DB.Exec(ctx,
		"INSERT INTO im_web_login_sessions (ticket_hash, status, expires_at) VALUES ($1, 'pending', $2)",
		clawWebLoginTicketHash(ticket), expiresAt,
	); err != nil {
		slog.Error("claw: web qr login insert session", "error", err)
		clawJSON(w, 500, 500, "创建登录二维码失败", nil)
		return
	}

	clawJSON(w, 200, 200, "ok", map[string]interface{}{
		"ticket":    ticket,
		"status":    "pending",
		"expiresAt": clawWebLoginExpiresAt(expiresAt),
		"qrContent": clawWebLoginQRContent(ticket, expiresAt),
	})
}

// ClawGetWebQrLoginStatus GET /api/auth/web-qr/sessions/{ticket}
//
// 只回状态与过期时间，**不回用户资料、不回 token** —— 持有 ticket 的人（包括
// 二维码被拍照外传的情况）不该能从这条读到账号信息。
func (h *Handler) ClawGetWebQrLoginStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ticket := strings.TrimSpace(chi.URLParam(r, "ticket"))
	if ticket == "" {
		clawJSON(w, 404, 404, "登录二维码无效", nil)
		return
	}
	hash := clawWebLoginTicketHash(ticket)

	var status string
	var expiresAt time.Time
	err := h.DB.QueryRow(ctx,
		"SELECT status, expires_at FROM im_web_login_sessions WHERE ticket_hash = $1", hash,
	).Scan(&status, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		clawJSON(w, 404, 404, "登录二维码无效", nil)
		return
	}
	if err != nil {
		slog.Error("claw: web qr login read status", "error", err)
		clawJSON(w, 500, 500, "读取登录二维码状态失败", nil)
		return
	}

	// 惰性过期：没有定时任务，查询时补写。已经是终态的两个状态不覆盖。
	if !expiresAt.After(time.Now()) && status != "consumed" && status != "expired" {
		status = "expired"
		if _, err := h.DB.Exec(ctx,
			"UPDATE im_web_login_sessions SET status = 'expired' WHERE ticket_hash = $1", hash,
		); err != nil {
			slog.Warn("claw: web qr login mark expired", "error", err)
		}
	}

	clawJSON(w, 200, 200, "ok", map[string]interface{}{
		"status":    status,
		"expiresAt": clawWebLoginExpiresAt(expiresAt),
	})
}

// ClawExchangeWebQrLogin POST /api/auth/web-qr/sessions/{ticket}/exchange
func (h *Handler) ClawExchangeWebQrLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ticket := strings.TrimSpace(chi.URLParam(r, "ticket"))
	if ticket == "" {
		clawJSON(w, 409, 409, "登录尚未确认或二维码已过期", nil)
		return
	}
	hash := clawWebLoginTicketHash(ticket)

	var userID string
	var expiresAt time.Time
	err := h.DB.QueryRow(ctx,
		"SELECT COALESCE(user_id, ''), expires_at FROM im_web_login_sessions "+
			"WHERE ticket_hash = $1 AND status = 'confirmed'", hash,
	).Scan(&userID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		clawJSON(w, 409, 409, "登录尚未确认或二维码已过期", nil)
		return
	}
	if err != nil {
		slog.Error("claw: web qr login read confirmed session", "error", err)
		clawJSON(w, 500, 500, "登录失败", nil)
		return
	}
	if !expiresAt.After(time.Now()) || userID == "" {
		clawJSON(w, 409, 409, "登录尚未确认或二维码已过期", nil)
		return
	}

	// **先 CAS 消费再换 token**，与旧 Python 的顺序相反，是有意为之：
	// clawResolveLoginToken 在库里没有 token 时会向融云 mint 并覆盖库内值，
	// 把其它已登录端踢下线。两个并发兑换若都走到 mint，就会连着踢两次；
	// 先把票据消费掉，保证「一次票据最多 mint 一次」。
	// 代价是 mint 失败时票据已作废，用户重新扫一次即可。
	tag, err := h.DB.Exec(ctx,
		"UPDATE im_web_login_sessions SET status = 'consumed', consumed_at = now() "+
			"WHERE ticket_hash = $1 AND user_id = $2 AND status = 'confirmed' AND expires_at > now()",
		hash, userID,
	)
	if err != nil {
		slog.Error("claw: web qr login consume session", "error", err)
		clawJSON(w, 500, 500, "登录失败", nil)
		return
	}
	if tag.RowsAffected() != 1 {
		// 并发下被别人先兑换掉了。
		clawJSON(w, 409, 409, "登录二维码已使用或已过期", nil)
		return
	}

	u, err := h.clawQueryUser(ctx, "user_id", userID)
	if err != nil || u.Status != "active" {
		clawJSON(w, 403, 403, "用户不存在或已被禁用", nil)
		return
	}

	// 与 ClawLogin 完全同一条 token 策略（复用而非重发）；见 claw_compat.go:459-465。
	portrait := u.PortraitURI
	if portrait == "" {
		portrait = h.clawDefaultPortraitForNewUser()
		if _, err := h.DB.Exec(ctx,
			"UPDATE claw_im_users SET portrait_uri=$1, updated_at=now() "+
				"WHERE user_id=$2 AND (portrait_uri IS NULL OR portrait_uri='')",
			portrait, u.UserID); err != nil {
			slog.Warn("claw: web qr login backfill portrait", "error", err, "user_id", u.UserID)
		}
		u.PortraitURI = portrait
	}

	stored := ""
	if u.RCStored.Valid {
		stored = u.RCStored.String
	}
	token, minted := clawResolveLoginToken(stored, func() (string, error) {
		rc := h.clawRong(ctx)
		if rc == nil {
			return "", errors.New("rongcloud 未配置")
		}
		return rc.getUserToken(ctx, u.UserID, clawRongDisplayName(u.Nickname, u.Username), u.PortraitURI)
	})
	if minted {
		if _, err := h.DB.Exec(ctx,
			"UPDATE claw_im_users SET rongcloud_token=$1 WHERE user_id=$2", token, u.UserID,
		); err != nil {
			slog.Error("claw: web qr login persist token", "error", err, "user_id", u.UserID)
		}
	}
	if token == "" {
		clawJSON(w, 500, 500, "登录成功，但获取IM Token失败", nil)
		return
	}

	clawJSON(w, 200, 200, "登录成功", map[string]interface{}{
		"username":    u.Username,
		"nickname":    u.Nickname,
		"portraitUri": u.PortraitURI,
		"token":       token,
		"userId":      u.UserID,
	})
}
