package rongcloud

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// portrait_default_test.go 覆盖「默认头像统一成 default_user.png 并同步融云」这
// 条需求的写入侧契约：
//
//  1. 图源唯一 —— 真人 / AI 节点 / 聊天室成员取的是同一串（群类会话见
//     portrait_group_test.go：群聊 / 讨论组 / 聊天室各有专属图）；
//  2. NeedsDefaultPortrait 的判定表 —— 只命中空值与已知旧默认值，用户自定义
//     头像绝不能命中（存量补齐最容易出错的地方）；
//  3. 每条写路径（注册 / 刷新 token / 资料更新 / 建群 / 聊天室 / 讨论组）都不再
//     产出空 portrait_uri；
//  4. backfill（SyncUserPortraits）的 dry-run 语义 —— 默认零写、零融云请求。
//
// DB 用例在数据库不可达时 Skip，与包内既有 fixture 约定一致。

// ---- 脚手架 ------------------------------------------------------------------

// portraitTestClearEnv 清掉所有参与默认图决议的 env，避免调用方 shell 里的残留
// 值让断言随环境漂移。
func portraitTestClearEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAW_DEFAULT_PORTRAIT_URI", "")
	t.Setenv("CLAW_AI_NODE_PORTRAIT_URI", "")
	t.Setenv("MULTICA_PUBLIC_URL", "")
}

// newPortraitTestDB 打开测试库；不可达时 Skip。
func newPortraitTestDB(t *testing.T) (*pgxpool.Pool, *db.Queries) {
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
	return pool, db.New(pool)
}

// rongCloudFormRecorder 是一个只记录、不联外的假融云 Server API。
type rongCloudFormRecorder struct {
	mu    sync.Mutex
	forms map[string][]url.Values
	// failPaths 命中的路径统一回 {code:1000}，用来验证「推送失败」分支。
	failPaths map[string]bool
}

func newRongCloudFormRecorder() *rongCloudFormRecorder {
	return &rongCloudFormRecorder{
		forms:     map[string][]url.Values{},
		failPaths: map[string]bool{},
	}
}

func (r *rongCloudFormRecorder) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := req.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		r.mu.Lock()
		r.forms[req.URL.Path] = append(r.forms[req.URL.Path], req.PostForm)
		fail := r.failPaths[req.URL.Path]
		r.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if fail {
			fmt.Fprint(w, `{"code":1000,"errorMessage":"boom"}`)
			return
		}
		if req.URL.Path == "/user/getToken.json" {
			fmt.Fprint(w, `{"code":200,"token":"test-token"}`)
			return
		}
		fmt.Fprint(w, `{"code":200}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// requests 返回发往 path 的全部表单（按请求顺序）。
func (r *rongCloudFormRecorder) requests(path string) []url.Values {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]url.Values(nil), r.forms[path]...)
}

// lastForm 返回发往 path 的最后一次表单，没有则 Fatal。
func (r *rongCloudFormRecorder) lastForm(t *testing.T, path string) url.Values {
	t.Helper()
	forms := r.requests(path)
	if len(forms) == 0 {
		t.Fatalf("no request reached %s", path)
	}
	return forms[len(forms)-1]
}

// seedPortraitUser 直接落一行 rongcloud_user（头像原文照写，用来构造各种存量状态）。
func seedPortraitUser(t *testing.T, q *db.Queries, ws pgtype.UUID, rcID, portrait string) db.RongcloudUser {
	t.Helper()
	var p pgtype.Text
	if portrait != "" {
		p = pgtype.Text{String: portrait, Valid: true}
	}
	u, err := q.CreateRongCloudUser(context.Background(), db.CreateRongCloudUserParams{
		WorkspaceID:     ws,
		RongcloudUserID: rcID,
		Name:            pgText("n-" + rcID),
		PortraitUri:     p,
		NodeType:        "ai",
		IsAiNode:        true,
	})
	if err != nil {
		t.Fatalf("seed rongcloud_user %s: %v", rcID, err)
	}
	return u
}

func cleanupPortraitUsers(t *testing.T, pool *pgxpool.Pool, rcIDs ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range rcIDs {
			_, _ = pool.Exec(ctx, `DELETE FROM rongcloud_user WHERE rongcloud_user_id = $1`, id)
		}
	})
}

func storedPortrait(t *testing.T, pool *pgxpool.Pool, rcID string) string {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(portrait_uri, '') FROM rongcloud_user WHERE rongcloud_user_id = $1`, rcID,
	).Scan(&got); err != nil {
		t.Fatalf("read portrait of %s: %v", rcID, err)
	}
	return got
}

// ---- 1. 图源唯一 -------------------------------------------------------------

