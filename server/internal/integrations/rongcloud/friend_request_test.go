package rongcloud

// friend_request_test.go 钉住「把 AI 节点加为好友，必须由节点拥有者审批」这条
// 链路（实现见 friend_request.go，表见 migrations/603）。
//
// 这条链路补的是移动端那套降级实现的缺口：申请只能发给拥有者账号，拥有者在
// 融云「新的朋友」里同意后建的是**人↔人**好友边，请求方拿不到那个 AI 节点。
// 所以本文件最重要的断言不是「接口回 success」，而是**审批通过后确实发出了
// 「请求方→节点」和「节点→请求方」两条好友边**，以及**审批权只在拥有者手里**。
//
// 第二个必须钉的点，方向与上一版相反：读路径**不允许**替拥有者批准。早先的实现
// 拿「请求方与拥有者当前是融云好友」当审批凭据，可那条边请求方自己就能造出来
// （friend/add 的 optType 由客户端决定、缺省 2 直加），于是「查一次关系」就能让
// 服务端把节点边建好、拥有者全程不知情。审批只能来自 approveRequest。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ---- 内存审批表 ----

// friendRequestMemory 是 FriendRequestStore 的内存实现，语义对齐
// friend_request.go 里的 SQL：Create 幂等（同 from+node 的待审批行原地更新）、
// Decide 只认 to_user_id 且只改待审批行。
type friendRequestMemory struct {
	mu     sync.Mutex
	rows   map[string]FriendRequest
	next   int
	lists  []string // ListPending 每次收到的审批人（钉「只按发送者查」）
	creates int
}

const (
	testRequestID       = "11111111-1111-4111-8111-111111111111"
	testNodeRCID        = "rc_node_abc_agent"
	testRequesterUserID = "user-requester"
	testOwnerUserID     = "user-owner"
)

func newFriendRequestMemory() (*FriendRequestStore, *friendRequestMemory) {
	mem := &friendRequestMemory{rows: make(map[string]FriendRequest)}
	store := &FriendRequestStore{
		Create: func(_ context.Context, req FriendRequest) (FriendRequest, error) {
			mem.mu.Lock()
			defer mem.mu.Unlock()
			mem.creates++
			for id, row := range mem.rows {
				if row.Status == friendRequestPending &&
					row.FromUserID == req.FromUserID && row.DeviceNodeID == req.DeviceNodeID {
					row.Message = req.Message
					row.ToUserID = req.ToUserID
					mem.rows[id] = row
					return row, nil
				}
			}
			req.ID = testRequestID
			req.Status = friendRequestPending
			req.CreatedAt = time.Unix(1_700_000_000, 0).UTC()
			mem.rows[req.ID] = req
			return req, nil
		},
		FindPending: func(_ context.Context, fromUserID, deviceNodeID string) (FriendRequest, bool, error) {
			mem.mu.Lock()
			defer mem.mu.Unlock()
			for _, row := range mem.rows {
				if row.Status == friendRequestPending &&
					row.FromUserID == fromUserID && row.DeviceNodeID == deviceNodeID {
					return row, true, nil
				}
			}
			return FriendRequest{}, false, nil
		},
		Get: func(_ context.Context, id string) (FriendRequest, bool, error) {
			mem.mu.Lock()
			defer mem.mu.Unlock()
			row, found := mem.rows[id]
			return row, found, nil
		},
		ListPending: func(_ context.Context, toUserID string) ([]FriendRequest, error) {
			mem.mu.Lock()
			defer mem.mu.Unlock()
			mem.lists = append(mem.lists, toUserID)
			pending := make([]FriendRequest, 0, len(mem.rows))
			for _, row := range mem.rows {
				if row.Status == friendRequestPending && row.ToUserID == toUserID {
					pending = append(pending, row)
				}
			}
			return pending, nil
		},
		Decide: func(_ context.Context, id, toUserID string, status int16) (FriendRequest, bool, error) {
			mem.mu.Lock()
			defer mem.mu.Unlock()
			row, found := mem.rows[id]
			if !found || row.ToUserID != toUserID || row.Status != friendRequestPending {
				return FriendRequest{}, false, nil
			}
			row.Status = status
			mem.rows[id] = row
			return row, true, nil
		},
		HasApproved: func(_ context.Context, fromUserID, deviceNodeID, toUserID string) (bool, error) {
			mem.mu.Lock()
			defer mem.mu.Unlock()
			for _, row := range mem.rows {
				if row.Status == friendRequestApproved &&
					row.FromUserID == fromUserID && row.DeviceNodeID == deviceNodeID &&
					row.ToUserID == toUserID {
					return true, nil
				}
			}
			return false, nil
		},
	}
	return store, mem
}

// seed 直接落一行（SQL 侧对应 status 1/2 的历史行，Create 造不出来）。
func (m *friendRequestMemory) seed(row FriendRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[row.ID] = row
}

func (m *friendRequestMemory) row(t *testing.T) FriendRequest {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	row, found := m.rows[testRequestID]
	if !found {
		t.Fatalf("expected a stored friend request, rows=%#v", m.rows)
	}
	return row
}

func (m *friendRequestMemory) rowCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

func (m *friendRequestMemory) listQueries() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.lists...)
}

// ---- 记录好友边与回包的假融云 ----

