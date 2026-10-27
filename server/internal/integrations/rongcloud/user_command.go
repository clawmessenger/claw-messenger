package rongcloud

// user_command.go implements service=user 的微信小程序登录（action=wxLogin）。
//
// 客户端调用点：clawmessenger-uniapp/src/components/login-popup.vue:300 —
// `sendRequest('user','wxLogin',{code, phoneCode})`，拿 `result.data.token` 建 IM
// 连接并写入本地存储。因此回包形状必须与既有 HTTP 登录（ClawLogin，
// internal/handler/claw_compat.go:467-473）一致：
//
//	{userId, username, nickname, portraitUri, token}
//
// 实现范式直接照 Google OAuth（internal/handler/auth.go:558-600）：读 env →
// 缺失则明确的「功能未启用」→ code 换 token。这里是微信的 jscode2session。
//
// 配置：WECHAT_MP_APPID / WECHAT_MP_SECRET。缺失时回 503 + 「微信登录未配置」，
// 不 panic、也不让客户端拿到 500（约束：缺配置必须给出可读的未启用错误）。
//
// openid 绑定策略：
//  1. openid 已存在 → 直接登录该账号（不重建、不改用户名）；
//  2. 不存在 → 新建 claw_im_users 记录。既有注册流程（ClawRegister）强依赖
//     密码（claw_im_users.password_hash 是 NOT NULL），无法从集成层复用，因此
//     这里写入一个不可解析的哨兵哈希：微信账号不能被密码登录，但列约束与格式
//     都合法（clawVerifyPassword 对非法格式一律返回 false）。详见实现报告。
//  3. token：优先复用库内 rongcloud_token（与 ClawLogin 的「复用而非重发」
//     策略一致，避免把其它端已登录的会话踢下线，见 claw_compat.go:370-394），
//     仅在缺失时向融云换发并回写。
//
// phoneCode（getPhoneNumber 的 code）当前**未使用**：换手机号需要额外的
// access_token 流程（wxa/business/getuserphonenumber），本服务不做静默绑定，
// 报告里已列为后续项。

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// weChatAPIBase 是微信开放接口的默认域名（测试通过 handler 字段覆盖）。
	defaultWeChatAPIBase = "https://api.weixin.qq.com"
	// weChatHTTPTimeout 与客户端 sendRequest 的 15s 超时留足余量。
	weChatHTTPTimeout = 8 * time.Second
	// wechatOnlyPasswordHash 是微信-only 账号的 password_hash 哨兵值。
	// 格式故意不满足 pbkdf2_sha256$iter$salt$dk，clawVerifyPassword 会直接
	// 返回 false，因此这类账号无法用密码登录（符合预期），而列（NOT NULL）
	// 与审计语义都保持完整。
	wechatOnlyPasswordHash = "wechat_only$no_password_set"
	// wechatOpenIDEnvAppID / wechatOpenIDEnvSecret 是仅有的两个配置项。
	wechatOpenIDEnvAppID  = "WECHAT_MP_APPID"
	wechatOpenIDEnvSecret = "WECHAT_MP_SECRET"
)

// WeChatUser 是本地账号（claw_im_users）在本服务里用到的最小视图。
type WeChatUser struct {
	UserID         string
	Username       string
	Nickname       string
	PortraitURI    string
	RongCloudToken string
	Status         string
}

// CreateWeChatUserParams 是新建微信账号的入参。
type CreateWeChatUserParams struct {
	OpenID       string
	Nickname     string
	PortraitURI  string
	PasswordHash string
}

// UserStore 是 service=user 需要的本地表读写。生产实现见 NewUserStore。
type UserStore struct {
	FindByOpenID func(ctx context.Context, openID string) (WeChatUser, bool, error)
	// CreateWeChatUser 的第二个返回值是「这条记录是不是本次真的插入了」。
	// 并发登录同一个 openid 时，后到的那个会走 ON CONFLICT 回查拿到别人刚建
	// 的行 —— 那一次必须回 false，否则回包 isNewUser 会误报 true（DEF-4）。
	CreateWeChatUser   func(ctx context.Context, params CreateWeChatUserParams) (WeChatUser, bool, error)
	SaveRongCloudToken func(ctx context.Context, userID, token string) error
	// SearchUsers 按关键字搜账号（service=user/action=search，见
	// user_search_command.go）。limit 由调用方给定，生产值是 userSearchLimit。
	SearchUsers func(ctx context.Context, keyword string, limit int) ([]UserSummary, error)
}

// wechatAPIError 表示微信侧返回的业务错误（errcode != 0），属于调用方问题。
type wechatAPIError struct {
	code    int
	message string
}

func (e *wechatAPIError) Error() string {
	return fmt.Sprintf("wechat errcode %d: %s", e.code, e.message)
}

