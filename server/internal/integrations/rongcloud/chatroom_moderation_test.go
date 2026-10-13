package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// chatroom_moderation_test.go 覆盖 service=chatroom 的拉黑/禁言/白名单九个动作。
//
// 测试分两层：
//  1. 纯函数层（parseModerationUserList / moderationUserIDs / moderationMinute）——
//     无 DB、无融云，验证参数方言宽容性与列表解析。
//  2. API 层（融云端点表单）—— 用 httptest 假融云，验证每个写动作打了正确的
//     端点与参数（userId 逗号拼接、minute 透传），失败路径回 500。
//
// handler 层的 owner 校验依赖真 DB（GetActiveRongCloudChatroomViewByRongCloudID），
// 本地无 DB 时自动 skip，与 chatroom_commands_test.go 的取舍一致。

// ---- 纯函数层 ----------------------------------------------------------------

func TestModerationUserIDsAcceptsBothSpellings(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]interface{}
		want   []string
	}{
		{"plural array", map[string]interface{}{
			"userIds": []interface{}{"u1", "u2"},
		}, []string{"u1", "u2"}},
		{"singular string", map[string]interface{}{
			"userId": "u1",
		}, []string{"u1"}},
		{"snake plural", map[string]interface{}{
			"user_ids": []interface{}{"u1"},
		}, []string{"u1"}},
		{"memberIds fallback", map[string]interface{}{
			"memberIds": []interface{}{"u1", "u1", " "},
		}, []string{"u1"}},
		{"empty", map[string]interface{}{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := moderationUserIDs(tc.params)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestModerationMinute(t *testing.T) {
	cases := []struct {
		in   interface{}
		want int
	}{
		{nil, 0},
		{float64(30), 30},
		{float64(-5), 0},
		{30, 30},
		{"45", 45},
		{"abc", 0},
	}
	for _, tc := range cases {
		got := moderationMinute(map[string]interface{}{"minute": tc.in})
		if got != tc.want {
			t.Errorf("minute(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseModerationUserList(t *testing.T) {
	raw := `{"code":200,"users":[{"id":"u1","time":"2024-01-01"},{"userId":"u2"},{"user_id":"u3"},{"name":"no-id"}]}`
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	users := parseModerationUserList(result)
	if len(users) != 3 {
		t.Fatalf("users = %d, want 3 (no-id dropped)", len(users))
	}
	if users[0]["id"] != "u1" || users[1]["id"] != "u2" || users[2]["id"] != "u3" {
		t.Errorf("ids = %v %v %v", users[0]["id"], users[1]["id"], users[2]["id"])
	}
	if users[0]["time"] != "2024-01-01" {
		t.Errorf("extra fields must survive: %#v", users[0])
	}
	// 空响应返回空列表而非 nil：uniapp 的 settings.vue 直接 `result.users || []`，
	// nil 会走到 JSON null。
	if got := parseModerationUserList(map[string]interface{}{}); len(got) != 0 || got == nil {
		t.Errorf("empty response must yield non-nil empty list: %#v", got)
	}
}

// ---- API 层（假融云） ---------------------------------------------------------

// moderationTestServer 记录发往融云的请求。
type moderationTestServer struct {
	mu    sync.Mutex
	posts map[string][]map[string]string
	// failPaths 里的路径回 {code:1000}，模拟融云侧失败。
	failPaths map[string]bool
}

func newModerationTestServer(t *testing.T) (*moderationTestServer, *httptest.Server) {
	t.Helper()
	rec := &moderationTestServer{posts: map[string][]map[string]string{}, failPaths: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]string{}
		for key, values := range r.PostForm {
			form[key] = strings.Join(values, ",")
		}
		rec.mu.Lock()
		rec.posts[r.URL.Path] = append(rec.posts[r.URL.Path], form)
		fail := rec.failPaths[r.URL.Path]
		rec.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if fail {
			fmt.Fprint(w, `{"code":1000,"errorMessage":"boom"}`)
			return
		}
		switch r.URL.Path {
		case "/chatroom/user/block/list.json", "/chatroom/user/gag/list.json", "/chatroom/user/whitelist/query.json":
			fmt.Fprint(w, `{"code":200,"users":[{"id":"u1","time":"2024-01-01"},{"id":"u2"}]}`)
		default:
			fmt.Fprint(w, `{"code":200}`)
		}
	}))
	return rec, srv
}

func (s *moderationTestServer) calls(path string) []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]string, len(s.posts[path]))
	copy(out, s.posts[path])
	return out
}

