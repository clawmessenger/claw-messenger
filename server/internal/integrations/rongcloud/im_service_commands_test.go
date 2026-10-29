package rongcloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 本文件覆盖 P1/P2 新增的三个 service：user(wxLogin) / friend / lobster。
//
// 脚手架沿用同包既有范式（system_handler_web_test.go:14-53 的 httptest 假融云 +
// 直接喂 handleCommand），另外三处必须知道：
//   - 回包断言走 web 方言（requestId/payload）：客户端（uniapp）发的就是它，
//     replyOK/replyErr 会按信封自动切。
//   - 身份解析用 actorResolver 注入点，避免单测依赖真库（生产走 resolveActor
//     的双表兜底，见 system_handler.go 的 serviceActor）。
//   - lobster 的表读写用内存 stub 顶替 LobsterStore。

// --- 假融云 + 假微信 ---------------------------------------------------------

type imServiceServerOptions struct {
	// friendList 是 /friend/get.json 回的好友列表；nil 时回空列表。
	friendList []map[string]interface{}
	// weChatOpenID 是 jscode2session 回的 openid；空串则回 errcode（见下）。
	weChatOpenID string
	// weChatErrCode 非 0 时 jscode2session 回业务错误。
	weChatErrCode int
	weChatErrMsg  string
	// userInfo 是 /user/info.json 回的资料，按 userId 索引（值即 data 子字典）。
	// 缺席的用户回空 data —— 搜索的资料回填只在本地缺字段时才发这次请求，其余
	// 用例不该因为多了一次调用就变红。
	userInfo map[string]map[string]interface{}
}

type imServiceServer struct {
	mu      sync.Mutex
	paths   []string
	replies []map[string]interface{}
	// forms 记录每个路径最后收到的表单：friend/add 的 optType 只能从这里断言
	// （回来的 data 里也有，但那验证不了「到底发出去了什么」）。
	forms map[string]url.Values
}

func newIMServiceServer(t *testing.T, opts imServiceServerOptions) (*imServiceServer, *httptest.Server) {
	t.Helper()
	rec := &imServiceServer{forms: make(map[string]url.Values)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		cloned := make(url.Values, len(r.PostForm))
		for k, v := range r.PostForm {
			cloned[k] = append([]string(nil), v...)
		}
		rec.mu.Lock()
		rec.paths = append(rec.paths, r.URL.Path)
		rec.forms[r.URL.Path] = cloned
		rec.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sns/jscode2session":
			if opts.weChatErrCode != 0 {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"errcode": opts.weChatErrCode,
					"errmsg":  opts.weChatErrMsg,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"openid":      opts.weChatOpenID,
				"session_key": "session-key-must-not-leak",
			})
		case "/friend/get.json":
			friendList := opts.friendList
			if friendList == nil {
				friendList = []map[string]interface{}{}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":       200,
				"friendList": friendList,
			})
		case "/user/getToken.json":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":  200,
				"token": "rc-token-minted",
			})
		case "/user/info.json":
			// 缺省回空 data（= 融云不认识这个号）。搜索回填只在本地缺名字/头像
			// 时才发这次请求，所以其余用例不受影响。
			profile := opts.userInfo[r.PostForm.Get("userId")]
			if profile == nil {
				profile = map[string]interface{}{}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 200,
				"data": profile,
			})
		case "/message/private/publish.json":
			var content map[string]interface{}
			if err := json.Unmarshal([]byte(r.PostForm.Get("content")), &content); err == nil {
				rec.mu.Lock()
				rec.replies = append(rec.replies, content)
				rec.mu.Unlock()
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 200, "msgUID": "uid-1"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 200, "msgUID": "uid-1"})
		}
	}))
	return rec, srv
}

func (s *imServiceServer) last(t *testing.T) map[string]interface{} {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.replies) == 0 {
		t.Fatal("no command_result was published")
	}
	return s.replies[len(s.replies)-1]
}

func (s *imServiceServer) replyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.replies)
}

func (s *imServiceServer) called(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.paths {
		if p == path {
			return true
		}
	}
	return false
}

// lastForm 返回某个路径最后收到的表单（没收到过则返回 nil）。
func (s *imServiceServer) lastForm(path string) url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forms[path]
}

// newIMServiceHandler 构造一个不带数据库的 handler：身份解析走注入的
// actorResolver，微信接口指向假服务。
func newIMServiceHandler(t *testing.T, srv *httptest.Server) *systemHandler {
	t.Helper()
	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, nil, nil, nil, nil, nil)
	handler.actorResolver = func(_ context.Context, fromUserID string) (string, error) {
		if strings.TrimSpace(fromUserID) == "" {
			return "", errors.New("missing sender")
		}
		return fromUserID, nil
	}
	handler.wechatAPIBase = srv.URL
	handler.wechatHTTPClient = srv.Client()
	return handler
}

