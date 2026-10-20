package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/quota"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 用户配额 API 的集成测试（真实库，不可达时 Skip —— 与 rongcloud/quota 包同一约定）。
//
// 覆盖验收标准：
//   - admin PUT 保存即生效（下一次 Resolve 读到新值）且 updated_by 落库；
//   - GET 返回 override/effective/usage 三段；
//   - 非法取值 400、目标用户不存在 404、无 token 401；
//   - C 端 GET /api/quota/me 返回三类 limit/used/remaining，未登录 401。

// quotaTestPool 连本地开发库；不可达时 Skip。
func quotaTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("skipping: cannot build pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skipping: db unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func quotaUnique(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// quotaFixture 备一个 C 端用户（带 rongcloud_token）+ 一个超管账号 + 装好依赖的 Handler。
type quotaFixture struct {
	pool     *pgxpool.Pool
	h        *Handler
	userID   string
	token    string
	adminID  string
	adminJWT string
}

func setupQuotaFixture(t *testing.T) *quotaFixture {
	t.Helper()
	pool := quotaTestPool(t)
	ctx := context.Background()
	f := &quotaFixture{
		pool:    pool,
		userID:  quotaUnique("quota-rest-u"),
		token:   quotaUnique("tok"),
		adminID: quotaUnique("quota-admin"),
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO claw_im_users (user_id, username, nickname, password_hash, status, rongcloud_token)
		VALUES ($1, $2, $3, 'x', 'active', $4)`,
		f.userID, "u-"+f.userID, "Quota User", f.token); err != nil {
		t.Fatalf("seed claw user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO claw_admin_users (admin_id, username, nickname, password_hash, role_id)
		VALUES ($1, $2, $3, 'x', 'super_admin')`,
		f.adminID, "boss-"+f.adminID, "配额管理员"); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DELETE FROM claw_im_user_quota WHERE user_id=$1", f.userID)
		_, _ = pool.Exec(ctx, "DELETE FROM claw_im_users WHERE user_id=$1", f.userID)
		_, _ = pool.Exec(ctx, "DELETE FROM claw_admin_users WHERE admin_id=$1", f.adminID)
	})
	token, err := issueClawAdminJWT(f.adminID, "access", time.Hour)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	f.adminJWT = token
	f.h = &Handler{DB: pool, Queries: db.New(pool), ClawQuota: quota.New(pool)}
	return f
}

// adminReq 造一个带超管 Bearer 与 chi {id} 路由参数的请求。
func (f *quotaFixture) adminReq(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.adminJWT)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", f.userID)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func decodeQuotaBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rr.Body.String())
	}
	return body
}

