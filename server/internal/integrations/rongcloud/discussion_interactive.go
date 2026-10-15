package rongcloud

// discussion_interactive.go —— P7：user_confirmed（v3）交互式轮次引擎。
//
// 移植自旧后端 discussion/interactive_rounds.py 的 InteractiveRoundCoordinator，
// 但**刻意收窄了传输层**：Python 用一套独立的 v3 设备协议
// （discussion_assignment / discussion_host_turn + wire 分帧 + delta 增量流 +
// cancel ack），而当前没有任何设备端实现它（xiachat-cli 只认 your_turn）。
// 所以这里复用**已有的 your_turn 命令通道**：服务端按 speaking_order 依次派发，
// 设备跑完 agent 回一条 command_result，服务端据此推进下一位。这样：
//
//   - 设备端（CLI）零改动，也不需要 wire/delta 两张表；
//   - C 端（clawmessenger-web）拿到的仍是**标准 v3 事件**（v3_envelope +
//     discussion_event 广播），前端 lib/discussion-v3.ts 无需改动。
//
// 服务端状态机（与 Python 同构）：
//
//	begin    planning → running(round 1)，落 discussion_started + round_started，派发首位成员
//	dispatch 按 speaking_order 找下一个「本轮未完成」的普通成员 → 落 outbox(dispatch) + 事件 + 私发 your_turn
//	全员完成 summarizing → 派发 host 出本轮纪要
//	host 回包 落 round_document + round_summary_completed → awaiting_user（等人确认）
//	continue 用户确认 → running(round+1)，可选 opinion 作为本轮插话 → 再派发
//	finish   用户结束 → completed
//	cancel   用户取消 → cancelled
//
// 关键约束（踩过的坑）：
//  1. 所有推进都基于 rongcloud_discussion.version 的 CAS（AdvanceState），
//     冲突方回 409 + 最新快照（前端靠它做重同步）。
//  2. 事件 seq 必须连续、且带 v3 信封的事件类型必须落在前端白名单里，否则
//     前端 events 尾读会整段解析失败 —— 非白名单事件（assignment_created 等）
//     在 discussionEventView 里统一降级为 discussion_progress。
//  3. 幂等：全部推进按 idempotency_key 去重（融云回调 at-least-once、设备重连
//     会整段重放离线历史）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// interactiveMaxSummaryBytes 是单轮纪要的字节上限。前端 v3 校验里
	// contribution 上限是 100_000 字符，超了会被整条丢弃。
	interactiveMaxSummaryBytes = 90_000

	// interactivePromptContextLimit 是注入到设备提示词里的上下文（话题/目标/插话）
	// 的字符上限：CLI 的 prompt 走 argv 时有 8000 字符上限，超了会退化到 stdin，
	// 这里保守收口，避免把角色人设挤没。
	interactivePromptContextLimit = 4000

	// interactiveMaxHostRetries 是主持人出纪要失败后的最大重试次数（超过即 failed）。
	interactiveMaxHostRetries = 2

	// interactiveDefaultTurnTimeout 是 host.timeout_seconds 不可用时的兜底等待上限。
	interactiveDefaultTurnTimeout = 300 * time.Second

	// interactiveMinTurnTimeout 是单轮等待的下限：一次 agent 调用不可能更快。
	interactiveMinTurnTimeout = 10 * time.Second

	// interactiveSweepInterval 是超时恢复扫描的周期。它只决定「超时被发现的
	// 滞后」，不决定超时本身（后者由 outbox 的 available_at 精确表达），
	// 所以 30s 的粒度足够，也不至于让空库上的扫描成为负担。
	interactiveSweepInterval = 30 * time.Second
)

// interactiveV3EventTypes 是 C 端前端（web/src/lib/discussion-v3.ts 的 eventData）
// 严格校验的事件类型白名单，与旧后端 protocol_v3._event_data 逐字一致。
//
// 带 v3 信封但不在白名单里的事件（assignment_created / host_turn_requested /
// control_accepted / callback_accepted / task_timeout / retry_accepted）对外必须
// 降级成 discussion_progress —— 见 discussionEventView。
var interactiveV3EventTypes = map[string]struct{}{
	"round_started": {}, "round_summary_started": {}, "round_summary_completed": {},
	"contribution_delta": {}, "contribution_completed": {}, "user_interjection": {},
	"member_retry_scheduled": {}, "member_skipped": {}, "request_interrupted": {},
	"interjection_applied": {}, "interruption_failed": {}, "discussion_started": {},
	"discussion_completed": {}, "discussion_cancelled": {}, "discussion_failed": {},
	"discussion_progress": {},
}

// isInteractiveV3EventType 报告某个事件类型是否被前端 v3 校验接受。
func isInteractiveV3EventType(eventType string) bool {
	_, ok := interactiveV3EventTypes[eventType]
	return ok
}

// interactiveRole 是角色板（role_snapshot）里的一行，按 speakingOrder 排序后作为
// 派发顺序的真实来源。memberId 就是节点的融云账号（v3 身份域），既是事件里的
// actor/target，也是私发 your_turn 的收件人。
type interactiveRole struct {
	MemberID         string
	NodeID           string
	Nickname         string
	RoleName         string
	RoleInstructions string
	Model            string
	SpeakingOrder    int
	IsHost           bool
}

// interactiveTask 是落进 rongcloud_discussion_command.payload 的任务描述
// （kind=dispatch）。command_id == requestId，设备回包按它索引回任务。
type interactiveTask struct {
	RequestID     string `json:"requestId"`
	Round         int    `json:"round"`
	RoundRevision int    `json:"roundRevision"`
	StateVersion  int    `json:"stateVersion"`
	TargetID      string `json:"targetId"`
	SpeakingOrder int    `json:"speakingOrder"`
	IsHost        bool   `json:"isHost"`
	Attempt       int    `json:"attempt"`
	Timestamp     int64  `json:"timestamp"`
	ChatroomID    string `json:"chatroomId"`
	// Prompt 是私发给设备的 your_turn params.role_prompt（人设 + 本轮上下文）。
	Prompt string `json:"prompt"`
	// Payload 是记录下来、供调试/后续 P8 对齐 v3 设备协议用的原始信封。
	Payload map[string]interface{} `json:"payload"`
}

// interactiveEvent 是一次带 v3 信封的事件写入。
type interactiveEvent struct {
	EventType string
	ActorID   string
	TargetID  string
	Payload   map[string]interface{}
	Key       string
	RequestID string
	StateVer  int
	Round     int
	Revision  int
}

// ---- 角色板解析 -----------------------------------------------------------

// interactiveRoles 解析讨论的角色快照，返回按 speakingOrder 排好序的普通成员与主持人。
// ok=false 表示快照不可用（没有普通成员或没有主持人），此时不能推进轮次。
func interactiveRoles(d DiscussionRecord) (ordinary []interactiveRole, host interactiveRole, ok bool) {
	var raw map[string]map[string]interface{}
	if err := json.Unmarshal(d.RoleSnapshot, &raw); err != nil {
		return nil, interactiveRole{}, false
	}
	for memberID, entry := range raw {
		role := interactiveRole{MemberID: memberID}
		if v, ok := entry["memberId"].(string); ok && v != "" {
			role.MemberID = v
		}
		role.NodeID, _ = entry["nodeId"].(string)
		role.Nickname, _ = entry["nickname"].(string)
		role.RoleName, _ = entry["roleName"].(string)
		role.RoleInstructions, _ = entry["roleInstructions"].(string)
		role.Model, _ = entry["model"].(string)
		if v, ok := entry["speakingOrder"].(float64); ok {
			role.SpeakingOrder = int(v)
		}
		if v, ok := entry["isHost"].(bool); ok {
			role.IsHost = v
		}
		if role.MemberID == "" {
			continue
		}
		if role.IsHost {
			host = role
			continue
		}
		ordinary = append(ordinary, role)
	}
	sort.SliceStable(ordinary, func(i, j int) bool {
		return ordinary[i].SpeakingOrder < ordinary[j].SpeakingOrder
	})
	return ordinary, host, len(ordinary) > 0 && host.MemberID != ""
}

// interactiveHostPrompt 取主持人提示词（host_config_snapshot.discussion_host_prompt）。
func interactiveHostPrompt(d DiscussionRecord) string {
	var raw map[string]interface{}
	if err := json.Unmarshal(d.HostConfigSnapshot, &raw); err != nil {
		return ""
	}
	prompt, _ := raw["discussion_host_prompt"].(string)
	return prompt
}

// ---- 事件写入 -------------------------------------------------------------