// sendWebCommand 以 web 信封（requestId/payload）发一条 system 命令。
func sendWebCommand(t *testing.T, handler *systemHandler, fromUserID, service, action string, payload map[string]interface{}) {
	t.Helper()
	content, err := json.Marshal(map[string]interface{}{
		"requestId": "req-1",
		"service":   service,
		"action":    action,
		"payload":   payload,
	})
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: fromUserID,
		Content:    string(content),
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand %s/%s: %v", service, action, err)
	}
}

// wantWebError 断言回包是 web 方言的错误，且 message 满足子串约束。
func wantWebError(t *testing.T, reply map[string]interface{}, code int, messageParts ...string) {
	t.Helper()
	if reply["status"] != "error" {
		t.Fatalf("status = %#v, want error (reply %#v)", reply["status"], reply)
	}
	if reply["code"] != float64(code) {
		t.Fatalf("code = %#v, want %d (reply %#v)", reply["code"], code, reply)
	}
	message, _ := reply["message"].(string)
	for _, part := range messageParts {
		if !strings.Contains(message, part) {
			t.Errorf("message %q must contain %q", message, part)
		}
	}
}

// --- 404 契约 ----------------------------------------------------------------

// 三个新 service 的未知 action 必须走与 chatroom/discussion 相同的 404 契约：
// code 404 + 「未知的服务或操作: <service>/<action>」——前端 isLegacyChatroomActionError
// 靠这段文案做滚动发布期的静默降级。
func TestIMServiceUnknownActionsReplyWithTheLegacy404Contract(t *testing.T) {
	cases := []struct{ service, action string }{
		{"lobster", "feedLobster"},
		{"friend", "feedFriend"},
		{"user", "feedUser"},
	}
	for _, tc := range cases {
		t.Run(tc.service+"/"+tc.action, func(t *testing.T) {
			rec, srv := newIMServiceServer(t, imServiceServerOptions{})
			defer srv.Close()
			handler := newIMServiceHandler(t, srv)
			// 只有 lobster 需要 store；其余两个 service 未装配 store 时是先回
			// 503 还是先判 action？—— 三个 service 都先判 store，所以这里把
			// store 挂上，确保走的是 action 分支。
			handler.lobsterStore = newLobsterFixture().store()
			handler.friendStore = &FriendStore{}
			handler.userStore = newUserFixture().store()

			sendWebCommand(t, handler, "user-7", tc.service, tc.action, map[string]interface{}{})

			wantWebError(t, rec.last(t), 404, "未知的服务或操作", tc.service+"/"+tc.action)
			if rec.replyCount() != 1 {
				t.Errorf("expected exactly one reply, got %d", rec.replyCount())
			}
		})
	}
}

// --- lobster ----------------------------------------------------------------

// lobsterFixture 是 LobsterStore 的内存实现：行为与 SQL 版一致 —— 所有查询都带
// user_id，越权与不存在返回同一个「查不到」。
type lobsterFixture struct {
	mu   sync.Mutex
	rows map[string]Lobster
}

func newLobsterFixture(seed ...Lobster) *lobsterFixture {
	f := &lobsterFixture{rows: make(map[string]Lobster)}
	for _, row := range seed {
		f.rows[row.ID] = row
	}
	return f
}

func (f *lobsterFixture) get(id string) (Lobster, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[id]
	return row, ok
}

func (f *lobsterFixture) store() *LobsterStore {
	return &LobsterStore{
		List: func(_ context.Context, userID string) ([]Lobster, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			out := make([]Lobster, 0, len(f.rows))
			for _, row := range f.rows {
				if row.UserID == userID {
					out = append(out, row)
				}
			}
			return out, nil
		},
		Get: func(_ context.Context, userID, lobsterID string) (Lobster, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			row, ok := f.rows[lobsterID]
			if !ok || row.UserID != userID {
				return Lobster{}, false, nil
			}
			return row, true, nil
		},
		Create: func(_ context.Context, row Lobster) (Lobster, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			now := time.Now().UTC().Truncate(time.Second)
			row.CreatedAt = now
			row.UpdatedAt = now
			f.rows[row.ID] = row
			return row, nil
		},
		Update: func(_ context.Context, userID, lobsterID string, patch LobsterPatch) (Lobster, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			row, ok := f.rows[lobsterID]
			if !ok || row.UserID != userID {
				return Lobster{}, false, nil
			}
			if patch.Name != nil {
				row.Name = *patch.Name
			}
			if patch.Variety != nil {
				row.Variety = *patch.Variety
			}
			if patch.Weight != nil {
				row.Weight = *patch.Weight
			}
			if patch.Status != nil {
				row.Status = *patch.Status
			}
			row.UpdatedAt = time.Now().UTC().Truncate(time.Second)
			f.rows[lobsterID] = row
			return row, true, nil
		},
		SoftDelete: func(_ context.Context, userID, lobsterID string) (bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			row, ok := f.rows[lobsterID]
			if !ok || row.UserID != userID {
				return false, nil
			}
			delete(f.rows, lobsterID)
			return true, nil
		},
	}
}

