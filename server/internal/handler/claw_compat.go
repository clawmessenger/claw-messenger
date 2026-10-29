// Package handler - 虾说（ClawMessenger Web/uniapp）兼容层。
//
// 纯增量扩展：把旧 Python 后端的公开 HTTP 契约（/api/login、/api/register、
// /api/user/*、/api/config/user-guide）移植到 Go 后端，供 clawmessenger-web /
// clawmessenger-uniapp 前端直连。账号存于独立表 claw_im_users（不动 multica
// 的 "user" 表）。
package handler

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ---- 响应包装（与 Python 端一致：{code, message, data}） ----

type clawEnvelope struct {
	Code    int         `json:"code"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

func clawJSON(w http.ResponseWriter, status, code int, message string, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(clawEnvelope{Code: code, Message: message, Data: data})
}

// ---- 密码哈希：PBKDF2-HMAC-SHA256（格式 pbkdf2_sha256$iter$salt$dk） ----

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	var buf [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	u := make([]byte, hashLen)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf[:4])
		u = u[:0]
		u = prf.Sum(u)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(t)
			t = t[:0]
			t = prf.Sum(t)
			for j := range t {
				u[j] ^= t[j]
			}
		}
		dk = append(dk, u...)
	}
	return dk[:keyLen]
}

func clawHashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	dk := pbkdf2SHA256([]byte(password), salt, 100_000, 32)
	return fmt.Sprintf("pbkdf2_sha256$100000$%s$%s", hex.EncodeToString(salt), hex.EncodeToString(dk))
}

func clawVerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	var iter int
	if _, err := fmt.Sscanf(parts[1], "%d", &iter); err != nil || iter <= 0 || iter > 10_000_000 {
		return false
	}
	salt, err1 := hex.DecodeString(parts[2])
	want, err2 := hex.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	dk := pbkdf2SHA256([]byte(password), salt, iter, len(want))
	return hmac.Equal(dk, want)
}

// ---- 融云 Server API：获取用户 token（与 Python 端 REST 签名一致） ----

type clawRongClient struct {
	appKey    string
	appSecret string
}

// post 直接调融云 Server API（REST 签名），返回响应体。getUserToken 与
// refreshUser 共用，避免签名样板重复。域名可经 CLAW_RONG_API_BASE 覆盖
// （测试/代理场景），默认官方 api.rong-api.com。
func (c *clawRongClient) post(ctx context.Context, path string, form url.Values) ([]byte, error) {
	apiBase := strings.TrimSpace(os.Getenv("CLAW_RONG_API_BASE"))
	if apiBase == "" {
		apiBase = "https://api.rong-api.com"
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	nonceStr := hex.EncodeToString(nonce)
	timestamp := fmt.Sprintf("%d", time.Now().UnixMilli())
	// 融云签名规则：SHA1(AppSecret + Nonce + Timestamp)
	sum := sha1.Sum([]byte(c.appSecret + nonceStr + timestamp))
	signature := hex.EncodeToString(sum[:])

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBase+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("App-Key", c.appKey)
	req.Header.Set("Nonce", nonceStr)
	req.Header.Set("Timestamp", timestamp)
	req.Header.Set("Signature", signature)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (c *clawRongClient) getUserToken(ctx context.Context, userID, name, portraitURI string) (string, error) {
	form := url.Values{}
	form.Set("userId", userID)
	form.Set("name", name)
	if portraitURI != "" {
		form.Set("portraitUri", portraitURI)
	}
	body, err := c.post(ctx, "/user/getToken.json", form)
	if err != nil {
		return "", err
	}
	var out struct {
		Code  int    `json:"code"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("rongcloud response: %.120s", body)
	}
	if out.Code != 200 {
		return "", fmt.Errorf("rongcloud code %d", out.Code)
	}
	return out.Token, nil
}

// refreshUser 更新融云侧用户资料（昵称/头像）。getToken 对已存在用户不会
// 更新资料 —— 改头像后必须走这里推融云，否则融云好友列表/会话渲染的还是
// 旧头像（或空白）。
func (c *clawRongClient) refreshUser(ctx context.Context, userID, name, portraitURI string) error {
	form := url.Values{}
	form.Set("userId", userID)
	if name != "" {
		form.Set("name", name)
	}
	if portraitURI != "" {
		form.Set("portraitUri", portraitURI)
	}
	body, err := c.post(ctx, "/user/refresh.json", form)
	if err != nil {
		return err
	}
	var out struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("rongcloud response: %.120s", body)
	}
	if out.Code != 200 {
		return fmt.Errorf("rongcloud code %d", out.Code)
	}
	return nil
}

// ---- 用户行 ----

// clawRongDisplayName 返回推给融云的用户显示名。
//
// 融云 /user/getToken.json 的 name 是必填项（getUserToken 无条件带上它，空值
// 会让 token mint 失败 → 登录 500），而本地 claw_im_users.nickname 允许为
// NULL（= 用户没设昵称）。因此**只在融云投影这一层**用账号名兜底：库里的
// nickname 仍保持 NULL，接口仍照实返回空串。两件事不能混为一谈。
func clawRongDisplayName(nickname, username string) string {
	if n := strings.TrimSpace(nickname); n != "" {
		return n
	}
	return username
}

// clawRegisterRongToken 把新注册用户推给融云并返回 token。
//
// best-effort：融云没配 / 调用失败时返回空串，注册本身照常成功（之后登录会
// 走 mint 重试）。单独抽出来是为了让「昵称为空时推的是账号名」这条契约能被
// 一个假融云服务直接断言（见 claw_register_nickname_test.go）。
func clawRegisterRongToken(ctx context.Context, rc *clawRongClient, userID, nickname, username, portrait string) string {
	if rc == nil {
		return ""
	}
	token, err := rc.getUserToken(ctx, userID, clawRongDisplayName(nickname, username), portrait)
	if err != nil {
		return ""
	}
	return token
}

type clawUserRow struct {
	UserID       string
	Username     string
	Nickname     string
	PortraitURI  string
	Email        string
	Phone        string
	Signature    string
	Gender       string
	Birthday     string
	Status       string
	PasswordHash string
	RCStored     sql.NullString
	// WeChatOpenID 非空即「已绑定微信」。原样留在结构体里、但**不出现在接口回包**
	// 中：openid 是微信侧的身份，客户端拿到也没有用，多给一个字段就多一处泄漏面。
	WeChatOpenID string
}

// clawHasPassword 判断这个账号是不是「真的设过密码」。
//
// 微信登录建出来的账号写的是不可解析的哨兵哈希（`wechat_only$no_password_set`，
// 见 integrations/rongcloud/user_command.go），`clawVerifyPassword` 对它一律返回
// false。所以「有密码」不能按 password_hash 非空判断 —— 那样微信-only 账号会被
// 要求输入一个它从来没有的旧密码，改密入口直接死锁。
func clawHasPassword(hash string) bool {
	return strings.HasPrefix(strings.TrimSpace(hash), "pbkdf2_sha256$")
}

