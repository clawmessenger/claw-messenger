package rongcloud

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseRecommendedRoles(t *testing.T) {
	valid := []interface{}{
		map[string]interface{}{"role_name": "主持人", "role_prompt": "引导讨论", "speaking_order": float64(0)},
		map[string]interface{}{"role_name": "质疑者", "role_prompt": "提出反例", "speaking_order": float64(1)},
	}

	t.Run("accepts a well-formed answer and sorts by speaking order", func(t *testing.T) {
		shuffled := []interface{}{
			map[string]interface{}{"role_name": "B", "role_prompt": "p", "speaking_order": "1"},
			map[string]interface{}{"role_name": "A", "role_prompt": "p", "speaking_order": float64(0)},
		}
		roles, err := parseRecommendedRoles(shuffled, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(roles) != 2 || roles[0]["role_name"] != "A" || roles[1]["role_name"] != "B" {
			t.Fatalf("roles not normalised/sorted: %#v", roles)
		}
		if roles[0]["speaking_order"] != 0 {
			t.Fatalf("speaking_order not coerced to int: %#v", roles[0])
		}
	})

	t.Run("keeps a CJK role name inside the rune budget", func(t *testing.T) {
		name := strings.Repeat("角", recommendRolesMaxName)
		roles, err := parseRecommendedRoles([]interface{}{
			map[string]interface{}{"role_name": name, "role_prompt": "p", "speaking_order": float64(0)},
		}, 1)
		if err != nil {
			t.Fatalf("a name of exactly %d runes must pass: %v", recommendRolesMaxName, err)
		}
		if roles[0]["role_name"] != name {
			t.Fatalf("name not preserved")
		}

		tooLong := strings.Repeat("角", recommendRolesMaxName+1)
		if _, err := parseRecommendedRoles([]interface{}{
			map[string]interface{}{"role_name": tooLong, "role_prompt": "p", "speaking_order": float64(0)},
		}, 1); err == nil {
			t.Fatal("expected an error for an over-long name")
		}
	})

	reject := []struct {
		name     string
		raw      interface{}
		maxRoles int
	}{
		{"nil", nil, 8},
		{"empty list", []interface{}{}, 8},
		{"too many roles", []interface{}{valid[0], valid[1]}, 1},
		{"not a list", map[string]interface{}{"role_name": "x"}, 8},
		{"duplicate name", []interface{}{valid[0], map[string]interface{}{"role_name": "主持人", "role_prompt": "p", "speaking_order": float64(1)}}, 8},
		{"blank name", []interface{}{map[string]interface{}{"role_name": "  ", "role_prompt": "p", "speaking_order": float64(0)}}, 8},
		{"blank prompt", []interface{}{map[string]interface{}{"role_name": "A", "role_prompt": " ", "speaking_order": float64(0)}}, 8},
		{"order gap", []interface{}{map[string]interface{}{"role_name": "A", "role_prompt": "p", "speaking_order": float64(3)}}, 8},
		{"order duplicate", []interface{}{valid[0], map[string]interface{}{"role_name": "B", "role_prompt": "p", "speaking_order": float64(0)}}, 8},
		{"order not an int", []interface{}{map[string]interface{}{"role_name": "A", "role_prompt": "p", "speaking_order": "first"}}, 8},
		{"role not an object", []interface{}{"nope"}, 8},
		{"prompt not text", []interface{}{map[string]interface{}{"role_name": "A", "role_prompt": 7, "speaking_order": float64(0)}}, 8},
		{"non integral number", []interface{}{map[string]interface{}{"role_name": "A", "role_prompt": "p", "speaking_order": float64(1.5)}}, 8},
	}
	for _, tc := range reject {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if _, err := parseRecommendedRoles(tc.raw, tc.maxRoles); err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
		})
	}
}