func TestLobsterDetailHidesAnotherUsersRecordWith404(t *testing.T) {
	rec, srv := newIMServiceServer(t, imServiceServerOptions{})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	handler.lobsterStore = newLobsterFixture(Lobster{
		ID:     "lob_owned_by_someone_else",
		UserID: "user-owner",
		Name:   "红龙",
		Status: "healthy",
	}).store()

	// 注意：越权与不存在必须同码同文案，否则 403/404 的差异能被用来探测 id。
	sendWebCommand(t, handler, "user-intruder", "lobster", "getDetail",
		map[string]interface{}{"lobsterId": "lob_owned_by_someone_else"})

	wantWebError(t, rec.last(t), 404, "lobster not found")
}

func TestLobsterCreateUsesTheSenderAsOwnerAndIgnoresPayloadUserID(t *testing.T) {
	rec, srv := newIMServiceServer(t, imServiceServerOptions{})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	store := newLobsterFixture()
	handler.lobsterStore = store.store()

	// 客户端（edit.vue:146）会把 uni.getStorageSync('user').username 塞进
	// userId；它不能决定归属，否则任何人都能把记录写到别人名下。
	sendWebCommand(t, handler, "user-1", "lobster", "create", map[string]interface{}{
		"name":    "  澳洲龙虾 ",
		"variety": "澳洲龙虾",
		"weight":  "650.5", // 表单给字符串也要能解析
		"status":  "healthy",
		"userId":  "someone-else",
	})

	reply := rec.last(t)
	if reply["status"] != "success" || reply["code"] != float64(200) {
		t.Fatalf("create should succeed, got %#v", reply)
	}
	data, ok := reply["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("data = %#v, want an object", reply["data"])
	}
	if data["name"] != "澳洲龙虾" {
		t.Errorf("name = %#v, want the trimmed value", data["name"])
	}
	if data["weight"] != 650.5 {
		t.Errorf("weight = %#v, want 650.5", data["weight"])
	}
	if data["userId"] != "user-1" {
		t.Errorf("userId = %#v, want the sender", data["userId"])
	}
	if _, hasCreatedAt := data["createdAt"].(string); !hasCreatedAt {
		t.Errorf("createdAt must be an RFC3339 string, got %#v", data["createdAt"])
	}
	id, _ := data["id"].(string)
	if !strings.HasPrefix(id, "lob_") {
		t.Errorf("id = %q, want a lob_ prefixed opaque id", id)
	}
	if stored, found := store.get(id); !found || stored.UserID != "user-1" {
		t.Errorf("stored row = %#v (found=%v), want owner user-1", stored, found)
	}
}

func TestLobsterUpdateRejectsAnEmptyPatch(t *testing.T) {
	rec, srv := newIMServiceServer(t, imServiceServerOptions{})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	handler.lobsterStore = newLobsterFixture(Lobster{
		ID: "lob_1", UserID: "user-1", Name: "红龙", Status: "healthy",
	}).store()

	sendWebCommand(t, handler, "user-1", "lobster", "update",
		map[string]interface{}{"lobsterId": "lob_1"})

	wantWebError(t, rec.last(t), 400, "no fields to update")
}

// --- friend -----------------------------------------------------------------

// friend 的四个审批 action 已有真实实现（审批表见 migrations/603，逻辑见
// friend_request.go）。**没装配审批表**时必须是 503「能力未启用」，而不是
// 501（那是「融云没这接口」的旧结论，已不成立）也不能伪造成功 —— 客户端据此
// 区分「后端没这能力」与「本实例没接库」，前者不会再出现。
//
// 具体行为（申请落库、拥有者独家审批、批后建节点边）在 friend_request_test.go。
func TestFriendRequestActionsFailClosedWhenTheApprovalTableIsNotWired(t *testing.T) {
	rec, srv := newIMServiceServer(t, imServiceServerOptions{})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	handler.friendStore = &FriendStore{}

	for _, action := range []string{"getRequests", "sendRequest", "approveRequest", "rejectRequest"} {
		sendWebCommand(t, handler, "user-1", "friend", action,
			map[string]interface{}{
				"toUserId":     "user-2",
				"requestId":    "fr-1",
				"deviceNodeId": "rc_node_abc_agent",
			})
		wantWebError(t, rec.last(t), 503, "friend service is unavailable")
	}
}

func TestFriendAddCreatesTheRongCloudEdge(t *testing.T) {
	rec, srv := newIMServiceServer(t, imServiceServerOptions{})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	handler.friendStore = &FriendStore{}

	sendWebCommand(t, handler, "user-1", "friend", "add",
		map[string]interface{}{"friendId": "rc_node_abc_agent", "message": "我是张三"})

	reply := rec.last(t)
	if reply["status"] != "success" {
		t.Fatalf("add should succeed, got %#v", reply)
	}
	data, _ := reply["data"].(map[string]interface{})
	if data["friendId"] != "rc_node_abc_agent" {
		t.Errorf("friendId = %#v", data["friendId"])
	}
	if !rec.called("/friend/add.json") {
		t.Errorf("expected a call to /friend/add.json, paths=%v", rec.paths)
	}
}