func (h *Handler) clawQueryUser(ctx context.Context, field string, arg string) (*clawUserRow, error) {
	allowed := map[string]bool{"user_id": true, "username": true}
	if !allowed[field] {
		return nil, fmt.Errorf("invalid field")
	}
	row := h.DB.QueryRow(ctx,
		"SELECT user_id, username, COALESCE(nickname,''), COALESCE(portrait_uri,''), "+
			"COALESCE(email,''), COALESCE(phone,''), COALESCE(signature,''), COALESCE(gender,''), "+
			"COALESCE(birthday,''), status, password_hash, rongcloud_token, COALESCE(wechat_openid,'') "+
			"FROM claw_im_users WHERE "+field+" = $1", arg)
	var u clawUserRow
	err := row.Scan(&u.UserID, &u.Username, &u.Nickname, &u.PortraitURI, &u.Email, &u.Phone,
		&u.Signature, &u.Gender, &u.Birthday, &u.Status, &u.PasswordHash, &u.RCStored, &u.WeChatOpenID)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// clawQueryUserByIdentifier 按「登录账号 / 虾说号 / 手机号 / 邮箱」找账号。
//
// 为什么要一次查四种：用户记不住自己当初是用哪个注册的，而 `claw_im_users` 里
// 这四列都可能唯一（虾说号是主键、username 有 UNIQUE、手机号/邮箱由绑定接口保证
// 唯一）。登录框只给一个输入框，服务端就得负责认出它是哪种。
//
// 优先级写进 ORDER BY 而不是分四次查：一次往返就把「username 优先于手机号」
// 这类歧义定死 —— 两个账号一个把手机号设成 username、另一个真用这个手机号时，
// 命中的必须是前者（他一直在用用户名登录）。email 用 lower() 比较，大小写不敏感。
func (h *Handler) clawQueryUserByIdentifier(ctx context.Context, identifier string) (*clawUserRow, error) {
	row := h.DB.QueryRow(ctx,
		"SELECT user_id, username, COALESCE(nickname,''), COALESCE(portrait_uri,''), "+
			"COALESCE(email,''), COALESCE(phone,''), COALESCE(signature,''), COALESCE(gender,''), "+
			"COALESCE(birthday,''), status, password_hash, rongcloud_token, COALESCE(wechat_openid,'') "+
			"FROM claw_im_users "+
			"WHERE username=$1 OR user_id=$1 OR phone=$1 OR lower(email)=lower($1) "+
			"ORDER BY (username=$1) DESC, (user_id=$1) DESC, (phone=$1) DESC, created_at ASC "+
			"LIMIT 1", identifier)
	var u clawUserRow
	err := row.Scan(&u.UserID, &u.Username, &u.Nickname, &u.PortraitURI, &u.Email, &u.Phone,
		&u.Signature, &u.Gender, &u.Birthday, &u.Status, &u.PasswordHash, &u.RCStored, &u.WeChatOpenID)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func clawUserJSON(u *clawUserRow) map[string]interface{} {
	return map[string]interface{}{
		"userId":      u.UserID,
		"username":    u.Username,
		"nickname":    u.Nickname,
		"portraitUri": u.PortraitURI,
		"phone":       u.Phone,
		"email":       u.Email,
		"signature":   u.Signature,
		"gender":      u.Gender,
		"birthday":    u.Birthday,
		"status":      u.Status,
		// 账号安全面板要据此决定「改密码」是「设置」还是「修改」，以及微信那一行
		// 显示「已绑定」还是「去绑定」。只回布，不回 openid 本身。
		"hasPassword": clawHasPassword(u.PasswordHash),
		"wechatBound": u.WeChatOpenID != "",
	}
}

// clawGenerateUserID 生成 6 位起全局唯一纯数字 ID（与旧系统约定一致）。
func (h *Handler) clawGenerateUserID(ctx context.Context) (string, error) {
	for i := 0; i < 20; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(900000))
		if err != nil {
			return "", err
		}
		id := fmt.Sprintf("%d", 100000+n.Int64())
		var exists bool
		err = h.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM claw_im_users WHERE user_id=$1)", id).Scan(&exists)
		if err != nil {
			return "", err
		}
		if !exists {
			return id, nil
		}
	}
	return "", fmt.Errorf("无法生成唯一用户ID")
}

func (h *Handler) clawRong(ctx context.Context) *clawRongClient {
	if h.RongCloudInstall == nil {
		return nil
	}
	appKey := h.RongCloudInstall.GetAppKey(ctx)
	secret, err := h.RongCloudInstall.GetAppSecret(ctx)
	if appKey == "" || err != nil || secret == "" {
		return nil
	}
	return &clawRongClient{appKey: appKey, appSecret: secret}
}

// ---- HTTP handlers ----