// interactiveEmit 幂等地落一条事件：同 idempotency_key 已存在就直接返回既存行
// （融云 at-least-once 回调 + 设备重放都会走到这里）。
func (rt *discussionRuntime) interactiveEmit(ctx context.Context, d DiscussionRecord, e interactiveEvent) (DiscussionEventRecord, bool, error) {
	if existing, err := rt.repo.EventByIdempotencyKey(ctx, d.DiscussionID, e.Key); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrDiscussionEventNotFound) {
		return DiscussionEventRecord{}, false, err
	}
	envelope := map[string]interface{}{
		"protocolVersion": 3,
		"discussionId":    d.DiscussionID,
		"chatroomId":      d.ChatroomID,
		"requestId":       e.RequestID,
		"stateVersion":    e.StateVer,
		"round":           e.Round,
		"roundRevision":   e.Revision,
		"timestamp":       time.Now().UnixMilli(),
	}
	if envelope["requestId"] == "" {
		envelope["requestId"] = uuid.NewString()
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		seq, err := rt.repo.NextEventSeq(ctx, d.DiscussionID)
		if err != nil {
			return DiscussionEventRecord{}, false, err
		}
		rec, err := rt.repo.AppendEvent(ctx, AppendDiscussionEventParams{
			DiscussionID:   d.DiscussionID,
			Seq:            seq,
			EventType:      e.EventType,
			ActorID:        e.ActorID,
			TargetID:       e.TargetID,
			Payload:        mustJSON(e.Payload),
			IdempotencyKey: e.Key,
			V3Envelope:     mustJSON(envelope),
		})
		if err == nil {
			return rec, true, nil
		}
		lastErr = err
		// seq 抢占失败（并发写者先落了一步）→ 重新取 seq 再试。
		if existing, gErr := rt.repo.EventByIdempotencyKey(ctx, d.DiscussionID, e.Key); gErr == nil {
			return existing, false, nil
		}
	}
	return DiscussionEventRecord{}, false, lastErr
}

// interactiveBroadcast 把一条 v3 事件广播到融云聊天室，让 C 端实时刷新。
// best-effort：广播失败只记日志（前端在用户动作/进入房间时会走 HTTP 重同步兜底）。
func (rt *discussionRuntime) interactiveBroadcast(ctx context.Context, d DiscussionRecord, rec DiscussionEventRecord) {
	if rt.client == nil {
		return
	}
	view := discussionEventView(rec)
	payload := map[string]interface{}{
		"msg_type":        "discussion_event",
		"protocolVersion": 3,
		"discussionId":    d.DiscussionID,
		"chatroomId":      d.ChatroomID,
		"seq":             rec.Seq,
		"eventType":       view["eventType"],
		"actorId":         view["actorId"],
		"targetId":        view["targetId"],
		"data":            view["payload"],
		"stateVersion":    d.Version,
		"timestamp":       time.Now().UnixMilli(),
	}
	// v3 信封里的身份字段（requestId/round/roundRevision）必须原样带上：前端会对
	// round_summary_completed 这类事件做跨字段一致性校验。
	for _, key := range []string{"requestId", "round", "roundRevision", "stateVersion"} {
		if v, ok := view[key]; ok {
			payload[key] = v
		}
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return
	}
	sender := discussionEventSender(d, rec, rt.systemNodeID)
	if err := rt.sendChatroomJSON(ctx, sender, d.ChatroomID, content); err != nil {
		rt.logger.Warn("discussion: v3 event broadcast failed",
			"discussionId", d.DiscussionID, "eventType", rec.EventType, "error", err)
	}
}

// discussionEventSender 决定一条 v3 事件广播的融云发件人。
//
// 讨论组本质是群聊：成员发言（contribution_completed）与主持人纪要
// （round_summary_completed）以**发言节点自己的融云账号**广播，C 端与原生
// IM 客户端都能按发送者归属。只有 actorId 是角色板里的成员（memberId 即节点
// 融云账号）才用节点身份；引擎事件（actorId="system"）、用户操作事件
// （actorId=操作者 rc id，不在角色板里）等一律回落到平台系统账号。
func discussionEventSender(d DiscussionRecord, rec DiscussionEventRecord, fallback string) string {
	actor := strings.TrimSpace(rec.ActorID)
	if actor == "" || actor == "system" {
		return fallback
	}
	ordinary, host, ok := interactiveRoles(d)
	if !ok {
		return fallback
	}
	if host.MemberID == actor {
		return actor
	}
	for _, role := range ordinary {
		if role.MemberID == actor {
			return actor
		}
	}
	return fallback
}

// sendChatroomJSON 把一条 command 消息发到融云聊天室，发件人由 senderID 指定
// （发言事件是节点账号，引擎/操作事件是平台系统账号）。
//
// senderID 为空时**不发**（退化到「C 端靠 HTTP 重同步」）。融云会把本端发出的
// 消息回灌 webhook：系统账号发的由 handleCommand 自环守卫拦，节点账号发的由
// handleWebhook 的 isDiscussionEventBroadcast 内容嗅探拦，两道缺一不可。
func (rt *discussionRuntime) sendChatroomJSON(ctx context.Context, senderID, chatroomID string, content []byte) error {
	if rt.client == nil {
		return errors.New("rongcloud client not available")
	}
	if senderID == "" {
		return errors.New("broadcast sender not available; skipped chatroom broadcast")
	}
	_, err := rt.client.sendChatroomMessage(ctx, senderID, chatroomID, objectNameCommand, string(content))
	return err
}

// ---- 参数化推进（CAS 重试） ------------------------------------------------

// interactiveAdvance 是「读最新 → CAS 推进」的重试包装。冲突（并发推进）时重读再试，
// 三次仍冲突才放弃 —— 调用方拿到的是 ErrDiscussionVersionConflict。
func (rt *discussionRuntime) interactiveAdvance(ctx context.Context, discussionID string, mutate func(DiscussionRecord) (AdvanceDiscussionParams, bool, error)) (DiscussionRecord, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		d, err := rt.repo.Get(ctx, discussionID)
		if err != nil {
			return DiscussionRecord{}, err
		}
		params, proceed, err := mutate(d)
		if err != nil {
			return DiscussionRecord{}, err
		}
		if !proceed {
			return d, nil
		}
		advanced, err := rt.repo.AdvanceState(ctx, params)
		if err == nil {
			return advanced, nil
		}
		if !errors.Is(err, ErrDiscussionVersionConflict) {
			return DiscussionRecord{}, err
		}
		lastErr = err
	}
	return DiscussionRecord{}, lastErr
}

// ---- begin：开局 ----------------------------------------------------------

// interactiveBegin 把 createRoundtable 建好的 planning 讨论推进成 running 的第 1 轮，
// 并派发首位成员。幂等：内部用 'start:<discussionID>' 事件作为「已开局」标记。
func (rt *discussionRuntime) interactiveBegin(ctx context.Context, discussionID string) error {
	if !rt.interactiveReady() {
		return nil
	}
	d, err := rt.repo.Get(ctx, discussionID)
	if err != nil {
		return err
	}
	if d.RoundPolicy != RoundPolicyUserConfirmed {
		return nil
	}
	if _, err := rt.repo.EventByIdempotencyKey(ctx, discussionID, "start:"+discussionID); err == nil {
		return nil // 已开局（createRoundtable 重放）
	} else if !errors.Is(err, ErrDiscussionEventNotFound) {
		return err
	}
	if _, _, ok := interactiveRoles(d); !ok {
		return fmt.Errorf("rongcloud: interactive discussion %s has no host/participants", discussionID)
	}

	// 只有把 planning 真正 CAS 成 running 的那一个调用者才继续派发。两个并发的
	// createRoundtable（同 client_request_id）会同时通过上面的幂等探针，若都往下走
	// 就会给首位成员派发两次 your_turn、让他把同一轮说两遍。
	proceeded := false
	advanced, err := rt.interactiveAdvance(ctx, discussionID, func(cur DiscussionRecord) (AdvanceDiscussionParams, bool, error) {
		if cur.Status != DiscussionStatusPlanning {
			// 并发推进者已经开过局：不再重复落事件、不重复派发。
			return AdvanceDiscussionParams{}, false, nil
		}
		proceeded = true
		return AdvanceDiscussionParams{
			DiscussionID: cur.DiscussionID, ExpectedVersion: cur.Version,
			Status: DiscussionStatusRunning, CurrentRound: 1, RoundRevision: 0,
		}, true, nil
	})
	if err != nil {
		return err
	}
	if !proceeded {
		return nil
	}
	start, _, err := rt.interactiveEmit(ctx, advanced, interactiveEvent{
		EventType: "discussion_started", ActorID: advanced.CreatedBy,
		Payload: map[string]interface{}{"roundPolicy": RoundPolicyUserConfirmed},
		Key:     "start:" + discussionID, RequestID: "", StateVer: advanced.Version,
		Round: 1, Revision: 0,
	})
	if err != nil {
		return err
	}
	rt.interactiveBroadcast(ctx, advanced, start)

	roundStarted, _, err := rt.interactiveEmit(ctx, advanced, interactiveEvent{
		EventType: "round_started", ActorID: "system",
		Payload:  map[string]interface{}{"round": 1, "roundFocus": ""},
		Key:      fmt.Sprintf("round-start:%s:1", discussionID),
		StateVer: advanced.Version,
		Round:    1,
		Revision: 0,
	})
	if err != nil {
		return err
	}
	rt.interactiveBroadcast(ctx, advanced, roundStarted)

	fresh, err := rt.repo.Get(ctx, discussionID)
	if err != nil {
		return err
	}
	return rt.interactiveNext(ctx, fresh)
}

