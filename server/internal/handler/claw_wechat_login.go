// Package handler —— 微信小程序登录的 HTTP 通道。
//
// 背景：微信手机号登录（getPhoneNumber → wx.login）发生在「用户还没有任何
// 凭据」的时刻，而融云 command 通道（service=user/action=wxLogin）的前提是
// 「已经连上 IM」。只有 command 通道时构成死锁：客户端 sendRequest 先做
// checkConnection，未登录连不上 → 等 10 秒 → 抛「融云连接失败，请检查网络」。
// 密码登录不受影响，它直接走 POST /api/login。
//
// 因此这里把同一份业务逻辑（internal/integrations/rongcloud/wechat_login.go
// 的 WeChatLogin）暴露成 HTTP，客户端不必先连 IM。
//
// 回包形状刻意对齐 ClawLogin（claw_compat.go）：
//
//	{code:200, data:{username, nickname, portraitUri, token, userId, isNewUser}}
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
)

// ClawWeChatLogin POST /api/wechat/login
//
// 入参：{"code": "<wx.login 的 code>", "phoneCode": "<getPhoneNumber 的 code，可选>"}
// — phoneCode 与 command 通道一致地暂不使用：换手机号需要单独的
// access_token + wxa/business/getuserphonenumber 流程，本服务不做静默绑定。
//
// 状态码语义（由业务层决定，原样回传）：
//
//	400 code 缺失 / 微信拒绝（code 过期或复用）
//	401 账号被禁用
//	500 微信接口不可达 / 建号失败 / IM token 换发失败
//	503 WECHAT_MP_APPID / WECHAT_MP_SECRET 未配置
//
// 公开端点，与 /api/login 同级：不校验会话（正是为了拿会话）。微信 code 一次性
// 且 5 分钟有效，无法重放；如需限流应放在网关层，而不是在这里堆业务判断。
func (h *Handler) ClawWeChatLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code      string `json:"code"`
		PhoneCode string `json:"phoneCode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		clawJSON(w, 400, 400, "请求参数错误", nil)
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		clawJSON(w, 400, 400, "缺少微信登录 code", nil)
		return
	}

	ctx := r.Context()
	deps := rongcloud.WeChatLoginDeps{
		Store:      rongcloud.NewUserStore(h.DB),
		Logger:     slog.Default(),
		APIBase:    h.wechatAPIBase,
		HTTPClient: h.wechatHTTPClient,
	}
	// 融云未配置时把 MintToken 留空：库内已有 token 的存量账号仍能登录成功，
	// 只有需要新换发 token 的新账号才会拿到 503「user service is unavailable」。
	if rc := h.clawRong(ctx); rc != nil {
		deps.MintToken = func(ctx context.Context, userID, name, portraitURI string) (string, error) {
			// 与 ClawLogin 一致：昵称为空时推账号名，否则 mint 会失败。
			// 微信账号的 username 恒等于 user_id，所以这里用 userID 兜底。
			return rc.getUserToken(ctx, userID, clawRongDisplayName(name, userID), portraitURI)
		}
	}

	result, err := rongcloud.WeChatLogin(ctx, deps, code)
	if err != nil {
		var loginErr *rongcloud.WeChatLoginError
		if errors.As(err, &loginErr) {
			clawJSON(w, loginErr.Status, loginErr.Status, loginErr.Message, nil)
			return
		}
		slog.Error("claw: wechat login failed", "error", err)
		clawJSON(w, 500, 500, "微信登录失败，请稍后重试", nil)
		return
	}

	clawJSON(w, 200, 200, "登录成功", map[string]interface{}{
		"username":    result.Username,
		"nickname":    result.Nickname,
		"portraitUri": result.PortraitURI,
		"token":       result.Token,
		"userId":      result.UserID,
		"isNewUser":   result.IsNewUser,
	})
}