type friendEdgeRecorder struct {
	mu       sync.Mutex
	edges    []string // "userId->targetId"，/friend/add.json
	removals []string // "userId->targetId"，/friend/delete.json
	optType  []string
	paths    []string
	replies  []map[string]interface{}
	// friends 是 /friend/get.json 回的关系（双向边的两端都会在列表里）。
	friends []string
	// alreadyFriends 里的 "user->target" 会被 /friend/add.json 回 25460
	// （融云对已存在的边就是这么回的），用来钉「本来就存在的边不该被撤」。
	alreadyFriends map[string]bool
	// failAdd 里的 "user->target" 会被 /friend/add.json 回 500（模拟建边失败），
	// 用来钉「中途失败要回滚本次已建出的边」。
	failAdd map[string]bool
}

func newFriendEdgeServer(t *testing.T, friends ...string) (*friendEdgeRecorder, *httptest.Server) {
	t.Helper()
	rec := &friendEdgeRecorder{friends: friends}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		alreadyFriend := false
		failedAdd := false
		rec.mu.Lock()
		rec.paths = append(rec.paths, r.URL.Path)
		switch r.URL.Path {
		case "/friend/add.json":
			pair := r.PostForm.Get("userId") + "->" + r.PostForm.Get("targetId")
			rec.edges = append(rec.edges, pair)
			rec.optType = append(rec.optType, r.PostForm.Get("optType"))
			alreadyFriend = rec.alreadyFriends[pair]
			failedAdd = rec.failAdd[pair]
		case "/friend/delete.json":
			for _, target := range strings.Split(r.PostForm.Get("targetIds"), ",") {
				if target = strings.TrimSpace(target); target == "" {
					continue
				}
				rec.removals = append(rec.removals, r.PostForm.Get("userId")+"->"+target)
			}
		case "/message/private/publish.json":
			var content map[string]interface{}
			if err := json.Unmarshal([]byte(r.PostForm.Get("content")), &content); err == nil {
				rec.replies = append(rec.replies, content)
			}
		}
		rec.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/friend/get.json":
			list := make([]map[string]interface{}, 0, len(rec.friends))
			for _, id := range rec.friends {
				list = append(list, map[string]interface{}{"userId": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 200, "friendList": list})
		case "/friend/add.json":
			code := 200
			if alreadyFriend {
				code = 25460
			}
			if failedAdd {
				code = 500
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": code})
		case "/message/private/publish.json":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 200, "msgUID": "uid-1"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 200})
		}
	}))
	return rec, srv
}

// markAlreadyFriends 让 /friend/add.json 对这对 (user, target) 回 25460。
func (r *friendEdgeRecorder) markAlreadyFriends(userID, targetID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.alreadyFriends == nil {
		r.alreadyFriends = make(map[string]bool)
	}
	r.alreadyFriends[userID+"->"+targetID] = true
}

// markAddFailure 让 /friend/add.json 对这对 (user, target) 回 500。
func (r *friendEdgeRecorder) markAddFailure(userID, targetID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failAdd == nil {
		r.failAdd = make(map[string]bool)
	}
	r.failAdd[userID+"->"+targetID] = true
}

func (r *friendEdgeRecorder) last(t *testing.T) map[string]interface{} {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.replies) == 0 {
		t.Fatal("no command_result was published")
	}
	return r.replies[len(r.replies)-1]
}

func (r *friendEdgeRecorder) recordedEdges() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.edges...)
}

func (r *friendEdgeRecorder) recordedOptTypes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.optType...)
}

func (r *friendEdgeRecorder) recordedRemovals() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.removals...)
}

func (r *friendEdgeRecorder) callCount(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, path2 := range r.paths {
		if path2 == path {
			count++
		}
	}
	return count
}

// newFriendRequestHandler 装配一个「有审批表 + 有归属解析」的 handler。
func newFriendRequestHandler(
	t *testing.T,
	srv *httptest.Server,
	store *FriendRequestStore,
	ownerOf func(ctx context.Context, nodeRCID string) (string, bool, error),
) *systemHandler {
	t.Helper()
	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	handler := newSystemHandler(client, "sys_node", testLogger(), nil, nil, nil, nil, nil, nil)
	handler.actorResolver = func(_ context.Context, fromUserID string) (string, error) {
		if strings.TrimSpace(fromUserID) == "" {
			return "", fmt.Errorf("missing sender")
		}
		return fromUserID, nil
	}
	handler.friendRequests = store
	handler.friendStore = &FriendStore{NodeOwner: ownerOf}
	return handler
}

func fixedOwner(ownerID string) func(context.Context, string) (string, bool, error) {
	return func(context.Context, string) (string, bool, error) {
		return ownerID, true, nil
	}
}

func replyData(t *testing.T, reply map[string]interface{}) map[string]interface{} {
	t.Helper()
	data, ok := reply["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("reply has no data object: %#v", reply)
	}
	return data
}

// ---- 发起申请 ----