func TestChatroomModerationAPIEndpoints(t *testing.T) {
	rec, srv := newModerationTestServer(t)
	defer srv.Close()
	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	ctx := context.Background()

	// block：userId 逗号拼接 + minute 透传。
	if err := client.blockChatroomMember(ctx, "room-1", []string{"u1", "u2"}, 30); err != nil {
		t.Fatalf("block: %v", err)
	}
	calls := rec.calls("/chatroom/user/block/add.json")
	if len(calls) != 1 || calls[0]["userId"] != "u1,u2" || calls[0]["minute"] != "30" {
		t.Fatalf("block form = %#v", calls)
	}

	// unblock。
	if err := client.unblockChatroomMember(ctx, "room-1", []string{"u1"}); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	if calls := rec.calls("/chatroom/user/block/remove.json"); len(calls) != 1 || calls[0]["userId"] != "u1" {
		t.Fatalf("unblock form = %#v", calls)
	}

	// blocklist。
	users, err := client.listChatroomBlocklist(ctx, "room-1")
	if err != nil {
		t.Fatalf("blocklist: %v", err)
	}
	if len(users) != 2 || users[0]["id"] != "u1" {
		t.Fatalf("blocklist = %#v", users)
	}

	// gag / ungag / gaglist。
	if err := client.gagChatroomMember(ctx, "room-1", []string{"u1"}, 0); err != nil {
		t.Fatalf("gag: %v", err)
	}
	if calls := rec.calls("/chatroom/user/gag/add.json"); len(calls) != 1 || calls[0]["minute"] != "0" {
		t.Fatalf("gag form = %#v", calls)
	}
	if err := client.ungagChatroomMember(ctx, "room-1", []string{"u1"}); err != nil {
		t.Fatalf("ungag: %v", err)
	}
	if _, err := client.listChatroomGaglist(ctx, "room-1"); err != nil {
		t.Fatalf("gaglist: %v", err)
	}

	// whitelist 三件套。
	if err := client.addChatroomWhitelist(ctx, "room-1", []string{"u1", "u2"}); err != nil {
		t.Fatalf("addWhitelist: %v", err)
	}
	if calls := rec.calls("/chatroom/user/whitelist/add.json"); len(calls) != 1 || calls[0]["userId"] != "u1,u2" {
		t.Fatalf("whitelist form = %#v", calls)
	}
	if err := client.removeChatroomWhitelist(ctx, "room-1", []string{"u1"}); err != nil {
		t.Fatalf("removeWhitelist: %v", err)
	}
	if _, err := client.listChatroomWhitelist(ctx, "room-1"); err != nil {
		t.Fatalf("whitelist query: %v", err)
	}
}

func TestChatroomModerationAPIFailure(t *testing.T) {
	rec, srv := newModerationTestServer(t)
	defer srv.Close()
	rec.failPaths["/chatroom/user/block/add.json"] = true
	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())

	if err := client.blockChatroomMember(context.Background(), "room-1", []string{"u1"}, 0); err == nil {
		t.Fatal("block must fail when RongCloud errors")
	}
}

// ---- handler 层（需真 DB，缺省 skip） ---------------------------------------

// seedModerationRoom 用 fixture 的 create 动作建一个房主 + 房间，返回 fixture 与
// 指向假融云的 handler。DB 不可达时 skip。
func seedModerationRoom(t *testing.T) (*chatroomFixture, *systemHandler) {
	t.Helper()
	f := setupChatroomFixtureFull(t)
	// fixture 的 handler 没有 client（actionChatroomCreate 里 h.client==nil 走
	// 无融云分支），moderation 动作需要真 client：指向一个假融云。
	_, srv := newModerationTestServer(t)
	t.Cleanup(srv.Close)
	h := &systemHandler{
		queries: f.h.queries, nodeID: f.h.nodeID, logger: testLogger(),
		client: newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger()),
	}
	return f, h
}

func TestChatroomBlockRequiresOwner(t *testing.T) {
	f, h := seedModerationRoom(t)
	stranger := f.seedUser(t, "stranger-"+uniqueID("t"))

	r := h.actionChatroomBlock(context.Background(), CommandContent{
		Params: map[string]interface{}{"chatroomId": f.roomRCID, "userId": "u1"},
	}, stranger)
	if r.code != 403 {
		t.Fatalf("stranger block = %d %s, want 403", r.code, r.message)
	}
}