// wechatSessionResponse 是 jscode2session 的回包。session_key 只用于解密
// 开放数据，本服务不用它，也绝不写日志。
type wechatSessionResponse struct {
	OpenID     string `json:"openid"`
	UnionID    string `json:"unionid"`
	SessionKey string `json:"session_key"`
	ErrCode    int    `json:"errcode"`
	ErrMsg     string `json:"errmsg"`
}

func (h *systemHandler) handleUserCommand(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	switch cmd.Action {
	case "wxLogin":
		return h.handleWeChatLogin(ctx, msg, cmd)
	case "confirmWebLogin":
		// Web 扫码登录的移动端确认（web_login_command.go）。前端两处扫码入口
		// （会话页、搜索页）用它把 pending 票据置成 confirmed。
		return h.handleConfirmWebLogin(ctx, msg, cmd)
	case "search":
		// web「添加好友」的账号搜索（user_search_command.go）。
		return h.handleUserSearch(ctx, msg, cmd)
	default:
		// user 服务的其余 action（login/getInfo/getProfiles/…）没有 IM 实现：
		// 走 404 契约，客户端据此降级到 HTTP（/api/login、/api/user/info）。
		return h.replyErr(ctx, msg, cmd, 404, unknownServiceOrActionMessage(cmd))
	}
}

func (h *systemHandler) handleWeChatLogin(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	// 业务逻辑在 WeChatLogin（wechat_login.go），HTTP 通道 POST /api/wechat/login
	// 复用同一份实现。这里只做 command 通道的入参解析与回包映射。
	code := strings.TrimSpace(getStringAny(cmd.Params, "code", "jsCode", "js_code"))
	deps := WeChatLoginDeps{
		Store:      h.userStore,
		Logger:     h.logger,
		APIBase:    h.wechatAPIBase,
		HTTPClient: h.wechatHTTPClient,
	}
	if h.client != nil {
		deps.MintToken = func(ctx context.Context, userID, name, portraitURI string) (string, error) {
			return h.client.getUserToken(ctx, userID, name, portraitURI)
		}
	}
	result, err := WeChatLogin(ctx, deps, code)
	if err != nil {
		var loginErr *WeChatLoginError
		if errors.As(err, &loginErr) {
			return h.replyErr(ctx, msg, cmd, loginErr.Status, loginErr.Message)
		}
		return h.replyErr(ctx, msg, cmd, 500, err.Error())
	}
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{
		"userId":      result.UserID,
		"id":          result.UserID,
		"username":    result.Username,
		"nickname":    result.Nickname,
		"portraitUri": result.PortraitURI,
		"token":       result.Token,
		"isNewUser":   result.IsNewUser,
	})
}

// wechatCode2Session 用 wx.login 的 code 换 openid。
//
// 微信对业务错误也返回 HTTP 200，只有 errcode 区分成功/失败，所以必须读
// errcode；网络/解析失败按 500 处理（不是调用方的错）。
func (h *systemHandler) wechatCode2Session(ctx context.Context, appID, appSecret, code string) (string, error) {
	return wechatCode2Session(ctx, h.wechatAPIBase, h.wechatHTTPClient, appID, appSecret, code)
}

// wechatClient 允许测试注入假微信服务；默认 8s 超时，避免命令通道被拖到
// 客户端 15s 超时。
func (h *systemHandler) wechatClient() *http.Client {
	if h.wechatHTTPClient != nil {
		return h.wechatHTTPClient
	}
	return &http.Client{Timeout: weChatHTTPTimeout}
}

// sanitizeWeChatTransportError 剥掉 *url.Error 里那条含 AppSecret 的 URL。
//
// 微信 jscode2session 只能用 query string 传 secret，而 (*url.Error).Error()
// 的输出是 `<Op> "<完整 URL>": <底因>`；net/url 的 stripPassword 只脱敏
// userinfo（`user:pass@`），对 query 完全不管。调用方会把错误写进日志，于是
// 线上任何一次 DNS 抖动、TLS 失败、8s 超时、连接重置都会把 AppSecret 明文写
// 进服务端日志 —— 这是 DEF-1。
//
// 因此这里只保留底因（dial tcp .../context deadline exceeded/...），非
// *url.Error 的包装也照样交给调用方（它们不带 URL）。**不要**把它改回直接
// `%w` 透传：TestWeChatTransportErrorNeverLeaksTheSecret 会红。
func sanitizeWeChatTransportError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		inner := urlErr.Err
		if inner == nil {
			inner = errors.New("request failed")
		}
		return fmt.Errorf("wechat jscode2session: request failed: %w", inner)
	}
	return err
}