// friend/add 必须把客户端的 optType 透传给融云，缺省值不能变。
//
// 缺省保持 "2"（直加）是硬要求：所有既有客户端都不传 optType，一旦缺省漂成
// "1"，线上每一次加好友都会变成「等对方审批」，而审批入口在服务端并不存在。
func TestFriendAddForwardsTheOptType(t *testing.T) {
	cases := []struct {
		name        string
		payload     map[string]interface{}
		wantOptType string
		wantPending bool
	}{
		{
			name:        "omitted stays a direct add",
			payload:     map[string]interface{}{"friendId": "user-2"},
			wantOptType: "2",
			wantPending: false,
		},
		{
			name:        "explicit 2 stays a direct add",
			payload:     map[string]interface{}{"friendId": "user-2", "optType": "2"},
			wantOptType: "2",
			wantPending: false,
		},
		{
			name:        "explicit 1 sends a friend application",
			payload:     map[string]interface{}{"friendId": "user-2", "optType": "1"},
			wantOptType: "1",
			wantPending: true,
		},
		{
			name:        "opt_type alias is accepted too",
			payload:     map[string]interface{}{"friendId": "user-2", "opt_type": "1"},
			wantOptType: "1",
			wantPending: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, srv := newIMServiceServer(t, imServiceServerOptions{})
			defer srv.Close()
			handler := newIMServiceHandler(t, srv)
			handler.friendStore = &FriendStore{}

			sendWebCommand(t, handler, "user-1", "friend", "add", tc.payload)

			reply := rec.last(t)
			if reply["status"] != "success" {
				t.Fatalf("add should succeed, got %#v", reply)
			}
			form := rec.lastForm("/friend/add.json")
			if form == nil {
				t.Fatalf("/friend/add.json was never called, paths=%v", rec.paths)
			}
			if got := form.Get("optType"); got != tc.wantOptType {
				t.Errorf("optType sent = %q, want %q (form=%v)", got, tc.wantOptType, form)
			}
			if got := form.Get("targetId"); got != "user-2" {
				t.Errorf("targetId sent = %q, want user-2", got)
			}
			data, _ := reply["data"].(map[string]interface{})
			if data["optType"] != tc.wantOptType {
				t.Errorf("data.optType = %#v, want %q", data["optType"], tc.wantOptType)
			}
			// optType=1 只是把申请发出去，好友边还没建，客户端不能把对方当好友。
			if data["pending"] != tc.wantPending {
				t.Errorf("data.pending = %#v, want %v", data["pending"], tc.wantPending)
			}
		})
	}
}

// 无意义的 optType 必须当参数错误拒掉，而不是悄悄按直加处理 —— 静默改写用户
// 意图比报错更糟（用户以为发了申请，其实直接把人加成好友了）。
func TestFriendAddRejectsAnUnknownOptType(t *testing.T) {
	rec, srv := newIMServiceServer(t, imServiceServerOptions{})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	handler.friendStore = &FriendStore{}

	sendWebCommand(t, handler, "user-1", "friend", "add",
		map[string]interface{}{"friendId": "user-2", "optType": "3"})

	wantWebError(t, rec.last(t), 400, "optType")
	if rec.called("/friend/add.json") {
		t.Errorf("a rejected optType must not reach RongCloud, paths=%v", rec.paths)
	}
}

func TestFriendListReturnsTheLocalListShape(t *testing.T) {
	rec, srv := newIMServiceServer(t, imServiceServerOptions{
		friendList: []map[string]interface{}{
			{"userId": "user-2", "name": "鲍勃", "portraitUri": "https://example.test/b.png"},
		},
	})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	handler.friendStore = &FriendStore{}

	sendWebCommand(t, handler, "user-1", "friend", "getList", map[string]interface{}{})

	reply := rec.last(t)
	if reply["status"] != "success" {
		t.Fatalf("getList should succeed, got %#v", reply)
	}
	data, _ := reply["data"].(map[string]interface{})
	list, ok := data["list"].([]interface{})
	if !ok || len(list) != 1 {
		t.Fatalf("data.list = %#v, want one entry", data["list"])
	}
	entry, _ := list[0].(map[string]interface{})
	if entry["userId"] != "user-2" || entry["nickname"] != "鲍勃" {
		t.Errorf("entry = %#v", entry)
	}
	if data["total"] != float64(1) {
		t.Errorf("total = %#v, want 1", data["total"])
	}
}

func TestFriendCheckRelationReportsTheFriendEdge(t *testing.T) {
	rec, srv := newIMServiceServer(t, imServiceServerOptions{
		friendList: []map[string]interface{}{{"userId": "user-2"}},
	})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	handler.friendStore = &FriendStore{}

	sendWebCommand(t, handler, "user-1", "friend", "checkRelation",
		map[string]interface{}{"friendId": "user-2"})
	data, _ := rec.last(t)["data"].(map[string]interface{})
	if data["isFriend"] != true {
		t.Errorf("isFriend = %#v, want true", data["isFriend"])
	}
	if data["isPending"] != false {
		t.Errorf("isPending = %#v, want false (a friend edge exists, so nothing can be pending)", data["isPending"])
	}
	// 好友边已建时 isPending=false 是可信的；不是好友时服务端查不出来（融云
	// 没有读申请状态的接口），必须显式标 unknown，别让客户端以为「没有申请」。
	if data["isPendingKnown"] != true {
		t.Errorf("isPendingKnown = %#v, want true for a confirmed friend", data["isPendingKnown"])
	}
}