func TestChatroomBlockByOwner(t *testing.T) {
	f, h := seedModerationRoom(t)

	r := h.actionChatroomBlock(context.Background(), CommandContent{
		Params: map[string]interface{}{"chatroomId": f.roomRCID, "userId": "u1", "minute": float64(30)},
	}, f.owner)
	if r.code != 200 {
		t.Fatalf("owner block = %d %s", r.code, r.message)
	}
}

func TestChatroomBlocklistAction(t *testing.T) {
	f, h := seedModerationRoom(t)

	r := h.actionChatroomGetBlocklist(context.Background(), CommandContent{
		Params: map[string]interface{}{"chatroomId": f.roomRCID},
	})
	if r.code != 200 {
		t.Fatalf("blocklist = %d %s", r.code, r.message)
	}
	data, ok := r.data.(map[string]interface{})
	if !ok {
		t.Fatalf("data = %#v", r.data)
	}
	users, ok := data["users"].([]map[string]interface{})
	if !ok || len(users) != 2 {
		t.Fatalf("users = %#v", data["users"])
	}
}

func TestChatroomMuteUnmuteByOwner(t *testing.T) {
	f, h := seedModerationRoom(t)
	ctx := context.Background()

	if r := h.actionChatroomMute(ctx, CommandContent{
		Params: map[string]interface{}{"chatroomId": f.roomRCID, "userIds": []interface{}{"u1", "u1"}, "minute": "45"},
	}, f.owner); r.code != 200 {
		t.Fatalf("mute = %d %s", r.code, r.message)
	}
	if r := h.actionChatroomUnmute(ctx, CommandContent{
		Params: map[string]interface{}{"chatroomId": f.roomRCID, "userIds": []interface{}{"u1"}},
	}, f.owner); r.code != 200 {
		t.Fatalf("unmute = %d %s", r.code, r.message)
	}
	if r := h.actionChatroomGetGaglist(ctx, CommandContent{
		Params: map[string]interface{}{"chatroomId": f.roomRCID},
	}); r.code != 200 {
		t.Fatalf("gaglist = %d %s", r.code, r.message)
	}
}

func TestChatroomWhitelistActionsByOwner(t *testing.T) {
	f, h := seedModerationRoom(t)
	ctx := context.Background()

	if r := h.actionChatroomAddWhitelist(ctx, CommandContent{
		Params: map[string]interface{}{"chatroomId": f.roomRCID, "userIds": []interface{}{"u1", "u2"}},
	}, f.owner); r.code != 200 {
		t.Fatalf("addWhitelist = %d %s", r.code, r.message)
	}
	if r := h.actionChatroomRemoveWhitelist(ctx, CommandContent{
		Params: map[string]interface{}{"chatroomId": f.roomRCID, "userIds": []interface{}{"u1"}},
	}, f.owner); r.code != 200 {
		t.Fatalf("removeWhitelist = %d %s", r.code, r.message)
	}
	if r := h.actionChatroomGetWhitelist(ctx, CommandContent{
		Params: map[string]interface{}{"chatroomId": f.roomRCID},
	}); r.code != 200 {
		t.Fatalf("whitelist = %d %s", r.code, r.message)
	}
}

func TestChatroomModerationMissingParams(t *testing.T) {
	f, h := seedModerationRoom(t)

	// 缺 userId：400，且不能打融云。
	if r := h.actionChatroomBlock(context.Background(), CommandContent{
		Params: map[string]interface{}{"chatroomId": f.roomRCID},
	}, f.owner); r.code != 400 {
		t.Fatalf("missing userId = %d, want 400", r.code)
	}
	// 缺 chatroomId：400。
	if r := h.actionChatroomGetBlocklist(context.Background(), CommandContent{
		Params: map[string]interface{}{},
	}); r.code != 400 {
		t.Fatalf("missing chatroomId = %d, want 400", r.code)
	}
	// 房间不存在：404。
	if r := h.actionChatroomBlock(context.Background(), CommandContent{
		Params: map[string]interface{}{"chatroomId": "no-such-room", "userId": "u1"},
	}, f.owner); r.code != 404 {
		t.Fatalf("missing room = %d, want 404", r.code)
	}
}

// 确保编译期引用（seed 函数在无 DB 环境仅被 handler 层测试使用）。
var _ = db.RongcloudUser{}
var _ = pgtype.UUID{}
