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

// webReplyServer 记录处理器发出的私信内容，用来断言回包信封的确切形状。
type webReplyServer struct {
	mu       sync.Mutex
	contents []map[string]interface{}
}

func newWebReplyServer(t *testing.T) (*webReplyServer, *httptest.Server) {
	t.Helper()
	rec := &webReplyServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path == "/message/private/publish.json" {
			var content map[string]interface{}
			if err := json.Unmarshal([]byte(r.PostForm.Get("content")), &content); err == nil {
				rec.mu.Lock()
				rec.contents = append(rec.contents, content)
				rec.mu.Unlock()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200,"msgUID":"uid-1"}`)
	}))
	return rec, srv
}

func (s *webReplyServer) last(t *testing.T) map[string]interface{} {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.contents) == 0 {
		t.Fatal("no command_result was published")
	}
	return s.contents[len(s.contents)-1]
}

func testHandler(t *testing.T, srv *httptest.Server, registry *DiscussionRegistry) *systemHandler {
	t.Helper()
	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	return newSystemHandler(client, "sys_node", testLogger(), nil, registry, nil, nil, nil, nil)
}

// --- 信封方言 ----------------------------------------------------------------

func TestCommandContentRecordsTheEnvelopeDialect(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"web camelCase", `{"requestId":"r","service":"chatroom","action":"join","payload":{"chatroomId":"c"}}`, true},
		{"device snake_case", `{"request_id":"r","service":"chatroom","action":"join","params":{"chatroom_id":"c"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cmd CommandContent
			if err := json.Unmarshal([]byte(tc.raw), &cmd); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if cmd.WebEnvelope != tc.want {
				t.Errorf("WebEnvelope = %v, want %v", cmd.WebEnvelope, tc.want)
			}
		})
	}
}

func TestGetStringAnyReadsBothSpellings(t *testing.T) {
	if got := getStringAny(map[string]interface{}{"chatroom_id": "snake", "chatroomId": "camel"}, "chatroom_id", "chatroomId"); got != "snake" {
		t.Errorf("snake_case should win when both present: %q", got)
	}
	if got := getStringAny(map[string]interface{}{"chatroomId": "camel"}, "chatroom_id", "chatroomId"); got != "camel" {
		t.Errorf("camelCase fallback: %q", got)
	}
	if got := getStringAny(map[string]interface{}{}, "chatroom_id", "chatroomId"); got != "" {
		t.Errorf("missing key should be empty, got %q", got)
	}
}

// --- 回包形状 ----------------------------------------------------------------

