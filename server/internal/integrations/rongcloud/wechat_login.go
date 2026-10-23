package rongcloud

// wechat_login.go 把「微信小程序 code → 本地账号 → IM token」抽成唯一实现，
// 供两个通道共用：
//
//  1. 融云 command：service=user / action=wxLogin（客户端已连上 IM 时可用）；
//  2. HTTP：POST /api/wechat/login（客户端尚未连上 IM 时）。
//
// 为什么必须两条通道都有：微信手机号登录发生在「登录之前」，此时客户端还没有
// 任何凭据去连融云，而 command 通道的前提正是「已连接」。只有 command 时，
// 「登录需要连接、连接需要登录」构成死锁 —— 2026-10-23 线上表现就是
// `sendRequest('user','wxLogin')` 先做 checkConnection，等 10 秒后抛
// 「融云连接失败，请检查网络」。密码登录不受影响，因为它直接走 HTTP
// POST /api/login。
//
// 本文件只放与传输无关的业务逻辑：错误用 WeChatLoginError 携带 HTTP 语义，
// 由各通道自行映射（command 通道 → replyErr，HTTP 通道 → clawJSON）。
// 错误消息文案与抽取前逐字一致，命令通道的既有测试依赖它们。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// WeChatLoginDeps 是 WeChatLogin 的依赖集合。
type WeChatLoginDeps struct {
	// Store 是 claw_im_users 的读写实现，nil 时回 503。
	Store *UserStore
	// MintToken 在库内没有 IM token 时换发；nil 表示融云侧未配置（回 503）。
	MintToken func(ctx context.Context, userID, name, portraitURI string) (string, error)
	// Logger 为 nil 时用 slog.Default()。
	Logger *slog.Logger
	// APIBase / HTTPClient 仅测试注入：空值走 https://api.weixin.qq.com
	// 与 8s 超时客户端。
	APIBase    string
	HTTPClient *http.Client
}

// WeChatLoginResult 是登录成功的结果，字段与 HTTP /api/login 的回包对齐
// （{userId, username, nickname, portraitUri, token}）。
type WeChatLoginResult struct {
	UserID      string
	Username    string
	Nickname    string
	PortraitURI string
	Token       string
	IsNewUser   bool
}

// WeChatLoginError 是带传输语义的业务错误。
//
// Status 取 400（code 无效/微信拒绝）、401（账号被禁用）、500（服务端或融云
// 故障）、503（未配置/依赖不可用）。两个通道都按 Status 原样回给客户端，
// 这样 uniapp 能区分「配置没开」和「网络抖动」。
type WeChatLoginError struct {
	Status  int
	Message string
}

func (e *WeChatLoginError) Error() string { return e.Message }

func newWeChatLoginError(status int, message string) *WeChatLoginError {
	return &WeChatLoginError{Status: status, Message: message}
}

