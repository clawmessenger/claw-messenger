package rongcloud

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestInteractiveV3EventWhitelist 钉住前端 discussion-v3.ts 的 eventData 键集合。
// 白名单多一个键，前端就会把整段 events 尾读解析失败；少一个键，我们就得把一条
// 本可以正常展示的事件降级掉。两边必须逐字一致。
func TestInteractiveV3EventWhitelist(t *testing.T) {
	want := []string{
		"round_started", "round_summary_started", "round_summary_completed",
		"contribution_delta", "contribution_completed", "user_interjection",
		"member_retry_scheduled", "member_skipped", "request_interrupted",
		"interjection_applied", "interruption_failed", "discussion_started",
		"discussion_completed", "discussion_cancelled", "discussion_failed",
		"discussion_progress",
	}
	if len(interactiveV3EventTypes) != len(want) {
		t.Fatalf("whitelist size = %d, want %d", len(interactiveV3EventTypes), len(want))
	}
	for _, name := range want {
		if !isInteractiveV3EventType(name) {
			t.Errorf("whitelist missing %q", name)
		}
	}
	// 引擎内部的控制事件：绝不能出现在白名单里，否则前端会按未定契约校验它们。
	for _, name := range []string{
		"assignment_created", "host_turn_requested", "control_accepted",
		"callback_accepted", "task_timeout", "retry_accepted", "task_failed",
	} {
		if isInteractiveV3EventType(name) {
			t.Errorf("control event %q must not be advertised to the web", name)
		}
	}
}

// TestDiscussionEventViewDowngradesControlEvents 验证非白名单的 v3 事件被降级成
// discussion_progress + 空 data。少了这一步，getDiscussionEvents 会对整段历史抛错。
func TestDiscussionEventViewDowngradesControlEvents(t *testing.T) {
	envelope := mustJSON(map[string]interface{}{
		"protocolVersion": 3, "discussionId": "d1", "chatroomId": "c1",
		"requestId": "req-1", "stateVersion": 4, "round": 2, "roundRevision": 0,
		"timestamp": int64(1700000000000),
	})
	cases := []struct {
		eventType   string
		wantType    string
		wantPayload bool // true = payload 保留（白名单事件）
	}{
		{"assignment_created", "discussion_progress", false},
		{"host_turn_requested", "discussion_progress", false},
		{"control_accepted", "discussion_progress", false},
		{"task_timeout", "discussion_progress", false},
		{"task_failed", "discussion_progress", false},
		{"retry_accepted", "discussion_progress", false},
		{"round_started", "round_started", true},
		{"round_summary_completed", "round_summary_completed", true},
		{"discussion_progress", "discussion_progress", true},
	}
	for _, tc := range cases {
		view := discussionEventView(DiscussionEventRecord{
			DiscussionID: "d1", Seq: 7, EventType: tc.eventType,
			ActorID: "a1", TargetID: "t1",
			Payload:        mustJSON(map[string]interface{}{"round": 2, "content": "hi"}),
			IdempotencyKey: "k", V3Envelope: envelope,
		})
		if got := view["eventType"]; got != tc.wantType {
			t.Errorf("%s: eventType = %v, want %v", tc.eventType, got, tc.wantType)
		}
		if view["protocolVersion"] != 3 {
			t.Errorf("%s: protocolVersion = %v, want 3", tc.eventType, view["protocolVersion"])
		}
		payload, ok := view["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: payload not an object: %#v", tc.eventType, view["payload"])
		}
		if tc.wantPayload {
			if _, has := payload["content"]; !has {
				t.Errorf("%s: payload was dropped", tc.eventType)
			}
		} else if len(payload) != 0 {
			// 前端对 discussion_progress 的 data 是严格空对象校验。
			t.Errorf("%s: payload = %#v, want {}", tc.eventType, payload)
		}
		// v3 信封里的身份字段必须原样展开（前端靠它们做跨字段一致性校验）。
		for _, key := range []string{"requestId", "stateVersion", "round", "roundRevision"} {
			if _, has := view[key]; !has {
				t.Errorf("%s: view missing envelope key %s", tc.eventType, key)
			}
		}
	}
}