func TestClawAdminQuotaPutThenGetEffective(t *testing.T) {
	f := setupQuotaFixture(t)

	// PUT：discussion=1 / group=-1（不限）/ chatroom=null（跟随默认）。
	rr := httptest.NewRecorder()
	f.h.ClawAdminPutUserQuota(rr, f.adminReq(t, "PUT", "/api/admin/users/"+f.userID+"/quota",
		`{"discussion_limit": 1, "group_limit": -1, "chatroom_limit": null}`))
	if rr.Code != 200 {
		t.Fatalf("PUT = %d (%s)", rr.Code, rr.Body.String())
	}
	body := decodeQuotaBody(t, rr)
	data, _ := body["data"].(map[string]interface{})
	override, _ := data["override"].(map[string]interface{})
	if override["discussion_limit"].(float64) != 1 || override["group_limit"].(float64) != -1 {
		t.Fatalf("override = %#v, want {discussion:1, group:-1}", override)
	}
	if _, ok := override["chatroom_limit"]; ok && override["chatroom_limit"] != nil {
		t.Fatalf("override.chatroom_limit = %v, want null（跟随默认）", override["chatroom_limit"])
	}
	effective, _ := data["effective"].(map[string]interface{})
	if effective["discussion_limit"].(float64) != 1 {
		t.Fatalf("effective.discussion_limit = %v, want 1", effective["discussion_limit"])
	}
	if effective["group_limit"].(float64) != -1 {
		t.Fatalf("effective.group_limit = %v, want -1", effective["group_limit"])
	}
	if effective["chatroom_limit"].(float64) != float64(quota.DefaultChatroomLimit) {
		t.Fatalf("effective.chatroom_limit = %v, want 默认 %d", effective["chatroom_limit"], quota.DefaultChatroomLimit)
	}
	if data["updated_by"] != "boss-"+f.adminID {
		t.Fatalf("updated_by = %v, want 当前管理员", data["updated_by"])
	}

	// 保存即生效：同一个 Service 实例的 Resolve 立刻读到新值（无需重启）。
	limits, err := f.h.ClawQuota.Resolve(context.Background(), f.userID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if limits.DiscussionLimit != 1 || limits.GroupLimit != -1 || limits.ChatroomLimit != quota.DefaultChatroomLimit {
		t.Fatalf("Resolve = %+v, want {1,-1,%d}", limits, quota.DefaultChatroomLimit)
	}

	// GET：override/effective/usage 三段齐全。
	rr = httptest.NewRecorder()
	f.h.ClawAdminGetUserQuota(rr, f.adminReq(t, "GET", "/api/admin/users/"+f.userID+"/quota", ""))
	if rr.Code != 200 {
		t.Fatalf("GET = %d (%s)", rr.Code, rr.Body.String())
	}
	body = decodeQuotaBody(t, rr)
	data, _ = body["data"].(map[string]interface{})
	usage, _ := data["usage"].(map[string]interface{})
	disc, _ := usage["discussion"].(map[string]interface{})
	if disc["limit"].(float64) != 1 || disc["used"].(float64) != 0 {
		t.Fatalf("usage.discussion = %#v, want {limit:1, used:0}", disc)
	}
	if data["updated_by"] != "boss-"+f.adminID {
		t.Fatalf("GET updated_by = %v", data["updated_by"])
	}
}

func TestClawAdminQuotaValidation(t *testing.T) {
	f := setupQuotaFixture(t)

	// 非法取值（< -1）→ 400。
	rr := httptest.NewRecorder()
	f.h.ClawAdminPutUserQuota(rr, f.adminReq(t, "PUT", "/api/admin/users/"+f.userID+"/quota",
		`{"discussion_limit": -2}`))
	if rr.Code != 400 {
		t.Fatalf("非法取值 = %d, want 400 (%s)", rr.Code, rr.Body.String())
	}
	// 非法 JSON → 400。
	rr = httptest.NewRecorder()
	f.h.ClawAdminPutUserQuota(rr, f.adminReq(t, "PUT", "/api/admin/users/"+f.userID+"/quota", `{not json`))
	if rr.Code != 400 {
		t.Fatalf("非法 JSON = %d, want 400", rr.Code)
	}
	// 目标用户不存在 → 404。
	req := httptest.NewRequest("PUT", "/api/admin/users/ghost/quota", strings.NewReader(`{"discussion_limit": 1}`))
	req.Header.Set("Authorization", "Bearer "+f.adminJWT)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "ghost")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr = httptest.NewRecorder()
	f.h.ClawAdminPutUserQuota(rr, req)
	if rr.Code != 404 {
		t.Fatalf("不存在用户 = %d, want 404", rr.Code)
	}
	// 无 token → 401。
	rr = httptest.NewRecorder()
	f.h.ClawAdminGetUserQuota(rr, httptest.NewRequest("GET", "/api/admin/users/x/quota", nil))
	if rr.Code != 401 {
		t.Fatalf("无 token = %d, want 401", rr.Code)
	}
}