// ClawRegister POST /api/register
func (h *Handler) ClawRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Nickname string `json:"nickname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		clawJSON(w, 400, 400, "请求参数错误", nil)
		return
	}
	ctx := r.Context()
	username := strings.TrimSpace(req.Username)
	if req.Password == "" {
		clawJSON(w, 400, 400, "密码不能为空", nil)
		return
	}
	if len(req.Password) < 6 {
		clawJSON(w, 400, 400, "密码长度至少6位", nil)
		return
	}

	var exists bool
	if username != "" {
		if err := h.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM claw_im_users WHERE username=$1)", username).Scan(&exists); err != nil {
			clawJSON(w, 500, 500, err.Error(), nil)
			return
		}
		if exists {
			clawJSON(w, 409, 409, "用户名已存在", nil)
			return
		}
	}

	userID, err := h.clawGenerateUserID(ctx)
	if err != nil {
		clawJSON(w, 500, 500, err.Error(), nil)
		return
	}
	if username == "" {
		username = userID
	}
	// 昵称不再用账号名兜底：claw_im_users.nickname 为 NULL 就表示「用户没设
	// 昵称」，账号名只属于 username 一列。旧代码在这里写 username，让「显示
	// 昵称」和「登录名」永久粘连，之后的读路径（COALESCE）与融云投影只能一路
	// 放大这个污染。空串在下面落库时折成 NULL（NULLIF），两者语义等价。
	nickname := strings.TrimSpace(req.Nickname)

	// 默认头像：注册时即写库 + 推融云 —— 融云会话/好友列表渲染的是融云侧
	// portraitUri，客户端本地兜底图在「融云已知资料」场景不生效；不推的话
	// 新用户在对方客户端长期显示裸 id。
	portrait := h.clawDefaultPortraitForNewUser()

	_, err = h.DB.Exec(ctx, `
		INSERT INTO claw_im_users (user_id, username, nickname, portrait_uri, password_hash, status, created_at, updated_at)
		VALUES ($1, $2, NULLIF($3,''), $4, $5, 'active', now(), now())`,
		userID, username, nickname, portrait, clawHashPassword(req.Password))
	if err != nil {
		clawJSON(w, 500, 500, "注册失败", nil)
		return
	}

	// 同步注册到融云（best-effort，失败不阻塞注册）。
	//
	// 融云 /user/getToken.json 的 name 是必填字段（getUserToken 无条件
	// form.Set("name", …)，code != 200 就拿不到 token，登录会因此 500）。
	// 所以融云侧仍然要有名字：昵称为空时推账号名。这是**融云展示层的兜底**，
	// 不是写库的兜底 —— 库里的 nickname 保持 NULL，接口也照实返回空串。
	if rc := h.clawRong(ctx); rc != nil {
		if token := clawRegisterRongToken(ctx, rc, userID, nickname, username, portrait); token != "" {
			_, _ = h.DB.Exec(ctx, "UPDATE claw_im_users SET rongcloud_token=$1 WHERE user_id=$2", token, userID)
		}
	}

	clawJSON(w, 200, 200, "注册成功", map[string]interface{}{
		"username":    username,
		"nickname":    nickname,
		"userId":      userID,
		"portraitUri": portrait,
	})
}

// clawDefaultPortraitForNewUser 返回新注册用户的默认头像 URL（见 claw_static.go
// 的决议优先级）。
func (h *Handler) clawDefaultPortraitForNewUser() string {
	return clawDefaultPortraitURL()
}

// clawResolveLoginToken 决定登录返回哪个融云 token。
//
// claw_im_users.rongcloud_token 一物两用：既是客户端 IM SDK 的建连凭据，又是
// /api/claw/* 的 Bearer 会话凭据（服务端按该列做字符串匹配认会话）。因此登录
// 必须复用库内存量 token —— 每次登录都向融云换发并覆盖库内值，会让其它已登录
// 端（多标签页 / 小程序 / 设备）手里的旧 token 立即失效：IM 连接被踢、HTTP 鉴权
// 401，前端表现为「讨论工作流检测失败 / 系统主持状态检测失败」，设备列表也可能
// 落空。仅在库内没有 token 时才调用 mint 向融云申请，并把新值回写库内（minted=true
// 表示需要回写）。
//
// 确需续期的场景由客户端在 IM 连接失败时显式调用 POST /api/im/refresh-token，
// 该端点仍然走融云 getUserToken，不受这里「复用」策略的影响。
func clawResolveLoginToken(stored string, mint func() (string, error)) (token string, minted bool) {
	if existing := strings.TrimSpace(stored); existing != "" {
		return existing, false
	}
	fresh, err := mint()
	if err != nil {
		return "", false
	}
	if fresh = strings.TrimSpace(fresh); fresh == "" {
		return "", false
	}
	return fresh, true
}

// ClawLogin POST /api/login
func (h *Handler) ClawLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		clawJSON(w, 400, 400, "请求参数错误", nil)
		return
	}
	ctx := r.Context()
	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		clawJSON(w, 400, 400, "用户名和密码不能为空", nil)
		return
	}

	u, err := h.clawQueryUserByIdentifier(ctx, username)
	if err != nil || !clawVerifyPassword(req.Password, u.PasswordHash) {
		// 微信-only 账号（哨兵哈希）也落在这里：`clawVerifyPassword` 对它恒 false，
		// 所以「用密码登录微信账号」会被当成密码错误，而不是登进去一个没有密码的号。
		clawJSON(w, 401, 401, "用户名或密码错误", nil)
		return
	}
	if u.Status != "active" {
		clawJSON(w, 401, 401, "账户已被禁用", nil)
		return
	}

	// 复用库内存量 token：claw_im_users.rongcloud_token 同时充当 /api/claw/*
	// 的 Bearer 会话凭据，且客户端 IM SDK 用同一 token 建连。若每次登录都向融云
	// 换发新 token 并覆盖库内值，其它已登录端（多标签页 / 小程序 / 设备）手里的
	// 旧 token 会立刻失效——IM 连接被踢、HTTP 鉴权 401，表现为圆桌布置弹窗
	// 「讨论工作流检测失败 / 系统主持状态检测失败」，设备列表也可能落空。
	// 因此库内已有 token 时直接复用；仅在缺失时才向融云申请。确需续期由客户端
	// 在 IM 连接失败时显式调用 POST /api/im/refresh-token（它仍走 getUserToken）。
	var stored string
	if u.RCStored.Valid {
		stored = u.RCStored.String
	}
	// 存量用户头像兜底：库里没头像的老用户，登录时补默认头像并推融云（mint
	// 路径顺带写入；token 复用路径单独 refresh，best-effort）。新注册用户
	// 已在注册时写入，这里对它是 no-op。
	portrait := u.PortraitURI
	if portrait == "" {
		portrait = h.clawDefaultPortraitForNewUser()
		if _, err := h.DB.Exec(ctx,
			"UPDATE claw_im_users SET portrait_uri=$1, updated_at=now() WHERE user_id=$2 AND (portrait_uri IS NULL OR portrait_uri='')",
			portrait, u.UserID); err == nil && stored != "" {
			// token 复用（不会走 mint）时融云侧资料需要显式刷新。
			if rc := h.clawRong(ctx); rc != nil {
				if err := rc.refreshUser(ctx, u.UserID, u.Nickname, portrait); err != nil {
					slog.Warn("claw: backfill default portrait to rongcloud failed",
						"user_id", u.UserID, "error", err)
				}
			}
		}
		u.PortraitURI = portrait
	}
	token, minted := clawResolveLoginToken(stored, func() (string, error) {
		if rc := h.clawRong(ctx); rc != nil {
			// name 必填：昵称为 NULL 的用户推账号名，否则 mint 失败 → 500。
			return rc.getUserToken(ctx, u.UserID, clawRongDisplayName(u.Nickname, u.Username), u.PortraitURI)
		}
		return "", errors.New("rongcloud 未配置")
	})
	if minted {
		_, _ = h.DB.Exec(ctx, "UPDATE claw_im_users SET rongcloud_token=$1 WHERE user_id=$2", token, u.UserID)
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

// ClawUserList GET /api/user/list
func (h *Handler) ClawUserList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	exclude := strings.TrimSpace(r.URL.Query().Get("exclude"))
	rows, err := h.DB.Query(ctx, `
		SELECT user_id, username, COALESCE(nickname, ''), COALESCE(portrait_uri,''),
		       COALESCE(email,''), COALESCE(phone,'')
		FROM claw_im_users
		WHERE status = 'active' AND ($1 = '' OR user_id <> $1)
		ORDER BY created_at DESC`, exclude)
	if err != nil {
		clawJSON(w, 500, 500, err.Error(), nil)
		return
	}
	defer rows.Close()
	list := []map[string]interface{}{}
	for rows.Next() {
		var id, uname, nick, portrait, email, phone string
		if err := rows.Scan(&id, &uname, &nick, &portrait, &email, &phone); err != nil {
			continue
		}
		list = append(list, map[string]interface{}{
			"userId": id, "username": uname, "nickname": nick,
			"portraitUri": portrait, "email": email, "phone": phone,
		})
	}
	clawJSON(w, 200, 200, "", map[string]interface{}{"list": list})
}

// ClawUserInfo GET/POST /api/user/info
func (h *Handler) ClawUserInfo(w http.ResponseWriter, r *http.Request) {
	var userID string
	if r.Method == http.MethodGet {
		userID = r.URL.Query().Get("username")
		if userID == "" {
			userID = r.URL.Query().Get("userId")
		}
	} else {
		var body struct {
			UserId   string `json:"userId"`
			Username string `json:"username"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		userID = body.UserId
		if userID == "" {
			userID = body.Username
		}
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		clawJSON(w, 400, 400, "userId不能为空", nil)
		return
	}
	ctx := r.Context()
	u, err := h.clawQueryUser(ctx, "user_id", userID)
	if err != nil {
		u, err = h.clawQueryUser(ctx, "username", userID)
	}
	if err != nil {
		// 群聊/聊天室成员里的 AI 节点（rc_node_*）不在 claw_im_users：前端
		// loadAuthoritativeChatroom / loadGroupMembers 会对每个成员调这个
		// 接口取昵称头像，节点查不到就 404，气泡退化成裸 id。回落查
		// rongcloud_user（节点自己的融云账号资料），仍找不到才报 404。
		if node, nodeErr := h.clawQueryRongCloudUser(ctx, userID); nodeErr == nil {
			clawJSON(w, 200, 200, "", node)
			return
		}
		clawJSON(w, 404, 404, "用户不存在", nil)
		return
	}
	payload := clawUserJSON(u)
	// hasPassword / wechatBound 是**账号状态**，不是公开资料：这个接口任何登录态
	// （甚至无登录态）都能查别人，把「这人设没设密码、绑没绑微信」回出去等于给
	// 撞库和社工送情报。只有查自己时才带上（账号安全面板靠它渲染）。
	if viewer, _ := h.clawViewerUserID(ctx, r); viewer != u.UserID {
		delete(payload, "hasPassword")
		delete(payload, "wechatBound")
	}
	clawJSON(w, 200, 200, "", payload)
}

