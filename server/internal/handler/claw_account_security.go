package handler

// claw_account_security.go：账号安全相关的三个 C 端接口。
//
// 背景：账号此前只有「用户名 + 密码」一条命，密码一旦忘了就永久登不进来
// （微信-only 账号连密码都没有），手机号/邮箱两列虽然存在，却只被当作展示资料 ——
// 既不能在登录时使用，也没人保证唯一，谈不上「找回途径」。
//
// 这里补上三条，全部要求 **Bearer 登录态**（`clawRequireClawUser`，凭据是
// claw_im_users.rongcloud_token，与 /api/user/friends 同一套）：
//
//	POST /api/user/password     改密码（有密码要验旧密码；微信-only 账号直接设置）
//	POST /api/user/binding      绑定/换绑手机号、邮箱（格式校验 + 全局唯一）
//	POST /api/user/wechat-bind  已登录账号把微信 openid 绑到自己名下
//
// 三条接口的**共同前提**是「操作的是自己」：userID 只从 token 解出，请求体里
// 不接受 userId —— 老的 /api/user/update 就是拿 body 里的 username 直接改库，
// 任何人知道你的用户名就能改你的资料，这个坑不能再复制一遍。
//
// 关于「忘记密码」的边界：本项目没有短信/邮件网关（没有 SMTP，也没有短信
// 服务商配置），所以这里**不做**验证码式重置 —— 造一个「点了没反应」的入口比
// 没有入口更糟。可行的找回路径是「绑过微信 → 用微信登录 → 直接设置新密码」，
// 没有微信的账号由管理员在后台重置。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
)

const (
	// clawPasswordMinLen 与注册保持一致（/api/register 的 6 位下限），
	// 两处不同会让「注册能过、改密过不了」变成玄学。
	clawPasswordMinLen = 6
	// clawPasswordMaxLen 只是防呆：pbkdf2 对超长输入不会崩，但没有上限时
	// 一个 1MB 的密码就能让单次请求的 CPU 成本失控。
	clawPasswordMaxLen = 128
)

// 手机号只认中国大陆 11 位（与前端输入框提示一致）。邮箱用宽松校验：
// 严格 RFC 5322 既拦不住假地址，又经常误伤合法地址。
var (
	clawPhonePattern = regexp.MustCompile(`^1[3-9]\d{9}$`)
	clawEmailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
)

// ClawUserPassword POST /api/user/password
//
// 请求：{"oldPassword": "...", "newPassword": "..."}
// 回包：{"hasPassword": true}
//
// 微信-only 账号（哨兵哈希）允许**不传** oldPassword 直接设置新密码 —— 这类账号
// 从来没有密码，要求旧密码等于永久锁死改密入口。反过来，真正设过密码的账号一定
// 要验旧密码：否则拿到一个未过期的 Bearer token 就能直接改掉别人的密码。
func (h *Handler) ClawUserPassword(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	var req struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		clawJSON(w, 400, 400, "请求参数错误", nil)
		return
	}
	newPassword := req.NewPassword
	if len(newPassword) < clawPasswordMinLen {
		clawJSON(w, 400, 400, "新密码长度至少6位", nil)
		return
	}
	if len(newPassword) > clawPasswordMaxLen {
		clawJSON(w, 400, 400, "新密码过长", nil)
		return
	}
	if strings.TrimSpace(newPassword) != newPassword {
		// 首尾空格几乎总是复制粘贴带进来的，静默 trim 会让用户下次登录时
		// 明明「看着一样」却进不来。
		clawJSON(w, 400, 400, "密码首尾不能有空格", nil)
		return
	}

	ctx := r.Context()
	u, err := h.clawQueryUser(ctx, "user_id", userID)
	if err != nil {
		clawJSON(w, 404, 404, "用户不存在", nil)
		return
	}
	if clawHasPassword(u.PasswordHash) {
		if req.OldPassword == "" {
			clawJSON(w, 400, 400, "请输入原密码", nil)
			return
		}
		if !clawVerifyPassword(req.OldPassword, u.PasswordHash) {
			clawJSON(w, 400, 400, "原密码不正确", nil)
			return
		}
		if clawVerifyPassword(newPassword, u.PasswordHash) {
			// 新旧相同不算错误，但也不能假装改成功 —— 用户会以为换了密码。
			clawJSON(w, 400, 400, "新密码不能与原密码相同", nil)
			return
		}
	}

	if _, err := h.DB.Exec(ctx,
		"UPDATE claw_im_users SET password_hash=$1, updated_at=now() WHERE user_id=$2",
		clawHashPassword(newPassword), userID); err != nil {
		clawJSON(w, 500, 500, "保存新密码失败", nil)
		return
	}
	// 不轮换 rongcloud_token：它是客户端 IM 的建连凭据，换掉会把该账号在其它端
	// （小程序 / 别的浏览器）的会话全部踢下线。改密码不等于「退出所有设备」。
	clawJSON(w, 200, 200, "密码已更新", map[string]interface{}{"hasPassword": true})
}