// DEF-5：不是好友时，服务端并不知道有没有待处理申请 —— isPending=false 只代表
// 「好友边没建」，必须用 isPendingKnown=false 标出这一点（申请态只在 SDK 侧）。
func TestFriendCheckRelationDoesNotClaimToKnowThePendingState(t *testing.T) {
	rec, srv := newIMServiceServer(t, imServiceServerOptions{})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	handler.friendStore = &FriendStore{}

	sendWebCommand(t, handler, "user-1", "friend", "checkRelation",
		map[string]interface{}{"friendId": "user-9"})
	data, _ := rec.last(t)["data"].(map[string]interface{})
	if data["isFriend"] != false {
		t.Errorf("isFriend = %#v, want false", data["isFriend"])
	}
	if data["isPending"] != false {
		t.Errorf("isPending = %#v, want the compat value false", data["isPending"])
	}
	if data["isPendingKnown"] != false {
		t.Errorf("isPendingKnown = %#v, want false: the server cannot read application state", data["isPendingKnown"])
	}
}

// --- user（微信登录） --------------------------------------------------------

func TestWeChatLoginWithoutConfigurationRepliesAnExplicitError(t *testing.T) {
	t.Setenv(wechatOpenIDEnvAppID, "")
	t.Setenv(wechatOpenIDEnvSecret, "")

	rec, srv := newIMServiceServer(t, imServiceServerOptions{weChatOpenID: "openid-1"})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	handler.userStore = newUserFixture().store()

	sendWebCommand(t, handler, "user-1", "user", "wxLogin", map[string]interface{}{"code": "wx-code"})

	// 缺配置是「功能未启用」（503），不是 500 —— 客户端要能读出人话。
	wantWebError(t, rec.last(t), 503, "微信登录未配置", wechatOpenIDEnvAppID)
	if rec.called("/sns/jscode2session") {
		t.Error("must not call the WeChat API when it is not configured")
	}
}

func TestWeChatLoginReusesTheStoredTokenForAKnownOpenID(t *testing.T) {
	t.Setenv(wechatOpenIDEnvAppID, "wx-app-id")
	t.Setenv(wechatOpenIDEnvSecret, "wx-secret")

	rec, srv := newIMServiceServer(t, imServiceServerOptions{weChatOpenID: "openid-known"})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	fixture := newUserFixture(WeChatUser{
		UserID:         "123456",
		Username:       "123456",
		Nickname:       "老用户",
		PortraitURI:    "https://example.test/a.png",
		RongCloudToken: "rc-token-stored",
		Status:         "active",
	})
	fixture.openIDs["openid-known"] = "123456"
	handler.userStore = fixture.store()

	sendWebCommand(t, handler, "user-1", "user", "wxLogin", map[string]interface{}{"code": "wx-code"})

	reply := rec.last(t)
	if reply["status"] != "success" {
		t.Fatalf("wxLogin should succeed, got %#v", reply)
	}
	data, _ := reply["data"].(map[string]interface{})
	if data["token"] != "rc-token-stored" {
		t.Errorf("token = %#v, want the stored one (re-minting kicks other devices off)", data["token"])
	}
	if data["userId"] != "123456" || data["isNewUser"] != false {
		t.Errorf("data = %#v", data)
	}
	if rec.called("/user/getToken.json") {
		t.Error("must not mint a new token when one is stored")
	}
}

func TestWeChatLoginCreatesAnAccountAndMintsAToken(t *testing.T) {
	t.Setenv(wechatOpenIDEnvAppID, "wx-app-id")
	t.Setenv(wechatOpenIDEnvSecret, "wx-secret")

	rec, srv := newIMServiceServer(t, imServiceServerOptions{weChatOpenID: "openid-brand-new"})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	fixture := newUserFixture()
	handler.userStore = fixture.store()

	sendWebCommand(t, handler, "user-1", "user", "wxLogin", map[string]interface{}{"code": "wx-code"})

	reply := rec.last(t)
	if reply["status"] != "success" {
		t.Fatalf("wxLogin should succeed, got %#v", reply)
	}
	data, _ := reply["data"].(map[string]interface{})
	if data["token"] != "rc-token-minted" {
		t.Errorf("token = %#v, want the freshly minted one", data["token"])
	}
	if data["isNewUser"] != true {
		t.Errorf("isNewUser = %#v, want true", data["isNewUser"])
	}
	if nickname, _ := data["nickname"].(string); !strings.HasPrefix(nickname, "微信用户") {
		t.Errorf("nickname = %q, want a 微信用户 default", nickname)
	}
	created, found := fixture.byOpenID("openid-brand-new")
	if !found {
		t.Fatal("the account should be persisted under the openid")
	}
	if hash := fixture.passwordHashOf(created.UserID); hash != wechatOnlyPasswordHash {
		t.Errorf("password_hash = %q, want the wechat-only sentinel", hash)
	}
	if created.RongCloudToken != "rc-token-minted" {
		t.Errorf("stored token = %q, want it persisted", created.RongCloudToken)
	}
}

