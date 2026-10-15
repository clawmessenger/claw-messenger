// Package handler - 虾说 admin-web 兼容层。
//
// 移植旧 Python 后端 /api/admin/* 的公开 HTTP 契约到 Go，供
// clawmessenger-admin-web 直连。账号存 claw_admin_users（独立表）。
// auth/users/stats/system-config/audit-logs 在这里；admins/roles 提供基础 CRUD；
// nodes/groups 见 claw_admin_nodes.go / claw_admin_groups.go；
// chatrooms/discussions/messages 见 claw_admin_conversations.go。
package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
)

// ---- JWT (HS256, 紧凑形式 header.payload.signature) ----

func clawAdminJWTSecret() []byte {
	if s := os.Getenv("MULTICA_CLAW_ADMIN_JWT_SECRET"); s != "" {
		return []byte(s)
	}
	return []byte("claw-admin-dev-secret")
}

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func issueClawAdminJWT(adminID, tokenType string, ttl time.Duration) (string, error) {
	header := base64URL([]byte(`{"alg":"HS256","typ":"JWT"}`))
	now := time.Now()
	payload := fmt.Sprintf(`{"sub":%q,"typ":%q,"iat":%d,"exp":%d}`,
		adminID, tokenType, now.Unix(), now.Add(ttl).Unix())
	signingInput := header + "." + base64URL([]byte(payload))
	mac := hmac.New(sha256.New, clawAdminJWTSecret())
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64URL(mac.Sum(nil)), nil
}

func decodeClawAdminJWT(token, wantType string) (adminID string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	mac := hmac.New(sha256.New, clawAdminJWTSecret())
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(mac.Sum(nil), mustBase64URLDecode(parts[2])) {
		return "", false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var payload struct {
		Sub string `json:"sub"`
		Typ string `json:"typ"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(payloadBytes, &payload) != nil {
		return "", false
	}
	if payload.Typ != wantType || time.Now().Unix() >= payload.Exp {
		return "", false
	}
	return payload.Sub, true
}

func mustBase64URLDecode(s string) []byte {
	b, _ := base64.RawURLEncoding.DecodeString(s)
	return b
}

// ---- 管理员身份 ----

type clawAdminIdentity struct {
	AdminID     string
	Username    string
	Nickname    string
	RoleID      string
	RoleName    string
	Status      string
	Permissions map[string]bool
}

var clawAdminAllPermissions = []string{
	"admin:read", "admin:write", "role:read", "role:write",
	"user:read", "user:write", "node:read", "node:write",
	"group:read", "group:write", "message:read",
	"system:read", "system:write", "stats:read", "audit:read",
}

func (h *Handler) loadClawAdmin(adminID string) (*clawAdminIdentity, error) {
	row := h.DB.QueryRow(context.Background(), `
		SELECT u.admin_id, u.username, u.nickname, u.role_id, u.status,
		       COALESCE(r.role_name, ''), COALESCE(array_to_json(r.permissions), '[]')
		FROM claw_admin_users u
		LEFT JOIN claw_admin_roles r ON r.role_id = u.role_id
		WHERE u.admin_id = $1 LIMIT 1`, adminID)
	var id clawAdminIdentity
	var perms []byte
	if err := row.Scan(&id.AdminID, &id.Username, &id.Nickname, &id.RoleID, &id.Status,
		&id.RoleName, &perms); err != nil {
		return nil, err
	}
	id.Permissions = map[string]bool{}
	var list []string
	_ = json.Unmarshal(perms, &list)
	for _, p := range list {
		id.Permissions[p] = true
	}
	return &id, nil
}

// requireClawAdmin 解析 Bearer token 并校验权限。
func (h *Handler) requireClawAdmin(permission string, w http.ResponseWriter, r *http.Request) *clawAdminIdentity {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		clawAdminFail(w, 401, "Missing authentication token")
		return nil
	}
	adminID, ok := decodeClawAdminJWT(strings.TrimPrefix(auth, "Bearer "), "access")
	if !ok {
		clawAdminFail(w, 401, "Invalid authentication token")
		return nil
	}
	ident, err := h.loadClawAdmin(adminID)
	if err != nil || ident.Status != "active" {
		clawAdminFail(w, 401, "Invalid authentication token")
		return nil
	}
	// "*" 是角色编辑里可选的通配权限，前端 hasPermission 也这么认；
	// 后端此前只做字面比对，导致带 "*" 的角色处处 403。
	if permission != "" && !ident.Permissions[permission] && !ident.Permissions["*"] {
		clawAdminFail(w, 403, "没有权限执行此操作")
		return nil
	}
	return ident
}

func clawAdminFail(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": status, "message": message})
}

func clawAdminOK(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 0, "message": "ok", "data": data})
}

// ---- 分页 ----

func clawAdminPagination(r *http.Request) (page, pageSize int) {
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ = strconv.Atoi(r.URL.Query().Get("page_size"))
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	return page, pageSize
}

func clawAdminLikePattern(keyword string) string {
	escaped := strings.ReplaceAll(keyword, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "%", "\\%")
	escaped = strings.ReplaceAll(escaped, "_", "\\_")
	return "%" + escaped + "%"
}

// ---- 审计 ----

func (h *Handler) clawAdminAudit(r *http.Request, ident *clawAdminIdentity, action, resourceType, resourceID string, detail map[string]interface{}) {
	if ident == nil {
		return
	}
	detailJSON, _ := json.Marshal(detail)
	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	_, _ = h.DB.Exec(r.Context(), `
		INSERT INTO claw_admin_audit_logs (admin_id, action, resource_type, resource_id, detail, ip_address)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		ident.AdminID, action, resourceType, resourceID, detailJSON, ip)
}

// ---- /api/admin/auth ----

func (h *Handler) ClawAdminLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		clawAdminFail(w, 400, "JSON object is required")
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" || body.Password == "" {
		clawAdminFail(w, 400, "username and password are required")
		return
	}
	var adminID, passwordHash, status string
	err := h.DB.QueryRow(r.Context(), `
		SELECT admin_id, password_hash, status FROM claw_admin_users
		WHERE username = $1 LIMIT 1`, body.Username).Scan(&adminID, &passwordHash, &status)
	if err != nil || !clawVerifyPassword(body.Password, passwordHash) {
		clawAdminFail(w, 401, "Invalid credentials")
		return
	}
	if status != "active" {
		clawAdminFail(w, 403, "Account disabled")
		return
	}
	_, _ = h.DB.Exec(r.Context(), `UPDATE claw_admin_users SET last_login_at = now(), updated_at = now() WHERE admin_id = $1`, adminID)
	ident, _ := h.loadClawAdmin(adminID)
	h.clawAdminAudit(r, ident, "admin_login", "admin", adminID, map[string]interface{}{"auth_material_rotated": false})
	access, _ := issueClawAdminJWT(adminID, "access", 2*time.Hour)
	refresh, _ := issueClawAdminJWT(adminID, "refresh", 7*24*time.Hour)
	clawAdminOK(w, map[string]interface{}{
		"access_token": access, "refresh_token": refresh,
		"token_type": "bearer", "expires_in": 7200,
	})
}