func TestDefaultUserPortraitEnvChain(t *testing.T) {
	portraitTestClearEnv(t)

	// MULTICA_PUBLIC_URL 历史上参与过拼接，现在必须完全无效。
	t.Setenv("MULTICA_PUBLIC_URL", "https://selfhost.example.com/im-test")
	if got := DefaultUserPortrait(); got != DefaultUserPortraitURI {
		t.Fatalf("MULTICA_PUBLIC_URL must not affect the default, got %q", got)
	}

	// CLAW_AI_NODE_PORTRAIT_URI 已在 R3 删除：单独设置它不再有任何影响。
	// 这条断言是防回归用的 —— 一旦有人把「AI 专用图源」加回来，AI 与真人就又会
	// 长期显示两张不同的图，而这正是本次需求要消灭的问题。
	t.Setenv("CLAW_AI_NODE_PORTRAIT_URI", "  https://cdn.example.com/ai.png  ")
	if got := DefaultUserPortrait(); got != DefaultUserPortraitURI {
		t.Fatalf("removed CLAW_AI_NODE_PORTRAIT_URI must not affect the default, got %q", got)
	}
	// AI 节点入口也必须是同一张图（不是 agent 维度分叉）。
	if got := DefaultAgentNodePortrait("openclaw"); got != DefaultUserPortraitURI {
		t.Fatalf("AI node portrait = %q, want the unified %q", got, DefaultUserPortraitURI)
	}

	// 只剩 CLAW_DEFAULT_PORTRAIT_URI 可以覆盖。
	t.Setenv("CLAW_DEFAULT_PORTRAIT_URI", "https://cdn.example.com/all.png")
	if got := DefaultUserPortrait(); got != "https://cdn.example.com/all.png" {
		t.Fatalf("CLAW_DEFAULT_PORTRAIT_URI must be the only override, got %q", got)
	}

	// 主 env 为纯空白视作未设置 → 回到统一图（不再回落到已删除的 AI env）。
	t.Setenv("CLAW_DEFAULT_PORTRAIT_URI", "   ")
	if got := DefaultUserPortrait(); got != DefaultUserPortraitURI {
		t.Fatalf("blank primary env must fall back to the unified default, got %q", got)
	}
}

func TestDefaultPortraitIfEmpty(t *testing.T) {
	portraitTestClearEnv(t)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", DefaultUserPortraitURI},
		{"spaces", "   ", DefaultUserPortraitURI},
		{"tabs and newlines", "\t\n ", DefaultUserPortraitURI},
		{"custom keeps as is", "https://cdn.example.com/uploads/u/me.png", "https://cdn.example.com/uploads/u/me.png"},
		{"custom is trimmed", "  https://cdn.example.com/u.png  ", "https://cdn.example.com/u.png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DefaultPortraitIfEmpty(tc.in); got != tc.want {
				t.Fatalf("DefaultPortraitIfEmpty(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// env 覆盖时，空值兜底也走统一图源（不是写死的常量）。
	t.Setenv("CLAW_DEFAULT_PORTRAIT_URI", "https://cdn.example.com/all.png")
	if got := DefaultPortraitIfEmpty(""); got != "https://cdn.example.com/all.png" {
		t.Fatalf("fallback must follow the env override, got %q", got)
	}
}

func TestDefaultAgentNodePortraitIsTheSameImage(t *testing.T) {
	portraitTestClearEnv(t)

	for _, agent := range []string{"claude", "codex", "opencode", "openclaw", "hermes", "kimi", "", "agent-we-do-not-ship"} {
		name := agent
		if name == "" {
			name = "(empty)"
		}
		t.Run(name, func(t *testing.T) {
			if got := DefaultAgentNodePortrait(agent); got != DefaultUserPortrait() {
				t.Fatalf("DefaultAgentNodePortrait(%q) = %q, want the shared %q", agent, got, DefaultUserPortrait())
			}
			if got := DefaultAgentNodePortrait(agent); got != DefaultUserPortraitURI {
				t.Fatalf("DefaultAgentNodePortrait(%q) = %q, want %q", agent, got, DefaultUserPortraitURI)
			}
		})
	}

	// env 覆盖时 AI 也跟随同一图源（不再有 openclaw.png 这类分叉）。
	t.Setenv("CLAW_DEFAULT_PORTRAIT_URI", "https://cdn.example.com/all.png")
	if got := DefaultAgentNodePortrait("openclaw"); got != DefaultUserPortrait() {
		t.Fatalf("AI portrait diverged from the human one: %q", got)
	}
}

// ---- 2. NeedsDefaultPortrait 判定表 ------------------------------------------

func TestNeedsDefaultPortraitMatrix(t *testing.T) {
	portraitTestClearEnv(t)

	cases := []struct {
		name     string
		portrait string
		want     bool
	}{
		{"NULL / empty string", "", true},
		{"whitespace only", "   ", true},
		{"legacy openclaw", legacyDefaultPortraitURIs[0], true},
		{"legacy openclaw with spaces", "  " + legacyDefaultPortraitURIs[0] + "  ", true},
		{"legacy default (production host)", legacyDefaultPortraitURIs[1], true},
		{"legacy default under the nginx prefix", "https://newsradar.dreamdt.cn/im-test/api/static/claw/avatar-default.png", true},
		{"legacy default bare path", "/api/static/claw/avatar-default.png", true},
		{"legacy default on a self-hosted domain", "https://selfhost.example.com/api/static/claw/avatar-default.png", true},
		{"legacy default with a query string", "https://newsradar.dreamdt.cn/api/static/claw/avatar-default.png?v=2", true},

		{"already unified (idempotent skip)", DefaultUserPortraitURI, false},
		{"user upload", "https://cdn.example.com/uploads/u1/avatar.png", false},
		{"user upload literally named default", "https://cdn.example.com/uploads/u1/default.png", false},
		{"user upload named avatar-default.png but elsewhere", "https://cdn.example.com/uploads/u1/avatar-default.png", false},
		{"legacy path with a suffix", "https://cdn.example.com/api/static/claw/avatar-default.png.bak", false},
		{"third party url", "https://lh3.googleusercontent.com/a/abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsDefaultPortrait(tc.portrait); got != tc.want {
				t.Fatalf("NeedsDefaultPortrait(%q) = %v, want %v", tc.portrait, got, tc.want)
			}
		})
	}
}