// defaultWeChatNickname 给新账号一个不空且可辨识的昵称（客户端各处以 nickname
// 兜底显示，空串会露出裸 id）。
func defaultWeChatNickname(openID string) string {
	suffix := openID
	if len(suffix) > 4 {
		suffix = suffix[len(suffix)-4:]
	}
	if suffix == "" {
		return "微信用户"
	}
	return "微信用户" + suffix
}

// NewUserStore 是 UserStore 的生产实现（表：claw_im_users，openid 列见
// migrations/599）。
//
// 建号沿用既有约定：user_id 是 6 位纯数字且全局唯一（claw_compat.go:254-272
// 的 clawGenerateUserID 同款），注册时 username = user_id。openid 上有唯一索引，
// 并发建号时 ON CONFLICT DO NOTHING + 回查保证幂等。
func NewUserStore(dbtx db.DBTX) *UserStore {
	if dbtx == nil {
		return nil
	}
	store := &UserStore{}

	store.FindByOpenID = func(ctx context.Context, openID string) (WeChatUser, bool, error) {
		var user WeChatUser
		// 故意不带 status 过滤：被禁用的账号必须能被识别出来（回 401），
		// 否则会走到建号分支，被 openid 唯一索引拦下后报 500。
		err := dbtx.QueryRow(ctx, `
			SELECT user_id, username, COALESCE(nickname, ''), COALESCE(portrait_uri, ''),
			       COALESCE(rongcloud_token, ''), status
			FROM claw_im_users
			WHERE wechat_openid = $1`, openID).
			Scan(&user.UserID, &user.Username, &user.Nickname, &user.PortraitURI,
				&user.RongCloudToken, &user.Status)
		if err != nil {
			if isNoRows(err) {
				return WeChatUser{}, false, nil
			}
			return WeChatUser{}, false, err
		}
		return user, true, nil
	}

	store.CreateWeChatUser = func(ctx context.Context, params CreateWeChatUserParams) (WeChatUser, bool, error) {
		var lastErr error
		for attempt := 0; attempt < 20; attempt++ {
			userID, err := randomClawUserID()
			if err != nil {
				return WeChatUser{}, false, err
			}
			var user WeChatUser
			err = dbtx.QueryRow(ctx, `
				INSERT INTO claw_im_users (user_id, username, nickname, portrait_uri,
				                           password_hash, wechat_openid, status, created_at, updated_at)
				VALUES ($1, $1, $2, $3, $4, $5, 'active', now(), now())
				ON CONFLICT (wechat_openid) DO NOTHING
				RETURNING user_id, username, COALESCE(nickname, ''), COALESCE(portrait_uri, ''),
				          COALESCE(rongcloud_token, ''), status`,
				userID, params.Nickname, params.PortraitURI, params.PasswordHash, params.OpenID).
				Scan(&user.UserID, &user.Username, &user.Nickname, &user.PortraitURI,
					&user.RongCloudToken, &user.Status)
			if err == nil {
				return user, true, nil
			}
			if isNoRows(err) {
				// openid 已被并发请求插入：直接登录那条记录，保持幂等。created=false
				// 是 DEF-4 的修复点 —— 这条不是本次建的。
				if existing, found, findErr := store.FindByOpenID(ctx, params.OpenID); findErr == nil && found {
					return existing, false, nil
				}
				lastErr = err
				continue
			}
			if isClawUserIDConflict(err) {
				// 6 位数字撞号：换一个 id 重试。
				lastErr = err
				continue
			}
			return WeChatUser{}, false, err
		}
		if lastErr == nil {
			lastErr = errors.New("rongcloud: exhausted user id attempts")
		}
		return WeChatUser{}, false, lastErr
	}

	store.SaveRongCloudToken = func(ctx context.Context, userID, token string) error {
		_, err := dbtx.Exec(ctx,
			`UPDATE claw_im_users SET rongcloud_token = $1, updated_at = now() WHERE user_id = $2`,
			token, userID)
		return err
	}

	// 搜索的 SQL 与过滤口径都在 user_search_command.go（一个 action 一个文件）。
	store.SearchUsers = func(ctx context.Context, keyword string, limit int) ([]UserSummary, error) {
		return searchClawIMUsers(ctx, dbtx, keyword, limit)
	}

	return store
}

// randomClawUserID 生成 6 位纯数字 id（100000–999999），与旧系统约定一致。
func randomClawUserID() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", 100000+n.Int64()), nil
}

// isNoRows 判定「零行」：pgx.ErrNoRows 来自 QueryRow，INSERT ... RETURNING 在
// ON CONFLICT DO NOTHING 命中时也是它。
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// isClawUserIDConflict 判定是否撞了 claw_im_users 的主键/用户名唯一约束。
func isClawUserIDConflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	return pgErr.ConstraintName == "claw_im_users_pkey" || pgErr.ConstraintName == "claw_im_users_username_key"
}