func (h *Handler) ClawAdminRefresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	adminID, ok := decodeClawAdminJWT(body.RefreshToken, "refresh")
	if !ok {
		clawAdminFail(w, 401, "Invalid authentication token")
		return
	}
	ident, err := h.loadClawAdmin(adminID)
	if err != nil || ident.Status != "active" {
		clawAdminFail(w, 401, "Invalid authentication token")
		return
	}
	access, _ := issueClawAdminJWT(adminID, "access", 2*time.Hour)
	refresh, _ := issueClawAdminJWT(adminID, "refresh", 7*24*time.Hour)
	clawAdminOK(w, map[string]interface{}{
		"access_token": access, "refresh_token": refresh,
	})
}

func (h *Handler) ClawAdminLogout(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("", w, r)
	if ident == nil {
		return
	}
	h.clawAdminAudit(r, ident, "admin_logout", "admin", ident.AdminID, map[string]interface{}{"outcome": "complete"})
	clawAdminOK(w, map[string]interface{}{"ok": true})
}

func (h *Handler) ClawAdminMe(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("", w, r)
	if ident == nil {
		return
	}
	perms := []string{}
	for _, p := range clawAdminAllPermissions {
		if ident.Permissions[p] {
			perms = append(perms, p)
		}
	}
	clawAdminOK(w, map[string]interface{}{
		"admin_id": ident.AdminID, "username": ident.Username,
		"nickname": ident.Nickname, "role_id": ident.RoleID,
		"role_name": ident.RoleName, "permissions": perms,
	})
}

// ---- /api/admin/users（数据源 claw_im_users） ----

// clawAdminOperationUserPredicate 判定一个 IM 账号是不是「运维账号」。
//
// claw_im_users 只装人类 IM 账号（C 端注册写入，见 claw_compat.go），没有
// account_type / ai_type 列，所以这两列都是**推导**出来的，而不是写死：
//   - 运维账号 = 前缀 om_ 的运维单聊账号，或被 ai_type='ops' 的运维节点占用的账号；
//   - AI 类型 = 该账号被哪个节点占用（rongcloud_node.rongcloud_user_id）。
//
// 推导而不是硬编码的意义在于接口与筛选器真的会动：以前 /api/admin/users 把
// account_type 恒返回 'normal'、ai_type 恒返回 NULL，前端的「运维用户」和
// 「AI 类型」两个筛选器点了等于没点。
const clawAdminOperationUserPredicate = `(
	starts_with(u.user_id, 'om_')
	OR EXISTS(SELECT 1 FROM rongcloud_node n
	          WHERE n.rongcloud_user_id = u.user_id AND n.ai_type = 'ops')
)`

const clawAdminUserSelect = `
	SELECT u.user_id, u.username, COALESCE(u.nickname,''), COALESCE(u.phone,''), COALESCE(u.email,''),
	       COALESCE(u.portrait_uri,''), u.status,
	       CASE WHEN ` + clawAdminOperationUserPredicate + ` THEN 'operation' ELSE 'normal' END,
	       (SELECT NULLIF(n.ai_type,'') FROM rongcloud_node n
	        WHERE n.rongcloud_user_id = u.user_id AND NULLIF(n.ai_type,'') IS NOT NULL
	        ORDER BY n.created_at DESC LIMIT 1),
	       u.created_at, u.updated_at
	FROM claw_im_users u`

var clawAdminUserAccountTypes = map[string]bool{"normal": true, "operation": true}