// 显式把「用户自定义头像」列成一组回归：存量补齐绝不能覆盖它们。
func TestNeedsDefaultPortraitNeverMatchesCustomPortraits(t *testing.T) {
	portraitTestClearEnv(t)
	custom := []string{
		"https://cdn.example.com/uploads/2026/10/me.jpg",
		"https://newsradar.dreamdt.cn/newschat/static/imgs/rongyun/default_user_v2.png",
		"https://newsradar.dreamdt.cn/newschat/static/imgs/rongyun/openclaw.png?x=1",
		"http://example.com/imgs/abc.png",
		"data:image/png;base64,iVBORw0KGgo=",
	}
	for _, portrait := range custom {
		if NeedsDefaultPortrait(portrait) {
			t.Fatalf("custom portrait %q must never be treated as a default", portrait)
		}
	}
}

// ---- 3a. 建群：空头像落库 + 补一次 refreshGroupInfo --------------------------
func TestGroupCreateFillsDefaultPortraitAndRefreshesIt(t *testing.T) {
	portraitTestClearEnv(t)
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	// web 的 CreateGroupDialog 初始头像就是空串。
	msg := groupCommand("create", "user-7", `{"groupName":"头像群"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	// 融云的 /group/create.json 根本不接收 portraitUri —— 不能假装发了。
	creates := rec.calls("/group/create.json")
	if len(creates) != 1 {
		t.Fatalf("group create calls = %d, want 1", len(creates))
	}
	if got := creates[0]["portraitUri"]; got != "" {
		t.Fatalf("group/create must not carry a portrait, got %q", got)
	}

	// 建群成功后必须补一次 /group/refresh.json 写群资料头像。目标是**群聊**专属
	// 默认图（两人剪影），不是真人那张统一默认图 —— 移动端会话列表读的就是它。
	refreshes := rec.calls("/group/refresh.json")
	if len(refreshes) != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", len(refreshes))
	}
	groupPortrait := GroupPortraitURI(GroupPortraitKindGroup)
	if got := refreshes[0]["portraitUri"]; got != groupPortrait {
		t.Fatalf("refresh portraitUri = %q, want %q", got, groupPortrait)
	}
	if got := refreshes[0]["groupId"]; got != "100001" {
		t.Fatalf("refresh groupId = %q", got)
	}
	// 补头像不能顺手把群名改掉（空 groupName 在 refreshGroupInfo 里是不 set）。
	if got := refreshes[0]["groupName"]; got != "" {
		t.Fatalf("refresh must not touch the group name, got %q", got)
	}

	// 本地镜像也必须是群聊默认图，不是空串。
	if len(stub.saved) != 1 {
		t.Fatalf("mirror saved %d groups, want 1", len(stub.saved))
	}
	if got := stub.saved[0].PortraitURI; got != groupPortrait {
		t.Fatalf("mirror portrait = %q, want %q", got, groupPortrait)
	}

	// 回包里的 portraitUri 与落库值一致（web 直接渲染它）。
	result := rec.lastCommandResult(t)
	data, ok := result["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("result data is not an object: %#v", result["data"])
	}
	if got := data["portraitUri"]; got != groupPortrait {
		t.Fatalf("command result portraitUri = %v, want %q", got, groupPortrait)
	}
}

func TestGroupCreateKeepsUserChosenPortrait(t *testing.T) {
	portraitTestClearEnv(t)
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	const custom = "https://cdn.example.com/uploads/group.png"
	msg := groupCommand("create", "user-7", `{"groupName":"头像群","portraitUri":"`+custom+`"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	refreshes := rec.calls("/group/refresh.json")
	if len(refreshes) != 1 || refreshes[0]["portraitUri"] != custom {
		t.Fatalf("refresh forms = %#v, want one carrying the custom portrait", refreshes)
	}
	if len(stub.saved) != 1 || stub.saved[0].PortraitURI != custom {
		t.Fatalf("mirror saved = %#v, want the custom portrait", stub.saved)
	}
}

func TestGroupCreateSurvivesPortraitRefreshFailure(t *testing.T) {
	portraitTestClearEnv(t)
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	rec.failPaths["/group/refresh.json"] = true
	stub := newGroupMirrorStub()
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := groupCommand("create", "user-7", `{"groupName":"G"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	// best-effort：补头像失败不回滚已经建好的群。
	if got := rec.callCount("/group/create.json"); got != 1 {
		t.Fatalf("group create calls = %d, want 1", got)
	}
	if result := rec.lastCommandResult(t); result["status"] != "success" {
		t.Fatalf("a portrait refresh failure must not fail the command: %#v", result)
	}
	if len(stub.saved) != 1 || stub.saved[0].PortraitURI != GroupPortraitURI(GroupPortraitKindGroup) {
		t.Fatalf("mirror must still record the group default portrait, got %#v", stub.saved)
	}
}

// ---- 3b. 注册 / RefreshToken / UpdateUserProfile ------------------------------

func TestRegisterSendsUnifiedPortraitToRongCloud(t *testing.T) {
	portraitTestClearEnv(t)
	pool, queries := newPortraitTestDB(t)
	rec := newRongCloudFormRecorder()
	srv := rec.start(t)

	svc := NewNodeService(queries, newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger()), nil, testLogger())
	wsID := seedWorkspaceRow(t, pool)
	mac := "aa:bb:cc:dd:ee:" + fmt.Sprintf("%02x", os.Getpid()%256)
	cleanupRegisteredNodeID(t, pool, "rc_node_"+mac)

	result, err := svc.Register(context.Background(), NodeRegisterParams{
		Name:         "portrait-register-node",
		MacAddress:   mac,
		NodeType:     "ai",
		AIType:       "claude",
		Capabilities: []string{"chat"},
		WorkspaceID:  wsID,
		OwnerUserID:  wsID, // 任意合法 UUID 即可（这些表没有外键）
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if result.NodeID == "" {
		t.Fatalf("Register returned no node id: %+v", result)
	}

	form := rec.lastForm(t, "/user/getToken.json")
	if got := form.Get("portraitUri"); got != DefaultUserPortraitURI {
		t.Fatalf("machine node getUserToken portraitUri = %q, want %q", got, DefaultUserPortraitURI)
	}
	if got := storedPortrait(t, pool, "rc_node_"+mac); got != DefaultUserPortraitURI {
		t.Fatalf("rongcloud_user.portrait_uri = %q, want %q", got, DefaultUserPortraitURI)
	}
}

func cleanupRegisteredNodeID(t *testing.T, pool *pgxpool.Pool, rcUserID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM rongcloud_device WHERE node_id IN (SELECT id FROM rongcloud_node WHERE rongcloud_user_id = $1)`, rcUserID)
		_, _ = pool.Exec(ctx, `DELETE FROM rongcloud_node WHERE rongcloud_user_id = $1`, rcUserID)
		_, _ = pool.Exec(ctx, `DELETE FROM rongcloud_user WHERE rongcloud_user_id = $1`, rcUserID)
	})
}

