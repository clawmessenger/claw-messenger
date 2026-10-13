package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// ---- 测试脚手架 --------------------------------------------------------------

// groupTestServer 记录所有发往融云的请求，供断言使用。
type groupTestServer struct {
	mu       sync.Mutex
	paths    []string
	posts    map[string][]map[string]string // path -> 每次请求的完整表单
	requests int

	// failPaths 里的路径统一回 {code:1000}，用来模拟融云侧失败。
	failPaths map[string]bool

	// membersPages 是 /group/user/query.json 依次返回的成员分页（元素为 id 列表），
	// 最后一项不带游标；用完后再查返回空页。用来验证服务端翻页与去重。
	membersPages [][]string
	membersCalls int
}

func newGroupTestServer(t *testing.T) (*groupTestServer, *httptest.Server) {
	t.Helper()
	rec := &groupTestServer{posts: make(map[string][]map[string]string), failPaths: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		form := make(map[string]string, len(r.PostForm))
		for key, values := range r.PostForm {
			// userId 是重复参数，用逗号连起来方便断言。
			form[key] = strings.Join(values, ",")
		}
		rec.mu.Lock()
		rec.paths = append(rec.paths, r.URL.Path)
		rec.posts[r.URL.Path] = append(rec.posts[r.URL.Path], form)
		rec.requests++
		fail := rec.failPaths[r.URL.Path]
		memberPage := []string(nil)
		memberNext := ""
		if r.URL.Path == "/group/user/query.json" {
			if rec.membersCalls < len(rec.membersPages) {
				memberPage = rec.membersPages[rec.membersCalls]
			}
			rec.membersCalls++
			if rec.membersCalls < len(rec.membersPages) {
				memberNext = fmt.Sprintf("cursor-%d", rec.membersCalls)
			}
		}
		rec.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if fail {
			fmt.Fprint(w, `{"code":1000,"errorMessage":"boom"}`)
			return
		}
		if r.URL.Path == "/message/private/publish.json" {
			fmt.Fprint(w, `{"code":200,"msgUID":"uid-1"}`)
			return
		}
		if r.URL.Path == "/group/user/query.json" {
			// 融云 v3 的形状：data.members[].id + data.next 游标。
			items := make([]string, 0, len(memberPage))
			for _, id := range memberPage {
				items = append(items, fmt.Sprintf(`{"id":%q}`, id))
			}
			fmt.Fprintf(w, `{"code":200,"data":{"members":[%s],"next":%q}}`,
				strings.Join(items, ","), memberNext)
			return
		}
		fmt.Fprint(w, `{"code":200}`)
	}))
	return rec, srv
}

func (s *groupTestServer) calls(path string) []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts[path]
}

func (s *groupTestServer) callCount(path string) int {
	return len(s.calls(path))
}

// onlyReply asserts the sole outbound call was the command_result reply, i.e.
// the command was rejected before touching the RongCloud group APIs.
func (s *groupTestServer) onlyReply(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	total, replies := s.requests, len(s.posts["/message/private/publish.json"])
	s.mu.Unlock()
	if total != 1 || replies != 1 {
		t.Fatalf("expected exactly one outbound call (the reply), got %d total / %d publishes", total, replies)
	}
}

// lastCommandResult 解析最后一条发给 web 的 command 私信内容。
func (s *groupTestServer) lastCommandResult(t *testing.T) map[string]interface{} {
	t.Helper()
	calls := s.calls("/message/private/publish.json")
	if len(calls) == 0 {
		t.Fatal("no private message was published")
	}
	raw := calls[len(calls)-1]["content"]
	if raw == "" {
		t.Fatal("published private message has no content")
	}
	var content map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &content); err != nil {
		t.Fatalf("command result is not JSON: %v (raw=%s)", err, raw)
	}
	return content
}

// groupMirrorStub 是一个纯内存的 GroupMirror，省去数据库。
type groupMirrorStub struct {
	mu sync.Mutex

	nextID      string
	allocateErr error

	saved   []GroupRecord
	saveErr error

	owners   map[string]string
	ownerErr error

	metaCalls []groupMetaCall
	metaErr   error

	fallback    string
	fallbackOK  bool
	fallbackErr error

	// 成员/资料类动作的状态。members 是 groupID -> userID -> role 的模拟镜像，
	// profiles 是 getInfo/getProfiles/getMyGroups 读的资料行。
	members  map[string]map[string]string
	profiles map[string]GroupProfile

	isMemberErr error
	addErr      error
	removeErr   error
	listErr     error
	listByUserErr error
	profilesErr error
	setOwnerErr error
	deleteErr   error

	added     []string
	removed   []string
	newOwners []string
	deleted   []string
}

type groupMetaCall struct {
	GroupID     string
	GroupName   string
	PortraitURI string
	Description string
	MaxRounds   *int
	Stopped     *bool
}

func newGroupMirrorStub() *groupMirrorStub {
	return &groupMirrorStub{
		nextID:   "100001",
		owners:   map[string]string{},
		members:  map[string]map[string]string{},
		profiles: map[string]GroupProfile{},
	}
}