// ---- 派发 -----------------------------------------------------------------

// interactiveNext 按 speaking_order 找本轮下一个未完成的普通成员；都完成了就转入
// summarizing 并派发主持人出纪要。
func (rt *discussionRuntime) interactiveNext(ctx context.Context, d DiscussionRecord) error {
	if !IsLiveDiscussionStatus(d.Status) || d.Status == DiscussionStatusSummarizing {
		return nil
	}
	ordinary, host, ok := interactiveRoles(d)
	if !ok {
		return fmt.Errorf("rongcloud: interactive discussion %s role snapshot unusable", d.DiscussionID)
	}
	done, err := rt.interactiveRoundDone(ctx, d)
	if err != nil {
		return err
	}
	for _, role := range ordinary {
		if done[role.MemberID] {
			continue
		}
		if !rt.interactiveMemberEnabled(ctx, d, role.MemberID) {
			skipped, _, emitErr := rt.interactiveEmit(ctx, d, interactiveEvent{
				EventType: "member_skipped", ActorID: "system", TargetID: role.MemberID,
				Payload: map[string]interface{}{"round": d.CurrentRound, "memberId": role.MemberID,
					"reason": "Member is disabled or no longer available"},
				Key:       fmt.Sprintf("skip:%d:%s", d.Version, role.MemberID),
				RequestID: uuid.NewString(),
				StateVer:  d.Version,
				Round:     d.CurrentRound,
				Revision:  d.RoundRevision,
			})
			if emitErr != nil {
				return emitErr
			}
			rt.interactiveBroadcast(ctx, d, skipped)
			continue
		}
		return rt.interactiveDispatch(ctx, d, role, 1)
	}
	// 本轮全员完成 → 主持人汇总。
	summarizing, err := rt.interactiveAdvance(ctx, d.DiscussionID, func(cur DiscussionRecord) (AdvanceDiscussionParams, bool, error) {
		if !IsLiveDiscussionStatus(cur.Status) || cur.Status == DiscussionStatusSummarizing {
			return AdvanceDiscussionParams{}, false, nil
		}
		return AdvanceDiscussionParams{
			DiscussionID: cur.DiscussionID, ExpectedVersion: cur.Version,
			Status: DiscussionStatusSummarizing, CurrentRound: cur.CurrentRound,
			RoundRevision: cur.RoundRevision,
		}, true, nil
	})
	if err != nil {
		return err
	}
	return rt.interactiveDispatch(ctx, summarizing, host, 1)
}

// interactiveRoundDone 返回本轮已「完成或跳过」的成员集合。
func (rt *discussionRuntime) interactiveRoundDone(ctx context.Context, d DiscussionRecord) (map[string]bool, error) {
	events, err := rt.repo.ListEventsAfter(ctx, d.DiscussionID, 0)
	if err != nil {
		return nil, err
	}
	done := map[string]bool{}
	for _, e := range events {
		var payload map[string]interface{}
		_ = json.Unmarshal(e.Payload, &payload)
		if round, ok := payload["round"].(float64); !ok || int(round) != d.CurrentRound {
			continue
		}
		switch e.EventType {
		case "contribution_completed":
			if e.ActorID != "" {
				done[e.ActorID] = true
			}
		case "member_skipped":
			if member, ok := payload["memberId"].(string); ok && member != "" {
				done[member] = true
			}
		}
	}
	return done, nil
}

// interactiveMemberEnabled 判定角色板里的成员此刻是否仍是该聊天室的启用 AI 成员。
func (rt *discussionRuntime) interactiveMemberEnabled(ctx context.Context, d DiscussionRecord, memberID string) bool {
	room, err := rt.queries.GetRongCloudChatroomViewByRongCloudID(ctx, d.ChatroomID)
	if err != nil {
		return false
	}
	members, err := rt.queries.ListRongCloudChatroomMembersWithNode(ctx, room.ID)
	if err != nil {
		return false
	}
	for _, m := range members {
		if m.Enabled && m.MemberType == "ai" && m.MemberID.Valid && m.MemberID.String == memberID {
			return true
		}
	}
	return false
}

// interactiveBuildPrompt 拼出私发给设备的你的角色提示词：人设 + 本轮上下文
// （话题/目标 + 本轮用户插话）。设备端（CLI）把 role_prompt 原样塞进 agent 提示词，
// 所以插话就是从这里进入后续 agent 的上下文 —— 轮次指针不受影响。
func (rt *discussionRuntime) interactiveBuildPrompt(d DiscussionRecord, role interactiveRole, extra string, opinions []interactiveOpinion) string {
	parts := make([]string, 0, 4)
	if role.RoleInstructions != "" {
		parts = append(parts, role.RoleInstructions)
	}
	if role.IsHost {
		if hostPrompt := interactiveHostPrompt(d); hostPrompt != "" {
			parts = append(parts, hostPrompt)
		}
	}
	context := []string{}
	if d.Topic != "" {
		context = append(context, "讨论话题："+d.Topic)
	}
	if d.Goal != "" {
		context = append(context, "讨论目标："+d.Goal)
	}
	if len(opinions) > 0 {
		lines := []string{"本轮用户插话（必须纳入你的发言）："}
		for _, opinion := range opinions {
			lines = append(lines, "- "+opinion.content)
		}
		context = append(context, strings.Join(lines, "\n"))
	}
	if extra != "" {
		context = append(context, extra)
	}
	if len(context) > 0 {
		block := "[本轮上下文]\n" + strings.Join(context, "\n")
		if len(block) > interactivePromptContextLimit {
			block = block[:interactivePromptContextLimit]
		}
		parts = append(parts, block)
	}
	return strings.Join(parts, "\n\n")
}

// interactiveOpinion 是一条被本轮消费的用户插话：messageUID 用于回执（interjection_applied）
// 与前端气泡对账，content 用于拼进提示词。
type interactiveOpinion struct {
	messageUID string
	content    string
}

