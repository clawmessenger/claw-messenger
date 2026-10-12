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
	return &groupMirrorStub{nextID: "100001", owners: map[string]string{}}
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
	}
}

func newGroupCommandHandler(t *testing.T, rec *groupTestServer, srv *httptest.Server, mirror *GroupMirror) *systemHandler {
	t.Helper()
	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	return newSystemHandler(client, "sys-node", testLogger(), nil, nil, nil, mirror)
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

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-7",
		Content:    `{"request_id":"r1","service":"group","action":"dismiss","params":{"groupId":"1"}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	result := rec.lastCommandResult(t)
	if result["status"] != "error" || result["code"] != float64(400) {
		t.Errorf("expected 400, got %#v", result)
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