func TestClawGetMyQuotaREST(t *testing.T) {
	f := setupQuotaFixture(t)

	// 未登录 → 401。
	rr := httptest.NewRecorder()
	f.h.ClawGetMyQuota(rr, httptest.NewRequest("GET", "/api/quota/me", nil))
	if rr.Code != 401 {
		t.Fatalf("未登录 = %d, want 401", rr.Code)
	}

	// 默认套餐（免费版 3/5/1）生效：三类 limit/used/remaining 齐全。
	req := httptest.NewRequest("GET", "/api/quota/me", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	rr = httptest.NewRecorder()
	f.h.ClawGetMyQuota(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /api/quota/me = %d (%s)", rr.Code, rr.Body.String())
	}
	body := decodeQuotaBody(t, rr)
	data, _ := body["data"].(map[string]interface{})
	disc, _ := data["discussion"].(map[string]interface{})
	if disc["limit"].(float64) != float64(quota.DefaultDiscussionLimit) {
		t.Fatalf("discussion.limit = %v, want 默认 %d", disc["limit"], quota.DefaultDiscussionLimit)
	}
	if disc["used"].(float64) != 0 || disc["remaining"].(float64) != float64(quota.DefaultDiscussionLimit) {
		t.Fatalf("discussion used/remaining = %v/%v", disc["used"], disc["remaining"])
	}
	for _, kind := range []string{"group", "chatroom"} {
		if _, ok := data[kind].(map[string]interface{}); !ok {
			t.Fatalf("缺 %s 段: %#v", kind, data)
		}
	}
}

func TestClawListMyDiscussionsREST(t *testing.T) {
	f := setupQuotaFixture(t)
	ctx := context.Background()

	auth := func(target string) *http.Request {
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("Authorization", "Bearer "+f.token)
		return req
	}
	// status 缺省/未知 → 400。
	rr := httptest.NewRecorder()
	f.h.ClawListMyDiscussions(rr, auth("/api/my/discussions"))
	if rr.Code != 400 {
		t.Fatalf("缺 status = %d, want 400", rr.Code)
	}
	// 空列表 → 200 []。
	rr = httptest.NewRecorder()
	f.h.ClawListMyDiscussions(rr, auth("/api/my/discussions?status=configuring"))
	if rr.Code != 200 {
		t.Fatalf("空列表 = %d (%s)", rr.Code, rr.Body.String())
	}

	// 造一个半成品：镜像行 + 聊天室 + configuring 讨论。
	wsID := seedQuotaWorkspace(t, f.pool)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO rongcloud_user (workspace_id, rongcloud_user_id, name, node_type)
		VALUES ($1, $2, 'mirror', 'human') ON CONFLICT (rongcloud_user_id) DO NOTHING`,
		wsID, f.userID); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	chatroomID := quotaUnique("rc-quota-room")
	discussionID := quotaUnique("disc-quota")
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO rongcloud_chatroom (workspace_id, rongcloud_chatroom_id, owner_user_id, max_rounds, conversation_kind, config, status, name)
		VALUES ($1, $2, (SELECT id FROM rongcloud_user WHERE rongcloud_user_id=$3), 10, 'discussion', '{}', 'active', '配额测试组')`,
		wsID, chatroomID, f.userID); err != nil {
		t.Fatalf("seed chatroom: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO rongcloud_discussion (discussion_id, chatroom_id, workspace_id, created_by, topic, goal, mode, status, round_policy, setup_step)
		VALUES ($1, $2, $3, $4, '议题甲', '', 'roundtable', 'configuring', 'user_confirmed', 'roles')`,
		discussionID, chatroomID, wsID, f.userID); err != nil {
		t.Fatalf("seed discussion: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_discussion WHERE discussion_id=$1", discussionID)
		_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1", chatroomID)
		_, _ = f.pool.Exec(ctx, "DELETE FROM rongcloud_user WHERE rongcloud_user_id=$1", f.userID)
	})

	rr = httptest.NewRecorder()
	f.h.ClawListMyDiscussions(rr, auth("/api/my/discussions?status=configuring"))
	if rr.Code != 200 {
		t.Fatalf("列表 = %d (%s)", rr.Code, rr.Body.String())
	}
	body := decodeQuotaBody(t, rr)
	items, _ := body["data"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("半成品数 = %d, want 1 (%s)", len(items), rr.Body.String())
	}
	item, _ := items[0].(map[string]interface{})
	if item["chatroomId"] != chatroomID || item["step"] != "roles" || item["topic"] != "议题甲" || item["name"] != "配额测试组" {
		t.Fatalf("半成品条目形状错: %#v", item)
	}
}

// seedQuotaWorkspace 建一个测试 workspace，返回其 UUID 文本（rongcloud_user.workspace_id 外键用）。
func seedQuotaWorkspace(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	slug := quotaUnique("quota-ws")
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO workspace (name, slug, description) VALUES ($1, $2, '') RETURNING id`,
		"ws-"+slug, slug).Scan(&id); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	return id
}