func TestRefreshTokenUsesStoredNameAndDefaultPortrait(t *testing.T) {
	portraitTestClearEnv(t)
	pool, queries := newPortraitTestDB(t)
	rec := newRongCloudFormRecorder()
	srv := rec.start(t)

	svc := NewNodeService(queries, newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger()), nil, testLogger())
	wsID := seedWorkspaceRow(t, pool)
	node := seedNode(t, queries, wsID, "portrait-refresh-"+uniqueID("t"), `[]`)
	cleanupRegisteredNodeID(t, pool, node.RongcloudUserID)

	// 库内昵称有值、头像为空 —— 旧实现会把两个空串直接发给融云。
	if _, err := queries.CreateRongCloudUser(context.Background(), db.CreateRongCloudUserParams{
		WorkspaceID:     wsID,
		RongcloudUserID: node.RongcloudUserID,
		Name:            pgText("机器节点 A"),
		NodeType:        "ai",
		IsAiNode:        true,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	if _, err := svc.RefreshToken(context.Background(), node.NodeID); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}

	form := rec.lastForm(t, "/user/getToken.json")
	if got := form.Get("name"); got != "机器节点 A" {
		t.Fatalf("getUserToken name = %q, want the stored nickname", got)
	}
	if got := form.Get("portraitUri"); got != DefaultUserPortraitURI {
		t.Fatalf("getUserToken portraitUri = %q, want %q", got, DefaultUserPortraitURI)
	}
}

func TestUpdateUserProfileNeverStoresEmptyPortrait(t *testing.T) {
	portraitTestClearEnv(t)
	pool, queries := newPortraitTestDB(t)
	rec := newRongCloudFormRecorder()
	srv := rec.start(t)

	svc := NewNodeService(queries, newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger()), nil, testLogger())
	wsID := seedWorkspaceRow(t, pool)

	const emptyID = "portrait-uup-empty"
	const customID = "portrait-uup-custom"
	const custom = "https://cdn.example.com/uploads/u/me.png"
	cleanupPortraitUsers(t, pool, emptyID, customID)

	empty := seedPortraitUser(t, queries, wsID, emptyID, "")
	withCustom := seedPortraitUser(t, queries, wsID, customID, custom)

	// 1) 空头像 + 库内为空 → 落统一默认图，绝不写空串。
	if err := svc.UpdateUserProfile(context.Background(), empty.ID, "n-"+emptyID, ""); err != nil {
		t.Fatalf("UpdateUserProfile(empty): %v", err)
	}
	if got := storedPortrait(t, pool, emptyID); got != DefaultUserPortraitURI {
		t.Fatalf("empty profile became %q, want %q", got, DefaultUserPortraitURI)
	}
	if got := rec.lastForm(t, "/user/refresh.json").Get("portraitUri"); got != DefaultUserPortraitURI {
		t.Fatalf("refreshUser portraitUri = %q, want %q", got, DefaultUserPortraitURI)
	}

	// 2) 空头像 + 库内是用户自定义图 → 保留库内值，不能被默认图覆盖。
	if err := svc.UpdateUserProfile(context.Background(), withCustom.ID, "n-"+customID, ""); err != nil {
		t.Fatalf("UpdateUserProfile(custom): %v", err)
	}
	if got := storedPortrait(t, pool, customID); got != custom {
		t.Fatalf("custom portrait was overwritten: %q", got)
	}
	if got := rec.lastForm(t, "/user/refresh.json").Get("portraitUri"); got != custom {
		t.Fatalf("refreshUser portraitUri = %q, want the custom %q", got, custom)
	}
}