// ClawUserBinding POST /api/user/binding
//
// 请求：{"phone": "13800000000"} / {"email": "a@b.com"} / 两者同传
// 传空串 = 解绑该字段（前端「更换」时先清后填也依赖这个语义）。
// 回包：完整用户 JSON（含 hasPassword / wechatBound）。
func (h *Handler) ClawUserBinding(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	// 用指针区分「没传这个字段」和「传了空串要解绑」：前者不动库，后者写 NULL。
	var req struct {
		Phone *string `json:"phone"`
		Email *string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		clawJSON(w, 400, 400, "请求参数错误", nil)
		return
	}
	if req.Phone == nil && req.Email == nil {
		clawJSON(w, 400, 400, "请提供要绑定的手机号或邮箱", nil)
		return
	}

	ctx := r.Context()
	sets := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if req.Phone != nil {
		phone := strings.TrimSpace(*req.Phone)
		if phone != "" {
			if !clawPhonePattern.MatchString(phone) {
				clawJSON(w, 400, 400, "手机号格式不正确", nil)
				return
			}
			taken, err := h.clawIdentifierTaken(ctx, "phone", phone, userID)
			if err != nil {
				clawJSON(w, 500, 500, err.Error(), nil)
				return
			}
			if taken {
				clawJSON(w, 409, 409, "该手机号已绑定其他账号", nil)
				return
			}
		}
		args = append(args, phone)
		sets = append(sets, "phone = NULLIF($"+strconv.Itoa(len(args))+", '')")
	}
	if req.Email != nil {
		email := strings.TrimSpace(*req.Email)
		if email != "" {
			if !clawEmailPattern.MatchString(email) {
				clawJSON(w, 400, 400, "邮箱格式不正确", nil)
				return
			}
			taken, err := h.clawIdentifierTaken(ctx, "email", email, userID)
			if err != nil {
				clawJSON(w, 500, 500, err.Error(), nil)
				return
			}
			if taken {
				clawJSON(w, 409, 409, "该邮箱已绑定其他账号", nil)
				return
			}
		}
		args = append(args, email)
		sets = append(sets, "email = NULLIF($"+strconv.Itoa(len(args))+", '')")
	}
	args = append(args, userID)
	// phone/email 上的唯一性由「先查后写」保证：这两列**没有**库级唯一索引
	// （历史数据里可能已有重复），加索引需要先清洗存量，不在这次改动范围内。
	if _, err := h.DB.Exec(ctx,
		"UPDATE claw_im_users SET "+strings.Join(sets, ", ")+", updated_at=now() WHERE user_id=$"+strconv.Itoa(len(args)),
		args...); err != nil {
		clawJSON(w, 500, 500, "保存绑定信息失败", nil)
		return
	}
	updated, err := h.clawQueryUser(ctx, "user_id", userID)
	if err != nil {
		clawJSON(w, 500, 500, err.Error(), nil)
		return
	}
	clawJSON(w, 200, 200, "已保存", clawUserJSON(updated))
}