// WeChatLogin 用 wx.login 的 code 完成登录：code 换 openid → 查/建本地账号 →
// 复用或换发 IM token。
//
// openid 绑定策略：
//  1. openid 已存在 → 直接登录该账号（不重建、不改用户名）；
//  2. 不存在 → 新建 claw_im_users 记录，密码列写入不可解析的哨兵哈希
//     （wechatOnlyPasswordHash），这类账号不能用密码登录；
//  3. token 优先复用库内 rongcloud_token（避免把其它端踢下线），仅在缺失时
//     换发并回写。
func WeChatLogin(ctx context.Context, deps WeChatLoginDeps, code string) (*WeChatLoginResult, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	appID := strings.TrimSpace(os.Getenv(wechatOpenIDEnvAppID))
	appSecret := strings.TrimSpace(os.Getenv(wechatOpenIDEnvSecret))
	if appID == "" || appSecret == "" {
		logger.Warn("rongcloud: wechat login requested but not configured",
			"has_appid", appID != "", "has_secret", appSecret != "")
		return nil, newWeChatLoginError(503, "微信登录未配置：缺少 WECHAT_MP_APPID / WECHAT_MP_SECRET")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, newWeChatLoginError(400, "code is required")
	}
	if deps.Store == nil {
		return nil, newWeChatLoginError(503, "user service is unavailable")
	}

	openID, err := wechatCode2Session(ctx, deps.APIBase, deps.HTTPClient, appID, appSecret, code)
	if err != nil {
		var apiErr *wechatAPIError
		if errors.As(err, &apiErr) {
			// code 一次性、5 分钟有效：过期/复用时微信在这里回 40163/40029。
			logger.Warn("rongcloud: wechat jscode2session rejected",
				"errcode", apiErr.code, "errmsg", apiErr.message)
			return nil, newWeChatLoginError(400, "微信登录失败："+apiErr.message)
		}
		logger.Error("rongcloud: wechat jscode2session", "error", err)
		return nil, newWeChatLoginError(500, "failed to reach the WeChat login service")
	}

	user, found, err := deps.Store.FindByOpenID(ctx, openID)
	if err != nil {
		logger.Error("rongcloud: wechat login lookup", "error", err)
		return nil, newWeChatLoginError(500, "failed to load the account")
	}
	isNewUser := false
	if !found {
		var created bool
		user, created, err = deps.Store.CreateWeChatUser(ctx, CreateWeChatUserParams{
			OpenID:       openID,
			Nickname:     defaultWeChatNickname(openID),
			PortraitURI:  DefaultPortraitIfEmpty(""),
			PasswordHash: wechatOnlyPasswordHash,
		})
		if err != nil {
			logger.Error("rongcloud: wechat login create account", "error", err)
			return nil, newWeChatLoginError(500, "failed to create the account")
		}
		// created=false 表示并发下另一个请求抢先建了号（ON CONFLICT 回查），
		// 本次只是登录它 —— 不能记成「新建账号」（DEF-4）。
		if created {
			logger.Info("rongcloud: wechat account created", "user_id", user.UserID)
		}
		isNewUser = created
	}
	if user.Status != "" && user.Status != "active" {
		return nil, newWeChatLoginError(401, "账户已被禁用")
	}

	token := strings.TrimSpace(user.RongCloudToken)
	if token == "" {
		if deps.MintToken == nil {
			return nil, newWeChatLoginError(503, "user service is unavailable")
		}
		minted, err := deps.MintToken(ctx, user.UserID, user.Nickname, user.PortraitURI)
		if err != nil || strings.TrimSpace(minted) == "" {
			logger.Error("rongcloud: wechat login mint IM token", "error", err, "user_id", user.UserID)
			return nil, newWeChatLoginError(500, "微信登录成功，但获取 IM Token 失败")
		}
		token = strings.TrimSpace(minted)
		if err := deps.Store.SaveRongCloudToken(ctx, user.UserID, token); err != nil {
			// 回写失败不影响本次登录（token 已经拿到），但要留下日志：
			// 下次登录会再向融云换发，把其它端的旧 token 顶掉。
			logger.Warn("rongcloud: wechat login persist IM token", "error", err, "user_id", user.UserID)
		}
	}

	return &WeChatLoginResult{
		UserID:      user.UserID,
		Username:    user.Username,
		Nickname:    user.Nickname,
		PortraitURI: DefaultPortraitIfEmpty(user.PortraitURI),
		Token:       token,
		IsNewUser:   isNewUser,
	}, nil
}

// wechatCode2Session 用 wx.login 的 code 换 openid。
//
// 微信对业务错误也返回 HTTP 200，只有 errcode 区分成功/失败，所以必须读
// errcode；网络/解析失败按 500 处理（不是调用方的错）。
func wechatCode2Session(ctx context.Context, apiBase string, client *http.Client, appID, appSecret, code string) (string, error) {
	base := strings.TrimSpace(apiBase)
	if base == "" {
		base = defaultWeChatAPIBase
	}
	endpoint := strings.TrimRight(base, "/") + "/sns/jscode2session?" + url.Values{
		"appid":      {appID},
		"secret":     {appSecret},
		"js_code":    {code},
		"grant_type": {"authorization_code"},
	}.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, weChatHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		// url.Parse 的失败也是 *url.Error，同样会带上含 secret 的整条 URL。
		return "", sanitizeWeChatTransportError(err)
	}
	if client == nil {
		client = &http.Client{Timeout: weChatHTTPTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", sanitizeWeChatTransportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("wechat jscode2session: unexpected status %d", resp.StatusCode)
	}
	var parsed wechatSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("wechat jscode2session: decode response: %w", err)
	}
	if parsed.ErrCode != 0 {
		message := strings.TrimSpace(parsed.ErrMsg)
		if message == "" {
			message = fmt.Sprintf("errcode %d", parsed.ErrCode)
		}
		return "", &wechatAPIError{code: parsed.ErrCode, message: message}
	}
	openID := strings.TrimSpace(parsed.OpenID)
	if openID == "" {
		return "", errors.New("wechat jscode2session: empty openid")
	}
	return openID, nil
}