// clawQueryRongCloudUser 按 rongcloud_user_id 查节点/系统账号的公开资料。
// 返回的形态与 clawUserJSON 对齐（前端只读 username/nickname/portraitUri），
// 额外带 isAiNode 供前端区分人类与节点。机器主节点（is_system_reserved）
// 不对外暴露：它是身份派生的内部锚点，不该出现在成员资料里。
func (h *Handler) clawQueryRongCloudUser(ctx context.Context, rcID string) (map[string]interface{}, error) {
	row := h.DB.QueryRow(ctx, `
		SELECT rongcloud_user_id, COALESCE(name, ''), COALESCE(portrait_uri, ''),
		       COALESCE(is_ai_node, FALSE), COALESCE(is_system_reserved, FALSE)
		FROM rongcloud_user WHERE rongcloud_user_id = $1`, rcID)
	var id, name, portrait string
	var isAI, isReserved bool
	if err := row.Scan(&id, &name, &portrait, &isAI, &isReserved); err != nil {
		return nil, err
	}
	if isReserved {
		return nil, fmt.Errorf("system reserved account: %s", id)
	}
	display := name
	if display == "" {
		display = id
	}
	return map[string]interface{}{
		"userId":      id,
		"username":    id,
		"nickname":    display,
		"portraitUri": portrait,
		"isAiNode":    isAI,
	}, nil
}