// 申请必须落库，并且同时以 optType=1 投递给拥有者 —— 后者是拥有者在融云
// 「新的朋友」里能看到申请的唯一途径（移动端就靠这个入口审批）。
func TestFriendSendRequestStoresTheRequestAndDeliversItToTheOwner(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{
		"deviceNodeId": testNodeRCID,
		"message":      "想加你这台设备",
	})

	reply := rec.last(t)
	if reply["status"] != "success" {
		t.Fatalf("sendRequest should succeed, got %#v", reply)
	}
	data := replyData(t, reply)
	if data["status"] != "pending" {
		t.Errorf("status = %#v, want pending", data["status"])
	}
	if data["ownerId"] != testOwnerUserID {
		t.Errorf("ownerId = %#v, want the resolved owner", data["ownerId"])
	}
	if data["delivered"] != true {
		t.Errorf("delivered = %#v, want true when RongCloud accepted the application", data["delivered"])
	}
	row := mem.row(t)
	if row.FromUserID != testRequesterUserID || row.ToUserID != testOwnerUserID || row.DeviceNodeID != testNodeRCID {
		t.Errorf("stored row = %#v, want from->to->node", row)
	}
	edges := rec.recordedEdges()
	if len(edges) != 1 || edges[0] != testRequesterUserID+"->"+testOwnerUserID {
		t.Errorf("edges = %v, want one requester->owner application", edges)
	}
	if optTypes := rec.recordedOptTypes(); len(optTypes) != 1 || optTypes[0] != friendOptTypeRequest {
		t.Errorf("optType = %v, want %q (an application, not a direct add)", optTypes, friendOptTypeRequest)
	}
}

// 重复点「提交申请」不能堆出一串僵尸行。
func TestFriendSendRequestIsIdempotentForTheSameDevice(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	for i := 0; i < 3; i++ {
		sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{
			"deviceNodeId": testNodeRCID,
			"message":      "第 " + strconv.Itoa(i) + " 次",
		})
		if reply := rec.last(t); reply["status"] != "success" {
			t.Fatalf("attempt %d should succeed, got %#v", i, reply)
		}
	}
	if got := mem.rowCount(); got != 1 {
		t.Errorf("stored rows = %d, want 1 (one pending request per requester+device)", got)
	}
	if got := mem.row(t).Message; got != "第 2 次" {
		t.Errorf("message = %q, want the latest remark", got)
	}
}

// 已经是好友时再点申请：不落行、不再投递融云申请，直接回 alreadyFriend。
//
// 这不只是省一次往返。Create 的唯一索引只约束 status = 0，所以「批过之后再申请
// 一次」会新建一条 pending 行；拥有者随后点拒绝就会走到撤边逻辑，把**已经生效**
// 的节点关系删掉。挡住入口比事后兜底省事，也不给拥有者制造一条假申请。
func TestFriendSendRequestReturnsAlreadyFriendInsteadOfCreatingANewRequest(t *testing.T) {
	// 好友列表里已经有这台节点 ⇒ 关系已成立（审批早生效过）。
	rec, srv := newFriendEdgeServer(t, testNodeRCID)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{
		"deviceNodeId": testNodeRCID,
	})

	reply := rec.last(t)
	if reply["status"] != "success" {
		t.Fatalf("sendRequest to an existing friend should succeed, got %#v", reply)
	}
	data := replyData(t, reply)
	if data["status"] != "alreadyFriend" {
		t.Errorf("status = %#v, want alreadyFriend", data["status"])
	}
	if data["ownerId"] != testOwnerUserID || data["nodeId"] != testNodeRCID {
		t.Errorf("payload = %#v, want ownerId+nodeId for the caller to render", data)
	}
	if got := mem.rowCount(); got != 0 {
		t.Errorf("stored rows = %d, want 0（已是好友不该再落一条待审批行）", got)
	}
	if got := rec.callCount("/friend/add.json"); got != 0 {
		t.Errorf("/friend/add.json calls = %d, want 0（不该再向拥有者投递申请）", got)
	}
}

// 没有 deviceNodeId 时必须是明确的 400 + 正确指引：普通好友申请要走
// friend/add optType=1，而不是在这个节点专属通道里被糊过去。
func TestFriendSendRequestRequiresADeviceNodeID(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, _ := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{
		"toUserId": testOwnerUserID,
	})

	wantWebError(t, rec.last(t), 400, "deviceNodeId is required", "optType=1")
}

// 拥有者不能把自己的设备加为好友。
func TestFriendSendRequestRejectsYourOwnDevice(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, _ := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testRequesterUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{
		"deviceNodeId": testNodeRCID,
	})

	wantWebError(t, rec.last(t), 400, "cannot add your own device")
}

// 客户端带的 toUserId 只用于暴露「你看到的拥有者已过期」，不能用来指定审批人。
func TestFriendSendRequestRejectsAStaleOwnerClaim(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, _ := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{
		"deviceNodeId": testNodeRCID,
		"toUserId":     "user-someone-else",
	})

	wantWebError(t, rec.last(t), 409, "device owner changed")
}

// ---- 审批 ----

// 审批通过必须建出四条审批边：拥有者→请求方、请求方→拥有者（融云好友边是
// **单向**的，只建一条时请求方的通讯录里看不到拥有者）、请求方→节点、节点→请求方。
// 后两条才是「把智能体加为好友」，缺了人际边就是「同意完只有设备、没有拥有者」。
func TestFriendApproveRequestCreatesBothNodeEdgesThenMarksApproved(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{
		"deviceNodeId": testNodeRCID,
	})
	sendWebCommand(t, handler, testOwnerUserID, "friend", "approveRequest", map[string]interface{}{
		"requestId": testRequestID,
	})

	reply := rec.last(t)
	if reply["status"] != "success" {
		t.Fatalf("approveRequest should succeed, got %#v", reply)
	}
	edges := rec.recordedEdges()
	// 第一条是发起申请时的 requester->owner（optType=1），其余四条是审批建的边。
	want := []string{
		testRequesterUserID + "->" + testOwnerUserID,
		testOwnerUserID + "->" + testRequesterUserID,
		testRequesterUserID + "->" + testOwnerUserID,
		testRequesterUserID + "->" + testNodeRCID,
		testNodeRCID + "->" + testRequesterUserID,
	}
	if strings.Join(edges, ",") != strings.Join(want, ",") {
		t.Errorf("edges = %v\nwant      %v", edges, want)
	}
	if got := mem.row(t).Status; got != friendRequestApproved {
		t.Errorf("stored status = %d, want approved", got)
	}
}

