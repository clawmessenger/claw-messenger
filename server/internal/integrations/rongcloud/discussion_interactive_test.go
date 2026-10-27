package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestInteractiveV3EventWhitelist 钉住前端 discussion-v3.ts 的 eventData 键集合。
// 白名单多一个键，前端就会把整段 events 尾读解析失败；少一个键，我们就得把一条
// 本可以正常展示的事件降级掉。两边必须逐字一致。
//
// 人工核对清单（17 既有 + 3 引导式新增 = 20）——改白名单时同步核对该注释与
// web src/lib/discussion-v3.ts 的 eventData 校验器：
//
//	既有 17：round_started / round_summary_started / round_summary_completed /
//	  round_checkpoint / contribution_delta / contribution_completed /
//	  user_interjection / member_retry_scheduled / member_skipped /
//	  request_interrupted / interjection_applied / interruption_failed /
//	  discussion_started / discussion_completed / discussion_cancelled /
//	  discussion_failed / discussion_progress
//	引导式 3：setup_prompt / setup_step_completed / setup_recommend_failed
func TestInteractiveV3EventWhitelist(t *testing.T) {
	want := []string{
		"round_started", "round_summary_started", "round_summary_completed", "round_checkpoint",
		"contribution_delta", "contribution_completed", "user_interjection",
		"member_retry_scheduled", "member_skipped", "request_interrupted",
		"interjection_applied", "interruption_failed", "discussion_started",
		"discussion_completed", "discussion_cancelled", "discussion_failed",
		"discussion_progress",
		"setup_prompt", "setup_step_completed", "setup_recommend_failed",
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
		{"round_checkpoint", "round_checkpoint", true},
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

// TestDiscussionEventViewNullsEmptyIds 钉住「讨论历史响应无效」的根因回归：
// actor_id / target_id 是可为 NULL 的列，Go 读回来是空串；若原样回 ""，前端
// v3 契约（nullable(id)，要求 null 或合法 id）会把整条事件判无效，而
// getDiscussionEvents 的 map 是不放行的 —— 一条不合规整段就炸。所以这两个
// 字段的空串必须转成 JSON null。线上 2026-10-13 的讨论 discussion_started /
// round_started（target 无指代对象）正是因此整段失败。
func TestDiscussionEventViewNullsEmptyIds(t *testing.T) {
	envelope := mustJSON(map[string]interface{}{
		"protocolVersion": 3, "discussionId": "d1", "chatroomId": "c1",
		"requestId": "req-1", "stateVersion": 2, "round": 1, "roundRevision": 0,
		"timestamp": int64(1700000000000),
	})
	// 修复点：空串 → null（前端 nullable(id) 只接受 null 或合法 id）。
	view := discussionEventView(DiscussionEventRecord{
		DiscussionID: "d1", Seq: 1, EventType: "discussion_started",
		ActorID: "", TargetID: "",
		Payload:    mustJSON(map[string]interface{}{"roundPolicy": "user_confirmed"}),
		V3Envelope: envelope,
	})
	if view["actorId"] != nil {
		t.Errorf("actorId = %#v, want nil (JSON null)", view["actorId"])
	}
	if view["targetId"] != nil {
		t.Errorf("targetId = %#v, want nil (JSON null)", view["targetId"])
	}

	// 非空值必须原样保留（正常事件的 actor/target 是融云账号）。
	filled := discussionEventView(DiscussionEventRecord{
		DiscussionID: "d1", Seq: 4, EventType: "contribution_completed",
		ActorID: "rc_node_3300776275_opencode", TargetID: "rc_node_3300776275_opencode",
		Payload:    mustJSON(map[string]interface{}{"round": 1, "content": "x"}),
		V3Envelope: envelope,
	})
	if filled["actorId"] != "rc_node_3300776275_opencode" {
		t.Errorf("actorId = %#v, want rc id", filled["actorId"])
	}
	if filled["targetId"] != "rc_node_3300776275_opencode" {
		t.Errorf("targetId = %#v, want rc id", filled["targetId"])
	}
}

// TestDiscussionEventSender 验证广播发件人选择：角色板成员（普通成员/主持人）
// 以自己的融云账号发言；引擎事件（"system"）、操作者事件（不在角色板里的
// rc id）、空 actor 一律回落到平台系统账号。
func TestDiscussionEventSender(t *testing.T) {
	snapshot := mustJSON(map[string]interface{}{
		"ai-a": map[string]interface{}{
			"memberId": "ai-a", "nodeId": "n-a", "roleName": "A", "speakingOrder": 0,
		},
		"host": map[string]interface{}{
			"memberId": "ai-host", "nodeId": "n-host", "isHost": true, "speakingOrder": -1,
		},
	})
	d := DiscussionRecord{RoleSnapshot: snapshot}
	cases := []struct {
		name  string
		actor string
		want  string
	}{
		{"member speaks as itself", "ai-a", "ai-a"},
		{"host speaks as itself", "ai-host", "ai-host"},
		{"engine event falls back", "system", "claw-system"},
		{"empty actor falls back", "", "claw-system"},
		{"operator rc id not in roles falls back", "rc-user-1", "claw-system"},
	}
	for _, tc := range cases {
		if got := discussionEventSender(d, DiscussionEventRecord{ActorID: tc.actor}, "claw-system"); got != tc.want {
			t.Errorf("%s: sender = %q, want %q", tc.name, got, tc.want)
		}
	}
	// 角色板不可用（坏快照）→ 一律回落，不 panic。
	bad := DiscussionRecord{RoleSnapshot: []byte("{")}
	if got := discussionEventSender(bad, DiscussionEventRecord{ActorID: "ai-a"}, "claw-system"); got != "claw-system" {
		t.Errorf("bad snapshot: sender = %q, want fallback", got)
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

// TestInteractiveTurnTimeoutFloor 验证单轮等待上限不会被配置压到下限以下。
//
// 讨论是按序发言的：成员没说完就不该推进下一位，等多久都不算错。管理端
// 「系统主持 timeout_seconds」初始只有 60s，而设备端从 agent 冷启动到首个
// token 常常就要几十秒 —— 60s 会让**正常**轮次成批假超时（线上表现为
// 「第 N 轮响应超时 → 重试 → 跳过 → 主持人汇总超时 → summary_failed」）。
func TestInteractiveTurnTimeoutFloor(t *testing.T) {
	rt := &discussionRuntime{}
	if got := rt.interactiveTurnTimeout(); got != interactiveDefaultTurnTimeout {
		t.Errorf("default timeout = %v", got)
	}
	// 配置比保底值小：抬到保底值，绝不按 1s / 60s 掐正常轮次。
	for _, seconds := range []int{1, 60, 120} {
		rt.systemHostLoader = func(ctx context.Context) (DiscussionSystemHostConfig, bool, error) {
			return DiscussionSystemHostConfig{TimeoutSeconds: seconds}, true, nil
		}
		if got := rt.interactiveTurnTimeout(); got != interactiveTurnTimeoutFloor {
			t.Errorf("configured %ds must be raised to floor, got %v", seconds, got)
		}
	}
	// 配置比保底值大：按配置走（管理端确实想等更久时不被压回来）。
	rt.systemHostLoader = func(ctx context.Context) (DiscussionSystemHostConfig, bool, error) {
		return DiscussionSystemHostConfig{TimeoutSeconds: 900}, true, nil
	}
	if got := rt.interactiveTurnTimeout(); got.Seconds() != 900 {
		t.Errorf("configured timeout = %v", got)
	}
}

// TestInteractiveHeartbeatWindow 验证心跳续期窗口不小于空闲窗口：设备每 15s
// 一条心跳，窗口 120s 意味着漏掉 8 条才判掉线；配置更长时按配置。
func TestInteractiveHeartbeatWindow(t *testing.T) {
	rt := &discussionRuntime{}
	if got := rt.interactiveHeartbeatWindow(); got < interactiveHeartbeatIdleWindow {
		t.Errorf("heartbeat window %v must be >= idle window %v", got, interactiveHeartbeatIdleWindow)
	}
	rt.systemHostLoader = func(ctx context.Context) (DiscussionSystemHostConfig, bool, error) {
		return DiscussionSystemHostConfig{TimeoutSeconds: 600}, true, nil
	}
	if got := rt.interactiveHeartbeatWindow(); got.Seconds() != 600 {
		t.Errorf("heartbeat window should follow longer config, got %v", got)
	}
}

// TestInteractiveHostRetryExhausted 钉住主持人汇总的重试语义：按**本次派发链**
// 的 attempt 判，而不是整场讨论累计的重试事件数。
//
// 旧实现统计全场的 member_retry_scheduled（memberId == host），7 轮讨论里前几轮
// 的零星重试会把计数吃满，最后一轮主持人**第一次**超时就直接判 summary_failed。
func TestInteractiveHostRetryExhausted(t *testing.T) {
	if interactiveMaxHostRetries < 1 {
		t.Fatalf("host must get at least one retry, got %d", interactiveMaxHostRetries)
	}
	for attempt := 1; attempt <= interactiveMaxHostRetries; attempt++ {
		if interactiveHostRetryExhausted(attempt) {
			t.Errorf("attempt %d must still be retryable", attempt)
		}
	}
	if !interactiveHostRetryExhausted(interactiveMaxHostRetries + 1) {
		t.Error("attempt past the cap must fail the discussion")
	}
	// 缺省/异常 attempt（老 payload 没有该字段 = 0）按第一次处理，不能被当成用尽。
	if interactiveHostRetryExhausted(0) {
		t.Error("attempt 0 must be treated as the first try")
	}
}

// TestParseStreamPing 钉住心跳信封的解析：前缀剥完就得到 outbox 的 command_id，
// 讨论组（UUID）与闲聊中继（relay_*）共用同一条通道。
func TestParseStreamPing(t *testing.T) {
	if id, ok := parseStreamPing("relay_ping_3f1c2e4d-0000-4000-8000-000000000000"); !ok ||
		id != "3f1c2e4d-0000-4000-8000-000000000000" {
		t.Errorf("uuid ping parse = %q, %v", id, ok)
	}
	if id, ok := parseStreamPing("relay_ping_relay_9_0"); !ok || id != "relay_9_0" {
		t.Errorf("relay ping parse = %q, %v", id, ok)
	}
	// 普通回包不是心跳：前缀不匹配必须放过，否则每轮完成回包都会被吞掉。
	if _, ok := parseStreamPing("3f1c2e4d-0000-4000-8000-000000000000"); ok {
		t.Error("plain request id must not parse as ping")
	}
	if _, ok := parseStreamPing("relay_ping_"); ok {
		t.Error("empty ping id must not parse")
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

// TestInteractiveInterjectionRoundFor 锁住插话归属轮次：轮末与未开局的插话必须
// 顺延到「真正会被派发、因而会被读到」的轮次，否则气泡显示「已接受」而无人消费。
func TestInteractiveInterjectionRoundFor(t *testing.T) {
	ptr := func(n int) *int { return &n }
	cases := []struct {
		name         string
		status       string
		currentRound int
		allDone      bool
		maxRounds    *int
		want         int
	}{
		{"planning 未开局 → 首轮", DiscussionStatusPlanning, 0, false, nil, 1},
		{"轮次异常为 0 的 running → 首轮", DiscussionStatusRunning, 0, false, nil, 1},
		{"running 本轮仍有成员待发言 → 当前轮", DiscussionStatusRunning, 3, false, nil, 3},
		{"running 但本轮全员已说完（推进间隙）→ 下一轮", DiscussionStatusRunning, 3, true, nil, 4},
		{"summarizing 正在汇总 → 下一轮", DiscussionStatusSummarizing, 3, false, nil, 4},
		{"awaiting_user 等人确认 → 下一轮", DiscussionStatusAwaitingUser, 3, false, nil, 4},
		{"legacy_capped 已达上限 → 0（无可承载轮次）", DiscussionStatusRunning, 2, true, ptr(2), 0},
		{"legacy_capped 未达上限 → 当前轮", DiscussionStatusRunning, 1, false, ptr(2), 1},
	}
	for _, tc := range cases {
		if got := interactiveInterjectionRoundFor(tc.status, tc.currentRound, tc.allDone, tc.maxRounds); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestInteractiveRetryAcceptsStatus 钉住手动重试的状态门限：前端「重试主持任务」
// 按钮在 summarizing / synthesizing（含 failed）时亮起，服务端必须同样接受，
// 否则按钮形同虚设（永远 409 no failed work to retry）。awaiting_user /
// completed / cancelled 必须继续拒绝。
func TestInteractiveRetryAcceptsStatus(t *testing.T) {
	accepts := []string{DiscussionStatusFailed, DiscussionStatusSummarizing, DiscussionStatusSynthesizing, DiscussionStatusRunning}
	for _, status := range accepts {
		if !interactiveRetryAcceptsStatus(status) {
			t.Errorf("status %q must be retryable", status)
		}
	}
	rejects := []string{DiscussionStatusAwaitingUser, DiscussionStatusCompleted, DiscussionStatusCancelled, DiscussionStatusPlanning, ""}
	for _, status := range rejects {
		if interactiveRetryAcceptsStatus(status) {
			t.Errorf("status %q must stay rejected", status)
		}
	}
}

// ---- 前序发言上下文（2026-10-15 线上：「第二轮不延续第一轮」）----------------

// TestInteractiveHistoryBlock_FeedsHostThisRound 钉住主持人拿不到素材的回归：
// 出本轮纪要的那一次派发必须把本轮成员发言带上，否则主持人只会回
// 「缺少前序发言记录，无法复述成员立场与分歧。请提供第 N 轮的发言内容」。
// TestInteractiveHistoryBlock_HostRevisionCarriesFullPriorDocument 修订模式下主持人
// 必须拿到上一版产物全文（按剩余预算尽量完整），而不是旧逻辑的 1500 字截断 ——
// 任务要求「在上一版基础上修订」，底稿缺席就只能重写。
func TestInteractiveHistoryBlock_HostRevisionCarriesFullPriorDocument(t *testing.T) {
	doc := strings.Repeat("产", 2600) // 超过旧 interactiveSummaryClip(1500)
	history := interactiveHistory{
		contributions: []interactiveStatement{{Round: 2, MemberID: "m1", RoleName: "甲", Content: "本轮新观点"}},
		summaries:     []interactiveStatement{{Round: 1, MemberID: "h", RoleName: "主持", Content: doc}},
	}
	block := interactiveHistoryBlock(history, 2, true, interactivePromptContextLimit)
	if !strings.Contains(block, "[上一版产物]") {
		t.Fatalf("host revision context must carry prior document: %q", block[:200])
	}
	// 底稿长度 ~2600 + 头尾标注 ≤ 预算 4000：必须整段给到，不能按 1500 截断。
	if !strings.Contains(block, strings.Repeat("产", 2600)) {
		t.Errorf("prior document was clipped; revision mode needs the full text, got %d runes of it",
			strings.Count(block, "产"))
	}
	if !strings.Contains(block, "本轮新观点") {
		t.Errorf("this-round statements must still be included: %q", block[:200])
	}
}

func TestInteractiveHistoryBlock_FeedsHostThisRound(t *testing.T) {
	history := interactiveHistory{contributions: []interactiveStatement{
		{Round: 1, MemberID: "m1", RoleName: "观看着", Content: "我认为应该先做 A。"},
		{Round: 1, MemberID: "m2", RoleName: "提问者", Content: "我担心 A 的成本。"},
	}}
	block := interactiveHistoryBlock(history, 1, true, interactivePromptContextLimit)
	for _, want := range []string{"成员发言", "观看着", "先做 A", "提问者", "成本"} {
		if !strings.Contains(block, want) {
			t.Errorf("host context missing %q: %q", want, block)
		}
	}
}

// TestInteractiveHistoryBlock_ContinuesPreviousRound_Scoped 第二轮成员必须同时看到
// 上一轮纪要与上一轮发言 —— 只给纪要会把上一轮的具体观点丢掉，成员照样从头重开。
//
// 新语义（按智能体自身发言截断）：窗口起点是该成员**自己最后一次发言**，更早的
// 轮次不再进入提示词。所以这里断言的是「上一轮（= 自己上次开口那一轮）在、更早的
// 一轮不在」，而不是旧语义的「所有轮次全在」。
func TestInteractiveHistoryBlock_ContinuesPreviousRound_Scoped(t *testing.T) {
	history := interactiveHistory{
		contributions: []interactiveStatement{
			{Round: 1, MemberID: "m1", RoleName: "观看着", Content: "第一轮的观点"},
			{Round: 1, MemberID: "m2", RoleName: "提问者", Content: "第一轮的质疑"},
			{Round: 2, MemberID: "m1", RoleName: "观看着", Content: "第二轮的观点"},
			{Round: 2, MemberID: "m2", RoleName: "提问者", Content: "第二轮的质疑"},
		},
		summaries: []interactiveStatement{
			{Round: 1, MemberID: "h", RoleName: "中立主持人", Content: "第一轮纪要：共识是 X"},
			{Round: 2, MemberID: "h", RoleName: "中立主持人", Content: "第二轮纪要：共识是 Y"},
		},
	}
	// m1 在第 2 轮说过话 → 窗口从「第二轮的观点」起，第 1 轮被裁掉。
	scoped := history.windowFor("m1", 2)
	if len(scoped.contributions) != 2 || scoped.contributions[0].Content != "第二轮的观点" {
		t.Fatalf("window must start at the member's own last turn: %+v", scoped.contributions)
	}
	block := interactiveHistoryBlock(scoped, 3, false, interactivePromptContextLimit)
	for _, want := range []string{"上一轮主持纪要", "第二轮纪要：共识是 Y", "第 2 轮成员发言", "第二轮的观点"} {
		if !strings.Contains(block, want) {
			t.Errorf("round-3 member context missing %q: %q", want, block)
		}
	}
	// 旧语义的残留：被裁掉的更早轮次不能再出现在上下文里。
	for _, gone := range []string{"第 1 轮成员发言", "第一轮纪要：共识是 X", "第一轮的观点"} {
		if strings.Contains(block, gone) {
			t.Errorf("scoped context must not carry %q: %q", gone, block)
		}
	}
}

// TestInteractiveHistoryBlock_FirstRoundShowsEarlierPeers 第 1 轮没有纪要可给，
// 但同轮的先发言者必须给到 —— 否则第二位成员只能凭空表态。
func TestInteractiveHistoryBlock_FirstRoundShowsEarlierPeers(t *testing.T) {
	history := interactiveHistory{contributions: []interactiveStatement{
		{Round: 1, MemberID: "m1", RoleName: "观看着", Content: "我先说观察"},
	}}
	block := interactiveHistoryBlock(history, 1, false, interactivePromptContextLimit)
	if !strings.Contains(block, "本轮已发言成员的观点") || !strings.Contains(block, "我先说观察") {
		t.Errorf("round-1 member context must carry earlier peers: %q", block)
	}
	if strings.Contains(block, "上一轮主持纪要") {
		t.Errorf("round 1 has no previous summary to inject: %q", block)
	}
}

// TestInteractiveHistoryBlock_EmptyKeepsLegacyShape 没有历史时整段省略（旧报文逐字不变）。
func TestInteractiveHistoryBlock_EmptyKeepsLegacyShape(t *testing.T) {
	if block := interactiveHistoryBlock(interactiveHistory{}, 1, false, 4000); block != "" {
		t.Errorf("empty history must render nothing, got %q", block)
	}
	if block := interactiveHistoryBlock(interactiveHistory{}, 1, true, 4000); block != "" {
		t.Errorf("empty history must render nothing for the host, got %q", block)
	}
}

// TestInteractiveHistoryBlock_HonoursBudget 预算不足时先装最新锚点（上一轮纪要），
// 且整体绝不超预算 —— prompt 走 argv 有 8000 字符上限，超了整轮退化 stdin 空转。
func TestInteractiveHistoryBlock_HonoursBudget(t *testing.T) {
	history := interactiveHistory{
		contributions: []interactiveStatement{
			{Round: 1, MemberID: "m1", RoleName: "甲", Content: strings.Repeat("一", 500)},
			{Round: 2, MemberID: "m1", RoleName: "甲", Content: strings.Repeat("二", 500)},
			{Round: 2, MemberID: "m2", RoleName: "乙", Content: strings.Repeat("三", 500)},
		},
		summaries: []interactiveStatement{{Round: 1, MemberID: "h", RoleName: "主持", Content: strings.Repeat("纪", 800)}},
	}
	block := interactiveHistoryBlock(history, 2, false, 900)
	if got := len([]rune(block)); got > 900 {
		t.Fatalf("block = %d runes, want <= 900", got)
	}
	if !strings.Contains(block, "上一轮主持纪要") {
		t.Errorf("latest anchor must be packed first: %q", block)
	}
}

// TestInteractiveBuildPrompt_StaysWithinRuneBudget_Scoped 超长发言不会把上下文顶过
// 上限，也不能按字节切片切出乱码（旧实现 block[:limit] 就干过这件事）。
//
// 新语义（按智能体自身发言截断）：提问者 m2 自己上次发言在第 2 轮，窗口里只有
// 那一条 → 第 1 轮成员的发言段整段消失。旧断言「第 1 轮成员发言必须出现」正是
// 全量累计时代的残留，这里反过来钉住它不该出现。
func TestInteractiveBuildPrompt_StaysWithinRuneBudget_Scoped(t *testing.T) {
	long := strings.Repeat("很长的发言内容", 2000)
	history := interactiveHistory{
		contributions: []interactiveStatement{
			{Round: 1, MemberID: "m1", RoleName: "观看着", Content: long},
			{Round: 2, MemberID: "m2", RoleName: "提问者", Content: long},
		},
		summaries: []interactiveStatement{{Round: 1, MemberID: "h", RoleName: "主持", Content: long}},
	}
	history = history.windowFor("m2", 2)
	d := DiscussionRecord{Topic: strings.Repeat("话题", 50), Goal: "目标", CurrentRound: 2}
	rt := &discussionRuntime{}
	prompt := rt.interactiveBuildPrompt(d, interactiveRole{MemberID: "m2", RoleName: "提问者"},
		"这是第 2 轮：请承接上文[前序发言]继续推进。", nil, history)
	if !utf8.ValidString(prompt) {
		t.Fatal("prompt contains invalid UTF-8 (byte-sliced multi-byte text)")
	}
	_, contextBlock, found := strings.Cut(prompt, "[本轮上下文]\n")
	if !found {
		t.Fatalf("prompt lost its context block: %q", prompt)
	}
	if got := len([]rune(contextBlock)); got > interactivePromptContextLimit {
		t.Fatalf("context block = %d runes, want <= %d", got, interactivePromptContextLimit)
	}
	for _, want := range []string{"讨论话题：", "上一轮主持纪要", "本轮已发言成员的观点"} {
		if !strings.Contains(contextBlock, want) {
			t.Errorf("context block missing %q", want)
		}
	}
	if strings.Contains(contextBlock, "第 1 轮成员发言") {
		t.Errorf("scoped context must not reach back to round 1: %q", contextBlock)
	}
}

// TestInteractivePromptInputsFromRealEvents 用真库把「事件流 → 前序发言/纪要」这条
// 聚合链路钉住：线上 2026-10-15 的「第二轮不延续第一轮」「主持人索要第 1 轮发言」
// 就是这一步只回了插话、没有任何发言导致的。
func TestInteractivePromptInputsFromRealEvents(t *testing.T) {
	repo, pool := newDiscussionTestRepo(t)
	ctx := context.Background()
	roleSnapshot := mustJSON(map[string]interface{}{
		"m1":   map[string]interface{}{"memberId": "m1", "nodeId": "n1", "nickname": "观看着", "roleName": "观看着", "speakingOrder": float64(0)},
		"m2":   map[string]interface{}{"memberId": "m2", "nodeId": "n2", "nickname": "提问者", "roleName": "提问者", "speakingOrder": float64(1)},
		"host": map[string]interface{}{"memberId": "host", "nodeId": "n3", "nickname": "中立主持人", "roleName": "中立主持人", "isHost": true},
	})
	created, err := repo.Create(ctx, CreateDiscussionParams{
		DiscussionID: uniqueID("disc"), ChatroomID: uniqueID("room"),
		WorkspaceID: testUUID(t, "550e8400-e29b-41d4-a716-446655440000"),
		CreatedBy:   "user-1", Topic: "Q4 排期", Goal: "定下里程碑",
		Mode: DiscussionModeRoundtable, Status: DiscussionStatusRunning,
		RoundPolicy:        RoundPolicyUserConfirmed,
		RoleSnapshot:       roleSnapshot,
		HostConfigSnapshot: mustJSON(map[string]interface{}{"discussion_host_prompt": "你是中立主持人"}),
	})
	if err != nil {
		t.Fatalf("create discussion: %v", err)
	}
	cleanupDiscussion(t, pool, created.DiscussionID)

	seq := int64(0)
	appendEvent := func(eventType, actor string, payload map[string]interface{}) {
		seq++
		if _, err := repo.AppendEvent(ctx, AppendDiscussionEventParams{
			DiscussionID: created.DiscussionID, Seq: seq, EventType: eventType,
			ActorID: actor, Payload: mustJSON(payload),
			IdempotencyKey: fmt.Sprintf("%s-%s-%d", eventType, actor, seq),
		}); err != nil {
			t.Fatalf("append %s: %v", eventType, err)
		}
	}
	appendEvent("contribution_completed", "m1", map[string]interface{}{"round": 1, "content": "第一轮甲的判断"})
	appendEvent("contribution_completed", "m2", map[string]interface{}{"round": 1, "content": "第一轮乙的判断"})
	appendEvent("round_summary_completed", "host", map[string]interface{}{"round": 1, "content": "第一轮纪要：共识 X"})
	appendEvent("contribution_completed", "m1", map[string]interface{}{"round": 2, "content": "第二轮甲的判断"})
	appendEvent("user_interjection", "owner", map[string]interface{}{"consumedFromRound": 2, "content": "用户插话", "messageUid": "u1"})

	record, err := repo.Get(ctx, created.DiscussionID)
	if err != nil {
		t.Fatalf("get discussion: %v", err)
	}
	record.CurrentRound = 2

	rt := &discussionRuntime{repo: repo, logger: testLogger()}
	opinions, history := rt.interactivePromptInputs(record)
	if len(opinions) != 1 || opinions[0].content != "用户插话" {
		t.Fatalf("opinions = %+v, want only the current-round interjection", opinions)
	}
	if len(history.contributions) != 3 {
		t.Fatalf("contributions = %d, want 3: %+v", len(history.contributions), history.contributions)
	}
	if len(history.summaries) != 1 {
		t.Fatalf("summaries = %+v, want 1", history.summaries)
	}
	if history.contributions[0].RoleName != "观看着" {
		t.Errorf("statement must carry the role-board label: %+v", history.contributions[0])
	}

	memberPrompt := rt.interactiveBuildPrompt(record, interactiveRole{MemberID: "m2", RoleName: "提问者"},
		"这是第 2 轮：请承接上文[前序发言]继续推进。", opinions, history)
	for _, want := range []string{"第一轮纪要：共识 X", "第一轮甲的判断", "第二轮甲的判断", "用户插话"} {
		if !strings.Contains(memberPrompt, want) {
			t.Errorf("round-2 member prompt missing %q:/n%s", want, memberPrompt)
		}
	}

	hostPrompt := rt.interactiveBuildPrompt(record, interactiveRole{MemberID: "host", RoleName: "中立主持人", IsHost: true},
		"本轮（第 2 轮）所有成员已发言完毕，请输出本轮纪要。", nil, history)
	for _, want := range []string{"你是中立主持人", "第二轮甲的判断", "第一轮纪要：共识 X"} {
		if !strings.Contains(hostPrompt, want) {
			t.Errorf("host prompt missing %q:/n%s", want, hostPrompt)
		}
	}
}

// ---- 空轮守卫（2026-10-15 线上：「明明都发言完了，主持人却说没收到发言」）------

// newFixtureDiscussion 在 fixture 的聊天室里建一条 user_confirmed 讨论：
// 一名普通成员（fixture 里那个 AI 节点）+ 一名主持人。
func newFixtureDiscussion(t *testing.T, f *discussionFixture) DiscussionRecord {
	t.Helper()
	memberRC := f.memberNode.RongcloudUserID
	roleSnapshot := mustJSON(map[string]interface{}{
		memberRC: map[string]interface{}{
			"memberId": memberRC, "nodeId": f.memberNode.NodeID,
			"nickname": "观看着", "roleName": "观看着", "speakingOrder": float64(0),
		},
		"host-rc": map[string]interface{}{
			"memberId": "host-rc", "nodeId": "host-node",
			"nickname": "中立主持人", "roleName": "中立主持人", "isHost": true,
		},
	})
	ctx := context.Background()
	// 真实链路是 createRoundtable 先建 planning 再 CAS 推进到 running 第 1 轮；
	// CreateDiscussionParams 没有轮次字段，直接建成 running 会留下 CurrentRound=0，
	// 于是事件流里 round=1 的记录全被按「不是本轮」过滤掉，守卫与推进都会看错。夹具必须
	// 复刻「先 planning、再推进」这两步。
	created, err := f.rt.repo.Create(ctx, CreateDiscussionParams{
		DiscussionID: uniqueID("disc"), ChatroomID: f.roomRCID, WorkspaceID: f.wsID,
		CreatedBy: f.ownerRCID, Topic: "AI 能发展到什么程度", Goal: "形成共识清单",
		Mode: DiscussionModeRoundtable, Status: DiscussionStatusPlanning,
		RoundPolicy: RoundPolicyUserConfirmed, RoleSnapshot: roleSnapshot,
		HostConfigSnapshot: mustJSON(map[string]interface{}{}),
	})
	if err != nil {
		t.Fatalf("create discussion: %v", err)
	}
	running, err := f.rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: created.DiscussionID, ExpectedVersion: created.Version,
		Status: DiscussionStatusRunning, CurrentRound: 1, RoundRevision: 0,
	})
	if err != nil {
		t.Fatalf("advance to running: %v", err)
	}
	return running
}

// TestInteractiveNext_FailsRoundWithNoContribution 钉住「全员被跳光仍请主持人写纪要」的
// 回归：成员节点全部超时/失败时主持人手上没有任何素材，只会回「尚未收到任何成员的
// 发言记录」，把一次真实的全员失败伪装成主持人不会写纪要。这时必须判讨论失败。
func TestInteractiveNext_FailsRoundWithNoContribution(t *testing.T) {
	f := setupDiscussionFixture(t)
	ctx := context.Background()
	created := newFixtureDiscussion(t, f)
	memberRC := f.memberNode.RongcloudUserID
	// 本轮唯一的成员两次尝试都失败 → member_skipped，一条发言都没落地。
	if _, err := f.rt.repo.AppendEvent(ctx, AppendDiscussionEventParams{
		DiscussionID: created.DiscussionID, Seq: 1, EventType: "member_skipped",
		ActorID: "system", TargetID: memberRC,
		Payload:        mustJSON(map[string]interface{}{"round": 1, "memberId": memberRC, "category": "timeout"}),
		IdempotencyKey: "member-skipped:1",
	}); err != nil {
		t.Fatalf("append member_skipped: %v", err)
	}
	d, err := f.rt.repo.Get(ctx, created.DiscussionID)
	if err != nil {
		t.Fatalf("get discussion: %v", err)
	}
	if err := f.rt.interactiveNext(ctx, d); err != nil {
		t.Fatalf("interactiveNext: %v", err)
	}
	after, err := f.rt.repo.Get(ctx, created.DiscussionID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.Status != DiscussionStatusFailed {
		t.Fatalf("status = %q, want %q（空轮必须失败，不能进汇总）", after.Status, DiscussionStatusFailed)
	}
	events, err := f.rt.repo.ListEventsAfter(ctx, created.DiscussionID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	failed, hostAsked := false, false
	for _, e := range events {
		switch e.EventType {
		case "discussion_failed":
			failed = true
			// reason 必须在 web 的严格枚举里，否则前端会把整段讨论历史判为无效。
			var payload map[string]interface{}
			_ = json.Unmarshal(e.Payload, &payload)
			if payload["reason"] != "dispatch_failed" {
				t.Errorf("failure reason = %v, want dispatch_failed", payload["reason"])
			}
		case "host_turn_requested", "round_summary_started":
			hostAsked = true
		}
	}
	if !failed {
		t.Error("空轮必须落一条 discussion_failed")
	}
	if hostAsked {
		t.Error("空轮不能派发主持人汇总")
	}
}

// TestInteractiveNext_SummarizesWhenRoundHasContribution 反向回归：有产出时照常进入
// 汇总并派发主持人 —— 空轮守卫不能把正常路径也挡掉。
func TestInteractiveNext_SummarizesWhenRoundHasContribution(t *testing.T) {
	f := setupDiscussionFixture(t)
	ctx := context.Background()
	created := newFixtureDiscussion(t, f)
	memberRC := f.memberNode.RongcloudUserID
	if _, err := f.rt.repo.AppendEvent(ctx, AppendDiscussionEventParams{
		DiscussionID: created.DiscussionID, Seq: 1, EventType: "contribution_completed",
		ActorID: memberRC, TargetID: memberRC,
		Payload:        mustJSON(map[string]interface{}{"round": 1, "content": "我的判断是……"}),
		IdempotencyKey: "complete:1",
	}); err != nil {
		t.Fatalf("append contribution: %v", err)
	}
	d, err := f.rt.repo.Get(ctx, created.DiscussionID)
	if err != nil {
		t.Fatalf("get discussion: %v", err)
	}
	if err := f.rt.interactiveNext(ctx, d); err != nil {
		t.Fatalf("interactiveNext: %v", err)
	}
	after, err := f.rt.repo.Get(ctx, created.DiscussionID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.Status != DiscussionStatusSummarizing {
		t.Fatalf("status = %q, want %q", after.Status, DiscussionStatusSummarizing)
	}
	events, err := f.rt.repo.ListEventsAfter(ctx, created.DiscussionID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	hostAsked := false
	for _, e := range events {
		if e.EventType == "host_turn_requested" || e.EventType == "round_summary_started" {
			hostAsked = true
		}
	}
	if !hostAsked {
		t.Error("有产出时必须派发主持人汇总")
	}
}

// TestInteractiveDispatch_EmptyRoundTellsHostTheTruth 手动重试主持任务的兜底路径：
// 本轮确实没有任何成员产出时，提示词要直说「没有可依据的发言」，而不是留空让主持人
// 去猜、去向用户索要材料。
func TestInteractiveDispatch_EmptyRoundTellsHostTheTruth(t *testing.T) {
	f := setupDiscussionFixture(t)
	ctx := context.Background()
	created := newFixtureDiscussion(t, f)
	d, err := f.rt.repo.Get(ctx, created.DiscussionID)
	if err != nil {
		t.Fatalf("get discussion: %v", err)
	}
	host := interactiveRole{MemberID: "host-rc", RoleName: "中立主持人", IsHost: true}
	if err := f.rt.interactiveDispatch(ctx, d, host, 1); err != nil {
		t.Fatalf("interactiveDispatch: %v", err)
	}
	commands, err := f.queries.ListRongCloudDiscussionCommandsByDiscussion(ctx, d.DiscussionID)
	if err != nil {
		t.Fatalf("list commands: %v", err)
	}
	prompted := ""
	for _, cmd := range commands {
		var task interactiveTask
		if err := json.Unmarshal(cmd.Payload, &task); err != nil {
			continue
		}
		if task.IsHost {
			prompted = task.Prompt
		}
	}
	if prompted == "" {
		t.Fatal("主持人派发没有落进 outbox")
	}
	if !strings.Contains(prompted, "没有任何成员产出") {
		t.Errorf("空轮的主持人提示词必须直说没有素材：%q", prompted)
	}
	if strings.Contains(prompted, "不要向我索要材料") {
		t.Errorf("空轮提示词不该复用「据现有发言撰写」那一版：%q", prompted)
	}
}