// ClawUserUpdate POST /api/user/update
func (h *Handler) ClawUserUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username    string `json:"username"`
		Nickname    string `json:"nickname"`
		PortraitUri string `json:"portraitUri"`
		Phone       string `json:"phone"`
		Email       string `json:"email"`
		Signature   string `json:"signature"`
		Gender      string `json:"gender"`
		Birthday    string `json:"birthday"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		clawJSON(w, 400, 400, "请求参数错误", nil)
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		clawJSON(w, 400, 400, "username不能为空", nil)
		return
	}
	ctx := r.Context()
	u, err := h.clawQueryUser(ctx, "username", strings.TrimSpace(req.Username))
	if err != nil {
		clawJSON(w, 404, 404, "用户不存在", nil)
		return
	}

	set := func(v, old string) string {
		if v == "" {
			return old
		}
		return v
	}
	// 存量用户头像兜底：库里没头像的老用户，改资料时若客户端也没传，用默认
	// 头像补齐（新注册已在注册时写入，此处只兜老数据）。
	nextPortrait := set(req.PortraitUri, u.PortraitURI)
	if nextPortrait == "" {
		nextPortrait = h.clawDefaultPortraitForNewUser()
	}
	_, err = h.DB.Exec(ctx, `
		UPDATE claw_im_users
		SET nickname=NULLIF($1,''), portrait_uri=$2, phone=$3, email=$4, signature=$5, gender=$6, birthday=$7, updated_at=now()
		WHERE user_id=$8`,
		set(req.Nickname, u.Nickname), nextPortrait,
		set(req.Phone, u.Phone), set(req.Email, u.Email),
		set(req.Signature, u.Signature), set(req.Gender, u.Gender),
		set(req.Birthday, u.Birthday), u.UserID)
	if err != nil {
		clawJSON(w, 500, 500, err.Error(), nil)
		return
	}
	// 推融云：getToken 不更新已存在用户的资料，改了昵称/头像必须 refresh，
	// 否则融云侧渲染的仍是旧资料（best-effort，失败不阻塞本地保存）。
	if rc := h.clawRong(ctx); rc != nil {
		nextNickname := set(req.Nickname, u.Nickname)
		if nextPortrait != u.PortraitURI || nextNickname != u.Nickname {
			if err := rc.refreshUser(ctx, u.UserID, nextNickname, nextPortrait); err != nil {
				// 不回错误：本地已保存，融云侧旧资料是展示瑕疵不是数据丢失。
				slog.Warn("claw: refresh rongcloud profile after update failed",
					"user_id", u.UserID, "error", err)
			}
		}
	}
	updated, _ := h.clawQueryUser(ctx, "user_id", u.UserID)
	clawJSON(w, 200, 200, "", clawUserJSON(updated))
}

// ClawUserFriends GET /api/user/friends —— 旧站好友列表（HTTP 通道兜底）。
// 认证 = claw_im_users.rongcloud_token；只返回当前账号自己的好友，避免越权。
func (h *Handler) ClawUserFriends(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	// Frontend sends ?userId=; it must match the authenticated account.
	if query := strings.TrimSpace(r.URL.Query().Get("userId")); query != "" && query != userID {
		clawJSON(w, 403, 403, "无权访问该用户好友列表", nil)
		return
	}
	if h.RongCloudNode == nil {
		clawJSON(w, 200, 200, "", map[string]interface{}{"list": []interface{}{}})
		return
	}
	// 归属投影读的是 rongcloud_node.claimed_by_user_id，要 DBTX；NodeService 在
	// router 里只拿到 *db.Queries，按需在这里挂连接池（与绑定、节点管理同做法，
	// 见 clawAttachNodeClaimDB）。漏了这一步，friendOwnerProjection 会静默返回
	// 空 map —— 好友列表永远不带 ownerId，前端只能把人和智能体平铺。
	h.clawAttachNodeClaimDB()
	friends, err := h.RongCloudNode.ListUserFriends(r.Context(), userID)
	if err != nil {
		// A failed lookup must not break the client: it falls back to the IM
		// SDK friend list, so answer with an empty list rather than an error.
		slog.Warn("claw: list user friends failed", "userId", userID, "error", err)
		clawJSON(w, 200, 200, "", map[string]interface{}{"list": []interface{}{}})
		return
	}
	clawJSON(w, 200, 200, "", map[string]interface{}{"list": friends})
}

// ClawUserGuide GET /api/config/user-guide —— 从 rongcloud_system_config 读
// user_guide 键（与旧系统一致；该表 config 为 jsonb，含 title/content/开关）。
func (h *Handler) ClawUserGuide(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var value []byte
	err := h.DB.QueryRow(ctx,
		"SELECT config FROM rongcloud_system_config WHERE config_key='user_guide' LIMIT 1").Scan(&value)
	if err != nil {
		clawJSON(w, 200, 200, "", map[string]interface{}{
			"title": "使用文档", "content": "", "web_enabled": false, "uniapp_enabled": false,
		})
		return
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(value, &cfg); err != nil {
		clawJSON(w, 500, 500, "使用指南加载失败", nil)
		return
	}
	for k, dv := range map[string]interface{}{"title": "使用文档", "content": "", "web_enabled": false, "uniapp_enabled": false} {
		if _, ok := cfg[k]; !ok {
			cfg[k] = dv
		}
	}
	clawJSON(w, 200, 200, "", cfg)
}

// ClawImRefreshToken POST /api/im/refresh-token —— 用业务会话换发融云 Token。
// 认证方式与旧 Python 端一致：Bearer 即 rongcloud_token（按 claw_im_users.rongcloud_token 认会话）。
func (h *Handler) ClawImRefreshToken(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		clawJSON(w, 401, 401, "登录令牌无效", nil)
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if token == "" {
		clawJSON(w, 401, 401, "登录令牌无效", nil)
		return
	}
	ctx := r.Context()
	var userID string
	err := h.DB.QueryRow(ctx,
		"SELECT user_id FROM claw_im_users WHERE rongcloud_token=$1 AND status='active' LIMIT 1",
		token).Scan(&userID)
	if err != nil {
		clawJSON(w, 401, 401, "登录令牌无效", nil)
		return
	}
	u, err := h.clawQueryUser(ctx, "user_id", userID)
	if err != nil || u == nil {
		clawJSON(w, 403, 403, "用户不存在或已被禁用", nil)
		return
	}
	rc := h.clawRong(ctx)
	if rc == nil {
		clawJSON(w, 500, 500, "换发 IM Token 失败", nil)
		return
	}
	// 存量用户头像兜底：与登录同策略（库内空头像补默认并写库）。
	if u.PortraitURI == "" {
		u.PortraitURI = h.clawDefaultPortraitForNewUser()
		_, _ = h.DB.Exec(ctx,
			"UPDATE claw_im_users SET portrait_uri=$1, updated_at=now() WHERE user_id=$2 AND (portrait_uri IS NULL OR portrait_uri='')",
			u.PortraitURI, u.UserID)
	}
	fresh, err := rc.getUserToken(ctx, u.UserID, u.Nickname, u.PortraitURI)
	if err != nil {
		slog.Warn("claw: refresh im token failed", "error", err, "userId", u.UserID)
		clawJSON(w, 500, 500, "换发 IM Token 失败: "+err.Error(), nil)
		return
	}
	_, _ = h.DB.Exec(ctx, "UPDATE claw_im_users SET rongcloud_token=$1, updated_at=now() WHERE user_id=$2", fresh, u.UserID)
	w.Header().Set("Cache-Control", "no-store")
	clawJSON(w, 200, 200, "换发成功", map[string]interface{}{"token": fresh, "userId": u.UserID})
}

// ---- clawmessenger 设备绑定（旧站兼容端点） ----

// clawPairingView 是旧站可见的票据会话投影（驼峰命名；不含 client_claim_key /
// idempotency_key 等凭据字段，与 PairingSessionView 同口径）。
type clawPairingView struct {
	Ticket    string `json:"ticket"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expiresAt"`
	// AIType 是 claimed 后回填节点的 ai_type（register 时注册的智能体类型），
	// 供旧站弹窗提示「clawmessenger run --agent <name>」；pending 时为空串。
	AIType string `json:"aiType,omitempty"`
	// ReportedAgents 是 pair 时设备自动上报的本机 agent 列表（pending 时可能为空）。
	ReportedAgents []string `json:"reportedAgents,omitempty"`
	// BoundAgents 是用户在弹窗勾选绑定后生成的 [{agent,nodeId}] 结果。
	BoundAgents []rongcloud.BoundAgentResult `json:"boundAgents,omitempty"`
}

// clawViewerUserID 是 clawRequireClawUser 的**静默版**：只探测可选身份，不写响应、
// 不因缺 token 失败。用于「公开接口，但对本人多回几个字段」的场景（/api/user/info
// 的账号安全状态）。无 token / token 失效一律按匿名（空串）处理。
func (h *Handler) clawViewerUserID(ctx context.Context, r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return "", nil
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if token == "" {
		return "", nil
	}
	var userID string
	err := h.DB.QueryRow(ctx,
		"SELECT user_id FROM claw_im_users WHERE rongcloud_token=$1 AND status='active' LIMIT 1",
		token).Scan(&userID)
	if err != nil {
		// ErrNoRows = token 过期/被换掉，属于匿名浏览的正常分支；真错才上报，
		// 调用方对错误的处理也是「当匿名」，等于 fail-closed（少回字段）。
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return userID, nil
}

// clawRequireClawUser 按旧站契约认证：Bearer 即 claw_im_users.rongcloud_token。
func (h *Handler) clawRequireClawUser(w http.ResponseWriter, r *http.Request) (userID string, ok bool) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		clawJSON(w, 401, 401, "登录令牌无效", nil)
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if token == "" {
		clawJSON(w, 401, 401, "登录令牌无效", nil)
		return "", false
	}
	ctx := r.Context()
	err := h.DB.QueryRow(ctx,
		"SELECT user_id FROM claw_im_users WHERE rongcloud_token=$1 AND status='active' LIMIT 1",
		token).Scan(&userID)
	if err != nil {
		clawJSON(w, 401, 401, "登录令牌无效", nil)
		return "", false
	}
	return userID, true
}