func (h *Handler) ClawAdminListUsers(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("user:read", w, r) == nil {
		return
	}
	page, pageSize := clawAdminPagination(r)
	q := r.URL.Query()
	keyword, status := strings.TrimSpace(q.Get("keyword")), strings.TrimSpace(q.Get("status"))
	accountType := strings.TrimSpace(q.Get("account_type"))
	aiType := strings.TrimSpace(q.Get("ai_type"))
	if accountType != "" && !clawAdminUserAccountTypes[accountType] {
		clawAdminFail(w, 400, "account_type has an unsupported value")
		return
	}
	where, args := " WHERE 1=1", []interface{}{}
	if keyword != "" {
		args = append(args, clawAdminLikePattern(keyword))
		where += fmt.Sprintf(" AND (u.user_id ILIKE $%d OR u.username ILIKE $%d OR u.nickname ILIKE $%d OR u.phone ILIKE $%d OR u.email ILIKE $%d)", len(args), len(args), len(args), len(args), len(args))
	}
	if status != "" {
		args = append(args, status)
		where += fmt.Sprintf(" AND u.status = $%d", len(args))
	}
	switch accountType {
	case "operation":
		where += " AND " + clawAdminOperationUserPredicate
	case "normal":
		where += " AND NOT " + clawAdminOperationUserPredicate
	}
	if aiType != "" {
		args = append(args, aiType)
		where += fmt.Sprintf(" AND EXISTS(SELECT 1 FROM rongcloud_node n WHERE n.rongcloud_user_id = u.user_id AND n.ai_type = $%d)", len(args))
	}
	var total int
	_ = h.DB.QueryRow(r.Context(), "SELECT COUNT(*) FROM claw_im_users u"+where, args...).Scan(&total)
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := h.DB.Query(r.Context(), clawAdminUserSelect+where+" ORDER BY created_at DESC LIMIT $"+strconv.Itoa(len(args)-1)+" OFFSET $"+strconv.Itoa(len(args)), args...)
	if err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}
	defer rows.Close()
	items := []map[string]interface{}{}
	for rows.Next() {
		var userID, username, nickname, phone, email, portrait, st, accountType string
		var aiType *string
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&userID, &username, &nickname, &phone, &email, &portrait, &st, &accountType, &aiType, &createdAt, &updatedAt); err != nil {
			continue
		}
		items = append(items, map[string]interface{}{
			"user_id": userID, "username": username, "nickname": nickname,
			"phone": phone, "email": email, "portrait_uri": portrait,
			"status": st, "account_type": accountType, "ai_type": aiType,
			"created_at": createdAt.UTC().Format(time.RFC3339), "updated_at": updatedAt.UTC().Format(time.RFC3339),
		})
	}
	clawAdminOK(w, map[string]interface{}{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func (h *Handler) ClawAdminGetUser(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("user:read", w, r) == nil {
		return
	}
	userID := chi.URLParam(r, "id")
	row := h.DB.QueryRow(r.Context(), clawAdminUserSelect+" WHERE u.user_id = $1", userID)
	var username, nickname, phone, email, portrait, st, accountType string
	var aiType *string
	var createdAt, updatedAt time.Time
	if err := row.Scan(&userID, &username, &nickname, &phone, &email, &portrait, &st, &accountType, &aiType, &createdAt, &updatedAt); err != nil {
		clawAdminFail(w, 404, "user not found")
		return
	}
	clawAdminOK(w, map[string]interface{}{
		"user_id": userID, "username": username, "nickname": nickname,
		"phone": phone, "email": email, "portrait_uri": portrait,
		"status": st, "account_type": accountType, "ai_type": aiType,
		"created_at": createdAt.UTC().Format(time.RFC3339), "updated_at": updatedAt.UTC().Format(time.RFC3339),
		"nodes": []interface{}{}, "groups": []interface{}{},
	})
}

func (h *Handler) ClawAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("user:write", w, r)
	if ident == nil {
		return
	}
	userID := chi.URLParam(r, "id")
	var body struct {
		Nickname    *string `json:"nickname"`
		Phone       *string `json:"phone"`
		Email       *string `json:"email"`
		PortraitURI *string `json:"portrait_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		clawAdminFail(w, 400, "JSON object is required")
		return
	}
	tag, err := h.DB.Exec(r.Context(), `
		UPDATE claw_im_users SET
			nickname = COALESCE($1, nickname), phone = COALESCE($2, phone),
			email = COALESCE($3, email), portrait_uri = COALESCE($4, portrait_uri),
			updated_at = now()
		WHERE user_id = $5`,
		body.Nickname, body.Phone, body.Email, body.PortraitURI, userID)
	affected := tag.RowsAffected()
	if err != nil || affected == 0 {
		clawAdminFail(w, 404, "user not found")
		return
	}
	h.clawAdminAudit(r, ident, "admin_user_update", "user", userID, nil)
	h.ClawAdminGetUser(w, r)
}

var clawAdminUserStatuses = map[string]bool{"active": true, "inactive": true, "banned": true, "disabled": true}

func (h *Handler) ClawAdminUpdateUserStatus(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("user:write", w, r)
	if ident == nil {
		return
	}
	userID := chi.URLParam(r, "id")
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || !clawAdminUserStatuses[body.Status] {
		clawAdminFail(w, 400, "status has an unsupported value")
		return
	}
	tag, err := h.DB.Exec(r.Context(), `UPDATE claw_im_users SET status = $1, updated_at = now() WHERE user_id = $2`, body.Status, userID)
	affected := tag.RowsAffected()
	if err != nil || affected == 0 {
		clawAdminFail(w, 404, "user not found")
		return
	}
	h.clawAdminAudit(r, ident, "admin_user_status", "user", userID, map[string]interface{}{"status": body.Status})
	clawAdminOK(w, map[string]interface{}{"ok": true})
}

// ClawAdminUserNodes 返回该用户名下（按融云好友边认领）的设备。
//
// 归属判定与「设备管理」页同源：rongcloud_node.owner_user_id 指向 multica 账号，
// 和 IM 账号不是一个命名空间，能用的只有 NodeClaimants 反查出来的融云好友边
// （见 claw_admin_nodes.go 的说明），所以这里直接复用同一套投影，两个页面列的
// 同一台设备不会出现不同的名字或状态。
func (h *Handler) ClawAdminUserNodes(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("user:read", w, r) == nil {
		return
	}
	ctx := r.Context()
	userID := strings.TrimSpace(chi.URLParam(r, "id"))
	if _, found, err := h.clawAdminUserExists(ctx, userID); err != nil {
		clawAdminFail(w, 503, "用户信息不可用")
		return
	} else if !found {
		clawAdminFail(w, 404, "user not found")
		return
	}
	if h.RongCloudNode == nil {
		// 融云集成未启用时归属无法推导，明确回空而不是报错：用户详情页的
		// 「拥有节点」只是一个标签页，不应因集成缺失整页 500。
		clawAdminOK(w, []interface{}{})
		return
	}
	claims, err := h.RongCloudNode.NodeClaimants(ctx, []string{userID})
	if err != nil {
		clawAdminOK(w, []interface{}{})
		return
	}
	owned := make(map[string]bool, len(claims))
	for nodeRCID, claimant := range claims {
		if claimant == userID {
			owned[nodeRCID] = true
		}
	}
	if len(owned) == 0 {
		clawAdminOK(w, []interface{}{})
		return
	}

	records, err := h.clawAdminListNodeRecords(ctx)
	if err != nil {
		clawAdminFail(w, 503, "获取设备列表失败")
		return
	}
	names, reserved, _ := h.clawAdminNodeUserNames(ctx)
	descriptions, _ := h.clawAdminNodeDescriptions(ctx)
	hostNodes := h.clawAdminReservedHostNodeKeys(ctx)
	controls := h.clawAdminNodeControls(ctx)

	items := []clawAdminNodeRow{}
	for _, rec := range records {
		if !owned[rec.RongcloudUserID] || !rongcloud.IsDisplayNode(rec.dbNode()) {
			continue
		}
		items = append(items, h.clawAdminNodeRowFor(
			rec, names[rec.RongcloudUserID], descriptions[rec.NodeID], userID, "",
			reserved[rec.RongcloudUserID] || hostNodes[rec.NodeID], controls[uuidToText(rec.ID)],
		))
	}
	clawAdminOK(w, items)
}

// ClawAdminUserGroups 返回该用户加入的群，直接复用群组的本地镜像查询，
// 保证与「群组管理」页看到的同一条群数据完全一致。
func (h *Handler) ClawAdminUserGroups(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("user:read", w, r) == nil {
		return
	}
	userID := strings.TrimSpace(chi.URLParam(r, "id"))
	if _, found, err := h.clawAdminUserExists(r.Context(), userID); err != nil {
		clawAdminFail(w, 503, "用户信息不可用")
		return
	} else if !found {
		clawAdminFail(w, 404, "user not found")
		return
	}
	rows, err := h.DB.Query(r.Context(), `
		SELECT g.group_id, g.group_name, g.owner_id, g.member_count, COALESCE(g.description, ''),
		       g.created_at, g.updated_at
		FROM rongcloud_group g
		JOIN rongcloud_group_member m ON m.group_id = g.group_id
		WHERE m.user_id = $1
		ORDER BY g.created_at DESC`, userID)
	if err != nil {
		clawAdminFail(w, 503, "用户群组不可用")
		return
	}
	defer rows.Close()
	items := []map[string]interface{}{}
	for rows.Next() {
		group, err := clawAdminScanGroup(rows)
		if err != nil {
			continue
		}
		items = append(items, group)
	}
	if err := rows.Err(); err != nil {
		clawAdminFail(w, 503, "用户群组不可用")
		return
	}
	clawAdminOK(w, items)
}

// clawAdminUserExists 用于区分「用户不存在」（404）与「存在但没有节点/群」。
func (h *Handler) clawAdminUserExists(ctx context.Context, userID string) (string, bool, error) {
	if userID == "" {
		return "", false, nil
	}
	var found string
	err := h.DB.QueryRow(ctx, `SELECT user_id FROM claw_im_users WHERE user_id = $1 LIMIT 1`, userID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return found, true, nil
}

// ---- /api/admin/stats ----

// ClawAdminStats 汇总看板卡片。这里所有数字都来自真实表，并且**与各自页面同源**：
//   - 用户：claw_im_users（与「用户管理」同一张表；运维账号用与列表筛选器完全相同的
//     推导规则，避免看板与列表页对不上）
//   - 节点：复用 clawAdminListNodeRecords + rongcloud.IsDisplayNode + NodeService.IsOnline，
//     与「节点管理」页的可见性与在线判据逐行一致（以前这里恒返回 0，
//     页面上 11 台设备显示成 0）
//   - 消息：rongcloud_discussion_event 行数（Go 侧唯一持久化的 IM 消息流）
func (h *Handler) ClawAdminStats(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("stats:read", w, r) == nil {
		return
	}
	ctx := r.Context()

	var users, normalUsers, operationUsers int
	_ = h.DB.QueryRow(ctx, `SELECT COUNT(*) FROM claw_im_users`).Scan(&users)
	_ = h.DB.QueryRow(ctx, `SELECT COUNT(*) FROM claw_im_users u WHERE u.status = 'active'`).Scan(&normalUsers)
	_ = h.DB.QueryRow(ctx, `SELECT COUNT(*) FROM claw_im_users u WHERE `+clawAdminOperationUserPredicate).Scan(&operationUsers)

	// nodes / online / offline / provider 分布。取记录后在 Go 里过滤，而不是把
	// IsDisplayNode 的判据（含 machineIdentityKey 的 sha256 分支）在 SQL 里重写一遍。
	nodes, onlineNodes := 0, 0
	providers := map[string]int{}
	if records, err := h.clawAdminListNodeRecords(ctx); err == nil {
		for _, rec := range records {
			if !rongcloud.IsDisplayNode(rec.dbNode()) {
				continue
			}
			nodes++
			if h.clawAdminNodeStatusIsOnline(rec) {
				onlineNodes++
			}
			if rec.AIType != "" {
				providers[rec.AIType]++
			}
		}
	}

	var groups, chatrooms, discussions, messages, roundDocuments int
	_ = h.DB.QueryRow(ctx, `SELECT COUNT(*) FROM rongcloud_group`).Scan(&groups)
	_ = h.DB.QueryRow(ctx, `SELECT COUNT(*) FROM rongcloud_chatroom WHERE status = 'active'`).Scan(&chatrooms)
	_ = h.DB.QueryRow(ctx, `SELECT COUNT(*) FROM rongcloud_discussion`).Scan(&discussions)
	_ = h.DB.QueryRow(ctx, `SELECT COUNT(*) FROM rongcloud_discussion_event`).Scan(&messages)
	_ = h.DB.QueryRow(ctx, `SELECT COUNT(*) FROM rongcloud_discussion_round_document`).Scan(&roundDocuments)

	providerNodes := map[string]interface{}{}
	for provider, count := range providers {
		providerNodes[provider] = count
	}

	// openclaw_nodes / opencode_nodes / … 是旧契约里的具名计数，从同一份 provider
	// 分布里取值，保证它们与 provider_nodes 永远一致。
	named := func(provider string) int { return providers[provider] }

	clawAdminOK(w, map[string]interface{}{
		"users": users, "normal_users": normalUsers, "operation_users": operationUsers,
		"nodes":          nodes,
		"openclaw_nodes": named("openclaw"), "opencode_nodes": named("opencode"),
		"codex_nodes": named("codex"), "kimi_nodes": named("kimi"),
		"online_nodes": onlineNodes, "offline_nodes": nodes - onlineNodes,
		"groups":    groups,
		"chatrooms": chatrooms, "discussions": discussions,
		"messages": messages, "im_messages": messages, "chat_messages": roundDocuments,
		"provider_nodes": providerNodes,
		"audit_logs":     h.clawAdminAuditCount(),
	})
}

func (h *Handler) clawAdminNodeStatusIsOnline(rec clawAdminNodeRecord) bool {
	if h.RongCloudNode == nil {
		return false
	}
	return h.RongCloudNode.IsOnline(rec.ID)
}

func (h *Handler) clawAdminAuditCount() int {
	var n int
	_ = h.DB.QueryRow(context.Background(), "SELECT COUNT(*) FROM claw_admin_audit_logs").Scan(&n)
	return n
}

// ---- /api/admin/audit-logs ----

func (h *Handler) ClawAdminAuditLogs(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("audit:read", w, r) == nil {
		return
	}
	page, pageSize := clawAdminPagination(r)
	q := r.URL.Query()
	adminID := strings.TrimSpace(q.Get("admin_id"))
	action := strings.TrimSpace(q.Get("action"))
	keyword := strings.TrimSpace(q.Get("keyword"))
	where, args := " WHERE 1=1", []interface{}{}
	if adminID != "" {
		args = append(args, adminID)
		where += fmt.Sprintf(" AND admin_id = $%d", len(args))
	}
	if action != "" {
		args = append(args, action)
		where += fmt.Sprintf(" AND action = $%d", len(args))
	}
	if keyword != "" {
		p := clawAdminLikePattern(keyword)
		args = append(args, p, p, p, p)
		where += fmt.Sprintf(" AND (action ILIKE $%d OR admin_id ILIKE $%d OR resource_type ILIKE $%d OR resource_id ILIKE $%d)", len(args)-3, len(args)-2, len(args)-1, len(args))
	}
	var total int
	_ = h.DB.QueryRow(r.Context(), "SELECT COUNT(*) FROM claw_admin_audit_logs"+where, args...).Scan(&total)
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := h.DB.Query(r.Context(), `
		SELECT id, admin_id, action, resource_type, resource_id, detail, ip_address, created_at
		FROM claw_admin_audit_logs`+where+
		" ORDER BY created_at DESC, id DESC LIMIT $"+strconv.Itoa(len(args)-1)+" OFFSET $"+strconv.Itoa(len(args)), args...)
	if err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}
	defer rows.Close()
	items := []map[string]interface{}{}
	for rows.Next() {
		var id int64
		var aID, act string
		var resType, resID, ip *string
		var detail []byte
		var createdAt time.Time
		if err := rows.Scan(&id, &aID, &act, &resType, &resID, &detail, &ip, &createdAt); err != nil {
			continue
		}
		var detailVal interface{}
		_ = json.Unmarshal(detail, &detailVal)
		if detailVal == nil {
			detailVal = map[string]interface{}{}
		}
		items = append(items, map[string]interface{}{
			"id": id, "admin_id": aID, "action": act,
			"resource_type": resType, "resource_id": resID, "detail": detailVal,
			"ip_address": ip, "created_at": createdAt.UTC().Format(time.RFC3339),
		})
	}
	clawAdminOK(w, map[string]interface{}{"items": items, "total": total, "page": page, "page_size": pageSize})
}

// ---- /api/admin/system/config ----

// clawAdminSystemConfigRows 返回 rongcloud_system_config 的通用行查询。
func (h *Handler) clawAdminSystemConfigRows(ctx context.Context, key string) ([]map[string]interface{}, error) {
	where, args := "", []interface{}{}
	if key != "" {
		where = " WHERE config_key = $1"
		args = append(args, key)
	}
	rows, err := h.DB.Query(ctx, `
		SELECT config_key, config, created_at, updated_at
		FROM rongcloud_system_config`+where+" ORDER BY config_key", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]interface{}{}
	for rows.Next() {
		var k string
		var cfg []byte
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&k, &cfg, &createdAt, &updatedAt); err != nil {
			continue
		}
		var value interface{}
		_ = json.Unmarshal(cfg, &value)
		if value == nil {
			value = map[string]interface{}{}
		}
		items = append(items, map[string]interface{}{
			"config_key": k, "config_value": value,
			"created_at": createdAt.UTC().Format(time.RFC3339),
			"updated_at": updatedAt.UTC().Format(time.RFC3339),
		})
	}
	return items, nil
}

func (h *Handler) ClawAdminSystemConfigList(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("system:read", w, r) == nil {
		return
	}
	items, err := h.clawAdminSystemConfigRows(r.Context(), "")
	if err != nil {
		clawAdminOK(w, []interface{}{})
		return
	}
	// 含密钥的配置（融云 Access Key 等）不在这里透出，只在专属接口里脱敏返回。
	secret := clawAdminSecretConfigKeys()
	visible := items[:0]
	for _, item := range items {
		if key, ok := item["config_key"].(string); ok && secret[key] {
			continue
		}
		visible = append(visible, item)
	}
	clawAdminOK(w, visible)
}

func (h *Handler) ClawAdminSystemConfigGet(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("system:read", w, r) == nil {
		return
	}
	key := chi.URLParam(r, "key")
	if clawAdminSecretConfigKeys()[key] {
		clawAdminFail(w, 404, "config not found")
		return
	}
	items, err := h.clawAdminSystemConfigRows(r.Context(), key)
	if err != nil || len(items) == 0 {
		clawAdminFail(w, 404, "config not found")
		return
	}
	clawAdminOK(w, items[0])
}

func (h *Handler) ClawAdminSystemConfigPut(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("system:write", w, r)
	if ident == nil {
		return
	}
	key := chi.URLParam(r, "key")
	var body struct {
		ConfigValue string `json:"config_value"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body); err != nil {
		clawAdminFail(w, 400, "JSON object is required")
		return
	}
	// config 列为 jsonb：value 可能是 JSON 文档，也可能纯文本（包成 {"value": ...}）。
	var cfgJSON []byte
	if json.Valid([]byte(body.ConfigValue)) {
		cfgJSON = []byte(body.ConfigValue)
	} else {
		b, _ := json.Marshal(map[string]string{"value": body.ConfigValue})
		cfgJSON = b
	}
	// upsert 以 (workspace_id, config_key) 为唯一约束；workspace 取该键现有行或默认工作区。
	var wsID string
	err := h.DB.QueryRow(r.Context(),
		"SELECT workspace_id::text FROM rongcloud_system_config WHERE config_key=$1 LIMIT 1", key).Scan(&wsID)
	if err != nil {
		err = h.DB.QueryRow(r.Context(),
			"SELECT id::text FROM workspace ORDER BY created_at LIMIT 1").Scan(&wsID)
	}
	if err != nil {
		clawAdminFail(w, 500, "no workspace available")
		return
	}
	_, err = h.DB.Exec(r.Context(), `
		INSERT INTO rongcloud_system_config (workspace_id, config_key, config)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, config_key)
		DO UPDATE SET config = $3, config_version = rongcloud_system_config.config_version + 1, updated_at = now()`,
		wsID, key, cfgJSON)
	if err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}
	h.clawAdminAudit(r, ident, "admin_system_config_update", "system_config", key, nil)
	clawAdminOK(w, map[string]interface{}{
		"config_key": key, "config_value": body.ConfigValue,
	})
}