// 审批权只在拥有者手里：别人批必须 403，且一条边都不许建（不能靠一条别人的
// 申请把节点边建给第三方）。
func TestFriendApproveRequestRejectsNonOwners(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{
		"deviceNodeId": testNodeRCID,
	})
	edgesBefore := len(rec.recordedEdges())

	sendWebCommand(t, handler, "user-intruder", "friend", "approveRequest", map[string]interface{}{
		"requestId": testRequestID,
	})

	wantWebError(t, rec.last(t), 403, "only the device owner")
	if got := len(rec.recordedEdges()); got != edgesBefore {
		t.Errorf("edges grew from %d to %d: a non-owner decision must not create edges", edgesBefore, got)
	}
	if got := mem.row(t).Status; got != friendRequestPending {
		t.Errorf("stored status = %d, want still pending", got)
	}
}

// 设备换了主人：历史申请必须作废并自动拒绝，不能靠一条旧申请把节点边建给
// 现在与设备无关的人。
func TestFriendApproveRequestInvalidatesTheRequestWhenTheOwnerChanged(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{
		"deviceNodeId": testNodeRCID,
	})
	// 拥有者换成别人之后再审批。
	handler.friendStore = &FriendStore{NodeOwner: fixedOwner("user-new-owner")}

	sendWebCommand(t, handler, testOwnerUserID, "friend", "approveRequest", map[string]interface{}{
		"requestId": testRequestID,
	})

	wantWebError(t, rec.last(t), 409, "device owner changed")
	if got := mem.row(t).Status; got != friendRequestRejected {
		t.Errorf("stored status = %d, want rejected (a stale request must not stay pending)", got)
	}
	// 除了发起申请那一条，审批不该再建任何边。
	if got := len(rec.recordedEdges()); got != 1 {
		t.Errorf("edges = %v, want only the original application", rec.recordedEdges())
	}
}

// 拒绝不建边；并且必须撤销可能已经建出来的节点边 —— approve 是逐条建边的，中途
// 失败会留下单向边而申请仍是待审批，此时拥有者点拒绝，那条边不能留着（否则请求方
// 好友列表里已经能看到这台节点，却从来没被批准）。
func TestFriendRejectRequestCreatesNoEdgesAndRevokesNodeEdges(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{
		"deviceNodeId": testNodeRCID,
	})
	edgesBefore := len(rec.recordedEdges())

	sendWebCommand(t, handler, testOwnerUserID, "friend", "rejectRequest", map[string]interface{}{
		"requestId": testRequestID,
	})

	if reply := rec.last(t); reply["status"] != "success" {
		t.Fatalf("rejectRequest should succeed, got %#v", reply)
	}
	if got := mem.row(t).Status; got != friendRequestRejected {
		t.Errorf("stored status = %d, want rejected", got)
	}
	if got := len(rec.recordedEdges()); got != edgesBefore {
		t.Errorf("edges = %v, want no new edges", rec.recordedEdges())
	}
	want := []string{
		testRequesterUserID + "->" + testNodeRCID,
		testNodeRCID + "->" + testRequesterUserID,
	}
	if got := rec.recordedRemovals(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("revoked edges = %v\nwant            %v", got, want)
	}
}

// 兜底：即使这对关系上还挂着一条重复的待审批行（存量数据、或并发路径造出来的），
// 拥有者点拒绝也不能把**已经生效**的节点关系撤掉。
func TestFriendRejectRequestKeepsLiveNodeEdgesWhenAnApprovedRequestExists(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	// 先有一条已通过的历史行（关系已生效），又冒出一条新的待审批行。
	mem.seed(FriendRequest{
		ID: "22222222-2222-4222-8222-222222222222", FromUserID: testRequesterUserID,
		ToUserID: testOwnerUserID, DeviceNodeID: testNodeRCID, Status: friendRequestApproved,
	})
	mem.seed(FriendRequest{
		ID: testRequestID, FromUserID: testRequesterUserID,
		ToUserID: testOwnerUserID, DeviceNodeID: testNodeRCID, Status: friendRequestPending,
	})

	sendWebCommand(t, handler, testOwnerUserID, "friend", "rejectRequest", map[string]interface{}{
		"requestId": testRequestID,
	})

	if reply := rec.last(t); reply["status"] != "success" {
		t.Fatalf("rejectRequest should succeed, got %#v", reply)
	}
	if got := mem.row(t).Status; got != friendRequestRejected {
		t.Errorf("stored status = %d, want rejected（这条重复的申请本身要拒掉）", got)
	}
	if got := rec.recordedRemovals(); len(got) != 0 {
		t.Errorf("revoked edges = %v\nwant               none（已通过的申请让这对关系生效，"+
			"重复申请被拒不能连带删掉它）", got)
	}
}