// interactiveConsumedOpinions 返回「本轮消费」的用户插话（consumedFromRound == 当前轮）。
// 与旧后端 _context 的取值一致：插话只在被投递的那一轮生效，不跨轮重复注入。
func (rt *discussionRuntime) interactiveConsumedOpinions(d DiscussionRecord) []interactiveOpinion {
	events, err := rt.repo.ListEventsAfter(context.Background(), d.DiscussionID, 0)
	if err != nil {
		return nil
	}
	out := []interactiveOpinion{}
	for _, e := range events {
		if e.EventType != "user_interjection" {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		round, _ := payload["consumedFromRound"].(float64)
		if int(round) != d.CurrentRound {
			continue
		}
		content, _ := payload["content"].(string)
		if content == "" {
			continue
		}
		uid, _ := payload["messageUid"].(string)
		out = append(out, interactiveOpinion{messageUID: uid, content: content})
	}
	return out
}

// interactiveRoundAllDone 判断本轮所有普通成员是否都已「完成或跳过」。
// 用于插话归属判定：全员已说完时本轮不会再派发，插话必须顺延到下一轮。
func (rt *discussionRuntime) interactiveRoundAllDone(ctx context.Context, d DiscussionRecord) bool {
	ordinary, _, ok := interactiveRoles(d)
	if !ok || len(ordinary) == 0 {
		return false
	}
	done, err := rt.interactiveRoundDone(ctx, d)
	if err != nil {
		return false
	}
	for _, role := range ordinary {
		if !done[role.MemberID] {
			return false
		}
	}
	return true
}

// interactiveInterjectionRound 返回这条插话应该由哪一轮消费。
// 不能一律记成当前轮：轮末（本轮全员已说完 / 正在汇总 / 已进入
// 等人确认）与未开局（planning，轮次还是 0）时，当前轮不会再派发给
// 任何成员，consumedFromRound 永不匹配 → 插话被静默丢弃。
//
// 返回 0 表示已无后续轮次可承载（仅 legacy_capped 有上限时可能发生；
// user_confirmed 的 MaxRounds 为 nil，下一轮总是存在）。
func (rt *discussionRuntime) interactiveInterjectionRound(ctx context.Context, d DiscussionRecord) int {
	allDone := false
	if d.Status != DiscussionStatusPlanning && d.CurrentRound > 0 &&
		d.Status != DiscussionStatusSummarizing && d.Status != DiscussionStatusAwaitingUser {
		allDone = rt.interactiveRoundAllDone(ctx, d)
	}
	return interactiveInterjectionRoundFor(d.Status, d.CurrentRound, allDone, d.MaxRounds)
}

// interactiveInterjectionRoundFor 是纯函数版的归属判定（便于回归测试）。
func interactiveInterjectionRoundFor(status string, currentRound int, roundAllDone bool, maxRounds *int) int {
	var round int
	switch {
	case status == DiscussionStatusPlanning || currentRound <= 0:
		// 尚未开局（创建后引擎还没 CAS 到 running）：插话归首轮。
		round = 1
	case status == DiscussionStatusSummarizing || status == DiscussionStatusAwaitingUser:
		// 本轮已进入汇总/等人确认，普通成员不会再被派发 → 下一轮。
		round = currentRound + 1
	default:
		if roundAllDone {
			// running 但本轮全员已说完（推进间隙）→ 下一轮。
			round = currentRound + 1
		} else {
			round = currentRound
		}
	}
	if round < 1 {
		round = 1
	}
	if maxRounds != nil && round > *maxRounds {
		return 0
	}
	return round
}

// interactiveDispatch 落 outbox + 事件，并把 your_turn 私发给目标节点。
func (rt *discussionRuntime) interactiveDispatch(ctx context.Context, d DiscussionRecord, role interactiveRole, attempt int) error {
	if !IsLiveDiscussionStatus(d.Status) {
		return nil
	}
	requestID := uuid.NewString()
	action := "assignment"
	eventType := "assignment_created"
	extra := "请针对当前轮次给出你的分析。"
	if role.IsHost {
		action = "summary"
		eventType = "host_turn_requested"
		extra = fmt.Sprintf("本轮（第 %d 轮）所有成员已发言完毕，请输出本轮纪要："+
			"成员立场、已达成的共识、仍存在的分歧、以及下一轮建议聚焦的问题。", d.CurrentRound)
	}
	// 本轮被消费的用户插话：同一份快照同时用于提示词拼接与回执落库，
	// 避免重复全量拉事件，也避免两处看到不同快照。
	opinions := rt.interactiveConsumedOpinions(d)
	prompt := rt.interactiveBuildPrompt(d, role, extra, opinions)

	// 插话消费回执：本轮真正注入该成员提示词的插话落 interjection_applied，
	// 前端据此把气泡从「已接受」升级为「已纳入发言参考」。
	//
	// payload 必须严格对齐 v3 契约：前端 shape 校验不允许多余字段，且要求
	// 信封 requestId == payload.replacementRequestId，否则整条事件被判无效丢弃。
	// key 不含成员：同一条插话只需一条「已纳入」回执，否则时间线会重复堆叠。
	for _, opinion := range opinions {
		if opinion.messageUID == "" {
			continue
		}
		applied, created, emitErr := rt.interactiveEmit(ctx, d, interactiveEvent{
			EventType: "interjection_applied", ActorID: role.MemberID, TargetID: role.MemberID,
			Payload: map[string]interface{}{
				"messageUid":           opinion.messageUID,
				"targetRequestId":      nil,
				"replacementRequestId": requestID,
			},
			Key:       "interjection-applied:" + opinion.messageUID,
			RequestID: requestID, StateVer: d.Version, Round: d.CurrentRound, Revision: d.RoundRevision,
		})
		if emitErr != nil {
			break
		}
		if created {
			rt.interactiveBroadcast(ctx, d, applied)
		}
	}

	timestamp := time.Now().UnixMilli()
	payload := map[string]interface{}{
		"msg_type":        "discussion_assignment",
		"protocolVersion": 3,
		"discussionId":    d.DiscussionID,
		"chatroomId":      d.ChatroomID,
		"requestId":       requestID,
		"stateVersion":    d.Version,
		"round":           d.CurrentRound,
		"roundRevision":   d.RoundRevision,
		"timestamp":       timestamp,
		"assignmentId":    requestID,
		"action":          action,
		"targetId":        role.MemberID,
		"topic":           d.Topic,
		"goal":            d.Goal,
		"model":           nullableString(role.Model),
		"role": map[string]interface{}{
			"roleName":         role.RoleName,
			"roleInstructions": role.RoleInstructions,
		},
		"speakingOrder": role.SpeakingOrder,
		"roundFocus":    "",
		"task":          extra,
		"attempt":       attempt,
	}
	if role.IsHost {
		payload["msg_type"] = "discussion_host_turn"
		payload["phase"] = "round_summary"
		payload["allowedDecisions"] = []string{"checkpoint"}
	}

	task := interactiveTask{
		RequestID: requestID, Round: d.CurrentRound, RoundRevision: d.RoundRevision,
		StateVersion: d.Version, TargetID: role.MemberID, SpeakingOrder: role.SpeakingOrder,
		IsHost: role.IsHost, Attempt: attempt, Timestamp: timestamp,
		ChatroomID: d.ChatroomID, Prompt: prompt, Payload: payload,
	}

	// 先落 outbox 再发事件/发消息：outbox 行是「这一轮在等谁」的唯一持久记录，
	// 超时恢复扫描靠它把讨论从「等一个永远不回来的回包」里救出来。反过来写
	//（先发事件后落 outbox）时，outbox 插入失败会让讨论永久停在 summarizing、
	// 且没有任何东西会来接管 —— 只能靠人工干预。
	if _, err := rt.queries.InsertRongCloudDiscussionCommand(ctx, db.InsertRongCloudDiscussionCommandParams{
		CommandID:    requestID,
		DiscussionID: d.DiscussionID,
		RequestID:    requestID,
		Kind:         "dispatch",
		Payload:      mustJSON(task),
		AvailableAt:  pgtype.Timestamptz{Time: time.Now().Add(rt.interactiveTurnTimeout()), Valid: true},
	}); err != nil {
		return err
	}

	// 主持人派发时同时落 round_summary_started（前端 Timeline 用它表示「正在汇总」）。
	if role.IsHost {
		started, _, err := rt.interactiveEmit(ctx, d, interactiveEvent{
			EventType: "round_summary_started", ActorID: "system", TargetID: role.MemberID,
			Payload:   map[string]interface{}{"round": d.CurrentRound},
			Key:       "summary-start:" + requestID,
			RequestID: requestID, StateVer: d.Version, Round: d.CurrentRound, Revision: d.RoundRevision,
		})
		if err != nil {
			return err
		}
		rt.interactiveBroadcast(ctx, d, started)
	}

	event, _, err := rt.interactiveEmit(ctx, d, interactiveEvent{
		EventType: eventType, ActorID: "system", TargetID: role.MemberID,
		Payload: payload, Key: "task:" + requestID,
		RequestID: requestID, StateVer: d.Version, Round: d.CurrentRound, Revision: d.RoundRevision,
	})
	if err != nil {
		return err
	}
	rt.interactiveBroadcast(ctx, d, event)

	// 私发 your_turn（复用设备端已有协议）。失败不阻断：outbox 里的 dispatched 记录
	// 会被恢复扫描按超时处理，前端也不会因为一次发送失败而卡死。
	params := map[string]interface{}{
		"round":          d.CurrentRound,
		"speaking_order": role.SpeakingOrder,
		"role_name":      role.RoleName,
		"chatroom_id":    d.ChatroomID,
		"role_prompt":    prompt,
	}
	if role.Model != "" {
		params["model"] = role.Model
	}
	content, _ := json.Marshal(map[string]interface{}{
		"request_id": requestID,
		"service":    "discussion",
		"action":     "your_turn",
		"params":     params,
	})
	if err := rt.sendPrivateJSON(ctx, role.MemberID, content); err != nil {
		rt.logger.Warn("discussion: your_turn dispatch failed",
			"discussionId", d.DiscussionID, "target", role.MemberID, "error", err)
	}
	return nil
}

// ---- 设备回包 -------------------------------------------------------------

// interactiveAcceptTurn 处理设备对一次 your_turn 的回包（成功或失败）。
// 返回值报告这条消息是否属于本次讨论的派发 —— false 时 webhook 会继续尝试其它
// 消费方（旧协调器）。
func (rt *discussionRuntime) interactiveAcceptTurn(ctx context.Context, senderID, requestID, content string, failed bool, errMsg string) bool {
	if !rt.interactiveReady() || requestID == "" {
		return false
	}
	cmd, err := rt.queries.GetRongCloudDiscussionCommand(ctx, requestID)
	if err != nil || cmd.Kind != "dispatch" {
		return false
	}
	if cmd.State != "pending" && cmd.State != "sent" {
		return true // 重复回包（设备重放）→ 已处理过
	}
	d, err := rt.repo.Get(ctx, cmd.DiscussionID)
	if err != nil {
		return true
	}
	if d.RoundPolicy != RoundPolicyUserConfirmed {
		return true
	}
	var task interactiveTask
	if err := json.Unmarshal(cmd.Payload, &task); err != nil {
		return true
	}
	if task.TargetID != "" && senderID != "" && task.TargetID != senderID {
		rt.logger.Warn("discussion: turn answer from unexpected node",
			"discussionId", d.DiscussionID, "expected", task.TargetID, "got", senderID)
		return true
	}
	if _, err := rt.queries.SettleRongCloudDiscussionCommand(ctx, db.SettleRongCloudDiscussionCommandParams{
		State: "acknowledged", CommandID: requestID,
	}); err != nil {
		rt.logger.Warn("discussion: settle dispatch failed", "error", err, "requestId", requestID)
	}
	if !IsLiveDiscussionStatus(d.Status) || d.Status == DiscussionStatusCancelled ||
		d.Status == DiscussionStatusCompleted || d.Status == DiscussionStatusFailed {
		return true
	}
	if task.Round != d.CurrentRound {
		return true // 上一轮的迟到回包：丢弃（轮次指针不回退）
	}
	if failed {
		rt.interactiveTurnFailed(ctx, d, task, errMsg)
		return true
	}
	if task.IsHost {
		rt.interactiveCompleteSummary(ctx, d, task, content)
		return true
	}
	rt.interactiveCompleteMember(ctx, d, task, content)
	return true
}

// interactiveCompleteMember 记录一位成员的发言，并接力派发下一位。
func (rt *discussionRuntime) interactiveCompleteMember(ctx context.Context, d DiscussionRecord, task interactiveTask, content string) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		rt.interactiveTurnFailed(ctx, d, task, "empty contribution")
		return
	}
	advanced, err := rt.interactiveAdvance(ctx, d.DiscussionID, func(cur DiscussionRecord) (AdvanceDiscussionParams, bool, error) {
		// 中途被取消/终结 → 放弃这次推进（迟到的设备回包不该把讨论拉回 running）。
		// summarizing 同理：本轮已进入汇总，普通成员的回包不再计数。
		if !IsLiveDiscussionStatus(cur.Status) || cur.CurrentRound != task.Round ||
			cur.Status == DiscussionStatusSummarizing {
			return AdvanceDiscussionParams{}, false, nil
		}
		return AdvanceDiscussionParams{
			DiscussionID: cur.DiscussionID, ExpectedVersion: cur.Version,
			Status: DiscussionStatusRunning, CurrentRound: cur.CurrentRound,
			RoundRevision: cur.RoundRevision,
		}, true, nil
	})
	if err != nil {
		rt.logger.Warn("discussion: advance after contribution failed", "error", err)
		return
	}
	event, _, emitErr := rt.interactiveEmit(ctx, advanced, interactiveEvent{
		EventType: "contribution_completed", ActorID: task.TargetID, TargetID: task.TargetID,
		Payload:   map[string]interface{}{"round": task.Round, "content": content},
		Key:       "complete:" + task.RequestID,
		RequestID: task.RequestID, StateVer: advanced.Version, Round: task.Round,
		Revision: task.RoundRevision,
	})
	if emitErr != nil {
		rt.logger.Warn("discussion: append contribution event failed", "error", emitErr)
		return
	}
	rt.interactiveBroadcast(ctx, advanced, event)
	if err := rt.interactiveNext(ctx, advanced); err != nil {
		rt.logger.Warn("discussion: dispatch next turn failed",
			"discussionId", d.DiscussionID, "error", err)
	}
}