// The web's handleSystemResponse resolves a request only from a flat object that
// carries a string `status`; the device CLI instead reads {request_id, payload}.
// A chatroom request from the web must therefore come back web-shaped, or it
// hangs until timeout.
func TestChatroomCommandRepliesInTheWebShapeForTheWeb(t *testing.T) {
	rec, srv := newWebReplyServer(t)
	defer srv.Close()
	handler := testHandler(t, srv, nil)

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "web-user",
		Content:    `{"requestId":"web-1","service":"chatroom","action":"join","payload":{"chatroomId":"c1"}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	result := rec.last(t)
	if result["requestId"] != "web-1" || result["service"] != "chatroom" || result["action"] != "join" {
		t.Errorf("web reply must echo requestId/service/action: %#v", result)
	}
	// A missing string status is exactly what made the web drop the reply.
	if _, ok := result["status"].(string); !ok {
		t.Fatalf("web reply must carry a string status: %#v", result)
	}
	if _, ok := result["code"].(float64); !ok {
		t.Errorf("web reply must carry a numeric code: %#v", result)
	}
	if result["msg_type"] != "command_result" {
		t.Errorf("msg_type = %#v, want command_result", result["msg_type"])
	}
}

func TestChatroomCommandKeepsTheDeviceShapeForTheCLI(t *testing.T) {
	rec, srv := newWebReplyServer(t)
	defer srv.Close()
	handler := testHandler(t, srv, nil)

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "cli-node",
		Content:    `{"request_id":"dev-1","service":"chatroom","action":"join","params":{"chatroom_id":"c1"}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	result := rec.last(t)
	if _, hasStatus := result["status"]; hasStatus {
		t.Errorf("device reply must not carry a top-level status: %#v", result)
	}
	if result["request_id"] != "dev-1" {
		t.Errorf("device reply keeps request_id: %#v", result)
	}
	payload, ok := result["payload"].(map[string]interface{})
	if !ok || payload["ok"] != false {
		t.Errorf("device reply payload = %#v", result["payload"])
	}
}

// --- 404 契约 ----------------------------------------------------------------

// Mirrors clawmessenger-web isLegacyChatroomActionError: code 404 AND the message
// containing both "chatroom/<action>" and "未知". Reword it and join/leave/
// invite/destroy stop degrading silently during a rolling deploy.
func TestUnknownActionMessageSatisfiesTheWebLegacyPredicate(t *testing.T) {
	for _, action := range []string{"join", "leave", "invite", "destroy"} {
		message := unknownServiceOrActionMessage(CommandContent{Service: "chatroom", Action: action})
		if !strings.Contains(message, "chatroom/"+action) {
			t.Errorf("action %q: message %q lacks chatroom/%s", action, message, action)
		}
		if !strings.Contains(message, "未知") {
			t.Errorf("action %q: message %q lacks 未知", action, message)
		}
	}
}

func TestUnknownDiscussionActionReplies404InTheWebShape(t *testing.T) {
	rec, srv := newWebReplyServer(t)
	defer srv.Close()
	handler := testHandler(t, srv, NewDiscussionRegistry())

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "web-user",
		Content:    `{"requestId":"web-2","service":"discussion","action":"unknownAction","payload":{}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	result := rec.last(t)
	if result["code"] != float64(404) || result["status"] != "error" {
		t.Errorf("expected a 404 error reply, got %#v", result)
	}
	message, _ := result["message"].(string)
	if !strings.Contains(message, "discussion/unknownAction") || !strings.Contains(message, "未知") {
		t.Errorf("message %q must satisfy the web's legacy-404 predicate", message)
	}
}

func TestRegisteredRoundtableActionsReturn503WhenRuntimeUnavailable(t *testing.T) {
	rec, srv := newWebReplyServer(t)
	defer srv.Close()
	handler := testHandler(t, srv, NewDiscussionRegistry())

	for _, action := range []string{"recommendRoles", "createRoundtable", "resumeRoundtable"} {
		msg := NormalizedMessage{
			ObjectName: objectNameCommand,
			FromUserID: "web-user",
			Content:    fmt.Sprintf(`{"requestId":"web-%s","service":"discussion","action":"%s","payload":{}}`, action, action),
		}
		if err := handler.handleCommand(context.Background(), msg); err != nil {
			t.Fatalf("handleCommand %s: %v", action, err)
		}

		result := rec.last(t)
		if result["code"] != float64(503) || result["status"] != "error" {
			t.Errorf("%s: expected a 503 error reply, got %#v", action, result)
		}
		message, _ := result["message"].(string)
		if message != "discussion runtime not available" {
			t.Errorf("%s: message %q should indicate runtime unavailable", action, message)
		}
	}
}

func TestUnknownServiceRepliesWithTheLegacyContract(t *testing.T) {
	rec, srv := newWebReplyServer(t)
	defer srv.Close()
	handler := testHandler(t, srv, nil)

	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "web-user",
		Content:    `{"requestId":"web-3","service":"bogus","action":"doIt","payload":{}}`,
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}

	result := rec.last(t)
	if result["code"] != float64(404) {
		t.Errorf("unknown service should be 404, got %#v", result)
	}
	if message, _ := result["message"].(string); !strings.Contains(message, "bogus/doIt") {
		t.Errorf("message %q should name the service/action", message)
	}
}