// clawIdentifierTaken 判断手机号/邮箱是否已被**别人**占用。
//
// column 只接受本文件内的两个常量，不接受外部输入（拼接进 SQL 的只有列名，
// 值走参数）。自己已绑同一个号不算占用，否则「重新保存一次」会报冲突。
func (h *Handler) clawIdentifierTaken(ctx context.Context, column, value, selfID string) (bool, error) {
	var exists bool
	var err error
	if column == "email" {
		err = h.DB.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM claw_im_users WHERE lower(email)=lower($1) AND user_id<>$2)",
			value, selfID).Scan(&exists)
	} else {
		err = h.DB.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM claw_im_users WHERE phone=$1 AND user_id<>$2)",
			value, selfID).Scan(&exists)
	}
	return exists, err
}

// ClawUserWeChatBind POST /api/user/wechat-bind
//
// 请求：{"code": "<wx.login 拿到的 code>"}
// 回包：{"wechatBound": true, "alreadyBound": bool}
//
// 场景：老账号（用户名 + 密码注册的）在小程序里已经登录，想把微信绑上来，
// 这样以后忘记密码还能用「微信登录」进来。wxLogin 那条路只认 openid → 建号/登录，
// 不会把 openid 挂到一个已登录的账号上，所以必须单开一条。
//
// 冲突一律 409、不回落到「登录另一个号」：openid 已经属于另一个用户时静默绑定
// 等于把两个账号的数据混在一起。
func (h *Handler) ClawUserWeChatBind(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		clawJSON(w, 400, 400, "请求参数错误", nil)
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		clawJSON(w, 400, 400, "缺少微信登录凭证 code", nil)
		return
	}

	ctx := r.Context()
	openID, err := rongcloud.WeChatOpenIDForCode(ctx, "", nil, code)
	if err != nil {
		var biz *rongcloud.WeChatLoginError
		if errors.As(err, &biz) {
			clawJSON(w, biz.Status, biz.Status, biz.Message, nil)
			return
		}
		clawJSON(w, 500, 500, "微信登录服务不可用", nil)
		return
	}

	u, err := h.clawQueryUser(ctx, "user_id", userID)
	if err != nil {
		clawJSON(w, 404, 404, "用户不存在", nil)
		return
	}
	if u.WeChatOpenID == openID {
		clawJSON(w, 200, 200, "已绑定该微信", map[string]interface{}{
			"wechatBound": true, "alreadyBound": true,
		})
		return
	}
	// 这个 openid 是否已经属于别人（包括「本来就是另一个微信账号登录建出来的号」）。
	var otherID string
	err = h.DB.QueryRow(ctx,
		"SELECT user_id FROM claw_im_users WHERE wechat_openid=$1 AND user_id<>$2 LIMIT 1",
		openID, userID).Scan(&otherID)
	if err == nil {
		clawJSON(w, 409, 409, "该微信已绑定其他虾说账号", nil)
		return
	}
	if err != pgx.ErrNoRows {
		clawJSON(w, 500, 500, "查询微信绑定失败", nil)
		return
	}

	if _, err := h.DB.Exec(ctx,
		"UPDATE claw_im_users SET wechat_openid=$1, updated_at=now() WHERE user_id=$2",
		openID, userID); err != nil {
		// 唯一索引兜底并发：两个账号同时绑同一个 openid 时，先到的成功，
		// 后到的在这里撞唯一键 —— 对外仍是 409，不会写进两条。
		clawJSON(w, 409, 409, "该微信已绑定其他虾说账号", nil)
		return
	}
	clawJSON(w, 200, 200, "微信已绑定", map[string]interface{}{
		"wechatBound": true, "alreadyBound": false,
	})
}