func TestParseRecommendedRolesDeviceAssignment(t *testing.T) {
	t.Run("passes through a usable node_id and model", func(t *testing.T) {
		roles, err := parseRecommendedRoles([]interface{}{
			map[string]interface{}{
				"role_name": "主持人", "role_prompt": "引导", "speaking_order": float64(0),
				"node_id": "node_abc", "model": " openai/gpt-4o ",
			},
		}, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if roles[0]["node_id"] != "node_abc" || roles[0]["model"] != "openai/gpt-4o" {
			t.Fatalf("assignment not carried: %#v", roles[0])
		}
	})

	t.Run("drops an unusable node_id instead of failing the whole answer", func(t *testing.T) {
		// 一台设备名不合法不值得把整份推荐判死：丢给 assignRoleNodes 重新分配。
		roles, err := parseRecommendedRoles([]interface{}{
			map[string]interface{}{
				"role_name": "主持人", "role_prompt": "引导", "speaking_order": float64(0),
				"node_id": "   ", "model": "",
			},
		}, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := roles[0]["node_id"]; ok {
			t.Fatalf("blank node_id must be dropped: %#v", roles[0])
		}
		if _, ok := roles[0]["model"]; ok {
			t.Fatalf("blank model must be dropped: %#v", roles[0])
		}
	})

	t.Run("an old device answer stays byte-identical", func(t *testing.T) {
		roles, err := parseRecommendedRoles([]interface{}{
			map[string]interface{}{"role_name": "A", "role_prompt": "p", "speaking_order": float64(0)},
		}, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(roles[0]) != 3 {
			t.Fatalf("no assignment keys may be invented: %#v", roles[0])
		}
	})
}

// TestNodeRequestBrokerCarriesDeviceAssignment 覆盖 C 端读到的回包形状：设备端给出
// 的 node_id/model 必须出现在推荐里，否则前端会因「角色没有设备」把整份推荐判无效。
func TestNodeRequestBrokerCarriesDeviceAssignment(t *testing.T) {
	broker, _, _ := startBroker(t)
	host := NodeRequestHost{AIUserID: "rc_node_host", TimeoutSeconds: 30}
	requestID, code := broker.StartRecommendation(context.Background(), host, "t", "", 2)
	if code != "" {
		t.Fatalf("start failed: %s", code)
	}
	answer := `{"request_id":"` + requestID + `","msg_type":"command_result","payload":{"roles":[` +
		`{"role_name":"A","role_prompt":"p","speaking_order":0,"node_id":"node_a","model":"p/m"},` +
		`{"role_name":"B","role_prompt":"p","speaking_order":1,"node_id":"node_b"}]}}`
	broker.AcceptResponse("rc_node_host", answer)

	payload, code := broker.AwaitRecommendation(context.Background(), requestID)
	if code != "" {
		t.Fatalf("await failed: %s", code)
	}
	roles, _ := payload["roles"].([]map[string]interface{})
	if len(roles) != 2 || roles[0]["node_id"] != "node_a" || roles[0]["model"] != "p/m" {
		t.Fatalf("role 0: %#v", roles)
	}
	if roles[1]["node_id"] != "node_b" {
		t.Fatalf("role 1: %#v", roles[1])
	}
	if _, ok := roles[1]["model"]; ok {
		t.Fatalf("an absent model must not be invented: %#v", roles[1])
	}
}

func TestNodeRequestBrokerOmitsAssignmentForAdminPreview(t *testing.T) {
	broker, _, _ := startBroker(t)
	requestID, _ := broker.StartRecommendation(context.Background(), NodeRequestHost{AIUserID: "rc_node_host", TimeoutSeconds: 30}, "t", "", 1)

	broker.AcceptResponse("rc_node_host", `{"request_id":"`+requestID+`","msg_type":"command_result","payload":{"roles":[`+
		`{"role_name":"A","role_prompt":"p","speaking_order":0}]}}`)
	payload, code := broker.AwaitRecommendation(context.Background(), requestID)
	if code != "" {
		t.Fatalf("await failed: %s", code)
	}
	roles, _ := payload["roles"].([]map[string]interface{})
	for _, key := range []string{"node_id", "model"} {
		if _, ok := roles[0][key]; ok {
			t.Fatalf("the admin preview shape must not grow a %q field: %#v", key, roles[0])
		}
	}
}

func TestMapNodeError(t *testing.T) {
	for _, value := range []interface{}{"timeout", "TIMEOUT", " model_timeout ", "role_recommendation_timeout"} {
		if got := mapNodeError(value); got != NodeRequestCodeRecommendTimeout {
			t.Errorf("mapNodeError(%v) = %q, want timeout", value, got)
		}
	}
	for _, value := range []interface{}{nil, "boom", 42} {
		if got := mapNodeError(value); got != NodeRequestCodeRecommendInvalid {
			t.Errorf("mapNodeError(%v) = %q, want invalid", value, got)
		}
	}
}

// startBroker returns a broker with a sender that records what it put on the
// wire.
func startBroker(t *testing.T) (*NodeRequestBroker, *string, *sync.Mutex) {
	t.Helper()
	var (
		mu  sync.Mutex
		got string
	)
	broker := NewNodeRequestBroker(nil)
	broker.BindSender(func(_ context.Context, toUserID, content string) error {
		mu.Lock()
		defer mu.Unlock()
		got = toUserID + "|" + content
		return nil
	})
	return broker, &got, &mu
}

func TestNodeRequestBrokerRoundTrip(t *testing.T) {
	broker, sent, mu := startBroker(t)
	host := NodeRequestHost{
		AIUserID:             "rc_node_host",
		DisplayName:          "主持",
		Avatar:               "https://example.test/a.png",
		RecommendationPrompt: "用中文",
		TimeoutSeconds:       30,
		ConfigVersion:        7,
	}

	requestID, code := broker.StartRecommendation(context.Background(), host, "AI 与就业", "找平衡", 3)
	if code != "" {
		t.Fatalf("StartRecommendation failed: %s", code)
	}
	if !strings.HasPrefix(requestID, "roles_") {
		t.Fatalf("request id %q must be namespaced so it cannot collide with turn_ ids", requestID)
	}

	mu.Lock()
	wire := *sent
	mu.Unlock()
	if !strings.HasPrefix(wire, "rc_node_host|") {
		t.Fatalf("request must be addressed to the host, got %q", wire)
	}
	var envelope struct {
		RequestID string                 `json:"request_id"`
		Service   string                 `json:"service"`
		Action    string                 `json:"action"`
		Params    map[string]interface{} `json:"params"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(wire, "rc_node_host|")), &envelope); err != nil {
		t.Fatalf("unmarshal wire payload: %v", err)
	}
	if envelope.RequestID != requestID || envelope.Service != "discussion" || envelope.Action != "recommend_roles" {
		t.Fatalf("unexpected envelope: %#v", envelope)
	}
	if envelope.Params["topic"] != "AI 与就业" {
		t.Fatalf("topic not carried: %#v", envelope.Params)
	}
	if envelope.Params["recommendation_prompt"] != "用中文" {
		t.Fatalf("operator prompt not carried: %#v", envelope.Params)
	}
	if _, ok := envelope.Params["timestamp"]; !ok {
		t.Fatalf("timestamp missing; the device needs it to drop replayed requests")
	}

	answer := `{"request_id":"` + requestID + `","msg_type":"command_result","payload":{"kind":"text","roles":[` +
		`{"role_name":"主持人","role_prompt":"引导","speaking_order":0},` +
		`{"role_name":"质疑者","role_prompt":"反驳","speaking_order":1}]}}`
	if claimed := broker.AcceptResponse("rc_node_host", answer); !claimed {
		t.Fatal("AcceptResponse must claim an answer for a request it issued")
	}

	payload, code := broker.AwaitRecommendation(context.Background(), requestID)
	if code != "" {
		t.Fatalf("AwaitRecommendation failed: %s", code)
	}
	if payload["config_version"] != 7 {
		t.Fatalf("config_version not echoed: %#v", payload["config_version"])
	}
	hostBlock, _ := payload["host"].(map[string]interface{})
	if hostBlock["display_name"] != "主持" || hostBlock["avatar"] != "https://example.test/a.png" {
		t.Fatalf("host identity missing: %#v", payload["host"])
	}
	roles, _ := payload["roles"].([]map[string]interface{})
	if len(roles) != 2 || roles[0]["role_name"] != "主持人" || roles[1]["role_name"] != "质疑者" {
		t.Fatalf("roles not returned in order: %#v", payload["roles"])
	}
	for _, role := range roles {
		if id, _ := role["role_id"].(string); id == "" {
			t.Fatalf("role_id missing: %#v", role)
		}
	}
	if id, _ := payload["recommendation_id"].(string); id == "" {
		t.Fatal("recommendation_id missing")
	}
}

func TestNodeRequestBrokerRejectsBeforeSending(t *testing.T) {
	broker, _, _ := startBroker(t)

	t.Run("unconfigured host", func(t *testing.T) {
		if _, code := broker.StartRecommendation(context.Background(), NodeRequestHost{}, "t", "", 3); code != NodeRequestCodeHostNotConfigured {
			t.Fatalf("got %q, want %q", code, NodeRequestCodeHostNotConfigured)
		}
	})

	t.Run("no sender bound", func(t *testing.T) {
		unbound := NewNodeRequestBroker(nil)
		host := NodeRequestHost{AIUserID: "rc_node_host", TimeoutSeconds: 30}
		if _, code := unbound.StartRecommendation(context.Background(), host, "t", "", 3); code != NodeRequestCodeHostOffline {
			t.Fatalf("got %q, want %q", code, NodeRequestCodeHostOffline)
		}
	})
}

func TestNodeRequestBrokerInvalidAnswer(t *testing.T) {
	broker, _, _ := startBroker(t)
	host := NodeRequestHost{AIUserID: "rc_node_host", TimeoutSeconds: 30}
	requestID, code := broker.StartRecommendation(context.Background(), host, "t", "", 3)
	if code != "" {
		t.Fatalf("start failed: %s", code)
	}

	// A non-permutation speaking order must be rejected, not forwarded.
	bad := `{"request_id":"` + requestID + `","msg_type":"command_result","payload":{"kind":"text","roles":[` +
		`{"role_name":"A","role_prompt":"p","speaking_order":0},` +
		`{"role_name":"B","role_prompt":"p","speaking_order":0}]}}`
	if !broker.AcceptResponse("rc_node_host", bad) {
		t.Fatal("AcceptResponse must claim the answer")
	}
	if _, code := broker.AwaitRecommendation(context.Background(), requestID); code != NodeRequestCodeRecommendInvalid {
		t.Fatalf("got %q, want %q", code, NodeRequestCodeRecommendInvalid)
	}
}

func TestNodeRequestBrokerErrorAnswer(t *testing.T) {
	broker, _, _ := startBroker(t)
	requestID, _ := broker.StartRecommendation(context.Background(), NodeRequestHost{AIUserID: "rc_node_host", TimeoutSeconds: 30}, "t", "", 3)

	broker.AcceptResponse("rc_node_host", `{"request_id":"`+requestID+`","msg_type":"command_result","payload":{"kind":"error","error":"timeout"}}`)
	if _, code := broker.AwaitRecommendation(context.Background(), requestID); code != NodeRequestCodeRecommendTimeout {
		t.Fatalf("got %q, want %q", code, NodeRequestCodeRecommendTimeout)
	}
}

func TestNodeRequestBrokerIgnoresForeignMessages(t *testing.T) {
	broker, _, _ := startBroker(t)

	// A discussion turn's command_result must fall through to the coordinator.
	for name, content := range map[string]string{
		"turn id":       `{"request_id":"turn_1_1_20260101","msg_type":"command_result","payload":{"content":"hi"}}`,
		"no request id": `{"msg_type":"command_result","payload":{}}`,
		"not a result":  `{"request_id":"roles_x","msg_type":"something_else"}`,
		"not json":      `{oops`,
	} {
		if broker.AcceptResponse("rc_node_host", content) {
			t.Errorf("%s: must not be claimed by the broker", name)
		}
	}
}

func TestNodeRequestBrokerIgnoresWrongSender(t *testing.T) {
	broker, _, _ := startBroker(t)
	requestID, _ := broker.StartRecommendation(context.Background(), NodeRequestHost{AIUserID: "rc_node_host", TimeoutSeconds: 30}, "t", "", 3)

	answer := `{"request_id":"` + requestID + `","msg_type":"command_result","payload":{"roles":[` +
		`{"role_name":"A","role_prompt":"p","speaking_order":0}]}}`
	// Claimed (so the discussion path is skipped) but not settled.
	if !broker.AcceptResponse("rc_node_impostor", answer) {
		t.Fatal("the broker owns this id and must claim the message")
	}
	if !broker.AcceptResponse("rc_node_host", answer) {
		t.Fatal("the real answer must be claimed")
	}
	if _, code := broker.AwaitRecommendation(context.Background(), requestID); code != "" {
		t.Fatalf("the real answer should still settle the request, got %q", code)
	}
}

func TestNodeRequestBrokerTimeout(t *testing.T) {
	broker, _, _ := startBroker(t)
	requestID, code := broker.StartRecommendation(context.Background(), NodeRequestHost{AIUserID: "rc_node_host", TimeoutSeconds: 1}, "t", "", 3)
	if code != "" {
		t.Fatalf("start failed: %s", code)
	}
	started := time.Now()
	if _, code := broker.AwaitRecommendation(context.Background(), requestID); code != NodeRequestCodeRecommendTimeout {
		t.Fatalf("got %q, want %q", code, NodeRequestCodeRecommendTimeout)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("returned after %v; it must actually wait out the host timeout", elapsed)
	}
	// A late answer must not resurrect the request.
	if _, found := broker.pending[requestID]; found {
		t.Fatal("a settled request must be dropped from the pending map")
	}
}

func TestNodeRequestBrokerFirstSettleWins(t *testing.T) {
	broker, _, _ := startBroker(t)
	requestID, _ := broker.StartRecommendation(context.Background(), NodeRequestHost{AIUserID: "rc_node_host", TimeoutSeconds: 30}, "t", "", 3)

	// Expire it first, then deliver a (late) good answer: the timeout stands.
	broker.settle(requestID, "", NodeRequestCodeRecommendTimeout, nil)
	answer := `{"request_id":"` + requestID + `","msg_type":"command_result","payload":{"roles":[` +
		`{"role_name":"A","role_prompt":"p","speaking_order":0}]}}`
	broker.AcceptResponse("rc_node_host", answer)

	if _, code := broker.AwaitRecommendation(context.Background(), requestID); code != NodeRequestCodeRecommendTimeout {
		t.Fatalf("got %q, want the timeout to win", code)
	}
}

func TestNodeRequestBrokerCancelledContext(t *testing.T) {
	broker, _, _ := startBroker(t)
	requestID, _ := broker.StartRecommendation(context.Background(), NodeRequestHost{AIUserID: "rc_node_host", TimeoutSeconds: 30}, "t", "", 3)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, code := broker.AwaitRecommendation(ctx, requestID); code != NodeRequestCodeRecommendTimeout {
		t.Fatalf("got %q, want %q", code, NodeRequestCodeRecommendTimeout)
	}
}

func TestNodeRequestBrokerUnknownRequest(t *testing.T) {
	broker := NewNodeRequestBroker(nil)
	if _, code := broker.AwaitRecommendation(context.Background(), "roles_missing"); code != NodeRequestCodeRecommendInvalid {
		t.Fatalf("got %q, want %q", code, NodeRequestCodeRecommendInvalid)
	}
}
