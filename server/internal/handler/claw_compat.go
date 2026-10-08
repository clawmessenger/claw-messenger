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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
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

func (c *clawRongClient) getUserToken(ctx context.Context, userID, name, portraitURI string) (string, error) {
	form := url.Values{}
	form.Set("userId", userID)
	form.Set("name", name)
	if portraitURI != "" {
		form.Set("portraitUri", portraitURI)
	}

	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	nonceStr := hex.EncodeToString(nonce)
	timestamp := fmt.Sprintf("%d", time.Now().UnixMilli())
	// 融云签名规则：SHA1(AppSecret + Nonce + Timestamp)
	sum := sha1.Sum([]byte(c.appSecret + nonceStr + timestamp))
	signature := hex.EncodeToString(sum[:])

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.rong-api.com/user/getToken.json", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("App-Key", c.appKey)
	req.Header.Set("Nonce", nonceStr)
	req.Header.Set("Timestamp", timestamp)
	req.Header.Set("Signature", signature)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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

// ---- 用户行 ----

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
}

func (h *Handler) clawQueryUser(ctx context.Context, field string, arg string) (*clawUserRow, error) {
	allowed := map[string]bool{"user_id": true, "username": true}
	if !allowed[field] {
		return nil, fmt.Errorf("invalid field")
	}
	row := h.DB.QueryRow(ctx,
		"SELECT user_id, username, COALESCE(nickname,username), COALESCE(portrait_uri,''), "+
			"COALESCE(email,''), COALESCE(phone,''), COALESCE(signature,''), COALESCE(gender,''), "+
			"COALESCE(birthday,''), status, password_hash, rongcloud_token "+
			"FROM claw_im_users WHERE "+field+" = $1", arg)
	var u clawUserRow
	err := row.Scan(&u.UserID, &u.Username, &u.Nickname, &u.PortraitURI, &u.Email, &u.Phone,
		&u.Signature, &u.Gender, &u.Birthday, &u.Status, &u.PasswordHash, &u.RCStored)
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
	nickname := strings.TrimSpace(req.Nickname)
	if nickname == "" {
		nickname = username
	}

	_, err = h.DB.Exec(ctx, `
		INSERT INTO claw_im_users (user_id, username, nickname, password_hash, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'active', now(), now())`,
		userID, username, nickname, clawHashPassword(req.Password))
	if err != nil {
		clawJSON(w, 500, 500, "注册失败", nil)
		return
	}

	// 同步注册到融云（best-effort，失败不阻塞注册）
	if rc := h.clawRong(ctx); rc != nil {
		if token, err := rc.getUserToken(ctx, userID, nickname, ""); err == nil {
			_, _ = h.DB.Exec(ctx, "UPDATE claw_im_users SET rongcloud_token=$1 WHERE user_id=$2", token, userID)
		}
	}

	clawJSON(w, 200, 200, "注册成功", map[string]interface{}{
		"username": username,
		"nickname": nickname,
		"userId":   userID,
	})
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

	u, err := h.clawQueryUser(ctx, "username", username)
	if err != nil || !clawVerifyPassword(req.Password, u.PasswordHash) {
		clawJSON(w, 401, 401, "用户名或密码错误", nil)
		return
	}
	if u.Status != "active" {
		clawJSON(w, 401, 401, "账户已被禁用", nil)
		return
	}

	// 每次登录换发融云 token（避免过期），失败回退库内 token
	token := ""
	if rc := h.clawRong(ctx); rc != nil {
		if t, err := rc.getUserToken(ctx, u.UserID, u.Nickname, u.PortraitURI); err == nil {
			token = t
			_, _ = h.DB.Exec(ctx, "UPDATE claw_im_users SET rongcloud_token=$1 WHERE user_id=$2", t, u.UserID)
		}
	}
	if token == "" && u.RCStored.Valid {
		token = u.RCStored.String
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
		SELECT user_id, username, COALESCE(nickname, username), COALESCE(portrait_uri,''),
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
		clawJSON(w, 404, 404, "用户不存在", nil)
		return
	}
	clawJSON(w, 200, 200, "", clawUserJSON(u))
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
	_, err = h.DB.Exec(ctx, `
		UPDATE claw_im_users
		SET nickname=$1, portrait_uri=$2, phone=$3, email=$4, signature=$5, gender=$6, birthday=$7, updated_at=now()
		WHERE user_id=$8`,
		set(req.Nickname, u.Nickname), set(req.PortraitUri, u.PortraitURI),
		set(req.Phone, u.Phone), set(req.Email, u.Email),
		set(req.Signature, u.Signature), set(req.Gender, u.Gender),
		set(req.Birthday, u.Birthday), u.UserID)
	if err != nil {
		clawJSON(w, 500, 500, err.Error(), nil)
		return
	}
	updated, _ := h.clawQueryUser(ctx, "user_id", u.UserID)
	clawJSON(w, 200, 200, "", clawUserJSON(updated))
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

// ---- xiachat 设备绑定（旧站兼容端点） ----

// clawPairingView 是旧站可见的票据会话投影（驼峰命名；不含 client_claim_key /
// idempotency_key 等凭据字段，与 PairingSessionView 同口径）。
type clawPairingView struct {
	Ticket    string `json:"ticket"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expiresAt"`
	// AIType 是 claimed 后回填节点的 ai_type（register 时注册的智能体类型），
	// 供旧站弹窗提示「xiachat run --agent <name>」；pending 时为空串。
	AIType string `json:"aiType,omitempty"`
	// ReportedAgents 是 pair 时设备自动上报的本机 agent 列表（pending 时可能为空）。
	ReportedAgents []string `json:"reportedAgents,omitempty"`
	// BoundAgents 是用户在弹窗勾选绑定后生成的 [{agent,nodeId}] 结果。
	BoundAgents []rongcloud.BoundAgentResult `json:"boundAgents,omitempty"`
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

// ClawCreatePairing POST /api/claw/pairing —— 旧站「绑定设备」生成 xiachat 票据。
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
	if _, ok := h.clawRequireClawUser(w, r); !ok {
		return
	}
	if h.RongCloudNode == nil {
		clawJSON(w, 503, 503, "RongCloud 未配置", nil)
		return
	}
	records, err := h.RongCloudNode.LegacyNodeRecords(r.Context())
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
	results, err := h.RongCloudNode.BindAgents(r.Context(), session, req.Agents)
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

// ClawDeviceNodes POST /api/claw/device/nodes —— xiachat supervisor 取回本机全部
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
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.NodeID == "" || req.CredentialID == "" || req.Secret == "" {
		clawJSON(w, 400, 400, "缺少设备凭据", nil)
		return
	}
	creds, err := h.RongCloudNode.MachineAgentCredentials(r.Context(), req.NodeID, req.CredentialID, req.Secret)
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

// ClawDeviceHeartbeat POST /api/claw/device/heartbeat ���� xiachat ���豸�Ĭ��
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
	clawJSON(w, 200, 200, "", map[string]interface{}{"online": h.RongCloudNode.IsOnline(node.ID)})
}

// ClawUpdateNode POST /api/claw/nodes/{nodeId} 旧站编辑设备资料（昵称/头像）走 HTTP，
// 替代原 IM 通道 ai/updateNode（Go 兼容层从未实现，旧链路恒报错）。
func (h *Handler) ClawUpdateNode(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.clawRequireClawUser(w, r); !ok {
		return
	}
	if h.RongCloudNode == nil {
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
	node, err := h.RongCloudNode.GetNodeByNodeID(r.Context(), nodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			clawJSON(w, 404, 404, "节点不存在", nil)
			return
		}
		slog.Warn("claw: node update lookup failed", "nodeId", nodeID, "error", err)
		clawJSON(w, 500, 500, "查询节点失败", nil)
		return
	}
	user, err := h.RongCloudNode.GetUserByRongCloudID(r.Context(), node.RongcloudUserID)
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
	if err := h.RongCloudNode.UpdateUserProfile(r.Context(), user.ID, name, portrait); err != nil {
		slog.Warn("claw: node update profile failed", "nodeId", nodeID, "error", err)
		clawJSON(w, 500, 500, "更新节点资料失败", nil)
		return
	}
	clawJSON(w, 200, 200, "", map[string]interface{}{"ok": true})
}