// interactiveCompleteSummary 落主持人本轮纪要（round_document）并转 awaiting_user。
func (rt *discussionRuntime) interactiveCompleteSummary(ctx context.Context, d DiscussionRecord, task interactiveTask, content string) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		rt.interactiveTurnFailed(ctx, d, task, "empty summary")
		return
	}
	if len(trimmed) > interactiveMaxSummaryBytes {
		trimmed = trimmed[:interactiveMaxSummaryBytes]
	}
	documentID := uuid.NewString()
	title := fmt.Sprintf("Round %d summary", task.Round)
	doc, err := rt.queries.InsertRongCloudRoundDocument(ctx, db.InsertRongCloudRoundDocumentParams{
		DocumentID: documentID, DiscussionID: d.DiscussionID, Round: int32(task.Round),
		Revision: int32(task.RoundRevision), SourceRequestID: task.RequestID,
		Title: title, Content: trimmed,
	})
	if err != nil {
		// 重放（唯一键冲突 → ON CONFLICT DO NOTHING 返回 ErrNoRows）：读回既存文档。
		existing, gErr := rt.queries.GetRongCloudRoundDocumentByIdentity(ctx, db.GetRongCloudRoundDocumentByIdentityParams{
			DiscussionID: d.DiscussionID, SourceRequestID: task.RequestID,
			Round: int32(task.Round), Revision: int32(task.RoundRevision),
		})
		if gErr != nil {
			rt.logger.Warn("discussion: persist round document failed", "error", err)
			return
		}
		doc = existing
	}
	advanced, advErr := rt.interactiveAdvance(ctx, d.DiscussionID, func(cur DiscussionRecord) (AdvanceDiscussionParams, bool, error) {
		// 迟到的汇总回包：轮次已变 / 已被取消 / 已进入等人确认 → 不再改状态。
		if !IsLiveDiscussionStatus(cur.Status) || cur.CurrentRound != task.Round ||
			cur.Status == DiscussionStatusAwaitingUser {
			return AdvanceDiscussionParams{}, false, nil
		}
		return AdvanceDiscussionParams{
			DiscussionID: cur.DiscussionID, ExpectedVersion: cur.Version,
			Status: DiscussionStatusAwaitingUser, CurrentRound: cur.CurrentRound,
			RoundRevision: cur.RoundRevision,
		}, true, nil
	})
	if advErr != nil {
		rt.logger.Warn("discussion: advance to awaiting_user failed", "error", advErr)
		return
	}
	event, _, emitErr := rt.interactiveEmit(ctx, advanced, interactiveEvent{
		EventType: "round_summary_completed", ActorID: task.TargetID, TargetID: task.TargetID,
		Payload: map[string]interface{}{
			"round": task.Round, "revision": task.RoundRevision,
			"sourceRequestId": task.RequestID, "documentId": doc.DocumentID,
			"title": doc.Title, "content": doc.Content,
		},
		Key:       "round-summary-completed:" + task.RequestID,
		RequestID: task.RequestID, StateVer: advanced.Version, Round: task.Round,
		Revision: task.RoundRevision,
	})
	if emitErr != nil {
		rt.logger.Warn("discussion: append round summary event failed", "error", emitErr)
		return
	}
	rt.interactiveBroadcast(ctx, advanced, event)
}