// member 直接往模拟镜像里放一行成员，供测试铺前置状态。
func (s *groupMirrorStub) member(groupID, userID, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.members[groupID] == nil {
		s.members[groupID] = map[string]string{}
	}
	s.members[groupID][userID] = role
}

// profile 往模拟镜像里放一条群资料。
func (s *groupMirrorStub) profile(profile GroupProfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles[profile.GroupID] = profile
}

func (s *groupMirrorStub) mirror() *GroupMirror {
	return &GroupMirror{
		AllocateID: func(context.Context) (string, error) {
			if s.allocateErr != nil {
				return "", s.allocateErr
			}
			return s.nextID, nil
		},
		SaveGroup: func(_ context.Context, group GroupRecord) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.saveErr != nil {
				return s.saveErr
			}
			s.saved = append(s.saved, group)
			return nil
		},
		OwnerOf: func(_ context.Context, groupID string) (string, bool, error) {
			if s.ownerErr != nil {
				return "", false, s.ownerErr
			}
			owner, ok := s.owners[groupID]
			return owner, ok, nil
		},
		UpdateMeta: func(_ context.Context, groupID, groupName, portraitURI, description string, maxRounds *int, stopped *bool) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.metaErr != nil {
				return s.metaErr
			}
			s.metaCalls = append(s.metaCalls, groupMetaCall{
				GroupID: groupID, GroupName: groupName, PortraitURI: portraitURI,
				Description: description, MaxRounds: maxRounds, Stopped: stopped,
			})
			return nil
		},
		FallbackAgent: func(context.Context) (string, bool, error) {
			return s.fallback, s.fallbackOK, s.fallbackErr
		},
		IsMember: func(_ context.Context, groupID, userID string) (bool, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.isMemberErr != nil {
				return false, s.isMemberErr
			}
			_, ok := s.members[groupID][userID]
			return ok, nil
		},
		AddMembers: func(_ context.Context, groupID string, userIDs []string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.addErr != nil {
				return s.addErr
			}
			if s.members[groupID] == nil {
				s.members[groupID] = map[string]string{}
			}
			for _, userID := range userIDs {
				role := "member"
				if s.owners[groupID] == userID {
					role = "owner"
				}
				s.members[groupID][userID] = role
				s.added = append(s.added, groupID+"/"+userID)
			}
			return nil
		},
		RemoveMember: func(_ context.Context, groupID, userID string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.removeErr != nil {
				return s.removeErr
			}
			delete(s.members[groupID], userID)
			s.removed = append(s.removed, groupID+"/"+userID)
			return nil
		},
		ListMembers: func(_ context.Context, groupID string) ([]GroupMember, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.listErr != nil {
				return nil, s.listErr
			}
			// 顺序由 user id 排序保证确定，真实实现按 created_at 排。
			members := []GroupMember{}
			ids := make([]string, 0, len(s.members[groupID]))
			for userID := range s.members[groupID] {
				ids = append(ids, userID)
			}
			sort.Strings(ids)
			for _, userID := range ids {
				members = append(members, GroupMember{
					UserID: userID, Nickname: "昵称-" + userID,
					PortraitURI: "https://img/" + userID + ".png",
					Role:        s.members[groupID][userID],
				})
			}
			return members, nil
		},
		ListGroupsByUser: func(_ context.Context, userID string) ([]GroupProfile, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.listByUserErr != nil {
				return nil, s.listByUserErr
			}
			groups := []GroupProfile{}
			ids := make([]string, 0, len(s.profiles))
			for groupID := range s.profiles {
				ids = append(ids, groupID)
			}
			sort.Strings(ids)
			for _, groupID := range ids {
				if _, ok := s.members[groupID][userID]; ok {
					groups = append(groups, s.profiles[groupID])
				}
			}
			return groups, nil
		},
		GroupProfiles: func(_ context.Context, groupIDs []string) ([]GroupProfile, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.profilesErr != nil {
				return nil, s.profilesErr
			}
			profiles := []GroupProfile{}
			for _, groupID := range groupIDs {
				if profile, ok := s.profiles[groupID]; ok {
					profiles = append(profiles, profile)
				}
			}
			return profiles, nil
		},
		SetOwner: func(_ context.Context, groupID, newOwnerID string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.setOwnerErr != nil {
				return s.setOwnerErr
			}
			s.owners[groupID] = newOwnerID
			for userID := range s.members[groupID] {
				if userID == newOwnerID {
					s.members[groupID][userID] = "owner"
				} else {
					s.members[groupID][userID] = "member"
				}
			}
			s.newOwners = append(s.newOwners, groupID+"/"+newOwnerID)
			return nil
		},
		DeleteGroup: func(_ context.Context, groupID string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.deleteErr != nil {
				return s.deleteErr
			}
			delete(s.profiles, groupID)
			delete(s.members, groupID)
			delete(s.owners, groupID)
			s.deleted = append(s.deleted, groupID)
			return nil
		},
	}
}

func newGroupCommandHandler(t *testing.T, rec *groupTestServer, srv *httptest.Server, mirror *GroupMirror) *systemHandler {
	t.Helper()
	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	return newSystemHandler(client, "sys-node", testLogger(), nil, nil, nil, mirror, nil)
}