// ---- 3c. 聊天室：create / update / join --------------------------------------

func TestChatroomCreateAndJoinNeverStoreEmptyPortrait(t *testing.T) {
	portraitTestClearEnv(t)
	f := setupChatroomFixtureFull(t)
	ctx := context.Background()

	// 房间头像（纯本地列，融云聊天室 API 没有该字段）：聊天室落「两个气泡」。
	// 成员行仍是人和 AI 节点，继续落统一默认图（下面逐行断言非空）。
	if got := chatroomPortrait(t, f.pool, f.roomRCID); got != GroupPortraitURI(GroupPortraitKindChatroom) {
		t.Fatalf("chatroom portrait = %q, want %q", got, GroupPortraitURI(GroupPortraitKindChatroom))
	}

	// 建聊天室时落下的成员行（owner / host / 普通 AI）都不能是空串。
	members := chatroomMemberPortraits(t, f.pool, f.roomRCID)
	if len(members) == 0 {
		t.Fatal("chatroom fixture created no member rows")
	}
	for memberID, portrait := range members {
		if strings.TrimSpace(portrait) == "" {
			t.Fatalf("member %s has an empty portrait", memberID)
		}
	}

	// join：actor 的 rongcloud_user 头像是空的（seedUser 不写头像），成员行仍要落默认图。
	joiner := f.seedUser(t, "joiner-portrait-"+uniqueID("t"))
	if r := f.h.actionChatroomJoin(ctx, CommandContent{
		Service: "chatroom", Action: "join",
		Params: map[string]interface{}{"chatroomId": f.roomRCID},
	}, joiner); r.code != 200 {
		t.Fatalf("join failed: %d %s", r.code, r.message)
	}
	joined := chatroomMemberPortraits(t, f.pool, f.roomRCID)[joiner.RongcloudUserID]
	if joined != DefaultUserPortraitURI {
		t.Fatalf("joined member portrait = %q, want %q", joined, DefaultUserPortraitURI)
	}
}

func TestChatroomUpdateEmptyPortraitPatchDoesNotClear(t *testing.T) {
	portraitTestClearEnv(t)
	f := setupChatroomFixtureFull(t)
	ctx := context.Background()

	const custom = "https://cdn.example.com/uploads/room.png"
	update := func(params map[string]interface{}) {
		t.Helper()
		if r := f.h.actionChatroomUpdate(ctx, CommandContent{
			Service: "chatroom", Action: "update", Params: params,
		}, f.owner); r.code != 200 {
			t.Fatalf("update %v failed: %d %s", params, r.code, r.message)
		}
	}

	update(map[string]interface{}{"chatroomId": f.roomRCID, "portraitUri": custom})
	if got := chatroomPortrait(t, f.pool, f.roomRCID); got != custom {
		t.Fatalf("portrait after explicit update = %q, want %q", got, custom)
	}

	// 客户端显式传空串：与群 COALESCE(NULLIF(...)) 语义一致，不覆盖。
	update(map[string]interface{}{"chatroomId": f.roomRCID, "portraitUri": ""})
	if got := chatroomPortrait(t, f.pool, f.roomRCID); got != custom {
		t.Fatalf("empty patch cleared the portrait: %q", got)
	}

	// 完全不传该字段同样不能改。
	update(map[string]interface{}{"chatroomId": f.roomRCID, "chatroomName": "改个名"})
	if got := chatroomPortrait(t, f.pool, f.roomRCID); got != custom {
		t.Fatalf("update without the field changed the portrait: %q", got)
	}
}