// DEF-1：微信接口把 AppSecret 放在 query string，而 *url.Error 会打印整条
// URL（net/url 的 stripPassword 只脱敏 userinfo）。这里用合成的 *url.Error
// 直接钉 sanitizeWeChatTransportError：剥不掉 URL，secret 就会进日志。
func TestSanitizeWeChatTransportErrorStripsTheSecretBearingURL(t *testing.T) {
	const secret = "SUPER-SECRET-VALUE"
	raw := &url.Error{
		Op:  "Get",
		URL: "https://api.weixin.qq.com/sns/jscode2session?appid=wx-app&secret=" + secret + "&js_code=code-1",
		Err: errors.New("dial tcp: lookup api.weixin.qq.com: no such host"),
	}

	sanitized := sanitizeWeChatTransportError(raw)
	if sanitized == nil {
		t.Fatal("a transport error must stay an error")
	}
	message := sanitized.Error()
	// 注意：底因里出现域名（DNS 失败会带 host）是正常且必要的，所以这里只钉
	// 「URL 的 scheme/路径/query 不能出现」+「secret 值不能出现」。
	for _, forbidden := range []string{secret, "secret=", "https://", "jscode2session?", "appid="} {
		if strings.Contains(message, forbidden) {
			t.Errorf("sanitized message leaks %q: %s", forbidden, message)
		}
	}
	// 底因要保留，否则排障时只剩一句「request failed」。
	if !strings.Contains(message, "no such host") {
		t.Errorf("sanitized message dropped the root cause: %s", message)
	}
	// 非 *url.Error 原样返回（它们不带 URL）。
	plain := errors.New("boom")
	if got := sanitizeWeChatTransportError(plain); got != plain {
		t.Errorf("a non-URL error must pass through unchanged, got %v", got)
	}
	if got := sanitizeWeChatTransportError(nil); got != nil {
		t.Errorf("nil must stay nil, got %v", got)
	}
}

// 同一条链路的端到端版本：真的让 http.Client.Do 失败（连不上的地址），确认
// wechatCode2Session 返回的错误里没有 secret。这条能挡住「有人把
// sanitizeWeChatTransportError 改回 %w 直传」。
func TestWeChatCode2SessionNeverLeaksTheSecretOnTransportFailure(t *testing.T) {
	const secret = "SUPER-SECRET-VALUE"
	_, srv := newIMServiceServer(t, imServiceServerOptions{})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	// 指向一个必然连不上的地址：拒绝连接/超时都会返回 *url.Error。
	handler.wechatAPIBase = "http://127.0.0.1:1"
	handler.wechatHTTPClient = &http.Client{Timeout: 2 * time.Second}

	_, err := handler.wechatCode2Session(context.Background(), "wx-app-id", secret, "code-1")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	message := err.Error()
	if strings.Contains(message, secret) || strings.Contains(message, "secret=") {
		t.Fatalf("AppSecret leaked into the error: %s", message)
	}
	// URL 还在 = 只是把 secret 打码而没剥 URL，下次换个参数名又会漏。
	// （底因里出现 `127.0.0.1:1` 这种 dial 地址是正常的，不算 URL。）
	if strings.Contains(message, "http://") || strings.Contains(message, "/sns/jscode2session") {
		t.Errorf("the request URL must be stripped entirely, got: %s", message)
	}
}

// DEF-4：并发登录同一个 openid 时，后到的请求走 ON CONFLICT 回查拿到别人刚建
// 的行，那不是「新建账号」—— isNewUser 必须是 false，否则客户端会把它当首次
// 注册（例如弹引导页 / 发注册事件）。
func TestWeChatLoginDoesNotClaimCreationWhenAConcurrentRequestWon(t *testing.T) {
	t.Setenv(wechatOpenIDEnvAppID, "wx-app-id")
	t.Setenv(wechatOpenIDEnvSecret, "wx-secret")

	rec, srv := newIMServiceServer(t, imServiceServerOptions{weChatOpenID: "openid-raced"})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	fixture := newUserFixture()
	fixture.concurrentInsert = &WeChatUser{
		UserID: "654321", Username: "654321", Nickname: "微信用户raced", Status: "active",
	}
	handler.userStore = fixture.store()

	sendWebCommand(t, handler, "user-1", "user", "wxLogin", map[string]interface{}{"code": "wx-code"})

	reply := rec.last(t)
	if reply["status"] != "success" {
		t.Fatalf("wxLogin should succeed, got %#v", reply)
	}
	data, _ := reply["data"].(map[string]interface{})
	if data["isNewUser"] != false {
		t.Errorf("isNewUser = %#v, want false: a concurrent request created the account", data["isNewUser"])
	}
	if data["userId"] != "654321" {
		t.Errorf("userId = %#v, want the winner's account", data["userId"])
	}
}