// TestDiscussionEventViewKeepsV2Events 验证没有 v3 信封的 legacy 事件不被改动
// （P4 的 discussion_started / final_synthesis_started 都走这条路径）。
func TestDiscussionEventViewKeepsV2Events(t *testing.T) {
	view := discussionEventView(DiscussionEventRecord{
		DiscussionID: "d1", Seq: 3, EventType: "final_synthesis_started",
		Payload: mustJSON(map[string]interface{}{"round": 1}),
	})
	if _, has := view["protocolVersion"]; has {
		t.Error("v2 event must not advertise protocolVersion")
	}
	if view["eventType"] != "final_synthesis_started" {
		t.Errorf("eventType = %v", view["eventType"])
	}
}

// TestInteractiveRolesParsing 验证角色板解析：按 speakingOrder 排序、host 单列、
// 缺 host 或缺普通成员时返回 ok=false（引擎据此拒绝推进）。
func TestInteractiveRolesParsing(t *testing.T) {
	snapshot := mustJSON(map[string]interface{}{
		"ai-b": map[string]interface{}{
			"memberId": "ai-b", "nodeId": "n-b", "roleName": "B",
			"roleInstructions": "ins-b", "speakingOrder": 2, "isHost": false,
		},
		"ai-a": map[string]interface{}{
			"memberId": "ai-a", "nodeId": "n-a", "roleName": "A",
			"roleInstructions": "ins-a", "speakingOrder": 0, "isHost": false,
		},
		"ai-c": map[string]interface{}{
			"memberId": "ai-c", "nodeId": "n-c", "roleName": "C",
			"roleInstructions": "ins-c", "speakingOrder": 1, "isHost": false,
		},
		"host": map[string]interface{}{
			"memberId": "ai-host", "nodeId": "n-host", "roleName": roundtableHostRoleName,
			"roleInstructions": "", "speakingOrder": -1, "isHost": true,
		},
	})
	ordinary, host, ok := interactiveRoles(DiscussionRecord{RoleSnapshot: snapshot})
	if !ok {
		t.Fatal("expected a usable role snapshot")
	}
	if host.MemberID != "ai-host" {
		t.Errorf("host = %q", host.MemberID)
	}
	if len(ordinary) != 3 {
		t.Fatalf("ordinary = %d", len(ordinary))
	}
	for i, want := range []string{"ai-a", "ai-c", "ai-b"} {
		if ordinary[i].MemberID != want {
			t.Errorf("ordinary[%d] = %q, want %q", i, ordinary[i].MemberID, want)
		}
		if ordinary[i].SpeakingOrder != i {
			t.Errorf("ordinary[%d].SpeakingOrder = %d", i, ordinary[i].SpeakingOrder)
		}
	}

	// 没有 host → 不可推进。
	noHost := mustJSON(map[string]interface{}{
		"ai-a": map[string]interface{}{"memberId": "ai-a", "speakingOrder": 0},
	})
	if _, _, ok := interactiveRoles(DiscussionRecord{RoleSnapshot: noHost}); ok {
		t.Error("expected ok=false without a host")
	}
	// 只有 host → 不可推进。
	hostOnly := mustJSON(map[string]interface{}{
		"host": map[string]interface{}{"memberId": "ai-host", "isHost": true},
	})
	if _, _, ok := interactiveRoles(DiscussionRecord{RoleSnapshot: hostOnly}); ok {
		t.Error("expected ok=false without ordinary members")
	}
	// 坏 JSON → 不可推进（不 panic）。
	if _, _, ok := interactiveRoles(DiscussionRecord{RoleSnapshot: []byte("{")}); ok {
		t.Error("expected ok=false on malformed snapshot")
	}
}

// TestInteractiveHostPromptReadsSnapshot 验证主持人提示词取自
// host_config_snapshot.discussion_host_prompt（HostPersonaLoader 的落库位置）。
func TestInteractiveHostPromptReadsSnapshot(t *testing.T) {
	d := DiscussionRecord{HostConfigSnapshot: mustJSON(map[string]interface{}{
		"node_id": "n-host", "discussion_host_prompt": "保持中立，控制节奏。",
	})}
	if got := interactiveHostPrompt(d); got != "保持中立，控制节奏。" {
		t.Errorf("host prompt = %q", got)
	}
	if got := interactiveHostPrompt(DiscussionRecord{HostConfigSnapshot: []byte("{}")}); got != "" {
		t.Errorf("missing host prompt = %q", got)
	}
}