// interactiveTurnFailed 处理一次派发失败（设备回错 / 超时）：
// 普通成员先重试一次，再跳过；主持人重试到上限后判讨论失败。
func (rt *discussionRuntime) interactiveTurnFailed(ctx context.Context, d DiscussionRecord, task interactiveTask, reason string) {
	category := "timeout"
	if reason != "" {
		category = "model_error"
	}
	failure, _, err := rt.interactiveEmit(ctx, d, interactiveEvent{
		EventType: "task_failed", ActorID: "system", TargetID: task.TargetID,
		Payload:   map[string]interface{}{"category": category},
		Key:       "task-failure:" + task.RequestID,
		RequestID: task.RequestID, StateVer: d.Version, Round: task.Round,
		Revision: task.RoundRevision,
	})
	if err != nil {
		rt.logger.Warn("discussion: append task_failed event failed", "error", err)
		return
	}
	rt.interactiveBroadcast(ctx, d, failure)

	if task.IsHost {
		retries := rt.interactiveHostRetryCount(ctx, d)
		if retries >= interactiveMaxHostRetries {
			advanced, advErr := rt.interactiveAdvance(ctx, d.DiscussionID, func(cur DiscussionRecord) (AdvanceDiscussionParams, bool, error) {
				if !IsLiveDiscussionStatus(cur.Status) {
					// 并发终结（用户取消）已经落定：不要再把它改写成 failed。
					return AdvanceDiscussionParams{}, false, nil
				}
				return AdvanceDiscussionParams{
					DiscussionID: cur.DiscussionID, ExpectedVersion: cur.Version,
					Status: DiscussionStatusFailed, CurrentRound: cur.CurrentRound,
					RoundRevision: cur.RoundRevision,
				}, true, nil
			})
			if advErr != nil {
				rt.logger.Warn("discussion: mark failed failed", "error", advErr)
				return
			}
			event, _, emitErr := rt.interactiveEmit(ctx, advanced, interactiveEvent{
				EventType: "discussion_failed", ActorID: "system",
				Payload:   map[string]interface{}{"reason": "summary_failed", "retryable": true},
				Key:       "summary-failed:" + task.RequestID,
				RequestID: task.RequestID, StateVer: advanced.Version, Round: task.Round,
				Revision: task.RoundRevision,
			})
			if emitErr == nil {
				rt.interactiveBroadcast(ctx, advanced, event)
			}
			return
		}
		retry, _, emitErr := rt.interactiveEmit(ctx, d, interactiveEvent{
			EventType: "member_retry_scheduled", ActorID: "system", TargetID: task.TargetID,
			Payload: map[string]interface{}{"round": task.Round, "memberId": task.TargetID,
				"attempt": 2, "category": category},
			Key:       "host-retry:" + task.RequestID,
			RequestID: task.RequestID, StateVer: d.Version, Round: task.Round,
			Revision: task.RoundRevision,
		})
		if emitErr == nil {
			rt.interactiveBroadcast(ctx, d, retry)
		}
		_, host, ok := interactiveRoles(d)
		if ok {
			if err := rt.interactiveDispatch(ctx, d, host, task.Attempt+1); err != nil {
				rt.logger.Warn("discussion: host retry dispatch failed", "error", err)
			}
		}
		return
	}

	if task.Attempt <= 1 {
		retry, _, emitErr := rt.interactiveEmit(ctx, d, interactiveEvent{
			EventType: "member_retry_scheduled", ActorID: "system", TargetID: task.TargetID,
			Payload: map[string]interface{}{"round": task.Round, "memberId": task.TargetID,
				"attempt": 2, "category": category},
			Key:       "member-retry:" + task.RequestID,
			RequestID: task.RequestID, StateVer: d.Version, Round: task.Round,
			Revision: task.RoundRevision,
		})
		if emitErr == nil {
			rt.interactiveBroadcast(ctx, d, retry)
		}
		_, _, ok := interactiveRoles(d)
		if ok {
			role := rt.interactiveRoleByID(d, task.TargetID)
			if err := rt.interactiveDispatch(ctx, d, role, task.Attempt+1); err != nil {
				rt.logger.Warn("discussion: member retry dispatch failed", "error", err)
			}
		}
		return
	}
	skipped, _, emitErr := rt.interactiveEmit(ctx, d, interactiveEvent{
		EventType: "member_skipped", ActorID: "system", TargetID: task.TargetID,
		Payload: map[string]interface{}{"round": task.Round, "memberId": task.TargetID,
			"category": category},
		Key:       "member-skipped:" + task.RequestID,
		RequestID: task.RequestID, StateVer: d.Version, Round: task.Round,
		Revision: task.RoundRevision,
	})
	if emitErr != nil {
		rt.logger.Warn("discussion: append member_skipped failed", "error", emitErr)
		return
	}
	rt.interactiveBroadcast(ctx, d, skipped)
	if err := rt.interactiveNext(ctx, d); err != nil {
		rt.logger.Warn("discussion: dispatch after skip failed", "error", err)
	}
}

// interactiveHostRetryCount 统计本讨论里主持人已被重试的次数。
func (rt *discussionRuntime) interactiveHostRetryCount(ctx context.Context, d DiscussionRecord) int {
	_, host, ok := interactiveRoles(d)
	if !ok {
		return 0
	}
	events, err := rt.repo.ListEventsAfter(ctx, d.DiscussionID, 0)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range events {
		if e.EventType != "member_retry_scheduled" {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if member, _ := payload["memberId"].(string); member == host.MemberID {
			count++
		}
	}
	return count
}

// interactiveRoleByID 从角色板里取一个成员（含主持人）。
func (rt *discussionRuntime) interactiveRoleByID(d DiscussionRecord, memberID string) interactiveRole {
	ordinary, host, _ := interactiveRoles(d)
	if host.MemberID == memberID {
		return host
	}
	for _, role := range ordinary {
		if role.MemberID == memberID {
			return role
		}
	}
	return interactiveRole{MemberID: memberID}
}

// ---- 超时恢复 -------------------------------------------------------------

// interactiveSweep 扫描到期的派发命令（可用时间即超时时刻），按超时处理。
// 由 channel 的恢复 ticker 周期调用；进程重启后也能续上（outbox 是持久的）。
func (rt *discussionRuntime) interactiveSweep(ctx context.Context) {
	if !rt.interactiveReady() {
		return
	}
	due, err := rt.queries.ListDueRongCloudDiscussionCommands(ctx, 32)
	if err != nil {
		rt.logger.Warn("discussion: sweep list due commands failed", "error", err)
		return
	}
	for _, cmd := range due {
		var task interactiveTask
		if err := json.Unmarshal(cmd.Payload, &task); err != nil {
			continue
		}
		d, err := rt.repo.Get(ctx, cmd.DiscussionID)
		if err != nil {
			continue
		}
		if !IsLiveDiscussionStatus(d.Status) || task.Round != d.CurrentRound {
			_, _ = rt.queries.SettleRongCloudDiscussionCommand(ctx, db.SettleRongCloudDiscussionCommandParams{
				State: "failed", CommandID: cmd.CommandID,
			})
			continue
		}
		if _, err := rt.queries.SettleRongCloudDiscussionCommand(ctx, db.SettleRongCloudDiscussionCommandParams{
			State: "failed", CommandID: cmd.CommandID,
		}); err != nil {
			continue
		}
		timeout, _, emitErr := rt.interactiveEmit(ctx, d, interactiveEvent{
			EventType: "task_timeout", ActorID: "system", TargetID: task.TargetID,
			Payload:   map[string]interface{}{},
			Key:       "timeout:" + cmd.CommandID,
			RequestID: cmd.CommandID, StateVer: d.Version, Round: task.Round,
			Revision: task.RoundRevision,
		})
		if emitErr == nil {
			rt.interactiveBroadcast(ctx, d, timeout)
		}
		rt.logger.Warn("discussion: turn timed out",
			"discussionId", d.DiscussionID, "target", task.TargetID, "isHost", task.IsHost)
		rt.interactiveTurnFailed(ctx, d, task, "")
	}
}

// interactiveTurnTimeout 是单轮等待上限：优先用系统主持配置的 timeout_seconds。
func (rt *discussionRuntime) interactiveTurnTimeout() time.Duration {
	timeout := interactiveDefaultTurnTimeout
	if rt.systemHostLoader != nil {
		if cfg, found, err := rt.systemHostLoader(context.Background()); err == nil && found && cfg.TimeoutSeconds > 0 {
			timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
		}
	}
	if timeout < interactiveMinTurnTimeout {
		timeout = interactiveMinTurnTimeout
	}
	return timeout
}

// ---- 用户动作（v3 分支） ---------------------------------------------------

// interactiveControl 实现 continue / finish（对齐 Python _control）：owner-only、
// 按 requestId 幂等、版本校验、只允许在 awaiting_user 时推进。
func (rt *discussionRuntime) interactiveControl(ctx context.Context, cmd CommandContent, d DiscussionRecord, ownerRCID, actorRCID string, finish bool) commandResult {
	if !canControl(d, ownerRCID, actorRCID) {
		return errResult(403, "only the discussion owner may control an interactive discussion")
	}
	requestID := paramTrim(cmd, "requestId", "request_id")
	if requestID == "" || len(requestID) > 128 {
		return errResult(400, "requestId is required")
	}
	opinion := paramTrim(cmd, "opinion")
	if len(opinion) > 8000 {
		return errResult(400, "opinion exceeds 8000 characters")
	}
	key := "interactive-control:" + requestID
	identity := map[string]interface{}{
		"actor": actorRCID, "action": map[bool]string{true: "finish", false: "continue"}[finish],
		"expectedVersion": d.Version, "opinion": opinion,
	}
	if !finish && opinion != "" && strings.TrimSpace(opinion) == "" {
		return errResult(400, "invalid continuation opinion")
	}
	if prior, err := rt.repo.EventByIdempotencyKey(ctx, d.DiscussionID, key); err == nil {
		var priorPayload map[string]interface{}
		_ = json.Unmarshal(prior.Payload, &priorPayload)
		if !sameJSONObject(priorPayload, identity) {
			return errResult(400, "conflicting control request identity")
		}
		return okResult(discussionSnapshotView(d, rt.latestArtifactPtr(ctx, d.DiscussionID)))
	}
	if version, ok := paramInt(cmd, "stateVersion", "state_version", "version"); !ok || version != d.Version {
		return rt.versionConflictResult(ctx, d.DiscussionID)
	}
	if d.Status != DiscussionStatusAwaitingUser {
		return errResult(409, "discussion is not awaiting user")
	}

	nextRound := d.CurrentRound
	status := DiscussionStatusCompleted
	if !finish {
		status = DiscussionStatusRunning
		nextRound = d.CurrentRound + 1
	}
	advanced, advErr := rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: d.DiscussionID, ExpectedVersion: d.Version,
		Status: status, CurrentRound: nextRound, RoundRevision: 0,
	})
	if advErr != nil {
		return rt.advanceErrorResult(ctx, advErr, d.DiscussionID)
	}

	accepted, _, emitErr := rt.interactiveEmit(ctx, advanced, interactiveEvent{
		EventType: "control_accepted", ActorID: actorRCID, Payload: identity, Key: key,
		RequestID: requestID, StateVer: advanced.Version, Round: nextRound, Revision: 0,
	})
	if emitErr != nil {
		return errResult(500, "写入讨论事件失败")
	}
	rt.interactiveBroadcast(ctx, advanced, accepted)

	if finish {
		done, _, err := rt.interactiveEmit(ctx, advanced, interactiveEvent{
			EventType: "discussion_completed", ActorID: actorRCID,
			Payload:   map[string]interface{}{"reason": "user_finished"},
			Key:       key + ":completed",
			RequestID: requestID, StateVer: advanced.Version, Round: nextRound, Revision: 0,
		})
		if err != nil {
			return errResult(500, "写入讨论事件失败")
		}
		rt.interactiveBroadcast(ctx, advanced, done)
		return okResult(discussionSnapshotView(advanced, rt.latestArtifactPtr(ctx, advanced.DiscussionID)))
	}

	roundStarted, _, err := rt.interactiveEmit(ctx, advanced, interactiveEvent{
		EventType: "round_started", ActorID: actorRCID,
		Payload:   map[string]interface{}{"round": nextRound, "roundFocus": ""},
		Key:       key + ":round",
		RequestID: requestID,
		StateVer:  advanced.Version,
		Round:     nextRound,
		Revision:  0,
	})
	if err != nil {
		return errResult(500, "写入讨论事件失败")
	}
	rt.interactiveBroadcast(ctx, advanced, roundStarted)

	if opinion != "" {
		messageUID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(d.DiscussionID+":"+requestID)).String()
		interjection, _, err := rt.interactiveEmit(ctx, advanced, interactiveEvent{
			EventType: "user_interjection", ActorID: actorRCID,
			Payload: map[string]interface{}{
				"messageUid": messageUID, "memberId": actorRCID, "content": opinion,
				"createdAt":         time.Now().UTC().Format(time.RFC3339Nano),
				"consumedFromRound": nextRound,
			},
			Key:       "interactive-opinion:" + messageUID,
			RequestID: messageUID, StateVer: advanced.Version, Round: nextRound, Revision: 0,
		})
		if err != nil {
			return errResult(500, "写入讨论事件失败")
		}
		rt.interactiveBroadcast(ctx, advanced, interjection)
	}
	if err := rt.interactiveNext(ctx, advanced); err != nil {
		rt.logger.Warn("discussion: dispatch after continue failed", "error", err)
	}
	return okResult(discussionSnapshotView(advanced, rt.latestArtifactPtr(ctx, advanced.DiscussionID)))
}