func TestWeChatLoginSurfacesAWeChatBusinessErrorAs400(t *testing.T) {
	t.Setenv(wechatOpenIDEnvAppID, "wx-app-id")
	t.Setenv(wechatOpenIDEnvSecret, "wx-secret")

	rec, srv := newIMServiceServer(t, imServiceServerOptions{
		weChatErrCode: 40029,
		weChatErrMsg:  "invalid code",
	})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)
	handler.userStore = newUserFixture().store()

	sendWebCommand(t, handler, "user-1", "user", "wxLogin", map[string]interface{}{"code": "bad-code"})

	wantWebError(t, rec.last(t), 400, "微信登录失败", "invalid code")
}

// --- user store fixture ------------------------------------------------------

// userFixture 是 UserStore 的内存实现，语义对齐 SQL 版：openid 唯一、建号时
// 生成 6 位数字 id 且 username = user_id。
type userFixture struct {
	mu        sync.Mutex
	users     map[string]WeChatUser
	openIDs   map[string]string
	passwords map[string]string
	seq       int
	// concurrentInsert 模拟「另一个并发请求抢先插入」：CreateWeChatUser 不新建，
	// 直接回这条记录且 created=false —— 生产里对应 ON CONFLICT DO NOTHING 命中
	// （ErrNoRows）后回查成功的那支（DEF-4）。
	concurrentInsert *WeChatUser
}

func newUserFixture(seed ...WeChatUser) *userFixture {
	f := &userFixture{
		users:     make(map[string]WeChatUser),
		openIDs:   make(map[string]string),
		passwords: make(map[string]string),
	}
	for _, user := range seed {
		f.users[user.UserID] = user
	}
	return f
}

func (f *userFixture) byOpenID(openID string) (WeChatUser, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	userID, ok := f.openIDs[openID]
	if !ok {
		return WeChatUser{}, false
	}
	user, found := f.users[userID]
	return user, found
}

func (f *userFixture) store() *UserStore {
	return &UserStore{
		FindByOpenID: func(_ context.Context, openID string) (WeChatUser, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			userID, ok := f.openIDs[openID]
			if !ok {
				return WeChatUser{}, false, nil
			}
			user, found := f.users[userID]
			return user, found, nil
		},
		CreateWeChatUser: func(_ context.Context, params CreateWeChatUserParams) (WeChatUser, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.concurrentInsert != nil {
				user := *f.concurrentInsert
				f.users[user.UserID] = user
				f.openIDs[params.OpenID] = user.UserID
				f.passwords[user.UserID] = params.PasswordHash
				return user, false, nil
			}
			if userID, exists := f.openIDs[params.OpenID]; exists {
				return f.users[userID], false, nil
			}
			f.seq++
			userID := fmt.Sprintf("%d", 100000+f.seq)
			user := WeChatUser{
				UserID:      userID,
				Username:    userID,
				Nickname:    params.Nickname,
				PortraitURI: params.PortraitURI,
				Status:      "active",
			}
			f.users[userID] = user
			f.openIDs[params.OpenID] = userID
			f.passwords[userID] = params.PasswordHash
			return user, true, nil
		},
		SaveRongCloudToken: func(_ context.Context, userID, token string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			user, ok := f.users[userID]
			if !ok {
				return errors.New("user not found")
			}
			user.RongCloudToken = token
			f.users[userID] = user
			return nil
		},
	}
}

func (f *userFixture) passwordHashOf(userID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.passwords[userID]
}

// --- 装配证据（registration.go + router.go 的 `DB: pool`） --------------------

// stubDBTX 是 db.DBTX 的最小实现：记录 QueryRow 调用次数，Scan 一律返回
// pgx.ErrNoRows（等价于「表里没有这行」）。Query 故意报错，用来证明
// FriendStore.NodeOwner 确实是被绑到这个 DBTX 上（而不是某种内存真值）。
type stubDBTX struct {
	mu        sync.Mutex
	queryRows int
}

func (s *stubDBTX) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 0"), nil
}

func (s *stubDBTX) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, errors.New("stub dbtx: Query must not be called in this test")
}

func (s *stubDBTX) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	s.mu.Lock()
	s.queryRows++
	s.mu.Unlock()
	return stubNoRows{}
}

func (s *stubDBTX) queryRowCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queryRows
}

type stubNoRows struct{}

func (stubNoRows) Scan(...interface{}) error { return pgx.ErrNoRows }