// ---- create ------------------------------------------------------------------

func TestGroupCreateHappyPath(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.fallback, stub.fallbackOK = "rc_node_machine1_opencode", true
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-7",
		Content: `{"request_id":"r1","service":"group","action":"create","params":{
			"groupName":"产品讨论","ownerId":"someone-else",
			"memberIds":["user-8","user-7"],"portraitUri":"https://img/x.png","openclawMaxRounds":30}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	creates := rec.calls("/group/create.json")
	if len(creates) != 1 {
		t.Fatalf("create calls = %d, want 1", len(creates))
	}
	if creates[0]["groupId"] != "100001" {
		t.Errorf("groupId = %q", creates[0]["groupId"])
	}
	if creates[0]["groupName"] != "产品讨论" {
		t.Errorf("groupName = %q", creates[0]["groupName"])
	}
	// 群主取消息发送者，而不是 payload 里的 ownerId；成员去重后补上兜底 agent。
	if got := creates[0]["userId"]; got != "user-7,user-8,rc_node_machine1_opencode" {
		t.Errorf("userId = %q, want owner first then deduped members then the fallback agent", got)
	}

	if len(stub.saved) != 1 {
		t.Fatalf("mirror saved %d groups, want 1", len(stub.saved))
	}
	saved := stub.saved[0]
	if saved.OwnerID != "user-7" {
		t.Errorf("mirror owner = %q, want the sender", saved.OwnerID)
	}
	if saved.MaxRounds != 30 {
		t.Errorf("mirror maxRounds = %d, want 30", saved.MaxRounds)
	}
	if len(saved.MemberIDs) != 3 {
		t.Errorf("mirror members = %v", saved.MemberIDs)
	}

	result := rec.lastCommandResult(t)
	if result["requestId"] != "r1" || result["status"] != "success" || result["msg_type"] != "command_result" {
		t.Fatalf("unexpected command result envelope: %#v", result)
	}
	data, ok := result["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("data is not an object: %#v", result["data"])
	}
	if data["groupId"] != "100001" || data["ownerId"] != "user-7" {
		t.Errorf("data = %#v", data)
	}
	if data["openclawMaxRounds"] != float64(30) {
		t.Errorf("openclawMaxRounds = %#v", data["openclawMaxRounds"])
	}
}

func TestGroupCreateSkipsFallbackWhenAnAgentIsSelected(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.fallback, stub.fallbackOK = "rc_node_machine1_opencode", true
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-7",
		Content:    `{"request_id":"r1","service":"group","action":"create","params":{"groupName":"G","memberIds":["rc_node_machine1_codex"]}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	creates := rec.calls("/group/create.json")
	if len(creates) != 1 {
		t.Fatalf("create calls = %d", len(creates))
	}
	if got := creates[0]["userId"]; got != "user-7,rc_node_machine1_codex" {
		t.Errorf("userId = %q, the selected agent must not be duplicated", got)
	}
}

func TestGroupCreateRequiresAName(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	handler := newGroupCommandHandler(t, rec, srv, newGroupMirrorStub().mirror())

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-7",
		Content:    `{"request_id":"r1","service":"group","action":"create","params":{"groupName":"   "}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	result := rec.lastCommandResult(t)
	if result["status"] != "error" || result["code"] != float64(400) {
		t.Errorf("expected a 400 error result, got %#v", result)
	}
}

func TestGroupCreateRejectsSelfSourcedCommand(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	handler := newGroupCommandHandler(t, rec, srv, newGroupMirrorStub().mirror())

	// 融云会把本端发出的 command 回灌 webhook，fromUserId 就是系统节点自身。
	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "sys-node",
		Content:    `{"request_id":"r1","service":"group","action":"create","params":{"groupName":"G"}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	if rec.requests != 0 {
		t.Fatalf("self-echo must be ignored, got %d RongCloud calls", rec.requests)
	}
}

func TestGroupCreateReportsRongCloudFailure(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	rec.failPaths["/group/create.json"] = true
	stub := newGroupMirrorStub()
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-7",
		Content:    `{"request_id":"r1","service":"group","action":"create","params":{"groupName":"G"}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	if len(stub.saved) != 0 {
		t.Fatal("mirror must not be written when RongCloud rejected the group")
	}
	result := rec.lastCommandResult(t)
	if result["status"] != "error" || result["code"] != float64(500) {
		t.Errorf("expected a 500 error result, got %#v", result)
	}
}

func TestGroupCreateKeepsGoingWhenTheMirrorFails(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.saveErr = fmt.Errorf("db down")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-7",
		Content:    `{"request_id":"r1","service":"group","action":"create","params":{"groupName":"G"}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	if rec.callCount("/group/create.json") != 1 {
		t.Fatal("the RongCloud group should still have been created")
	}
	if result := rec.lastCommandResult(t); result["status"] != "success" {
		t.Errorf("a mirror failure must not fail the command: %#v", result)
	}
}

func TestGroupCreateAcceptsTheWebCamelCaseEnvelope(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	handler := newGroupCommandHandler(t, rec, srv, newGroupMirrorStub().mirror())

	// clawmessenger-web 发的是 camelCase：{requestId, service, action, payload}。
	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-7",
		Content:    `{"requestId":"web-1","service":"group","action":"create","payload":{"groupName":"来自web","memberIds":["user-9"]}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	if rec.callCount("/group/create.json") != 1 {
		t.Fatal("a camelCase command must not be dropped")
	}
	result := rec.lastCommandResult(t)
	if result["requestId"] != "web-1" {
		t.Errorf("requestId = %#v", result["requestId"])
	}
}

// ---- update ------------------------------------------------------------------

func TestGroupUpdateByOwnerRefreshesRongCloud(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.owners["100001"] = "user-7"
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-7",
		Content:    `{"request_id":"r2","service":"group","action":"update","params":{"groupId":"100001","groupName":"新名字","portraitUri":"https://img/new.png","openclawMaxRounds":50,"openclawStopped":true}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	refreshes := rec.calls("/group/refresh.json")
	if len(refreshes) != 1 {
		t.Fatalf("refresh calls = %d, want 1", len(refreshes))
	}
	if refreshes[0]["groupName"] != "新名字" || refreshes[0]["portraitUri"] != "https://img/new.png" {
		t.Errorf("refresh form = %#v", refreshes[0])
	}
	if len(stub.metaCalls) != 1 {
		t.Fatalf("meta calls = %d, want 1", len(stub.metaCalls))
	}
	call := stub.metaCalls[0]
	if call.MaxRounds == nil || *call.MaxRounds != 50 {
		t.Errorf("max rounds = %#v", call.MaxRounds)
	}
	if call.Stopped == nil || !*call.Stopped {
		t.Errorf("stopped = %#v", call.Stopped)
	}
	if result := rec.lastCommandResult(t); result["status"] != "success" {
		t.Errorf("result = %#v", result)
	}
}

func TestGroupUpdateSkipsRefreshWhenOnlyHiddenSettingsChange(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.owners["100001"] = "user-7"
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-7",
		Content:    `{"request_id":"r2","service":"group","action":"update","params":{"groupId":"100001","openclawMaxRounds":15}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	if rec.callCount("/group/refresh.json") != 0 {
		t.Fatal("round limits are not a RongCloud group attribute")
	}
	if len(stub.metaCalls) != 1 {
		t.Fatalf("meta calls = %d", len(stub.metaCalls))
	}
}

func TestGroupUpdateRejectsNonOwner(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.owners["100001"] = "user-7"
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-99",
		Content:    `{"request_id":"r2","service":"group","action":"update","params":{"groupId":"100001","groupName":"劫持"}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	result := rec.lastCommandResult(t)
	if result["status"] != "error" || result["code"] != float64(403) {
		t.Errorf("expected 403, got %#v", result)
	}
}

func TestGroupUpdateUnknownGroup(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	handler := newGroupCommandHandler(t, rec, srv, newGroupMirrorStub().mirror())

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-7",
		Content:    `{"request_id":"r2","service":"group","action":"update","params":{"groupId":"404","groupName":"x"}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	result := rec.lastCommandResult(t)
	if result["status"] != "error" || result["code"] != float64(404) {
		t.Errorf("expected 404, got %#v", result)
	}
}

// ---- 通用 --------------------------------------------------------------------

func TestGroupCommandUnavailableWithoutMirror(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	handler := newGroupCommandHandler(t, rec, srv, nil)

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-7",
		Content:    `{"request_id":"r1","service":"group","action":"create","params":{"groupName":"G"}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	result := rec.lastCommandResult(t)
	if result["status"] != "error" || result["code"] != float64(503) {
		t.Errorf("expected 503, got %#v", result)
	}
}

func TestGroupCommandUnsupportedAction(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	handler := newGroupCommandHandler(t, rec, srv, newGroupMirrorStub().mirror())

	// 曾经这里用 dismiss 当「不支持的动作」的例子 —— dismiss 现在是实装的，改用
	// 一个真的不存在的动作，否则这条测试会因为假前提而失去意义。
	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-7",
		Content:    `{"request_id":"r1","service":"group","action":"noSuchAction","params":{"groupId":"1"}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	result := rec.lastCommandResult(t)
	if result["status"] != "error" || result["code"] != float64(400) {
		t.Errorf("expected 400, got %#v", result)
	}
	if message, _ := result["message"].(string); !strings.Contains(message, "noSuchAction") {
		t.Errorf("message should name the action, got %#v", result["message"])
	}
}

func TestGroupMaxRoundsClamping(t *testing.T) {
	cases := []struct {
		value interface{}
		want  int
	}{
		{nil, defaultGroupMaxRounds},
		{float64(0), groupMaxRoundsMin},
		{float64(-3), groupMaxRoundsMin},
		{float64(1000), groupMaxRoundsMax},
		{float64(42), 42},
		{"12", 12},
		{"nope", defaultGroupMaxRounds},
		{float64(1.5), defaultGroupMaxRounds},
	}
	for _, tc := range cases {
		if got := groupMaxRounds(tc.value); got != tc.want {
			t.Errorf("groupMaxRounds(%#v) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

func TestGetStringSliceShapes(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want []string
	}{
		{"array", []interface{}{"a", " b ", ""}, []string{"a", "b"}},
		{"string list", []string{"x", "y"}, []string{"x", "y"}},
		{"comma string", "p, q ,r", []string{"p", "q", "r"}},
		{"nil", nil, nil},
		{"number", 7, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := getStringSlice(map[string]interface{}{"k": tc.in}, "k")
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

func TestDedupeUserIDsKeepsOrder(t *testing.T) {
	got := dedupeUserIDs([]string{"a", " b ", "a", "", "c", "b"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// ---- 成员与资料动作 ----------------------------------------------------------
//
// 这一组动作此前全部落进 default 分支回 "unsupported group action"，也就是用户报的
// 「群聊添加成员报错」。下面既钉契约，也钉权限 —— 补接口时最容易顺手放宽的就是权限。

// groupCommand 拼一条最小编码的 group 命令（snake_case 信封）。
func groupCommand(action, sender, params string) NormalizedMessage {
	return NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: sender,
		Content: fmt.Sprintf(
			`{"request_id":"req-%s","service":"group","action":%q,"params":%s}`, action, action, params),
	}
}

func groupResultData(t *testing.T, rec *groupTestServer) interface{} {
	t.Helper()
	result := rec.lastCommandResult(t)
	if result["status"] != "success" {
		t.Fatalf("expected a success result, got %#v", result)
	}
	return result["data"]
}

// ---- addMember ---------------------------------------------------------------

func TestGroupAddMemberReachesRongCloudAndMirror(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	// 发送者是普通成员：旧后端语义是「群成员即可邀请」，不是只有群主。
	stub.member("100001", "user-7", "member")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := groupCommand("addMember", "user-7", `{"groupId":"100001","userIds":["user-8","user-9","user-8"]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	joins := rec.calls("/group/join.json")
	if len(joins) != 1 {
		t.Fatalf("join calls = %d, want 1", len(joins))
	}
	if joins[0]["groupId"] != "100001" {
		t.Errorf("groupId = %q", joins[0]["groupId"])
	}
	if joins[0]["userId"] != "user-8,user-9" {
		t.Errorf("userId = %q, duplicates must be dropped before touching the limit", joins[0]["userId"])
	}
	if len(stub.added) != 2 {
		t.Errorf("mirror added = %v, want the two new members", stub.added)
	}

	data, ok := groupResultData(t, rec).(map[string]interface{})
	if !ok {
		t.Fatalf("data is not an object: %#v", rec.lastCommandResult(t)["data"])
	}
	if data["addedCount"] != float64(2) {
		t.Errorf("addedCount = %#v", data["addedCount"])
	}
}

func TestGroupAddMemberRejectsOutsiders(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	// user-99 不在成员表里。
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := groupCommand("addMember", "user-99", `{"groupId":"100001","userIds":["user-8"]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	result := rec.lastCommandResult(t)
	if result["status"] != "error" || result["code"] != float64(403) {
		t.Errorf("expected 403, got %#v", result)
	}
}

func TestGroupAddMemberRequiresUserIDs(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.member("100001", "user-7", "member")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := groupCommand("addMember", "user-7", `{"groupId":"100001","userIds":[]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	if result := rec.lastCommandResult(t); result["code"] != float64(400) {
		t.Errorf("expected 400, got %#v", result)
	}
}

func TestGroupAddMemberKeepsTheMirrorCleanWhenRongCloudFails(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	rec.failPaths["/group/join.json"] = true
	stub := newGroupMirrorStub()
	stub.member("100001", "user-7", "member")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := groupCommand("addMember", "user-7", `{"groupId":"100001","userIds":["user-8"]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	// 融云拒绝就不能改本地，否则本端以为加进去了、别人读到的还是旧名单。
	if len(stub.added) != 0 {
		t.Fatal("the mirror must not record members RongCloud rejected")
	}
	if result := rec.lastCommandResult(t); result["code"] != float64(500) {
		t.Errorf("expected 500, got %#v", result)
	}
}

// ---- removeMember ------------------------------------------------------------

func TestGroupRemoveMemberByOwnerAcceptsBothFieldNames(t *testing.T) {
	for _, params := range []string{
		`{"groupId":"100001","userId":"user-8"}`,
		`{"groupId":"100001","memberId":"user-8"}`,
	} {
		rec, srv := newGroupTestServer(t)
		// t.Cleanup 而不是 defer：循环里的 t.Fatalf 会 Goexit，defer 就漏掉了。
		t.Cleanup(srv.Close)
		stub := newGroupMirrorStub()
		stub.owners["100001"] = "user-7"
		stub.member("100001", "user-8", "member")
		handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

		if err := handler.handleCommand(context.Background(), groupCommand("removeMember", "user-7", params)); err != nil {
			t.Fatalf("handleCommand: %v", err)
		}
		quits := rec.calls("/group/quit.json")
		if len(quits) != 1 || quits[0]["userId"] != "user-8" {
			t.Fatalf("quit form = %#v", quits)
		}
		if len(stub.removed) != 1 {
			t.Fatalf("mirror removed = %v", stub.removed)
		}
		if result := rec.lastCommandResult(t); result["status"] != "success" {
			t.Fatalf("result = %#v", result)
		}
	}
}

func TestGroupRemoveMemberRejectsNonOwner(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.owners["100001"] = "user-7"
	stub.member("100001", "user-8", "member")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	// user-8 是成员但不是群主。
	msg := groupCommand("removeMember", "user-8", `{"groupId":"100001","userId":"user-7"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	if result := rec.lastCommandResult(t); result["code"] != float64(403) {
		t.Errorf("expected 403, got %#v", result)
	}
}

func TestGroupRemoveMemberRefusesToDropTheOwner(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.owners["100001"] = "user-7"
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	// 群主要走 quit/transfer，直接移除会让群变成无主群。
	msg := groupCommand("removeMember", "user-7", `{"groupId":"100001","userId":"user-7"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	if result := rec.lastCommandResult(t); result["code"] != float64(400) {
		t.Errorf("expected 400, got %#v", result)
	}
}

func TestGroupRemoveMemberUnknownGroup(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	handler := newGroupCommandHandler(t, rec, srv, newGroupMirrorStub().mirror())

	msg := groupCommand("removeMember", "user-7", `{"groupId":"404","userId":"user-8"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	if result := rec.lastCommandResult(t); result["code"] != float64(404) {
		t.Errorf("expected 404, got %#v", result)
	}
}

// ---- getMembers --------------------------------------------------------------

func memberList(t *testing.T, data interface{}) []map[string]interface{} {
	t.Helper()
	list, ok := data.([]interface{})
	if !ok {
		t.Fatalf("member data is not an array: %#v", data)
	}
	out := make([]map[string]interface{}, 0, len(list))
	for _, item := range list {
		member, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("member entry is not an object: %#v", item)
		}
		out = append(out, member)
	}
	return out
}

func TestGroupGetMembersMergesRongCloudWithMirrorProfiles(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	rec.membersPages = [][]string{{"user-7", "user-8"}}
	stub := newGroupMirrorStub()
	stub.member("100001", "user-7", "owner")
	stub.member("100001", "user-8", "member")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	if err := handler.handleCommand(context.Background(),
		groupCommand("getMembers", "user-7", `{"groupId":"100001"}`)); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	members := memberList(t, groupResultData(t, rec))
	if len(members) != 2 {
		t.Fatalf("members = %#v", members)
	}
	if members[0]["userId"] != "user-7" || members[0]["role"] != "owner" {
		t.Errorf("first member = %#v", members[0])
	}
	// 昵称/头像来自本地镜像的补充，C 端 @ 列表靠它显示人名。
	if members[0]["nickname"] != "昵称-user-7" {
		t.Errorf("nickname = %#v", members[0]["nickname"])
	}
	if members[0]["portraitUri"] != "https://img/user-7.png" {
		t.Errorf("portraitUri = %#v", members[0]["portraitUri"])
	}
	if members[0]["groupNickname"] != "" {
		t.Errorf("groupNickname = %#v", members[0]["groupNickname"])
	}
}

func TestGroupGetMembersFallsBackToTheMirrorWhenRongCloudFails(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	rec.failPaths["/group/user/query.json"] = true
	stub := newGroupMirrorStub()
	stub.member("100001", "user-3", "member")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	if err := handler.handleCommand(context.Background(),
		groupCommand("getMembers", "user-7", `{"groupId":"100001"}`)); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	members := memberList(t, groupResultData(t, rec))
	if len(members) != 1 || members[0]["userId"] != "user-3" {
		t.Fatalf("members = %#v", members)
	}
}

func TestGroupGetMembersPaginatesAndDeduplicates(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	// 第二页重复了 b：融云在成员变动时会重复返回，不能因此列出两个人。
	rec.membersPages = [][]string{{"a", "b"}, {"b", "c"}}
	handler := newGroupCommandHandler(t, rec, srv, newGroupMirrorStub().mirror())

	if err := handler.handleCommand(context.Background(),
		groupCommand("getMembers", "user-7", `{"groupId":"100001"}`)); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	members := memberList(t, groupResultData(t, rec))
	if len(members) != 3 {
		t.Fatalf("members = %#v, want a/b/c once each", members)
	}
	queries := rec.calls("/group/user/query.json")
	if len(queries) != 2 {
		t.Fatalf("query calls = %d, want 2 pages", len(queries))
	}
	if queries[1]["nextId"] != "cursor-1" {
		t.Errorf("second page must carry the cursor, got %#v", queries[1])
	}
}

func TestGroupGetMembersReturnsAnEmptyArrayForAnEmptyGroup(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	handler := newGroupCommandHandler(t, rec, srv, newGroupMirrorStub().mirror())

	if err := handler.handleCommand(context.Background(),
		groupCommand("getMembers", "user-7", `{"groupId":"100001"}`)); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	// 空群是正常结果，不是错误；且必须是 []，返回 null 会让前端 Array.isArray 落空。
	members := memberList(t, groupResultData(t, rec))
	if len(members) != 0 {
		t.Fatalf("members = %#v", members)
	}
}

func TestGroupGetMembersFallsBackToTheMirrorOnAnEmptyRongCloudAnswer(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	// 融云成功但零成员（刚建好的群有可见性延迟）：镜像此刻才是对的。
	stub := newGroupMirrorStub()
	stub.member("100001", "user-7", "owner")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	if err := handler.handleCommand(context.Background(),
		groupCommand("getMembers", "user-7", `{"groupId":"100001"}`)); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	members := memberList(t, groupResultData(t, rec))
	if len(members) != 1 || members[0]["userId"] != "user-7" {
		t.Fatalf("members = %#v, want the mirrored owner", members)
	}
}

func TestGroupGetMembersRequiresGroupID(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	handler := newGroupCommandHandler(t, rec, srv, newGroupMirrorStub().mirror())

	if err := handler.handleCommand(context.Background(),
		groupCommand("getMembers", "user-7", `{}`)); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	if result := rec.lastCommandResult(t); result["code"] != float64(400) {
		t.Errorf("expected 400, got %#v", result)
	}
}

// ---- getMyGroups / getProfiles / getInfo -------------------------------------

func TestGroupGetMyGroupsUsesTheSenderNotThePayload(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.profile(GroupProfile{GroupID: "100001", GroupName: "产品讨论", OwnerID: "user-7"})
	stub.profile(GroupProfile{GroupID: "100002", GroupName: "别人的群", OwnerID: "user-9"})
	stub.member("100001", "user-7", "owner")
	stub.member("100002", "user-9", "owner")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	// payload 里冒充别人必须无效。
	msg := groupCommand("getMyGroups", "user-7", `{"userId":"user-9"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	data, ok := groupResultData(t, rec).([]interface{})
	if !ok {
		t.Fatalf("data is not an array: %#v", rec.lastCommandResult(t)["data"])
	}
	if len(data) != 1 {
		t.Fatalf("groups = %#v, want only the caller's group", data)
	}
	group := data[0].(map[string]interface{})
	if group["groupId"] != "100001" || group["groupName"] != "产品讨论" {
		t.Errorf("group = %#v", group)
	}
	// web / 移动端各自读不同的字段名，两个别名都要在。
	if group["name"] != "产品讨论" || group["avatar"] != "" {
		t.Errorf("aliases = %#v", group)
	}
	if group["openclawMaxRounds"] != float64(defaultGroupRounds) {
		t.Errorf("openclawMaxRounds = %#v", group["openclawMaxRounds"])
	}
}

func TestGroupGetProfilesSkipsUnknownIDs(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.profile(GroupProfile{GroupID: "100001", GroupName: "G", MaxRounds: 30, Stopped: true})
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := groupCommand("getProfiles", "user-7", `{"groupIds":["100001","404"]}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	data, ok := groupResultData(t, rec).([]interface{})
	if !ok {
		t.Fatalf("data is not an array: %#v", rec.lastCommandResult(t)["data"])
	}
	if len(data) != 1 {
		t.Fatalf("profiles = %#v, unknown ids must be skipped", data)
	}
	group := data[0].(map[string]interface{})
	if group["openclawMaxRounds"] != float64(30) || group["openclawStopped"] != true {
		t.Errorf("profile = %#v", group)
	}
}

func TestGroupGetProfilesAcceptsASingleGroupID(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.profile(GroupProfile{GroupID: "100001", GroupName: "G"})
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := groupCommand("getProfiles", "user-7", `{"groupId":"100001"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	if data, _ := groupResultData(t, rec).([]interface{}); len(data) != 1 {
		t.Fatalf("profiles = %#v", data)
	}
}

func TestGroupGetInfoUnknownGroupIs404(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	handler := newGroupCommandHandler(t, rec, srv, newGroupMirrorStub().mirror())

	if err := handler.handleCommand(context.Background(),
		groupCommand("getInfo", "user-7", `{"groupId":"404"}`)); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	if result := rec.lastCommandResult(t); result["code"] != float64(404) {
		t.Errorf("expected 404, got %#v", result)
	}
}

func TestGroupGetInfoReturnsTheProfileObject(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.profile(GroupProfile{
		GroupID: "100001", GroupName: "产品讨论", OwnerID: "user-7",
		PortraitURI: "https://img/g.png", Description: "说明", MemberCount: 3, MaxRounds: 25,
	})
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	if err := handler.handleCommand(context.Background(),
		groupCommand("getInfo", "user-7", `{"groupId":"100001"}`)); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	group, ok := groupResultData(t, rec).(map[string]interface{})
	if !ok {
		t.Fatalf("data is not an object: %#v", rec.lastCommandResult(t)["data"])
	}
	if group["ownerId"] != "user-7" || group["memberCount"] != float64(3) {
		t.Errorf("group = %#v", group)
	}
	if group["portraitUri"] != "https://img/g.png" || group["description"] != "说明" {
		t.Errorf("group = %#v", group)
	}
}

// ---- transfer / quit / dismiss -----------------------------------------------

func TestGroupTransferRequiresTheOwner(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.owners["100001"] = "user-7"
	stub.member("100001", "user-8", "member")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := groupCommand("transfer", "user-8", `{"groupId":"100001","newOwnerId":"user-8"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	if result := rec.lastCommandResult(t); result["code"] != float64(403) {
		t.Errorf("expected 403, got %#v", result)
	}
}

func TestGroupTransferRejectsANonMemberTarget(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.owners["100001"] = "user-7"
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := groupCommand("transfer", "user-7", `{"groupId":"100001","newOwnerId":"user-99"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	if result := rec.lastCommandResult(t); result["code"] != float64(400) {
		t.Errorf("expected 400, got %#v", result)
	}
}

func TestGroupTransferHappyPath(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.owners["100001"] = "user-7"
	stub.member("100001", "user-8", "member")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := groupCommand("transfer", "user-7", `{"groupId":"100001","newOwnerId":"user-8"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	transfers := rec.calls("/entrust/group/transfer/owner.json")
	if len(transfers) != 1 {
		t.Fatalf("transfer calls = %d, want 1", len(transfers))
	}
	if transfers[0]["groupId"] != "100001" || transfers[0]["newOwnerId"] != "user-8" {
		t.Errorf("transfer form = %#v", transfers[0])
	}
	if len(stub.newOwners) != 1 || stub.owners["100001"] != "user-8" {
		t.Errorf("mirror owner = %#v", stub.owners)
	}
}

func TestGroupQuitRefusesTheOwner(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.owners["100001"] = "user-7"
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := groupCommand("quit", "user-7", `{"groupId":"100001"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	if result := rec.lastCommandResult(t); result["code"] != float64(400) {
		t.Errorf("expected 400, got %#v", result)
	}
}

func TestGroupQuitRemovesTheSenderOnly(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.owners["100001"] = "user-7"
	stub.member("100001", "user-8", "member")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	// payload 里塞别人也不作数：退群的人永远是发送者本人。
	msg := groupCommand("quit", "user-8", `{"groupId":"100001","userId":"user-7"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	quits := rec.calls("/group/quit.json")
	if len(quits) != 1 || quits[0]["userId"] != "user-8" {
		t.Fatalf("quit form = %#v", quits)
	}
	if len(stub.removed) != 1 || stub.removed[0] != "100001/user-8" {
		t.Errorf("mirror removed = %#v", stub.removed)
	}
}

func TestGroupDismissRequiresTheOwner(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.owners["100001"] = "user-7"
	stub.member("100001", "user-8", "member")
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := groupCommand("dismiss", "user-8", `{"groupId":"100001"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	if result := rec.lastCommandResult(t); result["code"] != float64(403) {
		t.Errorf("expected 403, got %#v", result)
	}
}

func TestGroupDismissByOwnerClearsLocalAndRongCloud(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	stub := newGroupMirrorStub()
	stub.owners["100001"] = "user-7"
	stub.profile(GroupProfile{GroupID: "100001", GroupName: "G"})
	handler := newGroupCommandHandler(t, rec, srv, stub.mirror())

	msg := groupCommand("dismiss", "user-7", `{"groupId":"100001"}`)
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	dismisses := rec.calls("/group/dismiss.json")
	if len(dismisses) != 1 || dismisses[0]["groupId"] != "100001" {
		t.Fatalf("dismiss form = %#v", dismisses)
	}
	if dismisses[0]["memberId"] != "user-7" {
		t.Errorf("dismiss memberId = %q, want the owner", dismisses[0]["memberId"])
	}
	if len(stub.deleted) != 1 {
		t.Errorf("mirror deleted = %#v", stub.deleted)
	}
}

func TestGroupDismissUnknownGroup(t *testing.T) {
	rec, srv := newGroupTestServer(t)
	defer srv.Close()
	handler := newGroupCommandHandler(t, rec, srv, newGroupMirrorStub().mirror())

	if err := handler.handleCommand(context.Background(),
		groupCommand("dismiss", "user-7", `{"groupId":"404"}`)); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	rec.onlyReply(t)
	if result := rec.lastCommandResult(t); result["code"] != float64(404) {
		t.Errorf("expected 404, got %#v", result)
	}
}

// ---- 解析辅助 ----------------------------------------------------------------

func TestParseGroupMemberPageShapes(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantIDs    []string
		wantCursor string
	}{
		{
			name:       "v3 data.members",
			body:       `{"code":200,"data":{"members":[{"id":"u1"},{"id":"u2"}],"next":"c1"}}`,
			wantIDs:    []string{"u1", "u2"},
			wantCursor: "c1",
		},
		{
			name:    "data as a bare array",
			body:    `{"code":200,"data":["u1","u2"]}`,
			wantIDs: []string{"u1", "u2"},
		},
		{
			name:       "top level members",
			body:       `{"code":200,"members":[{"userId":"u1"}],"nextId":"c2"}`,
			wantIDs:    []string{"u1"},
			wantCursor: "c2",
		},
		{
			name:    "numeric ids",
			body:    `{"code":200,"users":[{"id":100001}]}`,
			wantIDs: []string{"100001"},
		},
		{name: "empty", body: `{"code":200}`, wantIDs: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]interface{}
			if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			ids, cursor := parseGroupMemberPage(body)
			if len(ids) != len(tc.wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
			for i := range ids {
				if ids[i] != tc.wantIDs[i] {
					t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
				}
			}
			if cursor != tc.wantCursor {
				t.Fatalf("cursor = %q, want %q", cursor, tc.wantCursor)
			}
		})
	}
}