// clawSingleInstallWorkspace 按单安装假设解析 rongcloud 渠道所在的 workspace：
// 取最早的 active rongcloud 渠道安装行（与 GetAppKey 的 single-install 假设一致）。
func (h *Handler) clawSingleInstallWorkspace(ctx context.Context) (pgtype.UUID, bool) {
	var wsID pgtype.UUID
	err := h.DB.QueryRow(ctx, `
		SELECT ci.workspace_id
		FROM channel_installation ci
		JOIN workspace w ON w.id = ci.workspace_id
		JOIN agent a ON a.id = ci.agent_id
		WHERE ci.status = 'active' AND ci.channel_type = 'rongcloud'
		ORDER BY ci.created_at ASC
		LIMIT 1`).Scan(&wsID)
	if err != nil {
		slog.Warn("claw compat: resolve rongcloud workspace failed", "error", err)
		return pgtype.UUID{}, false
	}
	return wsID, true
}

// ClawCreatePairing POST /api/claw/pairing —— 旧站「绑定设备」生成 clawmessenger 票据。
// Bearer=claw_im_users.rongcloud_token 认证；workspace 取单安装归属；
// candidates 留空（设备 register 带 ticket 时回填）；有效期 10 分钟。
func (h *Handler) ClawCreatePairing(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	if h.RongCloudPairing == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	wsID, ok := h.clawSingleInstallWorkspace(r.Context())
	if !ok {
		clawJSON(w, 503, 503, "RongCloud 渠道未安装", nil)
		return
	}
	var req struct {
		ClientClaimKey string `json:"client_claim_key"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	session, err := h.RongCloudPairing.CreateSession(r.Context(), rongcloud.PairingCreateParams{
		WorkspaceID:      wsID,
		CandidateNodeIDs: nil, // 设备端 register 时回填（node_service.go）
		ClientClaimKey:   req.ClientClaimKey,
		ExpiresIn:        10 * time.Minute,
	})
	if err != nil {
		slog.Warn("claw: create pairing session failed", "error", err, "userId", userID)
		clawJSON(w, 500, 500, "创建票据失败", nil)
		return
	}
	clawJSON(w, 200, 200, "创建成功", clawPairingView{
		Ticket:    session.Ticket,
		Status:    session.Status,
		ExpiresAt: session.ExpiresAt.Time.Format(time.RFC3339),
	})
}

// ClawGetPairing GET /api/claw/pairing/{ticket} —— 旧站轮询票据状态（pending →
// claimed / expired），供绑定弹窗自动确认。
func (h *Handler) ClawGetPairing(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.clawRequireClawUser(w, r); !ok {
		return
	}
	if h.RongCloudPairing == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	ticket := chi.URLParam(r, "ticket")
	if ticket == "" {
		clawJSON(w, 400, 400, "缺少票据", nil)
		return
	}
	session, err := h.RongCloudPairing.GetSession(r.Context(), ticket)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			clawJSON(w, 404, 404, "票据不存在", nil)
			return
		}
		slog.Warn("claw: get pairing session failed", "error", err, "ticket_prefix", ticket[:min(6, len(ticket))])
		clawJSON(w, 500, 500, "查询票据失败", nil)
		return
	}
	view := clawPairingView{
		Ticket:    session.Ticket,
		Status:    session.Status,
		ExpiresAt: session.ExpiresAt.Time.Format(time.RFC3339),
		AIType:    h.RongCloudPairing.SessionClaimedNodeAIType(r.Context(), session),
	}
	var reported []string
	if len(session.ReportedAgents) > 0 && json.Unmarshal(session.ReportedAgents, &reported) == nil && len(reported) > 0 {
		view.ReportedAgents = reported
	}
	var bound []rongcloud.BoundAgentResult
	if len(session.BoundAgents) > 0 && json.Unmarshal(session.BoundAgents, &bound) == nil && len(bound) > 0 {
		view.BoundAgents = bound
	}
	clawJSON(w, 200, 200, "", view)
}

// ClawListNodes GET /api/claw/nodes —— 旧站远程设备列表（HTTP 通道，不依赖浏览器 IM 连接）。
func (h *Handler) ClawListNodes(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	if h.RongCloudNode == nil {
		clawJSON(w, 503, 503, "RongCloud 未配置", nil)
		return
	}
	records, err := h.RongCloudNode.LegacyNodeRecords(r.Context(), userID)
	if err != nil {
		slog.Warn("claw nodes list failed", "error", err)
		clawJSON(w, 500, 500, "获取设备列表失败", nil)
		return
	}
	clawJSON(w, 200, 200, "ok", map[string]interface{}{"nodes": records})
}

// ClawBindPairing POST /api/claw/pairing/{ticket}/bind —— 旧站弹窗勾选绑定 agent。
// 票据必须已 claimed（设备已 pair）；每个被选 agent 独立建 rc user + node + 凭据。
func (h *Handler) ClawBindPairing(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	if h.RongCloudPairing == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	ticket := chi.URLParam(r, "ticket")
	if ticket == "" {
		clawJSON(w, 400, 400, "缺少票据", nil)
		return
	}
	var req struct {
		Agents []string `json:"agents"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || len(req.Agents) == 0 {
		clawJSON(w, 400, 400, "缺少要绑定的智能体", nil)
		return
	}
	session, err := h.RongCloudPairing.GetSession(r.Context(), ticket)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			clawJSON(w, 404, 404, "票据不存在", nil)
			return
		}
		clawJSON(w, 500, 500, "查询票据失败", nil)
		return
	}
	// 绑定时 bindAgentsToMachine 会写好 claimed_by_user_id 投影列，需要连接池。
	h.clawAttachNodeClaimDB()
	results, err := h.RongCloudNode.BindAgents(r.Context(), session, req.Agents, userID)
	if err != nil {
		switch {
		case errors.Is(err, rongcloud.ErrPairingSessionNotClaimed):
			clawJSON(w, 409, 409, "设备尚未完成配对", nil)
		case errors.Is(err, rongcloud.ErrPairingSessionExpired):
			clawJSON(w, 410, 410, "票据已过期", nil)
		default:
			slog.Warn("claw: bind pairing agents failed", "error", err, "ticket_prefix", ticket[:min(6, len(ticket))])
			clawJSON(w, 500, 500, "绑定智能体失败", nil)
		}
		return
	}
	clawJSON(w, 200, 200, "绑定成功", map[string]interface{}{"bound": results})
}