// interactiveInterject 记录一次用户插话（v3）：不重置轮次指针、不打断在跑的发言，
// 插话只影响本轮后续 agent 的上下文（consumedFromRound = 当前轮）。
func (rt *discussionRuntime) interactiveInterject(ctx context.Context, cmd CommandContent, d DiscussionRecord, roomID pgtype.UUID, ownerRCID, actorRCID string) commandResult {
	content := paramTrim(cmd, "content")
	messageUID := paramTrim(cmd, "messageUid", "message_uid")
	if content == "" || len(content) > 8000 || messageUID == "" || len(messageUID) > 128 {
		return errResult(400, "invalid interjection")
	}
	if !canControl(d, ownerRCID, actorRCID) && !rt.isHumanMember(ctx, roomID, actorRCID) {
		return errResult(403, "only enabled human members may interject")
	}
	key := "interactive-opinion:" + messageUID
	if prior, err := rt.repo.EventByIdempotencyKey(ctx, d.DiscussionID, key); err == nil {
		var payload map[string]interface{}
		_ = json.Unmarshal(prior.Payload, &payload)
		if member, _ := payload["memberId"].(string); member != actorRCID {
			return errResult(400, "conflicting message uid")
		}
		if existing, _ := payload["content"].(string); existing != content {
			return errResult(400, "conflicting message uid")
		}
		return okResult(discussionSnapshotView(d, rt.latestArtifactPtr(ctx, d.DiscussionID)))
	}
	if !IsLiveDiscussionStatus(d.Status) {
		return errResult(409, "discussion is no longer active")
	}
	if version, ok := paramInt(cmd, "stateVersion", "state_version", "version"); !ok || version != d.Version {
		return rt.versionConflictResult(ctx, d.DiscussionID)
	}
	// 插话归属轮次：轮末与未开局时当前轮不会再派发，必须顺延到真正会被消费的轮次，
	// 否则气泡显示「已接受」但永远无人读到。
	consumedRound := rt.interactiveInterjectionRound(ctx, d)
	if consumedRound == 0 {
		return errResult(409, "当前讨论已无后续轮次可承载这条意见")
	}
	event, _, err := rt.interactiveEmit(ctx, d, interactiveEvent{
		EventType: "user_interjection", ActorID: actorRCID,
		Payload: map[string]interface{}{
			"messageUid": messageUID, "memberId": actorRCID, "content": content,
			"createdAt":         time.Now().UTC().Format(time.RFC3339Nano),
			"consumedFromRound": consumedRound,
		},
		Key: key, RequestID: messageUID, StateVer: d.Version, Round: consumedRound,
		Revision: 0,
	})
	if err != nil {
		return errResult(500, "写入插话事件失败")
	}
	rt.interactiveBroadcast(ctx, d, event)
	return okResult(discussionSnapshotView(d, rt.latestArtifactPtr(ctx, d.DiscussionID)))
}

// interactiveCancel 取消一个 v3 讨论：置 cancelled + 落事件，并终结在跑的派发。
//
// 已终结（completed/cancelled/failed）时是**幂等无操作** —— 与 legacy actionStop /
// actionCancel 的 `!IsLiveDiscussionStatus → 直接回快照` 一致。少了这道守卫，一次
// 迟到的 cancel 会把已完成的讨论倒写成 cancelled。
func (rt *discussionRuntime) interactiveCancel(ctx context.Context, d DiscussionRecord, actorRCID, reason string) commandResult {
	if !IsLiveDiscussionStatus(d.Status) {
		return okResult(discussionSnapshotView(d, rt.latestArtifactPtr(ctx, d.DiscussionID)))
	}
	advanced, err := rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: d.DiscussionID, ExpectedVersion: d.Version,
		Status: DiscussionStatusCancelled, CurrentRound: d.CurrentRound,
		RoundRevision: d.RoundRevision,
	})
	if err != nil {
		return rt.advanceErrorResult(ctx, err, d.DiscussionID)
	}
	rt.interactiveSettleActive(ctx, d.DiscussionID)
	label := "user_cancelled"
	if reason != "" && strings.Contains(reason, "stop") {
		label = "stopped"
	}
	event, _, emitErr := rt.interactiveEmit(ctx, advanced, interactiveEvent{
		EventType: "discussion_cancelled", ActorID: actorRCID,
		Payload:   map[string]interface{}{"reason": label},
		Key:       fmt.Sprintf("cancel-discussion:%d", d.Version),
		RequestID: uuid.NewString(), StateVer: advanced.Version, Round: d.CurrentRound,
		Revision: d.RoundRevision,
	})
	if emitErr == nil {
		rt.interactiveBroadcast(ctx, advanced, event)
	}
	return okResult(discussionSnapshotView(advanced, rt.latestArtifactPtr(ctx, advanced.DiscussionID)))
}