// 与上一条正好相反的另一半：那台设备**换过主人**。已批准的那条行是上一个拥有者
// （owner-previous）签的，新拥有者拒掉请求方重新提的申请时，那条旧批准不能替新拥
// 有者表态 —— 节点边必须被撤掉，否则接口回「已拒绝」、请求方却仍挂着这台节点。
//
// 这两条是一对正反：只有同时在，`HasApproved` 的 to_user_id 参数才不可能被人当
// 「多余参数」优化掉。
func TestFriendRejectRevokesEdgesWhenTheApprovalBelongsToAPreviousOwner(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	// 上一个拥有者批准过（历史行，不会被改写），设备此后转到 testOwnerUserID 名下；
	// 请求方对新拥有者重新发了一次申请。
	mem.seed(FriendRequest{
		ID: "33333333-3333-4333-8333-333333333333", FromUserID: testRequesterUserID,
		ToUserID: "owner-previous", DeviceNodeID: testNodeRCID, Status: friendRequestApproved,
	})
	mem.seed(FriendRequest{
		ID: testRequestID, FromUserID: testRequesterUserID,
		ToUserID: testOwnerUserID, DeviceNodeID: testNodeRCID, Status: friendRequestPending,
	})

	sendWebCommand(t, handler, testOwnerUserID, "friend", "rejectRequest", map[string]interface{}{
		"requestId": testRequestID,
	})

	if reply := rec.last(t); reply["status"] != "success" {
		t.Fatalf("rejectRequest should succeed, got %#v", reply)
	}
	if got := mem.row(t).Status; got != friendRequestRejected {
		t.Errorf("stored status = %d, want rejected", got)
	}
	want := []string{
		testRequesterUserID + "->" + testNodeRCID,
		testNodeRCID + "->" + testRequesterUserID,
	}
	if got := rec.recordedRemovals(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("revoked edges = %v\nwant            %v（上一个拥有者的旧批准不能替新拥有者表态，"+
			"节点边必须撤掉）", got, want)
	}
}

// approve 与 reject 并发：approve 的 Decide 输给 reject 时，绝不能把「已拒绝」
// 报成成功 —— 前端会因此把一个根本没加上边的节点渲染成好友。而且它刚建出来的
// 节点边必须回滚，否则一条被拒绝的申请仍然拿到了节点。
func TestFriendApproveRequestDoesNotReportSuccessWhenAConcurrentRejectWins(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	base, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, base, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{"deviceNodeId": testNodeRCID})

	// 模拟并发：审批流程读到的还是 pending，但轮到 Decide 时另一条路径已经拒绝。
	racing := *base
	racing.Decide = func(ctx context.Context, id, toUserID string, _ int16) (FriendRequest, bool, error) {
		_, _, _ = base.Decide(ctx, id, toUserID, friendRequestRejected)
		return FriendRequest{}, false, nil
	}
	handler.friendRequests = &racing

	sendWebCommand(t, handler, testOwnerUserID, "friend", "approveRequest", map[string]interface{}{"requestId": testRequestID})

	wantWebError(t, rec.last(t), 409, "already decided")
	if got := mem.row(t).Status; got != friendRequestRejected {
		t.Errorf("stored status = %d, want rejected", got)
	}
	// 输掉 CAS 之前已经建好了边，必须撤掉节点那两条（拥有者↔请求方那条不撤的
	// 理由见 revokeNodeEdgesUnlessApproved / completeFriendRequest 的注释）。
	want := []string{
		testRequesterUserID + "->" + testNodeRCID,
		testNodeRCID + "->" + testRequesterUserID,
	}
	if got := rec.recordedRemovals(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("revoked edges = %v\nwant            %v（被拒绝的申请不能留下节点边）", got, want)
	}
}

// 与上一条同一路径的另一半：这对关系**早有一条已批准的行**（同一拥有者批过），
// 之后又冒出一条重复的待审批行；approve 输给并发 reject 时的回滚**不能**把已经
// 生效的节点边撤掉 —— 那正是 R2 要防的「删掉已生效关系」。
//
// 这条用例专门钉 completeFriendRequest 回滚路径传给 HasApproved 的实参：若有人把
// 它改成空串或别的字段，HasApproved 恒为 false，这条会红而上一条仍绿。
func TestFriendApproveRollbackKeepsEdgesWhenAPreviousApprovalExists(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	base, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, base, fixedOwner(testOwnerUserID))

	// 同一对 (请求方, 设备) 上先有一条已批准的行（关系已生效）。
	mem.seed(FriendRequest{
		ID: "44444444-4444-4444-8444-444444444444", FromUserID: testRequesterUserID,
		ToUserID: testOwnerUserID, DeviceNodeID: testNodeRCID, Status: friendRequestApproved,
	})

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{"deviceNodeId": testNodeRCID})

	// 模拟并发：审批流程读到的还是 pending，但轮到 Decide 时另一条路径已经拒绝。
	racing := *base
	racing.Decide = func(ctx context.Context, id, toUserID string, _ int16) (FriendRequest, bool, error) {
		_, _, _ = base.Decide(ctx, id, toUserID, friendRequestRejected)
		return FriendRequest{}, false, nil
	}
	handler.friendRequests = &racing

	sendWebCommand(t, handler, testOwnerUserID, "friend", "approveRequest", map[string]interface{}{"requestId": testRequestID})

	wantWebError(t, rec.last(t), 409, "already decided")
	if got := mem.row(t).Status; got != friendRequestRejected {
		t.Errorf("stored status = %d, want rejected", got)
	}
	if got := rec.recordedRemovals(); len(got) != 0 {
		t.Errorf("revoked edges = %v, want none（这对关系早有一条已批准，回滚不该带走它）", got)
	}
}