// TestInteractiveTurnTimeoutFloor 验证单轮等待上限不会被配置压到下限以下
// （一次 agent 调用不可能 10s 内完成，更小的值只会让每一轮都假超时）。
func TestInteractiveTurnTimeoutFloor(t *testing.T) {
	rt := &discussionRuntime{}
	if got := rt.interactiveTurnTimeout(); got != interactiveDefaultTurnTimeout {
		t.Errorf("default timeout = %v", got)
	}
	rt.systemHostLoader = func(ctx context.Context) (DiscussionSystemHostConfig, bool, error) {
		return DiscussionSystemHostConfig{TimeoutSeconds: 1}, true, nil
	}
	if got := rt.interactiveTurnTimeout(); got != interactiveMinTurnTimeout {
		t.Errorf("clamped timeout = %v, want %v", got, interactiveMinTurnTimeout)
	}
	rt.systemHostLoader = func(ctx context.Context) (DiscussionSystemHostConfig, bool, error) {
		return DiscussionSystemHostConfig{TimeoutSeconds: 120}, true, nil
	}
	if got := rt.interactiveTurnTimeout(); got.Seconds() != 120 {
		t.Errorf("configured timeout = %v", got)
	}
}

// TestInteractiveReadyGate 验证引擎总开关：这是 getWorkflowCapabilities 翻面的
// 唯一依据，所以任何一项缺失都必须返回 false（宁可少报）。
func TestInteractiveReadyGate(t *testing.T) {
	if (&discussionRuntime{}).interactiveReady() {
		t.Error("empty runtime must not be ready")
	}
	if (&discussionRuntime{interactiveEnabled: true}).interactiveReady() {
		t.Error("enabled without repo/queries must not be ready")
	}
	rt := &discussionRuntime{
		interactiveEnabled: true,
		repo:               &DiscussionRepository{},
	}
	if rt.interactiveReady() {
		t.Error("enabled without queries must not be ready")
	}
	if (*discussionRuntime)(nil).interactiveReady() {
		t.Error("nil runtime must not be ready")
	}
}

// TestInteractiveTaskRoundTrip 验证 outbox payload 的字段名稳定：设备回包按
// requestId 索引任务，改名会让在途派发全部失联。
func TestInteractiveTaskRoundTrip(t *testing.T) {
	task := interactiveTask{
		RequestID: "req-1", Round: 2, RoundRevision: 0, StateVersion: 9,
		TargetID: "ai-a", SpeakingOrder: 1, IsHost: false, Attempt: 1,
		Timestamp: 1700000000000, ChatroomID: "c1", Prompt: "p",
	}
	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"requestId"`, `"round"`, `"roundRevision"`, `"stateVersion"`,
		`"targetId"`, `"speakingOrder"`, `"isHost"`, `"attempt"`, `"chatroomId"`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("payload missing %s: %s", key, raw)
		}
	}
	var back interactiveTask
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 结构体含 map，不能用 == 比较；重编码后逐字节比对（encoding/json 对 map 键
	// 排序稳定）。
	again, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(again) != string(raw) {
		t.Errorf("round trip mismatch:\n got %s\nwant %s", again, raw)
	}
}

// TestActiveInteractiveDiscussionGate 验证只有 user_confirmed + 引擎就绪才认领；
// 否则 legacy 生命周期动作必须保持原样。
func TestActiveInteractiveDiscussionGate(t *testing.T) {
	// 引擎未就绪 → 一律不认领（连查询都不做，repo 为 nil 也不会 panic）。
	rt := &discussionRuntime{}
	if _, ok := rt.activeInteractiveDiscussion(nil, CommandContent{Params: map[string]interface{}{
		"discussionId": "d1",
	}}); ok {
		t.Error("engine not ready must not claim lifecycle actions")
	}
}