// TestChatroomInviteNeverStoresEmptyPortrait 钉住 invite 的取值链：
//
//	rongcloud_user → claw_im_users → 成员行现值 → DefaultPortraitIfEmpty
//
// 之所以连「成员行现值」都要断言：upsert 的 ON CONFLICT 会无条件写
// EXCLUDED.portrait_uri，没有新值时若不沿用原值，一次「邀请」就等于清空用户
// 自己设的头像（Phase 4 验证抓到的缺陷）。
func TestChatroomInviteNeverStoresEmptyPortrait(t *testing.T) {
	portraitTestClearEnv(t)
	f := setupChatroomFixtureFull(t)
	ctx := context.Background()
	room, err := f.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, f.roomRCID)
	if err != nil {
		t.Fatalf("load room: %v", err)
	}

	suffix := uniqueID("inv")
	const clawPortrait = "https://cdn.example.com/uploads/claw-invitee.png"
	const existingPortrait = "https://cdn.example.com/uploads/self-chosen.png"

	// 1) 只有 claw_im_users 行（真人首次进聊天室前没有 rongcloud_user 镜像）。
	clawOnly := "claw-only-" + suffix
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO claw_im_users (user_id, username, nickname, portrait_uri, password_hash, status)
		VALUES ($1, $2, $3, $4, 'x', 'active')`,
		clawOnly, "u-"+clawOnly, "Claw Only", clawPortrait); err != nil {
		t.Fatalf("seed claw_im_users: %v", err)
	}

	// 2) 两处都没有，但成员行已有自定义头像。
	existingMember := "existing-member-" + suffix
	if _, err := f.queries.UpsertRongCloudChatroomMemberByMemberID(ctx, db.UpsertRongCloudChatroomMemberByMemberIDParams{
		ChatroomID:   room.ID,
		MemberType:   "user",
		Capabilities: []byte("[]"),
		Enabled:      true,
		MemberID:     pgtype.Text{String: existingMember, Valid: true},
		Nickname:     "Existing Member",
		PortraitUri:  existingPortrait,
	}); err != nil {
		t.Fatalf("seed existing member: %v", err)
	}

	// 3) 三者都没有。
	unknown := "unknown-invitee-" + suffix
	invitees := []string{clawOnly, existingMember, unknown}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		for _, id := range invitees {
			_, _ = f.pool.Exec(cleanupCtx,
				`DELETE FROM rongcloud_chatroom_member WHERE chatroom_id = $1 AND member_id = $2`, room.ID, id)
		}
		_, _ = f.pool.Exec(cleanupCtx, `DELETE FROM claw_im_users WHERE user_id = $1`, clawOnly)
	})

	params := map[string]interface{}{"chatroomId": f.roomRCID, "userIds": []interface{}{clawOnly, existingMember, unknown}}
	if r := f.h.actionChatroomInvite(ctx, CommandContent{
		Service: "chatroom", Action: "invite", Params: params,
	}, f.owner); r.code != 200 {
		t.Fatalf("invite failed: %d %s", r.code, r.message)
	}

	members := chatroomMemberPortraits(t, f.pool, f.roomRCID)
	if got := members[clawOnly]; got != clawPortrait {
		t.Fatalf("invitee with only a claw_im_users row got %q, want the claw portrait %q", got, clawPortrait)
	}
	if got := members[existingMember]; got != existingPortrait {
		t.Fatalf("invite must not clear an existing member portrait: got %q, want %q", got, existingPortrait)
	}
	if got := members[unknown]; got != DefaultUserPortraitURI {
		t.Fatalf("invitee with no profile anywhere got %q, want %q", got, DefaultUserPortraitURI)
	}
	// 任何成员行都不允许落空串。
	for memberID, portrait := range members {
		if strings.TrimSpace(portrait) == "" {
			t.Fatalf("member %s ended up with an empty portrait after invite", memberID)
		}
	}
}

// ---- 3d. 讨论组建房 + 成员行 -------------------------------------------------

func TestRoundtableRoomAndMembersCarryDefaultPortrait(t *testing.T) {
	portraitTestClearEnv(t)
	f := setupSetupFixture(t)
	chatroomID, _ := f.createShell(t, "req-portrait-"+uniqueID("t"))

	if got := chatroomPortrait(t, f.pool, chatroomID); got != GroupPortraitURI(GroupPortraitKindDiscussion) {
		t.Fatalf("roundtable room portrait = %q, want %q", got, GroupPortraitURI(GroupPortraitKindDiscussion))
	}
	members := chatroomMemberPortraits(t, f.pool, chatroomID)
	if len(members) == 0 {
		t.Fatal("roundtable shell created no member rows")
	}
	for memberID, portrait := range members {
		if strings.TrimSpace(portrait) == "" {
			t.Fatalf("roundtable member %s has an empty portrait", memberID)
		}
	}
	// owner 是刚注册过、库里没有头像的真人 → 落默认图。
	if got := members[f.ownerRCID]; got != DefaultUserPortraitURI {
		t.Fatalf("owner member portrait = %q, want %q", got, DefaultUserPortraitURI)
	}
}

// chatroomPortrait 读房间的本地头像列。
func chatroomPortrait(t *testing.T, pool *pgxpool.Pool, rcID string) string {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(portrait_uri, '') FROM rongcloud_chatroom WHERE rongcloud_chatroom_id = $1`, rcID,
	).Scan(&got); err != nil {
		t.Fatalf("read chatroom portrait: %v", err)
	}
	return got
}