// ClawDeviceNodes POST /api/claw/device/nodes —— clawmessenger supervisor 取回本机全部
// 已绑定 agent 的连接凭据。认证 = 机器节点 device credential（nodeId + credentialId + secret）。
func (h *Handler) ClawDeviceNodes(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	var req struct {
		NodeID       string `json:"nodeId"`
		CredentialID string `json:"credentialId"`
		Secret       string `json:"secret"`
		// 新 CLI（0.2.39+）置真：为本机每个 agent 确保专属运维节点并随凭据
		// 下发（运维会话按智能体隔离）。缺省 false 保持旧行为。
		WantPerAgentOps bool `json:"wantPerAgentOps"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.NodeID == "" || req.CredentialID == "" || req.Secret == "" {
		clawJSON(w, 400, 400, "缺少设备凭据", nil)
		return
	}
	creds, err := h.RongCloudNode.MachineAgentCredentials(r.Context(), req.NodeID, req.CredentialID, req.Secret, req.WantPerAgentOps)
	if err != nil {
		if errors.Is(err, rongcloud.ErrInvalidDeviceCredential) {
			clawJSON(w, 401, 401, "设备凭据无效", nil)
			return
		}
		slog.Warn("claw: device nodes lookup failed", "nodeId", req.NodeID, "error", err)
		clawJSON(w, 500, 500, "查询智能体节点失败", nil)
		return
	}
	clawJSON(w, 200, 200, "", map[string]interface{}{"nodes": creds})
}

// ClawDeviceBindAgents POST /api/claw/device/bind-agents —— 已配对设备用机器
// device credential 主动绑定本机 agent（clawmessenger supervisor 为内置 ops 智能体
// 自动调用）。每个 agent 独立建 rc user + node + 凭据，幂等可重复调用。
func (h *Handler) ClawDeviceBindAgents(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	var req struct {
		NodeID       string   `json:"nodeId"`
		CredentialID string   `json:"credentialId"`
		Secret       string   `json:"secret"`
		Agents       []string `json:"agents"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.NodeID == "" || req.CredentialID == "" || req.Secret == "" {
		clawJSON(w, 400, 400, "缺少设备凭据", nil)
		return
	}
	if len(req.Agents) == 0 {
		clawJSON(w, 400, 400, "缺少要绑定的智能体", nil)
		return
	}
	results, err := h.RongCloudNode.BindAgentsByDeviceCredential(r.Context(), req.NodeID, req.CredentialID, req.Secret, req.Agents)
	if err != nil {
		if errors.Is(err, rongcloud.ErrInvalidDeviceCredential) {
			clawJSON(w, 401, 401, "设备凭据无效", nil)
			return
		}
		slog.Warn("claw: device bind agents failed", "nodeId", req.NodeID, "error", err)
		clawJSON(w, 500, 500, "绑定智能体失败", nil)
		return
	}
	clawJSON(w, 200, 200, "绑定成功", map[string]interface{}{"bound": results})
}

// ClawDeviceHeartbeat POST /api/claw/device/heartbeat ���� clawmessenger ���豸�Ĭ��
// �豸ÿ 30s ��һ��֤������ƾ�ݣ�����ڴ�¼�������豸��ʱ״̬�� 90s ����ȡ
func (h *Handler) ClawDeviceHeartbeat(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		clawJSON(w, 503, 503, "RongCloud ����δ����", nil)
		return
	}
	var req struct {
		NodeID       string `json:"nodeId"`
		CredentialID string `json:"credentialId"`
		Secret       string `json:"secret"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.NodeID == "" || req.CredentialID == "" || req.Secret == "" {
		clawJSON(w, 400, 400, "ȱ���豸ƾ��", nil)
		return
	}
	if err := h.RongCloudNode.Heartbeat(r.Context(), req.NodeID, req.CredentialID, req.Secret); err != nil {
		if errors.Is(err, rongcloud.ErrInvalidDeviceCredential) {
			clawJSON(w, 401, 401, "�豸ƾ����Ч", nil)
			return
		}
		slog.Warn("claw: device heartbeat failed", "nodeId", req.NodeID, "error", err)
		clawJSON(w, 500, 500, "�Ĵ����ʧ��", nil)
		return
	}
	clawJSON(w, 200, 200, "", map[string]interface{}{"ok": true})
}

// ClawNodeStatus GET /api/claw/nodes/{nodeId} �Ѱ�վ�����豸ʱ״̬�����ģ����ʱ״̬����
func (h *Handler) ClawNodeStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.clawRequireClawUser(w, r); !ok {
		return
	}
	if h.RongCloudNode == nil {
		clawJSON(w, 503, 503, "RongCloud ����δ����", nil)
		return
	}
	nodeID := chi.URLParam(r, "nodeId")
	if nodeID == "" {
		clawJSON(w, 400, 400, "ȱ���ڵ�ID", nil)
		return
	}
	node, err := h.RongCloudNode.GetNodeByNodeID(r.Context(), nodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			clawJSON(w, 404, 404, "�ڵ㲻����", nil)
			return
		}
		slog.Warn("claw: node status lookup failed", "nodeId", nodeID, "error", err)
		clawJSON(w, 500, 500, "��ѯ�ڵ�ʧ��", nil)
		return
	}
	resp := map[string]interface{}{"online": h.RongCloudNode.IsOnline(node.ID)}
	// 运维助手（ops 节点）在设备列表里是隐藏的，但它的会话页头需要真实的
	// 在线状态 —— 否则前端只能写死 false，「运维助手离线」就成了常态误报。
	// 机器上没有 ops 节点时不下发该字段，前端回退到节点自身状态。
	if omRC, omOnline, found, err := h.RongCloudNode.OmStatusForNode(r.Context(), node); err != nil {
		slog.Warn("claw: om status lookup failed", "nodeId", nodeID, "error", err)
	} else if found {
		resp["om_online"] = omOnline
		resp["om_rongcloud_id"] = omRC
	}
	clawJSON(w, 200, 200, "", resp)
}

// clawNodeOps 是 ClawUpdateNode / ClawDeleteNode 用到的 NodeService 方法子集。
//
// 抽成接口只为了可测：NodeService 带未导出的融云 HTTP 客户端参数，在 handler
// 包外无法构造（见 claw_device_auth_test.go 的说明），而这两个端点的越权分支
// 必须能断言「一个写方法都没被调用」。生产路径恒为 h.RongCloudNode。
type clawNodeOps interface {
	GetNodeByNodeID(ctx context.Context, nodeIDText string) (db.RongcloudNode, error)
	NodeOwnedBy(ctx context.Context, ownerRCUserID string, node db.RongcloudNode) (bool, error)
	GetUserByRongCloudID(ctx context.Context, rcUserID string) (db.RongcloudUser, error)
	UpdateUserProfile(ctx context.Context, id pgtype.UUID, name, portrait string) error
	UnbindNode(ctx context.Context, node db.RongcloudNode, ownerRongCloudID string) error
	UnregisterNode(ctx context.Context, node db.RongcloudNode, ownerRongCloudID string) error
}

// clawNodeService 返回这两个设备写端点要用的节点服务（nil = 未启用融云渠道）。
// 先判空再返回，避免把 nil 指针装进接口后接口本身不为 nil。
func (h *Handler) clawNodeService() clawNodeOps {
	if h.clawNodeOpsForTest != nil {
		return h.clawNodeOpsForTest
	}
	if h.RongCloudNode == nil {
		return nil
	}
	h.clawAttachNodeClaimDB()
	return h.RongCloudNode
}

// clawAttachNodeClaimDB 把连接池注入节点服务，供 rongcloud_node.claimed_by_user_id
// 的读写使用。该列是 600 号迁移新加的，sqlc 里还没有查询（本轮不做全量 regen），
// 只能直连；NodeService 在 router 里只拿到 *db.Queries，所以在 handler 取用时
// 按需注入。幂等、nil 安全 —— 没注入时投影列写不进去，但归属真相在融云好友边，
// 读路径会回退到反查并回填，不会答错。
func (h *Handler) clawAttachNodeClaimDB() {
	if h.RongCloudNode == nil {
		return
	}
	h.RongCloudNode.SetClaimDB(h.DB)
}

// clawNodeOwnershipMessage 是「节点存在但不属于当前用户」的返回文案，与
// 「节点不存在」逐字相同：node_id 形如 node_<16hex>，会出现在设备列表响应和
// 前端 URL 里，不是凭据；一旦归属失败回 403/401，「不存在」与「别人的」就能
// 靠错误码和文案区分开，node_id 于是成了可枚举的探针。
const clawNodeOwnershipMessage = "节点不存在"

// ClawUpdateNode POST /api/claw/nodes/{nodeId} 旧站编辑设备资料（昵称/头像）走 HTTP，
// 替代原 IM 通道 ai/updateNode（Go 兼容层从未实现，旧链路恒报错）。
func (h *Handler) ClawUpdateNode(w http.ResponseWriter, r *http.Request) {
	clawUserID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	ops := h.clawNodeService()
	if ops == nil {
		clawJSON(w, 503, 503, "RongCloud 渠道未安装", nil)
		return
	}
	nodeID := chi.URLParam(r, "nodeId")
	if nodeID == "" {
		clawJSON(w, 400, 400, "缺少节点ID", nil)
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		PortraitUri string `json:"portraitUri"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		clawJSON(w, 400, 400, "请求体格式错误", nil)
		return
	}
	node, err := ops.GetNodeByNodeID(r.Context(), nodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			clawJSON(w, 404, 404, clawNodeOwnershipMessage, nil)
			return
		}
		slog.Warn("claw: node update lookup failed", "nodeId", nodeID, "error", err)
		clawJSON(w, 500, 500, "查询节点失败", nil)
		return
	}
	// 归属校验必须挡在任何写之前。口径见 NodeOwnedBy：owner → node 的融云
	// 好友边，与绑定侧（bindAgentsToMachine）和设备列表同一份真相。
	owned, err := ops.NodeOwnedBy(r.Context(), clawUserID, node)
	if err != nil {
		slog.Warn("claw: node update ownership lookup failed", "nodeId", nodeID, "user", clawUserID, "error", err)
		clawJSON(w, 500, 500, "查询节点失败", nil)
		return
	}
	if !owned {
		slog.Warn("claw: node update denied for non-owner", "nodeId", nodeID, "user", clawUserID)
		clawJSON(w, 404, 404, clawNodeOwnershipMessage, nil)
		return
	}
	user, err := ops.GetUserByRongCloudID(r.Context(), node.RongcloudUserID)
	if err != nil {
		slog.Warn("claw: node update user lookup failed", "nodeId", nodeID, "error", err)
		clawJSON(w, 500, 500, "查询节点用户失败", nil)
		return
	}
	name := req.Name
	if name == "" {
		name = user.Name.String
	}
	portrait := req.PortraitUri
	if portrait == "" {
		portrait = user.PortraitUri.String
	}
	if err := ops.UpdateUserProfile(r.Context(), user.ID, name, portrait); err != nil {
		slog.Warn("claw: node update profile failed", "nodeId", nodeID, "error", err)
		clawJSON(w, 500, 500, "更新节点资料失败", nil)
		return
	}
	clawJSON(w, 200, 200, "", map[string]interface{}{"ok": true})
}