// ---- /api/admin/admins ----

func (h *Handler) ClawAdminListAdmins(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("admin:read", w, r) == nil {
		return
	}
	page, pageSize := clawAdminPagination(r)
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	where, args := " WHERE 1=1", []interface{}{}
	if keyword != "" {
		args = append(args, clawAdminLikePattern(keyword))
		where += fmt.Sprintf(" AND (username ILIKE $%d OR nickname ILIKE $%d)", len(args), len(args))
	}
	var total int
	_ = h.DB.QueryRow(r.Context(), "SELECT COUNT(*) FROM claw_admin_users"+where, args...).Scan(&total)
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := h.DB.Query(r.Context(), `
		SELECT u.admin_id, u.username, u.nickname, u.status, u.role_id,
		       COALESCE(r.role_name,''), u.last_login_at, u.created_at, u.updated_at
		FROM claw_admin_users u LEFT JOIN claw_admin_roles r ON r.role_id = u.role_id`+where+
		" ORDER BY u.created_at DESC LIMIT $"+strconv.Itoa(len(args)-1)+" OFFSET $"+strconv.Itoa(len(args)), args...)
	if err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}
	defer rows.Close()
	items := []map[string]interface{}{}
	for rows.Next() {
		var adminID, username, st, roleID, roleName string
		var nickname *string
		var lastLogin *time.Time
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&adminID, &username, &nickname, &st, &roleID, &roleName, &lastLogin, &createdAt, &updatedAt); err != nil {
			continue
		}
		item := map[string]interface{}{
			"admin_id": adminID, "username": username, "nickname": nickname,
			"status": st, "role_id": roleID, "role_name": roleName,
			"created_at": createdAt.UTC().Format(time.RFC3339), "updated_at": updatedAt.UTC().Format(time.RFC3339),
		}
		if lastLogin != nil {
			item["last_login_at"] = lastLogin.UTC().Format(time.RFC3339)
		}
		items = append(items, item)
	}
	clawAdminOK(w, map[string]interface{}{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func (h *Handler) ClawAdminGetAdmin(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("admin:read", w, r) == nil {
		return
	}
	adminID := chi.URLParam(r, "id")
	item, ok := h.clawAdminFetchAdmin(w, adminID, true)
	if !ok {
		return
	}
	clawAdminOK(w, item)
}

func (h *Handler) clawAdminFetchAdmin(w http.ResponseWriter, adminID string, withPerms bool) (map[string]interface{}, bool) {
	row := h.DB.QueryRow(context.Background(), `
		SELECT u.admin_id, u.username, u.nickname, u.status, u.role_id,
		       COALESCE(r.role_name,''), COALESCE(array_to_json(r.permissions), '[]'), u.last_login_at, u.created_at, u.updated_at
		FROM claw_admin_users u LEFT JOIN claw_admin_roles r ON r.role_id = u.role_id
		WHERE u.admin_id = $1`, adminID)
	var username, st, roleID, roleName string
	var nickname *string
	var perms []byte
	var lastLogin *time.Time
	var createdAt, updatedAt time.Time
	if err := row.Scan(&adminID, &username, &nickname, &st, &roleID, &roleName, &perms, &lastLogin, &createdAt, &updatedAt); err != nil {
		clawAdminFail(w, 404, "admin not found")
		return nil, false
	}
	item := map[string]interface{}{
		"admin_id": adminID, "username": username, "nickname": nickname,
		"status": st, "role_id": roleID, "role_name": roleName,
		"created_at": createdAt.UTC().Format(time.RFC3339), "updated_at": updatedAt.UTC().Format(time.RFC3339),
	}
	if lastLogin != nil {
		item["last_login_at"] = lastLogin.UTC().Format(time.RFC3339)
	}
	if withPerms {
		var list []string
		_ = json.Unmarshal(perms, &list)
		if list == nil {
			list = []string{}
		}
		item["permissions"] = list
	}
	return item, true
}

func (h *Handler) ClawAdminCreateAdmin(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("admin:write", w, r)
	if ident == nil {
		return
	}
	var body struct {
		Username string  `json:"username"`
		Password string  `json:"password"`
		Nickname *string `json:"nickname"`
		RoleID   string  `json:"role_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		clawAdminFail(w, 400, "JSON object is required")
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" || len(body.Password) < 6 || body.RoleID == "" {
		clawAdminFail(w, 400, "username, password(>=6), role_id are required")
		return
	}
	adminID := fmt.Sprintf("adm-%d", time.Now().UnixNano())
	_, err := h.DB.Exec(r.Context(), `
		INSERT INTO claw_admin_users (admin_id, username, nickname, password_hash, role_id)
		VALUES ($1, $2, $3, $4, $5)`,
		adminID, body.Username, body.Nickname, clawHashPassword(body.Password), body.RoleID)
	if err != nil {
		clawAdminFail(w, 409, "username already exists")
		return
	}
	h.clawAdminAudit(r, ident, "admin_admin_create", "admin", adminID, nil)
	item, _ := h.clawAdminFetchAdmin(w, adminID, true)
	clawAdminOK(w, item)
}

func (h *Handler) ClawAdminUpdateAdmin(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("admin:write", w, r)
	if ident == nil {
		return
	}
	adminID := chi.URLParam(r, "id")
	var body struct {
		Nickname *string `json:"nickname"`
		RoleID   *string `json:"role_id"`
		Password *string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		clawAdminFail(w, 400, "JSON object is required")
		return
	}
	if body.Password != nil && len(*body.Password) < 6 {
		clawAdminFail(w, 400, "password too short")
		return
	}
	var passwordHash *string
	if body.Password != nil {
		h := clawHashPassword(*body.Password)
		passwordHash = &h
	}
	tag, err := h.DB.Exec(r.Context(), `
		UPDATE claw_admin_users SET
			nickname = COALESCE($1, nickname), role_id = COALESCE($2, role_id),
			password_hash = COALESCE($3, password_hash), updated_at = now()
		WHERE admin_id = $4`,
		body.Nickname, body.RoleID, passwordHash, adminID)
	affected := tag.RowsAffected()
	if err != nil || affected == 0 {
		clawAdminFail(w, 404, "admin not found")
		return
	}
	h.clawAdminAudit(r, ident, "admin_admin_update", "admin", adminID, nil)
	item, _ := h.clawAdminFetchAdmin(w, adminID, true)
	clawAdminOK(w, item)
}

func (h *Handler) ClawAdminUpdateAdminStatus(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("admin:write", w, r)
	if ident == nil {
		return
	}
	adminID := chi.URLParam(r, "id")
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || (body.Status != "active" && body.Status != "inactive") {
		clawAdminFail(w, 400, "status must be active or inactive")
		return
	}
	if adminID == ident.AdminID && body.Status != "active" {
		clawAdminFail(w, 400, "cannot deactivate yourself")
		return
	}
	tag, err := h.DB.Exec(r.Context(), `UPDATE claw_admin_users SET status = $1, updated_at = now() WHERE admin_id = $2`, body.Status, adminID)
	affected := tag.RowsAffected()
	if err != nil || affected == 0 {
		clawAdminFail(w, 404, "admin not found")
		return
	}
	h.clawAdminAudit(r, ident, "admin_admin_status", "admin", adminID, map[string]interface{}{"status": body.Status})
	clawAdminOK(w, map[string]interface{}{"ok": true})
}

func (h *Handler) ClawAdminDeleteAdmin(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("admin:write", w, r)
	if ident == nil {
		return
	}
	adminID := chi.URLParam(r, "id")
	if adminID == ident.AdminID {
		clawAdminFail(w, 400, "cannot delete yourself")
		return
	}
	tag, err := h.DB.Exec(r.Context(), `DELETE FROM claw_admin_users WHERE admin_id = $1`, adminID)
	affected := tag.RowsAffected()
	if err != nil || affected == 0 {
		clawAdminFail(w, 404, "admin not found")
		return
	}
	h.clawAdminAudit(r, ident, "admin_admin_delete", "admin", adminID, nil)
	clawAdminOK(w, map[string]interface{}{"ok": true})
}

// ---- /api/admin/roles ----

func (h *Handler) ClawAdminListRoles(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("role:read", w, r) == nil {
		return
	}
	page, pageSize := clawAdminPagination(r)
	rows, err := h.DB.Query(r.Context(), `
		SELECT role_id, name, role_name, description, status, array_to_json(permissions), created_at, updated_at
		FROM claw_admin_roles ORDER BY created_at LIMIT $1 OFFSET $2`, pageSize, (page-1)*pageSize)
	if err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}
	defer rows.Close()
	items := []map[string]interface{}{}
	for rows.Next() {
		var roleID, name, roleName, st string
		var description *string
		var perms []byte
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&roleID, &name, &roleName, &description, &st, &perms, &createdAt, &updatedAt); err != nil {
			continue
		}
		var list []string
		_ = json.Unmarshal(perms, &list)
		if list == nil {
			list = []string{}
		}
		items = append(items, map[string]interface{}{
			"role_id": roleID, "name": name, "role_name": roleName,
			"description": description, "status": st, "permissions": list,
			"created_at": createdAt.UTC().Format(time.RFC3339), "updated_at": updatedAt.UTC().Format(time.RFC3339),
		})
	}
	var total int
	_ = h.DB.QueryRow(r.Context(), "SELECT COUNT(*) FROM claw_admin_roles").Scan(&total)
	clawAdminOK(w, map[string]interface{}{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func (h *Handler) ClawAdminGetRole(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("role:read", w, r) == nil {
		return
	}
	item, ok := h.clawAdminFetchRole(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	clawAdminOK(w, item)
}

func (h *Handler) clawAdminFetchRole(w http.ResponseWriter, roleID string) (map[string]interface{}, bool) {
	row := h.DB.QueryRow(context.Background(), `
		SELECT role_id, name, role_name, description, status, array_to_json(permissions), created_at, updated_at
		FROM claw_admin_roles WHERE role_id = $1`, roleID)
	var name, roleName, st string
	var description *string
	var perms []byte
	var createdAt, updatedAt time.Time
	if err := row.Scan(&roleID, &name, &roleName, &description, &st, &perms, &createdAt, &updatedAt); err != nil {
		clawAdminFail(w, 404, "role not found")
		return nil, false
	}
	var list []string
	_ = json.Unmarshal(perms, &list)
	if list == nil {
		list = []string{}
	}
	return map[string]interface{}{
		"role_id": roleID, "name": name, "role_name": roleName,
		"description": description, "status": st, "permissions": list,
		"created_at": createdAt.UTC().Format(time.RFC3339), "updated_at": updatedAt.UTC().Format(time.RFC3339),
	}, true
}

func (h *Handler) ClawAdminUpsertRole(w http.ResponseWriter, r *http.Request, create bool) {
	ident := h.requireClawAdmin("role:write", w, r)
	if ident == nil {
		return
	}
	var body struct {
		Name        string   `json:"name"`
		RoleName    string   `json:"role_name"`
		Description *string  `json:"description"`
		Permissions []string `json:"permissions"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		clawAdminFail(w, 400, "JSON object is required")
		return
	}
	roleID := body.Name
	if !create {
		roleID = chi.URLParam(r, "id")
	}
	if roleID == "" || body.RoleName == "" {
		clawAdminFail(w, 400, "name and role_name are required")
		return
	}
	perms := body.Permissions
	if perms == nil {
		perms = []string{}
	}
	permsJSON := perms
	var rowsAffected int64
	var err error
	if create {
		_, err = h.DB.Exec(r.Context(), `
			INSERT INTO claw_admin_roles (role_id, name, role_name, description, permissions)
			VALUES ($1, $2, $3, $4, $5)`,
			roleID, body.Name, body.RoleName, body.Description, permsJSON)
		rowsAffected = 1
	} else {
		tag, execErr := h.DB.Exec(r.Context(), `
			UPDATE claw_admin_roles SET name = $1, role_name = $2, description = $3,
				permissions = $4, updated_at = now()
			WHERE role_id = $5`,
			body.Name, body.RoleName, body.Description, permsJSON, roleID)
		err = execErr
		if execErr == nil {
			rowsAffected = tag.RowsAffected()
		}
	}
	if err != nil {
		clawAdminFail(w, 409, "role already exists")
		return
	}
	if !create && rowsAffected == 0 {
		clawAdminFail(w, 404, "role not found")
		return
	}
	action := "admin_role_update"
	if create {
		action = "admin_role_create"
	}
	h.clawAdminAudit(r, ident, action, "role", roleID, nil)
	item, _ := h.clawAdminFetchRole(w, roleID)
	clawAdminOK(w, item)
}

func (h *Handler) ClawAdminCreateRole(w http.ResponseWriter, r *http.Request) {
	h.ClawAdminUpsertRole(w, r, true)
}
func (h *Handler) ClawAdminUpdateRole(w http.ResponseWriter, r *http.Request) {
	h.ClawAdminUpsertRole(w, r, false)
}

func (h *Handler) ClawAdminDeleteRole(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("role:write", w, r)
	if ident == nil {
		return
	}
	roleID := chi.URLParam(r, "id")
	var inUse int
	_ = h.DB.QueryRow(r.Context(), "SELECT COUNT(*) FROM claw_admin_users WHERE role_id = $1", roleID).Scan(&inUse)
	if inUse > 0 {
		clawAdminFail(w, 409, "role is in use")
		return
	}
	tag, err := h.DB.Exec(r.Context(), `DELETE FROM claw_admin_roles WHERE role_id = $1`, roleID)
	affected := tag.RowsAffected()
	if err != nil || affected == 0 {
		clawAdminFail(w, 404, "role not found")
		return
	}
	h.clawAdminAudit(r, ident, "admin_role_delete", "role", roleID, nil)
	clawAdminOK(w, map[string]interface{}{"ok": true})
}