// interactiveRetryAcceptsStatus 报告 interactiveRetry 是否接受该讨论状态。
// failed 是经典入口（自动重试耗尽后用户手动拉起）；summarizing / synthesizing /
// running 是「汇总迟迟不出、用户手动重派主持」的入口 —— 前端「重试主持任务」
// 按钮在这些状态下就会亮起，只认 failed 会把这类请求全部挡在 409 上。
func interactiveRetryAcceptsStatus(status string) bool {
	switch status {
	case DiscussionStatusFailed, DiscussionStatusSummarizing,
		DiscussionStatusSynthesizing, DiscussionStatusRunning:
		return true
	}
	return false
}

// interactiveRetry 重试一次失败的主持人汇总。除 failed 外，也接受卡在
// summarizing / synthesizing / running 的讨论：前端「重试主持任务」按钮在这
// 些状态下就会亮起（用户看到汇总迟迟不出时正是最需要手动重试的时刻），
// 只认 failed 会把所有这类请求都挡在 409 上 —— 按钮形同虚设。
func (rt *discussionRuntime) interactiveRetry(ctx context.Context, d DiscussionRecord, cmd CommandContent, ownerRCID, actorRCID string) commandResult {
	if !canControl(d, ownerRCID, actorRCID) {
		return errResult(403, "only the discussion owner may retry")
	}
	// 前端带 stateVersion 做乐观并发：版本不一致说明它在看过期快照，让它先对齐。
	if version, ok := paramInt(cmd, "stateVersion", "state_version", "version"); ok && version != d.Version {
		return rt.versionConflictResult(ctx, d.DiscussionID)
	}
	retryable := interactiveRetryAcceptsStatus(d.Status)
	if !retryable {
		return errResult(409, "no failed work to retry")
	}
	var advanced DiscussionRecord
	if d.Status == DiscussionStatusFailed {
		var err error
		advanced, err = rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
			DiscussionID: d.DiscussionID, ExpectedVersion: d.Version,
			Status: DiscussionStatusSummarizing, CurrentRound: d.CurrentRound,
			RoundRevision: d.RoundRevision,
		})
		if err != nil {
			return rt.advanceErrorResult(ctx, err, d.DiscussionID)
		}
	} else {
		// summarizing / synthesizing / running 下重试 = 重派当前轮主持工作。
		// 旧行（仍在途的 dispatch）必须先置为终结态，否则超时恢复扫描仍会
		// 盯着旧 request_id 对账，新旧两次派发的回调会互相踩。
		rt.interactiveSettleActive(ctx, d.DiscussionID)
		advanced = d
	}
	accepted, _, emitErr := rt.interactiveEmit(ctx, advanced, interactiveEvent{
		EventType: "retry_accepted", ActorID: actorRCID,
		Payload:   map[string]interface{}{"action": "retry"},
		Key:       fmt.Sprintf("retry-accepted:%d", d.Version),
		RequestID: uuid.NewString(), StateVer: advanced.Version, Round: advanced.CurrentRound,
		Revision: advanced.RoundRevision,
	})
	if emitErr == nil {
		rt.interactiveBroadcast(ctx, advanced, accepted)
	}
	_, host, ok := interactiveRoles(advanced)
	if !ok {
		return errResult(409, "no failed work to retry")
	}
	if err := rt.interactiveDispatch(ctx, advanced, host, 1); err != nil {
		return errResult(500, "重试主持人汇总失败")
	}
	return okResult(discussionSnapshotView(advanced, rt.latestArtifactPtr(ctx, advanced.DiscussionID)))
}

// interactiveSettleActive 把该讨论仍在途的派发命令置为终结态（取消时用）。
func (rt *discussionRuntime) interactiveSettleActive(ctx context.Context, discussionID string) {
	commands, err := rt.queries.ListRongCloudDiscussionCommandsByDiscussion(ctx, discussionID)
	if err != nil {
		return
	}
	for _, cmd := range commands {
		if cmd.Kind != "dispatch" || (cmd.State != "pending" && cmd.State != "sent") {
			continue
		}
		_, _ = rt.queries.SettleRongCloudDiscussionCommand(ctx, db.SettleRongCloudDiscussionCommandParams{
			State: "acknowledged", CommandID: cmd.CommandID,
		})
	}
}

// interactiveLifecycleAction 是用户动作（continue/finish/cancel/interject/retry/
// stop）的 v3 入口。仅当该聊天室当前讨论是 user_confirmed 且引擎就绪时接管；
// handled=false 时 system_handler 回落到 P4 的 legacy 实现。
//
// 二选一而不是并行：一个聊天室的活跃讨论要么是 v3、要么是 legacy，两个实现的
// 状态机互斥（v3 的 continue 是「等用户确认再开下一轮」，legacy 是「按 max_rounds
// 计数」），绝不能同时套上。
func (rt *discussionRuntime) interactiveLifecycleAction(ctx context.Context, action string, cmd CommandContent, actor discussionActor) (commandResult, bool) {
	d, ok := rt.activeInteractiveDiscussion(ctx, cmd)
	if !ok {
		return commandResult{}, false
	}
	ownerRCID, roomID, err := rt.roomOwner(ctx, d.ChatroomID)
	if err != nil {
		return errResult(500, "查询聊天室失败"), true
	}
	switch action {
	case "continue":
		return rt.interactiveControl(ctx, cmd, d, ownerRCID, actor.RcID, false), true
	case "finish":
		return rt.interactiveControl(ctx, cmd, d, ownerRCID, actor.RcID, true), true
	case "interject":
		return rt.interactiveInterject(ctx, cmd, d, roomID, ownerRCID, actor.RcID), true
	case "cancel":
		if !canControl(d, ownerRCID, actor.RcID) {
			return errResult(403, "only the discussion owner may cancel"), true
		}
		return rt.interactiveCancel(ctx, d, actor.RcID, paramTrim(cmd, "reason")), true
	case "retry":
		return rt.interactiveRetry(ctx, d, cmd, ownerRCID, actor.RcID), true
	case "stop":
		if !canControl(d, ownerRCID, actor.RcID) {
			return errResult(403, "only the chatroom owner may stop a discussion"), true
		}
		return rt.interactiveCancel(ctx, d, actor.RcID, "stop"), true
	default:
		return commandResult{}, false
	}
}

// activeInteractiveDiscussion 定位这条命令作用的那场讨论：显式 discussionId 优先，
// 否则取聊天室当前的活跃讨论。只有 user_confirmed + 引擎就绪才认领。
func (rt *discussionRuntime) activeInteractiveDiscussion(ctx context.Context, cmd CommandContent) (DiscussionRecord, bool) {
	if !rt.interactiveReady() {
		return DiscussionRecord{}, false
	}
	if discussionID := paramTrim(cmd, "discussionId", "discussion_id"); discussionID != "" {
		d, err := rt.repo.Get(ctx, discussionID)
		if err != nil || d.RoundPolicy != RoundPolicyUserConfirmed {
			return DiscussionRecord{}, false
		}
		return d, true
	}
	chatroomID := paramTrim(cmd, "chatroom_id", "chatroomId")
	if chatroomID == "" {
		return DiscussionRecord{}, false
	}
	d, err := rt.repo.GetActiveByChatroom(ctx, chatroomID)
	if err != nil || d.RoundPolicy != RoundPolicyUserConfirmed {
		return DiscussionRecord{}, false
	}
	return d, true
}

// roomOwner 取聊天室的 owner 融云 id 与内部 UUID（权限判定与成员判定各用其一）。
func (rt *discussionRuntime) roomOwner(ctx context.Context, chatroomID string) (string, pgtype.UUID, error) {
	room, err := rt.queries.GetRongCloudChatroomViewByRongCloudID(ctx, chatroomID)
	if err != nil {
		return "", pgtype.UUID{}, err
	}
	return stringOrNull(room.OwnerRcUserID), room.ID, nil
}

// interactiveReady 报告 v3 引擎是否可用（能力位翻面的唯一开关）。
func (rt *discussionRuntime) interactiveReady() bool {
	return rt != nil && rt.interactiveEnabled && rt.repo != nil && rt.queries != nil
}

// ---- 小工具 ---------------------------------------------------------------

func nullableString(value string) interface{} {
	if value == "" {
		return nil
	}
	return value
}

// sameJSONObject 比较两个 JSON 对象是否等价（控制请求的幂等身份校验）。
func sameJSONObject(left, right map[string]interface{}) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		other, ok := right[key]
		if !ok || fmt.Sprintf("%v", other) != fmt.Sprintf("%v", value) {
			return false
		}
	}
	return true
}