// 建边期间设备换了主人：落状态之前必须复核，撤销刚建出来的边并把申请拒掉。
func TestFriendApproveRequestRevokesEdgesWhenTheOwnerChangesMidway(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{"deviceNodeId": testNodeRCID})

	// 第一次 NodeOwner 还是原拥有者（通过首检），第二次已经换人（落状态前复核）。
	ownerCalls := 0
	handler.friendStore = &FriendStore{NodeOwner: func(context.Context, string) (string, bool, error) {
		ownerCalls++
		if ownerCalls == 1 {
			return testOwnerUserID, true, nil
		}
		return "user-new-owner", true, nil
	}}

	sendWebCommand(t, handler, testOwnerUserID, "friend", "approveRequest", map[string]interface{}{"requestId": testRequestID})

	wantWebError(t, rec.last(t), 409, "device owner changed")
	if got := mem.row(t).Status; got != friendRequestRejected {
		t.Errorf("stored status = %d, want rejected（旧拥有者的申请不能生效）", got)
	}
	want := []string{
		testOwnerUserID + "->" + testRequesterUserID,
		testRequesterUserID + "->" + testOwnerUserID,
		testRequesterUserID + "->" + testNodeRCID,
		testNodeRCID + "->" + testRequesterUserID,
	}
	if got := rec.recordedRemovals(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("revoked edges = %v\nwant            %v（建出来的边必须被撤销）", got, want)
	}
}

// 同一场收尾，但请求方与拥有者**本来就是好友**（跟这次申请无关）：撤边只能撤节点
// 那两条，人↔人那条不能动 —— 撤它等于替两个用户删好友。
func TestFriendApproveRequestKeepsTheOwnerEdgeWhenTheTwoWereAlreadyFriends(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	// 拥有者↔请求方这两条边早就存在：融云对已存在的边回 25460（alreadyFriends）。
	// 两个方向都要标，任何一条缺失都等于「本次真的建了一条」，撤掉就是替用户删好友。
	rec.markAlreadyFriends(testOwnerUserID, testRequesterUserID)
	rec.markAlreadyFriends(testRequesterUserID, testOwnerUserID)

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{"deviceNodeId": testNodeRCID})

	ownerCalls := 0
	handler.friendStore = &FriendStore{NodeOwner: func(context.Context, string) (string, bool, error) {
		ownerCalls++
		if ownerCalls == 1 {
			return testOwnerUserID, true, nil
		}
		return "user-new-owner", true, nil
	}}

	sendWebCommand(t, handler, testOwnerUserID, "friend", "approveRequest", map[string]interface{}{"requestId": testRequestID})

	wantWebError(t, rec.last(t), 409, "device owner changed")
	if got := mem.row(t).Status; got != friendRequestRejected {
		t.Errorf("stored status = %d, want rejected", got)
	}
	want := []string{
		testRequesterUserID + "->" + testNodeRCID,
		testNodeRCID + "->" + testRequesterUserID,
	}
	if got := rec.recordedRemovals(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("revoked edges = %v\nwant            %v（本来就有的人↔人好友边必须保留，"+
			"节点边只可能由本次申请建出，照旧撤）", got, want)
	}
}

// 建边中途失败：本次已经建出来的人际边/单向节点边必须撤掉，申请保持待审批
// （重试还能补上）。不撤会留下「半成品」—— 申请没通过，请求方却已经能在好友
// 列表里看到拥有者或这台节点。
func TestFriendApproveRollsBackEdgesItAlreadyCreatedWhenAnEdgeFails(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{"deviceNodeId": testNodeRCID})
	// 第二条节点边（节点 → 请求方）失败：此时拥有者那两条边已经建好了。
	rec.markAddFailure(testNodeRCID, testRequesterUserID)

	sendWebCommand(t, handler, testOwnerUserID, "friend", "approveRequest", map[string]interface{}{"requestId": testRequestID})

	wantWebError(t, rec.last(t), 500, "failed to approve the friend request")
	want := []string{
		testOwnerUserID + "->" + testRequesterUserID,
		testRequesterUserID + "->" + testOwnerUserID,
		testRequesterUserID + "->" + testNodeRCID,
		testNodeRCID + "->" + testRequesterUserID,
	}
	if got := rec.recordedRemovals(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("revoked edges = %v\nwant            %v（本次建出来的边都要撤）", got, want)
	}
	if got := mem.row(t).Status; got != friendRequestPending {
		t.Errorf("stored status = %d, want still pending（边没建全，不能标记通过）", got)
	}
}

// 幂等重放：已经批过的申请再批一次要回成功而不是报错（客户端重试很常见）。
func TestFriendApproveRequestIsIdempotent(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, _ := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{"deviceNodeId": testNodeRCID})
	sendWebCommand(t, handler, testOwnerUserID, "friend", "approveRequest", map[string]interface{}{"requestId": testRequestID})
	edgesAfterApprove := len(rec.recordedEdges())

	sendWebCommand(t, handler, testOwnerUserID, "friend", "approveRequest", map[string]interface{}{"requestId": testRequestID})
	if reply := rec.last(t); reply["status"] != "success" {
		t.Fatalf("replayed approveRequest should succeed, got %#v", reply)
	}
	if got := len(rec.recordedEdges()); got != edgesAfterApprove {
		t.Errorf("edges grew on replay: %d -> %d (must not rebuild edges)", edgesAfterApprove, got)
	}
}

// ---- 拥有者审批列表 ----