// chatroomMemberPortraits 返回 member_id -> portrait_uri。
func chatroomMemberPortraits(t *testing.T, pool *pgxpool.Pool, rcID string) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT COALESCE(m.member_id::text, ''), COALESCE(m.portrait_uri, '')
		FROM rongcloud_chatroom_member m
		JOIN rongcloud_chatroom c ON c.id = m.chatroom_id
		WHERE c.rongcloud_chatroom_id = $1`, rcID)
	if err != nil {
		t.Fatalf("list chatroom members: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, portrait string
		if err := rows.Scan(&id, &portrait); err != nil {
			t.Fatalf("scan member: %v", err)
		}
		out[id] = portrait
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// ---- 4. backfill：SyncUserPortraits 的 dry-run / commit ----------------------

// newPortraitSyncFixture 铺四条典型存量记录：空、openclaw、已是统一图、用户自定义。
func newPortraitSyncFixture(t *testing.T) (*pgxpool.Pool, *db.Queries, *NodeService, *rongCloudFormRecorder, pgtype.UUID) {
	t.Helper()
	portraitTestClearEnv(t)
	pool, queries := newPortraitTestDB(t)
	rec := newRongCloudFormRecorder()
	srv := rec.start(t)
	svc := NewNodeService(queries, newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger()), nil, testLogger())

	wsID := seedWorkspaceRow(t, pool)
	suffix := uniqueID("p")
	emptyID := "portrait-empty-" + suffix
	legacyID := "portrait-legacy-" + suffix
	unifiedID := "portrait-unified-" + suffix
	customID := "portrait-custom-" + suffix
	cleanupPortraitUsers(t, pool, emptyID, legacyID, unifiedID, customID)

	seedPortraitUser(t, queries, wsID, emptyID, "")
	seedPortraitUser(t, queries, wsID, legacyID, legacyDefaultPortraitURIs[0])
	seedPortraitUser(t, queries, wsID, unifiedID, DefaultUserPortraitURI)
	seedPortraitUser(t, queries, wsID, customID, "https://cdn.example.com/uploads/u/me.png")

	return pool, queries, svc, rec, wsID
}

func outcomeStatuses(outcomes []PortraitSyncOutcome) map[string]string {
	out := make(map[string]string, len(outcomes))
	for _, o := range outcomes {
		out[o.RCUser] = o.Status
	}
	return out
}

func TestSyncUserPortraitsDryRunWritesNothing(t *testing.T) {
	pool, _, svc, rec, wsID := newPortraitSyncFixture(t)

	outcomes, err := svc.SyncUserPortraits(context.Background(), wsID, BackfillFriendsOptions{DryRun: true})
	if err != nil {
		t.Fatalf("SyncUserPortraits: %v", err)
	}
	statuses := outcomeStatuses(outcomes)
	var planned, skipped int
	for _, status := range statuses {
		switch status {
		case "planned":
			planned++
		case "skipped":
			skipped++
		default:
			t.Fatalf("unexpected status %q in a dry run: %#v", status, outcomes)
		}
	}
	if planned != 2 || skipped != 2 {
		t.Fatalf("planned=%d skipped=%d, want 2/2 (%#v)", planned, skipped, outcomes)
	}
	// dry-run 绝不能碰融云。
	if calls := len(rec.requests("/user/refresh.json")); calls != 0 {
		t.Fatalf("dry run made %d refresh calls, want 0", calls)
	}
	// 也不能碰库。
	var stillEmpty int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rongcloud_user WHERE workspace_id = $1 AND (portrait_uri IS NULL OR btrim(portrait_uri) = '')`,
		wsID).Scan(&stillEmpty); err != nil {
		t.Fatalf("count empty: %v", err)
	}
	if stillEmpty != 1 {
		t.Fatalf("dry run rewrote rows: empty portraits = %d, want 1", stillEmpty)
	}
	var legacy int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rongcloud_user WHERE workspace_id = $1 AND portrait_uri = $2`,
		wsID, legacyDefaultPortraitURIs[0]).Scan(&legacy); err != nil {
		t.Fatalf("count legacy: %v", err)
	}
	if legacy != 1 {
		t.Fatalf("dry run rewrote the legacy row: %d, want 1", legacy)
	}
}

func TestSyncUserPortraitsCommitPushesThenWrites(t *testing.T) {
	pool, _, svc, rec, wsID := newPortraitSyncFixture(t)
	ctx := context.Background()

	outcomes, err := svc.SyncUserPortraits(ctx, wsID, BackfillFriendsOptions{DryRun: false})
	if err != nil {
		t.Fatalf("SyncUserPortraits: %v", err)
	}
	statuses := outcomeStatuses(outcomes)
	var updated, skipped int
	for _, status := range statuses {
		switch status {
		case "updated":
			updated++
		case "skipped":
			skipped++
		default:
			t.Fatalf("unexpected status %q in a commit run: %#v", status, outcomes)
		}
	}
	if updated != 2 || skipped != 2 {
		t.Fatalf("updated=%d skipped=%d, want 2/2 (%#v)", updated, skipped, outcomes)
	}

	// 每条被补齐的记录都要推一次融云，推送值就是统一默认图。
	refreshes := rec.requests("/user/refresh.json")
	if len(refreshes) != 2 {
		t.Fatalf("refresh calls = %d, want 2", len(refreshes))
	}
	for _, form := range refreshes {
		if got := form.Get("portraitUri"); got != DefaultUserPortraitURI {
			t.Fatalf("refreshUser portraitUri = %q, want %q", got, DefaultUserPortraitURI)
		}
	}

	// 落库：空与 openclaw 两行被改写，自定义头像原样保留。
	var unified int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM rongcloud_user WHERE workspace_id = $1 AND portrait_uri = $2`,
		wsID, DefaultUserPortraitURI).Scan(&unified); err != nil {
		t.Fatalf("count unified: %v", err)
	}
	if unified != 3 { // 原本已是统一图的 1 行 + 补齐的 2 行
		t.Fatalf("rows on the unified portrait = %d, want 3", unified)
	}
	var custom int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM rongcloud_user WHERE workspace_id = $1 AND portrait_uri = $2`,
		wsID, "https://cdn.example.com/uploads/u/me.png").Scan(&custom); err != nil {
		t.Fatalf("count custom: %v", err)
	}
	if custom != 1 {
		t.Fatalf("the custom portrait was overwritten (count = %d, want 1)", custom)
	}
}

// 先推融云、再写库：推送失败时库内必须还是旧值，否则这条记录下次会被 skipped
// 永远修不好。
func TestSyncUserPortraitsLeavesRowUntouchedWhenPushFails(t *testing.T) {
	pool, _, svc, rec, wsID := newPortraitSyncFixture(t)
	rec.failPaths["/user/refresh.json"] = true
	ctx := context.Background()

	outcomes, err := svc.SyncUserPortraits(ctx, wsID, BackfillFriendsOptions{DryRun: false})
	if err != nil {
		t.Fatalf("SyncUserPortraits: %v", err)
	}
	var failed int
	for _, o := range outcomes {
		if o.Status == "failed" {
			failed++
			if o.Error == "" {
				t.Fatalf("failed outcome carries no error: %#v", o)
			}
		}
		if o.Status == "updated" {
			t.Fatalf("nothing may be reported updated when the push fails: %#v", o)
		}
	}
	if failed != 2 {
		t.Fatalf("failed = %d, want 2 (%#v)", failed, outcomes)
	}

	var stillEmpty int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM rongcloud_user WHERE workspace_id = $1 AND (portrait_uri IS NULL OR btrim(portrait_uri) = '')`,
		wsID).Scan(&stillEmpty); err != nil {
		t.Fatalf("count empty: %v", err)
	}
	if stillEmpty != 1 {
		t.Fatalf("a failed push must leave the empty row untouched, empty = %d", stillEmpty)
	}
	var legacy int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM rongcloud_user WHERE workspace_id = $1 AND portrait_uri = $2`,
		wsID, legacyDefaultPortraitURIs[0]).Scan(&legacy); err != nil {
		t.Fatalf("count legacy: %v", err)
	}
	if legacy != 1 {
		t.Fatalf("a failed push must leave the legacy row retryable, legacy = %d", legacy)
	}
}

// ---- 5. -user 白名单（R3：OnlyRCUserIDs） ------------------------------------

// TestSyncUserPortraitsOnlyRCUserIDsWhitelist 钉住 `-user` 的严格白名单语义：
// 不在白名单里的行必须**完全不出现在结果里**（不是 planned/skipped，而是跳过），
// 否则一次「只补某个用户」会顺手刷全 workspace 的 rongcloud_user —— 既是越权写，
// 也会把融云调用量放大到不可控。
func TestSyncUserPortraitsOnlyRCUserIDsWhitelist(t *testing.T) {
	portraitTestClearEnv(t)
	pool, queries := newPortraitTestDB(t)
	rec := newRongCloudFormRecorder()
	srv := rec.start(t)
	svc := NewNodeService(queries, newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger()), nil, testLogger())

	wsID := seedWorkspaceRow(t, pool)
	suffix := uniqueID("wl")
	pickedID := "portrait-wl-picked-" + suffix
	otherID := "portrait-wl-other-" + suffix
	customID := "portrait-wl-custom-" + suffix
	cleanupPortraitUsers(t, pool, pickedID, otherID, customID)

	seedPortraitUser(t, queries, wsID, pickedID, "")
	seedPortraitUser(t, queries, wsID, otherID, "")
	seedPortraitUser(t, queries, wsID, customID, "https://cdn.example.com/uploads/keep.png")

	ctx := context.Background()

	// 1) dry-run：只回白名单那一行，且不碰融云。首尾空白应被 trim 后仍命中。
	outcomes, err := svc.SyncUserPortraits(ctx, wsID, BackfillFriendsOptions{
		DryRun:        true,
		OnlyRCUserIDs: []string{"  " + pickedID + "  "},
	})
	if err != nil {
		t.Fatalf("SyncUserPortraits(dry run): %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("whitelisted dry run returned %d outcomes, want 1 (%#v)", len(outcomes), outcomes)
	}
	if outcomes[0].RCUser != pickedID || outcomes[0].Status != "planned" {
		t.Fatalf("outcome = %#v, want planned for %s", outcomes[0], pickedID)
	}
	if calls := len(rec.requests("/user/refresh.json")); calls != 0 {
		t.Fatalf("dry run made %d refresh calls, want 0", calls)
	}

	// 2) commit：只有白名单行被推 + 落库，其余两行原地不动。
	outcomes, err = svc.SyncUserPortraits(ctx, wsID, BackfillFriendsOptions{
		DryRun:        false,
		OnlyRCUserIDs: []string{pickedID},
	})
	if err != nil {
		t.Fatalf("SyncUserPortraits(commit): %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].Status != "updated" {
		t.Fatalf("whitelisted commit outcomes = %#v, want one updated", outcomes)
	}
	forms := rec.requests("/user/refresh.json")
	if len(forms) != 1 || forms[0].Get("userId") != pickedID {
		t.Fatalf("refresh calls = %#v, want exactly one for %s", forms, pickedID)
	}
	if got := storedPortrait(t, pool, pickedID); got != DefaultUserPortraitURI {
		t.Fatalf("whitelisted row = %q, want %q", got, DefaultUserPortraitURI)
	}
	if got := storedPortrait(t, pool, otherID); got != "" {
		t.Fatalf("a row outside the whitelist was rewritten: %q", got)
	}
	if got := storedPortrait(t, pool, customID); got != "https://cdn.example.com/uploads/keep.png" {
		t.Fatalf("custom portrait changed: %q", got)
	}

	// 3) 名单为空 → 恢复全量语义（本 workspace 三行都会出现）。
	all, err := svc.SyncUserPortraits(ctx, wsID, BackfillFriendsOptions{DryRun: true})
	if err != nil {
		t.Fatalf("SyncUserPortraits(no whitelist): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("no whitelist returned %d outcomes, want all 3 (%#v)", len(all), all)
	}
}