// ClawDeleteNode DELETE /api/claw/nodes/{nodeId} —— 旧站「取消绑定 / 删除设备」。
//
// 原链路是 IM 通道 claw/unbind，而 Go 兼容层只实现了 ping/chatroom/device/
// discussion/ai 五个服务，从没有 claw —— 请求被当成 unknown service 丢掉，
// 浏览器侧的 sendSystemRequest 只能等到超时（用户看到的「claw/unbind 请求超时」）。
// 与设备列表 / 状态 / 资料更新一样，这里改走 HTTP。
func (h *Handler) ClawDeleteNode(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	ops := h.clawNodeService()
	if ops == nil {
		clawJSON(w, 503, 503, "RongCloud 渠道未安装", nil)
		return
	}
	nodeID := chi.URLParam(r, "nodeId")
	if nodeID == "" {
		clawJSON(w, 400, 400, "缺少节点ID", nil)
		return
	}
	node, err := ops.GetNodeByNodeID(r.Context(), nodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			clawJSON(w, 404, 404, clawNodeOwnershipMessage, nil)
			return
		}
		slog.Warn("claw: node delete lookup failed", "nodeId", nodeID, "error", err)
		clawJSON(w, 500, 500, "查询节点失败", nil)
		return
	}
	// 归属校验挡在解绑/注销之前：两个分支都会删本地节点行（并尽力删融云好友
	// 边），unregister=true 还会把 agent 的融云账号永久注销，所以这里不能
	// 只靠「node_id 拿得到」。口径与 ClawUpdateNode 相同（NodeOwnedBy）。
	owned, err := ops.NodeOwnedBy(r.Context(), userID, node)
	if err != nil {
		slog.Warn("claw: node delete ownership lookup failed", "nodeId", nodeID, "user", userID, "error", err)
		clawJSON(w, 500, 500, "查询节点失败", nil)
		return
	}
	if !owned {
		slog.Warn("claw: node delete denied for non-owner", "nodeId", nodeID, "user", userID)
		clawJSON(w, 404, 404, clawNodeOwnershipMessage, nil)
		return
	}
	// unregister=true：用户在前端确认弹窗里选了「注销 Agent 并解绑」—— 融云
	// 账号永久注销（不可恢复），必须显式 opt-in；缺省仅解除绑定。
	if r.URL.Query().Get("unregister") == "true" {
		err = ops.UnregisterNode(r.Context(), node, userID)
	} else {
		err = ops.UnbindNode(r.Context(), node, userID)
	}
	if err != nil {
		slog.Warn("claw: node unbind failed", "nodeId", nodeID, "unregister", r.URL.Query().Get("unregister"), "error", err)
		clawJSON(w, 500, 500, "取消绑定失败", nil)
		return
	}
	// 归属列来自 NodeClaimants 的 15s 记忆化好友边反查。这里刚删掉节点行与
	// 好友边，缓存必须一起失效 —— 否则设备列表在 TTL 内仍会显示已解绑的归属
	// （admin 的删除路径一直有这一步，本端点漏了）。
	if h.RongCloudNode != nil {
		h.RongCloudNode.InvalidateNodeClaimants()
	}
	clawJSON(w, 200, 200, "节点删除成功", map[string]interface{}{"ok": true})
}