// 列表只按**发送者**查：客户端带的 userId 是兼容字段，不能当查询条件，
// 否则任何人都能读到别人的申请列表。
func TestFriendGetRequestsIgnoresTheClaimedUserID(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{"deviceNodeId": testNodeRCID})

	sendWebCommand(t, handler, testOwnerUserID, "friend", "getRequests", map[string]interface{}{
		"userId": "user-someone-else",
	})

	reply := rec.last(t)
	if reply["status"] != "success" {
		t.Fatalf("getRequests should succeed, got %#v", reply)
	}
	list, _ := replyData(t, reply)["list"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("list = %#v, want the one pending request for the sender", list)
	}
	item, _ := list[0].(map[string]interface{})
	if item["deviceNodeId"] != testNodeRCID {
		t.Errorf("deviceNodeId = %#v, want %q", item["deviceNodeId"], testNodeRCID)
	}
	if item["status"] != float64(friendRequestPending) && item["status"] != friendRequestPending {
		t.Errorf("status = %#v, want pending", item["status"])
	}
	for _, queried := range mem.listQueries() {
		if queried != testOwnerUserID {
			t.Errorf("ListPending queried %q, want the sender %q", queried, testOwnerUserID)
		}
	}
}

// ---- 读路径不许替拥有者批准 ----

// 反向钉子：「请求方与拥有者当前是融云好友」不是审批凭据。
//
// friend/add 的 optType 由客户端决定且缺省 2（直加），请求方自己就能造出那条边。
// 若读路径据此自动批准，他只要调一次 checkRelation，服务端就会替他把节点边建好，
// 拥有者全程不知情 —— 审批权等于交给了请求方。
func TestFriendCheckRelationDoesNotTreatAnExistingFriendEdgeAsAnApproval(t *testing.T) {
	// 好友列表里已有 owner ⇒ 等价于「双方已经是融云好友」，旧实现会据此自动批准。
	rec, srv := newFriendEdgeServer(t, testOwnerUserID)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{"deviceNodeId": testNodeRCID})
	edgesBefore := len(rec.recordedEdges())

	sendWebCommand(t, handler, testRequesterUserID, "friend", "checkRelation", map[string]interface{}{
		"friendId": testNodeRCID,
	})

	data := replyData(t, rec.last(t))
	if data["isFriend"] != false {
		t.Errorf("isFriend = %#v, want false：一条请求方自己就能造出来的好友边不算审批", data["isFriend"])
	}
	if data["isPending"] != true {
		t.Errorf("isPending = %#v, want true（还在等拥有者审批）", data["isPending"])
	}
	if got := mem.row(t).Status; got != friendRequestPending {
		t.Errorf("stored status = %d, want still pending（读路径不许改状态）", got)
	}
	if got := len(rec.recordedEdges()); got != edgesBefore {
		t.Errorf("checkRelation created edges: %v", rec.recordedEdges()[edgesBefore:])
	}
}

// 同一件事的另一半：拥有者拉取待审批列表时，也不能因为「双方已是好友」就把这条
// 申请从列表里抹掉并置为通过。
func TestFriendGetRequestsDoesNotAutoApproveWhenTheTwoAreAlreadyFriends(t *testing.T) {
	rec, srv := newFriendEdgeServer(t, testOwnerUserID)
	defer srv.Close()
	store, mem := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{"deviceNodeId": testNodeRCID})

	sendWebCommand(t, handler, testOwnerUserID, "friend", "getRequests", map[string]interface{}{})

	list, _ := replyData(t, rec.last(t))["list"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("list = %#v, want the pending request to stay visible", list)
	}
	if got := mem.row(t).Status; got != friendRequestPending {
		t.Errorf("stored status = %d, want still pending（列表读取不许改状态）", got)
	}
}

// 反过来：只是「发了申请、拥有者还没批」时，查关系必须如实回 pending，
// 且一条边都不许建。
func TestFriendCheckRelationReportsPendingWithoutCreatingEdges(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, _ := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	sendWebCommand(t, handler, testRequesterUserID, "friend", "sendRequest", map[string]interface{}{"deviceNodeId": testNodeRCID})
	edgesBefore := len(rec.recordedEdges())

	sendWebCommand(t, handler, testRequesterUserID, "friend", "checkRelation", map[string]interface{}{
		"friendId": testNodeRCID,
	})

	data := replyData(t, rec.last(t))
	if data["isFriend"] != false {
		t.Errorf("isFriend = %#v, want false before the owner approves", data["isFriend"])
	}
	if data["isPending"] != true {
		t.Errorf("isPending = %#v, want true from the local approval table", data["isPending"])
	}
	if data["isPendingKnown"] != true {
		t.Errorf("isPendingKnown = %#v, want true (the table is the source of truth)", data["isPendingKnown"])
	}
	if got := len(rec.recordedEdges()); got != edgesBefore {
		t.Errorf("checkRelation created edges: %v", rec.recordedEdges()[edgesBefore:])
	}
}

// ---- friend/add 不许绕过审批直加节点 ----