// TestFactoryAttachesTheLocalStoresOnlyWhenDBIsProvided 是「装配生效」的证据。
//
// 为什么必须有这条：其余用例全部通过 actorResolver / store stub 绕开真库，所以
// registration.go 里漏掉 NewXxxStore、或 router.go 传了 nil，单测仍然全绿而线上
// 恒回 503。这里直接调用生产用的 newRongCloudFactory，断言：
//   - ChannelDeps.DB 非 nil ⇒ 三个 store 全部非 nil，且真的绑在传入的 DBTX 上
//     （FindByOpenID 命中 QueryRow → ErrNoRows → found=false；NodeOwner 把
//     stub 的 Query 错误原样冒出来）；
//   - ChannelDeps.DB 为 nil ⇒ 三个 store 全为 nil（对应 service 回 503，不 panic）。
func TestFactoryAttachesTheLocalStoresOnlyWhenDBIsProvided(t *testing.T) {
	// installConfig 的形状见 config.go:13-17；Decrypter 用假实现，只验证
	// 「DB 有没有被接上」，不验证凭证解密。
	encrypted := base64.StdEncoding.EncodeToString([]byte("ciphertext"))
	raw := json.RawMessage(fmt.Sprintf(
		`{"app_key":"key","app_secret_encrypted":%q,"system_node_id":"sys_node"}`, encrypted))
	cfg := channel.Config{Type: TypeRongCloud, Raw: raw}
	decrypt := func([]byte) ([]byte, error) { return []byte("app-secret"), nil }

	build := func(dbConn db.DBTX) *rongcloudChannel {
		t.Helper()
		factory := newRongCloudFactory(ChannelDeps{
			Decrypt:    decrypt,
			Logger:     testLogger(),
			HTTPClient: http.DefaultClient,
			DB:         dbConn,
		})
		built, err := factory(cfg)
		if err != nil {
			t.Fatalf("factory: %v", err)
		}
		rc, ok := built.(*rongcloudChannel)
		if !ok {
			t.Fatalf("factory built %T, want *rongcloudChannel", built)
		}
		return rc
	}

	t.Run("DB provided", func(t *testing.T) {
		stub := &stubDBTX{}
		handler := build(stub).systemHandler
		if handler.lobsterStore == nil || handler.friendStore == nil || handler.userStore == nil {
			t.Fatalf("stores must all be attached: lobster=%v friend=%v user=%v",
				handler.lobsterStore != nil, handler.friendStore != nil, handler.userStore != nil)
		}
		// service=conversation（migrations/605）同理：漏装配的话客户端点
		// 「发消息」会重新收到 404「未知的服务或操作」。
		if handler.conversations == nil {
			t.Fatal("conversation store must be attached when a DBTX is provided")
		}
		if _, found, err := handler.conversations.Get(context.Background(), "u-absent", 1); err != nil || found {
			t.Errorf("conversation Get = (found %v, err %v), want (false, nil)", found, err)
		}

		// 存储层真的接到了这个 DBTX 上：零行 → (not found, 无错误)。
		if _, found, err := handler.userStore.FindByOpenID(context.Background(), "openid-absent"); err != nil || found {
			t.Errorf("FindByOpenID = (found %v, err %v), want (false, nil)", found, err)
		}
		if stub.queryRowCalls() == 0 {
			t.Error("FindByOpenID never reached the injected DBTX")
		}
		if _, found, err := handler.lobsterStore.Get(context.Background(), "user-1", "lob_missing"); err != nil || found {
			t.Errorf("lobster Get = (found %v, err %v), want (false, nil)", found, err)
		}
		// NodeOwner 走 Query（stub 故意报错）：错误冒出来即证明绑定成立。
		if _, _, err := handler.friendStore.NodeOwner(context.Background(), "rc_node_x"); err == nil {
			t.Error("NodeOwner should surface the stub DBTX error, got nil")
		}
	})

	t.Run("DB missing", func(t *testing.T) {
		handler := build(nil).systemHandler
		if handler.lobsterStore != nil || handler.friendStore != nil || handler.userStore != nil {
			t.Fatalf("stores must stay nil without a DBTX: lobster=%v friend=%v user=%v",
				handler.lobsterStore != nil, handler.friendStore != nil, handler.userStore != nil)
		}
		if handler.conversations != nil {
			t.Fatal("conversation store must stay nil without a DBTX (service replies 503)")
		}
	})
}

// --- 微信-only 密码哨兵 ------------------------------------------------------

// 本仓密码校验的唯一入口是 clawVerifyPassword（internal/handler/claw_compat.go:91-107），
// 它先把 hash 按 "$" 切成 4 段并要求首段等于 "pbkdf2_sha256"，否则直接 false。
// 这条测试钉住哨兵值的格式不变量：以后谁把 wechatOnlyPasswordHash 改成看起来
// 合法的 pbkdf2 串（4 段 + 正确前缀），这里会立刻红。
//
// 关于 bcrypt：本仓 0 处引用 golang.org/x/crypto/bcrypt（go.mod 也没有该依赖），
// claw_im_users.password_hash 只被 clawVerifyPassword 读，因此不存在
// bcrypt.CompareHashAndPassword 被喂非法格式的路径（该函数对非法格式返回错误、
// 不 panic，但本仓根本没有这条调用）。
func TestWeChatOnlySentinelCannotBeVerifiedAsAPasswordHash(t *testing.T) {
	parts := strings.Split(wechatOnlyPasswordHash, "$")
	if len(parts) == 4 && parts[0] == "pbkdf2_sha256" {
		t.Fatalf("sentinel %q looks like a verifiable pbkdf2 hash; it must not", wechatOnlyPasswordHash)
	}
	if strings.TrimSpace(wechatOnlyPasswordHash) == "" {
		t.Fatal("sentinel must not be empty: password_hash is NOT NULL and an empty value reads like a bug")
	}
}
