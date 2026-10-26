package rongcloud

// web_login_command.go 实现 service=user 的 action=confirmWebLogin
// —— Web 扫码登录里「移动端确认」那一半。
//
// 调用点：clawmessenger-uniapp 两个扫码入口都会识别
// `{type:'web_login', ticket}` 并调 `userService.confirmWebLogin(ticket)`
// （pages/conversation/index.vue:299、subPackages/social/pages/search-user/index.vue:564）。
//
// 为什么要有这个文件：Web 端的三条 HTTP 路由（internal/handler/claw_web_qr_login.go）
// 只负责「发牌」和「兑现」，真正的授权动作必须由**已登录的移动端账号**发出 ——
// 否则任何拿到 ticket 的人（二维码被人拍照外传）都能把自己登进别人账号。
// 少了这个 action，Web 端会一直轮询到 180 秒过期，用户看到的现象就是「扫了没反应」。
//
// 状态机与旧 Python web_qr_login.confirm_web_login 一致：
//
//	pending   + 未过期 → confirmed（写 user_id / confirmed_at）
//	confirmed + 同一账号 → 幂等返回成功（用户手滑点两次不该报错）
//	confirmed + 另一账号 → 409
//	consumed / expired   → 409 / 410
//	查不到                → 404
//
// 这里不用事务：db.DBTX 没有 Begin（pkg/db/generated/db.go:14），所以确认走
// 「先 CAS、没命中再回查原因」两步。两次并发确认的最坏结果是错误码不够精确，
// 但**状态本身不会出错** —— CAS 的 WHERE 已经把 pending 卡死了。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// WebLoginSessionTTL 与 internal/handler 的 clawWebLoginSessionTTL 必须一致
// （都是 180 秒）。两个包各自持有常量是因为签发在 handler、确认在校验过期，
// 各用各的；值一旦漂移，会出现「Web 端已经显示过期、移动端仍能确认」的错位。
const WebLoginSessionTTL = 180 * time.Second

// WebLoginTicketHash 把票据原文折成落库用的哈希。
//
// **只定义这一份**：internal/handler 的签发/查询/兑换都调它，这样
// 「算哈希」不会出现第二个版本 —— 两边算法一旦漂移，扫码确认会稳定 404，
// 而日志里只有一句「登录二维码无效」，极难定位。
func WebLoginTicketHash(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

// WebLoginConfirmResult 描述一次确认的结局。Code 直接用 HTTP 语义的数字，
// 调用方原样喂给 replyErr，不再做一层映射。
type WebLoginConfirmResult struct {
	OK      bool
	Code    int
	Status  string
	Message string
}

// WebLoginSessionStore 是 im_web_login_sessions（migrations/604）的确认侧读写。
type WebLoginSessionStore struct {
	// Confirm 把一个 pending 会话置成 confirmed，并绑定到 userID。
	Confirm func(ctx context.Context, ticketHash, userID string) (WebLoginConfirmResult, error)
}

// webLoginSessionRow 是回查出来的一行快照（只读判断所需的三列）。
type webLoginSessionRow struct {
	Status    string
	UserID    sql.NullString
	ExpiresAt time.Time
}

// decideWebLoginConfirmation 是确认动作的**全部判定**，纯函数、不碰 DB。
//
// 抽出来的理由：真链路要连库才能跑，而这里有 5 个分支（含「同一账号重复确认
// 应幂等」这种只有手滑才会走到的路径）。把判定和 IO 分开，分支才能在无库的
// CI 上被逐条钉住 —— 否则这段代码只会在真机上被验证，而真机验证不了 409/410。
//
// casWon=true 表示 UPDATE 已经命中（pending 且未过期），此时不需要再看行。
func decideWebLoginConfirmation(casWon bool, row *webLoginSessionRow, userID string, now time.Time) WebLoginConfirmResult {
	if casWon {
		return WebLoginConfirmResult{OK: true, Code: 200, Status: "confirmed", Message: "登录已确认"}
	}
	if row == nil {
		return WebLoginConfirmResult{Code: 404, Message: "登录二维码无效"}
	}
	if !row.ExpiresAt.After(now) {
		return WebLoginConfirmResult{Code: 410, Message: "登录二维码已过期"}
	}
	// 同一账号重复确认：幂等成功。用户手滑点两次、或网络重试，都不该报错。
	if row.Status == "confirmed" && row.UserID.Valid && row.UserID.String == userID {
		return WebLoginConfirmResult{OK: true, Code: 200, Status: "confirmed", Message: "登录已确认"}
	}
	// 剩下的情况：consumed（Web 已兑换）、expired、confirmed 但换了账号，
	// 以及「CAS 输了但还没过期」—— 那条被别人抢先确认了，对本次调用同样是失败。
	return WebLoginConfirmResult{Code: 409, Message: "登录二维码已使用"}
}

func NewWebLoginSessionStore(dbtx db.DBTX, logger *slog.Logger) *WebLoginSessionStore {
	if dbtx == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &WebLoginSessionStore{
		Confirm: func(ctx context.Context, ticketHash, userID string) (WebLoginConfirmResult, error) {
			// 第一步：CAS。只有「仍是 pending 且未过期」才写得动。
			tag, err := dbtx.Exec(ctx, `
				UPDATE im_web_login_sessions
				SET status = 'confirmed', user_id = $2, confirmed_at = now()
				WHERE ticket_hash = $1 AND status = 'pending' AND expires_at > now()`,
				ticketHash, userID)
			if err != nil {
				return WebLoginConfirmResult{}, err
			}
			now := time.Now()
			if tag.RowsAffected() == 1 {
				return decideWebLoginConfirmation(true, nil, userID, now), nil
			}

			// 第二步：没写动，回查到底是什么原因。
			var row webLoginSessionRow
			err = dbtx.QueryRow(ctx,
				"SELECT status, user_id, expires_at FROM im_web_login_sessions WHERE ticket_hash = $1",
				ticketHash,
			).Scan(&row.Status, &row.UserID, &row.ExpiresAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return decideWebLoginConfirmation(false, nil, userID, now), nil
			}
			if err != nil {
				return WebLoginConfirmResult{}, err
			}

			result := decideWebLoginConfirmation(false, &row, userID, now)
			// 惰性过期，与查询接口保持同一套口径。已经终态的不再改写。
			if result.Code == 410 {
				if _, err := dbtx.Exec(ctx,
					"UPDATE im_web_login_sessions SET status = 'expired' WHERE ticket_hash = $1",
					ticketHash,
				); err != nil {
					logger.Warn("rongcloud: mark web login session expired", "error", err)
				}
			}
			return result, nil
		},
	}
}

func (h *systemHandler) handleConfirmWebLogin(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	if h.webLoginSessions == nil {
		return h.replyErr(ctx, msg, cmd, 503, "web login service is unavailable")
	}
	actor, err := h.serviceActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
	}
	ticket := strings.TrimSpace(getStringAny(cmd.Params, "ticket"))
	if ticket == "" {
		return h.replyErr(ctx, msg, cmd, 400, "ticket is required")
	}
	result, err := h.webLoginSessions.Confirm(ctx, WebLoginTicketHash(ticket), actor)
	if err != nil {
		h.logger.Error("rongcloud: confirm web login", "error", err, "from", actor)
		return h.replyErr(ctx, msg, cmd, 500, "failed to confirm the web login")
	}
	if !result.OK {
		return h.replyErr(ctx, msg, cmd, result.Code, result.Message)
	}
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{"status": result.Status})
}