// friend/add 不许对 AI 节点建关系，**任何 optType 都不许**：
//   - optType=2（直加）等于任何人知道节点 id 就能把自己挂到节点上、拥有者不知情；
//   - optType=1（发申请）会把申请投给节点账号本身 —— 审批人应该是拥有者，节点
//     自己不会批，这条申请只会变成没人能处理的僵尸申请。
// 节点类申请只能走 friend/sendRequest。
func TestFriendAddRejectsAnyOptTypeForAgentNodes(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, _ := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))
	// 目标确实是节点：走 isAgentNode 的测试接缝（真查询要 DB，这条门禁的正例
	// 必须在没有数据库的 CI 里也跑起来）。伪造 Scan 的接收者列表会随生成代码
	// 静默失效，所以用接缝而不是假 DBTX。
	handler.agentNodeLookup = func(context.Context, string) (bool, error) { return true, nil }
	const nodeRCID = testNodeRCID

	// 直加：403，且一条融云好友请求都不许发出去。
	sendWebCommand(t, handler, testRequesterUserID, "friend", "add", map[string]interface{}{
		"friendId": nodeRCID,
		"optType":  friendOptTypeDirect,
	})
	wantWebError(t, rec.last(t), 403, "use friend/sendRequest")
	if got := rec.callCount("/friend/add.json"); got != 0 {
		t.Errorf("/friend/add.json calls = %d, want 0 (a blocked direct add must not reach RongCloud)", got)
	}

	// 发申请（optType=1）指向节点同样 403：投给节点账号的申请没人能批。
	sendWebCommand(t, handler, testRequesterUserID, "friend", "add", map[string]interface{}{
		"friendId": nodeRCID,
		"optType":  friendOptTypeRequest,
	})
	wantWebError(t, rec.last(t), 403, "use friend/sendRequest")
	if got := rec.callCount("/friend/add.json"); got != 0 {
		t.Errorf("/friend/add.json calls = %d, want 0 (an application to the node itself has no approver)", got)
	}
}

// nodeLookupErrDB 让每次 SQL 都失败，模拟「DB 整体不可用」——用来钉住门禁
// fail-closed，不需要真库（也就不用 actorTestPool）。
type nodeLookupErrDB struct{ err error }

type friendErrRow struct{ err error }

func (r friendErrRow) Scan(...any) error { return r.err }

func (d nodeLookupErrDB) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, d.err
}

func (d nodeLookupErrDB) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, d.err
}

func (d nodeLookupErrDB) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return friendErrRow{err: d.err}
}

// nodeLookupEmptyDB 让每次查询都回「没有这一行」—— 不接真库就能验证「非节点目标
// 照常放行」（isAgentNode 把 ErrNoRows 当「不是节点」）。
type nodeLookupEmptyDB struct{}

func (nodeLookupEmptyDB) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (nodeLookupEmptyDB) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, nil
}

func (nodeLookupEmptyDB) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return friendErrRow{err: pgx.ErrNoRows}
}

// 门禁只拦节点：普通用户查不到节点行（ErrNoRows），optType=1 与缺省直加都照常放行，
// 老客户端依赖的缺省行为一点没变。
func TestFriendAddAllowsNonAgentTargets(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, _ := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))
	handler.queries = db.New(nodeLookupEmptyDB{})
	const humanID = "user-some-human"

	// optType=1：普通好友申请照常发出去。
	sendWebCommand(t, handler, testRequesterUserID, "friend", "add", map[string]interface{}{
		"friendId": humanID,
		"optType":  friendOptTypeRequest,
	})
	if reply := rec.last(t); reply["status"] != "success" {
		t.Fatalf("optType=1 to a non-node should succeed, got %#v", reply)
	}

	// 不传 optType：缺省仍是直加（老客户端的行为）。
	sendWebCommand(t, handler, testRequesterUserID, "friend", "add", map[string]interface{}{
		"friendId": humanID,
	})
	if reply := rec.last(t); reply["status"] != "success" {
		t.Fatalf("default optType to a non-node should succeed, got %#v", reply)
	}
	if optTypes := rec.recordedOptTypes(); strings.Join(optTypes, ",") != friendOptTypeRequest+","+friendOptTypeDirect {
		t.Errorf("optType = %v, want [%s %s]", optTypes, friendOptTypeRequest, friendOptTypeDirect)
	}
}

// 门禁的判定依据读不到时必须 fail-closed：DB 抖一下不能让 friend/add 重新打通
// （否则制造抖动就能绕过审批）。任何 optType 都一样 —— 两个分支都要查节点。
func TestFriendAddFailsClosedWhenTheNodeLookupErrors(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, _ := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))
	handler.queries = db.New(nodeLookupErrDB{err: errors.New("connection reset by peer")})

	// optType=2：回 503（不是 403，也不是放行），且一条融云好友请求都不许发。
	sendWebCommand(t, handler, testRequesterUserID, "friend", "add", map[string]interface{}{
		"friendId": testNodeRCID,
		"optType":  friendOptTypeDirect,
	})
	wantWebError(t, rec.last(t), 503, "friend service is unavailable")
	if got := rec.callCount("/friend/add.json"); got != 0 {
		t.Errorf("/friend/add.json calls = %d, want 0 when the node gate cannot be evaluated", got)
	}

	// optType=1 同样要查节点（节点一律拦），所以也 503 —— 不能因为「只是发申请」
	// 就放行，那等于给节点留一条绕过审批的缝。
	sendWebCommand(t, handler, testRequesterUserID, "friend", "add", map[string]interface{}{
		"friendId": testNodeRCID,
		"optType":  friendOptTypeRequest,
	})
	wantWebError(t, rec.last(t), 503, "friend service is unavailable")
	if got := rec.callCount("/friend/add.json"); got != 0 {
		t.Errorf("/friend/add.json calls = %d, want 0 for optType=1 as well", got)
	}
}
