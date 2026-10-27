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
	"crypto/sha1"
	"encoding/hex"
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

	// interactivePromptContextLimit 是注入到设备提示词里的上下文（话题/目标/插话/前序发言）
	// 的字符上限：CLI 的 prompt 走 argv 时有 8000 字符上限，超了会退化到 stdin，
	// 这里保守收口，避免把角色人设挤没。
	interactivePromptContextLimit = 4000

	// interactiveReplyClip 是单条成员发言在上下文里的截断长度。主持人靠本轮发言出
	// 纪要，所以给得比闲聊中继（relayHistoryReplyClip=300）宽。
	interactiveReplyClip = 800

	// interactiveSummaryClip 是上一轮主持纪要在上下文里的截断长度：它是下一轮成员
	// 延续讨论的主锚点。
	interactiveSummaryClip = 1500

	// interactiveHostReplyClip 是主持人出纪要时单条成员发言的截断长度。它比普通成员
	// 那一档宽：本轮发言是纪要的唯一素材，截得太狠会丢立场细节。
	interactiveHostReplyClip = 1200

	// interactivePriorReplyClip 是更早轮次成员发言的截断长度：只作背景，压得更短，
	// 好把额度留给最近一轮。
	interactivePriorReplyClip = 400

	// interactiveMaxHostRetries 是主持人出纪要失败后的最大重试次数（超过即 failed）。
	interactiveMaxHostRetries = 2

	// interactiveDefaultTurnTimeout 是 host.timeout_seconds 不可用时的兜底等待上限。
	interactiveDefaultTurnTimeout = 300 * time.Second

	// interactiveTurnTimeoutFloor 是**派发时**的保底等待时长，不受管理端
	// 「系统主持 timeout_seconds」（初始 60s）影响。
	//
	// 讨论是严格按序的：成员没说完就不该推进下一位，等多久都不算错。设备端从
	// agent 启动到首个 token 常常就要几十秒（opencode/codex 冷启动 + 长思考），
	// 60s 的配置会让正常轮次成批假超时 —— 线上表现为「第 7 轮质疑者响应超时 →
	// 重试 → 跳过 → 主持人汇总同样超时 → summary_failed」。真正的掉线判定交给
	// 心跳的空闲窗口（interactiveHeartbeatIdleWindow），这里只防死锁。
	interactiveTurnTimeoutFloor = 300 * time.Second

	// interactiveHeartbeatIdleWindow 是「最后一次心跳之后还能等多久」。
	//
	// 设备端（xiachat-cli）对每个 your_turn 轮次每 15s 发一条 relay_ping_<requestId>
	// 心跳；每收到一条就把 outbox 的 available_at 推到「现在 + 窗口」。心跳在就
	// 无限续期，断了 120s（漏掉 8 条）才判超时 —— 既容忍融云限流/丢包，也不会
	// 让一个真挂死的成员把整场讨论按住十分钟。
	interactiveHeartbeatIdleWindow = 120 * time.Second

	// interactiveMaxTurnDuration 是单轮的**绝对**上限：从派发时刻算起，无论有
	// 没有心跳都不再等。
	//
	// 心跳的语义是「设备还活着」，但设备的 ping 定时器只跟随轮次存活，不跟随
	// agent —— agent 挂死时心跳照发，光靠续期会让这一轮永不超时，整场讨论卡在
	// summarizing 且没有任何东西会来接管。30 分钟远大于任何正常的单轮（含长思考
	// 与主持人的长纪要），只用来兜真死锁。
	interactiveMaxTurnDuration = 30 * time.Minute

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
	// round_checkpoint 是主持人纪要的结构化版本（成员立场/共识/分歧/下一轮聚焦）。
	// 前端一直有它的卡片与状态机分支，但引擎此前只发过 round_summary_completed，
	// 于是检查点卡片在线上从未出现。现在由 interactiveCompleteSummary 生产。
	"round_checkpoint":   {},
	"contribution_delta": {}, "contribution_completed": {}, "user_interjection": {},
	"member_retry_scheduled": {}, "member_skipped": {}, "request_interrupted": {},
	"interjection_applied": {}, "interruption_failed": {}, "discussion_started": {},
	"discussion_completed": {}, "discussion_cancelled": {}, "discussion_failed": {},
	"discussion_progress": {},
	// 引导式创建（configuring 链路）的三类系统卡片事件。与 web
	// lib/discussion-v3.ts 的 eventData 白名单同步登记，两侧漂移会被前端静默丢弃。
	"setup_prompt": {}, "setup_step_completed": {}, "setup_recommend_failed": {},
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
	// 开局前先对齐一次角色板：从创建到开局之间（planning / 引导式配置）用户可能
	// 已经改过聊天室成员。同步失败不阻断开局 —— 用旧快照照样能跑，下个轮边界
	// 还会再试。
	if synced, changed, syncErr := rt.syncDiscussionRoles(ctx, d); syncErr != nil {
		rt.logger.Warn("discussion: role snapshot sync failed at begin",
			"discussionId", discussionID, "error", syncErr)
	} else if changed {
		d = synced
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
	// 本轮成员发言阶段到此结束（全员完成、或全被跳过）：这是一个**轮边界**，
	// 把中途加入 / 移出的成员同步进角色板。新成员从下一轮开始被派发，本轮不插队
	// （本轮谁还没说是按事件判的，半路插进来的人没听过本轮任何发言）。
	synced, changed, syncErr := rt.syncDiscussionRoles(ctx, d)
	if syncErr != nil {
		rt.logger.Warn("discussion: role snapshot sync failed at round end",
			"discussionId", d.DiscussionID, "round", d.CurrentRound, "error", syncErr)
	} else if changed {
		d = synced
		if _, freshHost, ok := interactiveRoles(d); ok {
			host = freshHost // 同步保留了主持条目，这里重取只是不去假设它
		}
	}
	// 本轮全员完成 → 主持人汇总。但「全员完成」有两种含义：真说完了，和**全被跳光了**
	// （成员节点超时/异常，两次尝试都失败）。后者绝不能请主持人写纪要 —— 它手里一条
	// 发言都没有，只能回「尚未收到任何成员的发言记录…请第一位角色先行发言」，把一次
	// 真实的全员失败伪装成「主持人不会写纪要」（线上 2026-10-15 的观感）。
	contributions, countErr := rt.interactiveRoundContributions(ctx, d)
	if countErr != nil {
		return countErr
	}
	if contributions == 0 {
		return rt.interactiveFailEmptyRound(ctx, d)
	}
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

// interactiveRoundContributions 统计本轮真正落地的成员发言（按成员去重）。
// 与 interactiveRoundDone 的区别：后者把「跳过」也算完成，这里只认有产出的。
func (rt *discussionRuntime) interactiveRoundContributions(ctx context.Context, d DiscussionRecord) (int, error) {
	events, err := rt.repo.ListEventsAfter(ctx, d.DiscussionID, 0)
	if err != nil {
		return 0, err
	}
	speakers := map[string]bool{}
	for _, e := range events {
		if e.EventType != "contribution_completed" {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		round, ok := payload["round"].(float64)
		if !ok || int(round) != d.CurrentRound {
			continue
		}
		if e.ActorID != "" {
			speakers[e.ActorID] = true
		}
	}
	return len(speakers), nil
}

// interactiveFailEmptyRound 把「本轮没有任何成员产出」的讨论判为失败。
//
// 为什么不继续推进：主持人没素材（只能编造或索要）、进 awaiting_user 又会把一次真实
// 的全员失败说成「本轮已汇总」。失败态是唯一诚实的出口 —— 用户看到「讨论失败」，
// 检查节点后重开讨论；成员节点修好后也还能点重试。
//
// reason 必须落在前端 v3 校验器已有的枚举里（discussion-v3.ts 的 discussion_failed
// 是严格 choice('summary_failed','cancel_failed','cancel_unconfirmed','dispatch_failed')）：
// 加新值会让前端把**整段**讨论历史判为无效，除非前后端同时发版。
func (rt *discussionRuntime) interactiveFailEmptyRound(ctx context.Context, d DiscussionRecord) error {
	advanced, err := rt.interactiveAdvance(ctx, d.DiscussionID, func(cur DiscussionRecord) (AdvanceDiscussionParams, bool, error) {
		if !IsLiveDiscussionStatus(cur.Status) || cur.Status == DiscussionStatusSummarizing {
			return AdvanceDiscussionParams{}, false, nil
		}
		return AdvanceDiscussionParams{
			DiscussionID: cur.DiscussionID, ExpectedVersion: cur.Version,
			Status: DiscussionStatusFailed, CurrentRound: cur.CurrentRound,
			RoundRevision: cur.RoundRevision,
		}, true, nil
	})
	if err != nil {
		return err
	}
	event, _, emitErr := rt.interactiveEmit(ctx, advanced, interactiveEvent{
		EventType: "discussion_failed", ActorID: "system",
		Payload:   map[string]interface{}{"reason": "dispatch_failed", "retryable": true},
		Key:       fmt.Sprintf("no-contribution:%d", advanced.CurrentRound),
		RequestID: uuid.NewString(), StateVer: advanced.Version,
		Round: advanced.CurrentRound, Revision: advanced.RoundRevision,
	})
	if emitErr != nil {
		return emitErr
	}
	rt.interactiveBroadcast(ctx, advanced, event)
	rt.logger.Warn("discussion: round finished with no contribution",
		"discussionId", advanced.DiscussionID, "round", advanced.CurrentRound)
	return nil
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
//
// 身份判据**必须同时认两个账号列**：角色板里的 memberId 是节点融云账号
// （rongcloud_node.rongcloud_user_id）。两条成员写入链路填的账号列不一样 ——
// `setDiscussionTeam`（C 端成员设置）走 UpsertRongCloudChatroomMemberByMemberID，
// member_id 有值；运维后台的 ChatroomService.SetMembers 走
// CreateRongCloudChatroomMember，那条 SQL 的列清单里没有 member_id，重建出来的行
// member_id 是 NULL。
//
// 只认 member_id 的后果（线上形态）：管理员在后台改一遍成员 → 本房间所有 AI 行
// member_id 变 NULL → 角色板照常（同步按 node_rc_user_id 认人）→ 但派发前这里对
// 每个人返回 false → 全员只落 member_skipped → 本轮产出为 0 → 讨论被判 failed
// （dispatch_failed）。用户看到的是「我刚调完成员，讨论就失败了」，而 reason 文案
// 完全指不出是 member_id 为 NULL。
//
// 两个分支比的是同一个身份域（融云账号），所以放宽不会带来误判。根治仍是让
// SetMembers 补写 member_id / nickname / portrait_uri（另开 PR）。
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
		if !m.Enabled || m.MemberType != "ai" {
			continue
		}
		sameAccount := (m.MemberID.Valid && m.MemberID.String == memberID) ||
			(m.NodeRcUserID.Valid && m.NodeRcUserID.String == memberID)
		if sameAccount {
			return true
		}
	}
	return false
}

// ---- 角色板同步（中途加人 / 移人） -----------------------------------------

// syncDiscussionRoles 在**轮边界**把角色板（role_snapshot）与聊天室成员表对齐，
// 让中途加入 / 被移出的 AI 成员从下一轮开始生效。
//
// 为什么需要它：role_snapshot 只在创建 / 重开讨论时写一次，之后没有任何 UPDATE
// 路径，而派发只认快照 —— 中途拉进聊天室的新成员永远拿不到 your_turn，用户看到的
// 是「人加进来了，讨论里一次都没说过话」。
//
// 为什么只在轮边界做：派发是按 speaking_order 单线推进的顺序状态机。本轮中途插入
// 一个成员，interactiveRoundDone 会认为它「本轮还没说」而立刻派发 —— 但它没听过
// 本轮任何人的发言，只能凭空表态，插到队首还会打乱轮次推进的判定。轮边界同步让
// 新成员的第一次发言天然落在「完整的一轮」里。
//
// 返回 (record, changed, err)：changed=true 表示角色板变了并且已经落库，调用方必须
// 用返回的 record 继续推进。**Version 不变**（role_snapshot 不在乐观锁范围内，见
// DiscussionRepository.UpdateRoleSnapshot）。
func (rt *discussionRuntime) syncDiscussionRoles(ctx context.Context, d DiscussionRecord) (DiscussionRecord, bool, error) {
	if !rt.interactiveReady() || d.ChatroomID == "" || d.DiscussionID == "" {
		return d, false, nil
	}
	room, err := rt.queries.GetRongCloudChatroomViewByRongCloudID(ctx, d.ChatroomID)
	if err != nil {
		return d, false, err
	}
	rows, err := rt.queries.ListRongCloudChatroomMembersWithNode(ctx, room.ID)
	if err != nil {
		return d, false, err
	}
	snapshot, added, removed, ok := discussionRolesRebased(d, rows)
	if !ok || (len(added) == 0 && len(removed) == 0) {
		return d, false, nil
	}
	updated, err := rt.repo.UpdateRoleSnapshot(ctx, d.DiscussionID, snapshot)
	if err != nil {
		return d, false, err
	}
	round := updated.CurrentRound
	if round < 1 {
		round = 1 // planning 开局前同步：信封里的 round 不能是 0
	}
	// 事件类型必须落在 v3 白名单里：前端对讨论事件是严格 shape 校验，一条不合规
	// 就让整段讨论历史响应无效（见 interactiveV3EventTypes / discussionEventView）。
	// 所以既不新增事件类型，也不扩协议，只借既有的 discussion_progress 承载
	// 「角色板变了」这个信号。
	//
	// data **必须是空对象**：前端 discussion-v3.ts 里 `discussion_progress:
	// shape({})`，而 shape 会否掉任何 fields/optional 之外的键 —— 塞进 round /
	// reason / added 任何一个都会让整段讨论历史判为无效。角色名只进下面的日志。
	//
	// 幂等键用增删集的 sha1 前 16 位，而不是直接拼角色名：角色名上限 80 字符，
	// 中文明文按 UTF-8 是 3 字节/字，一轮增删十来个就能超过 btree 索引单条
	// ~2704 字节的上限（uq_rongcloud_discussion_event_idem），于是这条事件写不进去
	// 还白跑三次重试。摘要对同一组增删是确定的，重放去重能力不变。
	event, created, emitErr := rt.interactiveEmit(ctx, updated, interactiveEvent{
		EventType: "discussion_progress", ActorID: "system",
		Payload: map[string]interface{}{},
		Key: fmt.Sprintf("role-sync:%d:%s", round,
			roleSyncDigest(added, removed)),
		RequestID: uuid.NewString(), StateVer: updated.Version,
		Round: round, Revision: updated.RoundRevision,
	})
	switch {
	case emitErr != nil:
		rt.logger.Warn("discussion: role snapshot synced but event emit failed",
			"discussionId", d.DiscussionID, "error", emitErr)
	case created:
		rt.interactiveBroadcast(ctx, updated, event)
	default:
		// 幂等键命中（同一组增删已经落过事件）：不要重复广播既存事件，
		// 否则 C 端会收到两条同 seq 的 discussion_progress。与
		// interjection_applied 的 `if created { broadcast }` 对齐。
	}
	// 可观测性走日志而不是事件 payload：事件侧受前端严格 shape 校验，装不下这些字段。
	rt.logger.Info("discussion: role snapshot synced",
		"discussionId", d.DiscussionID, "round", round, "added", added, "removed", removed)
	return updated, true, nil
}

// roleSyncDigest 把一次角色板增删压成固定长度的幂等键后缀（16 位 hex）。
//
// 入参顺序参与摘要：added 按成员表顺序、removed 已排序，所以同一组变化永远得到
// 同一个值（重放去重靠它），不同的变化几乎不会撞。
func roleSyncDigest(added, removed []string) string {
	sum := sha1.Sum([]byte("+" + strings.Join(added, "\x1f") + "\x1e-" + strings.Join(removed, "\x1f")))
	return hex.EncodeToString(sum[:8])
}

// discussionRolesRebased 以成员表为权威重建角色板：
//
//   - 已有成员：保留原 role_name / role_instructions / speaking_order（相对顺序不变，
//     不拿成员表里的 speaking_order 覆盖 —— 讨论内的发言顺序是创建/推荐时定下的，
//     之后别人在成员设置页改顺序不该把进行中的讨论打乱）；
//   - 新增成员：追加到 speaking_order 末尾；
//   - 已被移除 / 禁用的成员：从快照剔除，剩余成员的 speaking_order 压缩为 0..n-1。
//
// 返回新快照、新增与被移除成员的角色名（给调用方的日志用）以及 ok。ok=false 表示
// 这次同步 **不该生效**：快照读不出来，或成员表里一个可用普通成员都没有。一次读空
// 不该把整块角色板清掉 —— 那会让讨论直接失去推进依据（interactiveRoles 会判
// ok=false，整场讨论停在原地）。
func discussionRolesRebased(d DiscussionRecord, rows []db.ListRongCloudChatroomMembersWithNodeRow) (json.RawMessage, []string, []string, bool) {
	var raw map[string]map[string]interface{}
	if len(d.RoleSnapshot) == 0 || json.Unmarshal(d.RoleSnapshot, &raw) != nil || len(raw) == 0 {
		return nil, nil, nil, false
	}
	next := map[string]interface{}{}
	existing := map[string]map[string]interface{}{}
	hostMemberID, hostNodeID := "", ""
	for key, entry := range raw {
		if entry == nil {
			continue
		}
		if isHost, _ := entry["isHost"].(bool); isHost {
			// 主持条目原样保留：主持人豁免上下文裁剪，也不参与 speaking_order。
			next[key] = entry
			hostMemberID = key
			if value, ok := entry["memberId"].(string); ok && value != "" {
				hostMemberID = value
			}
			if value, ok := entry["nodeId"].(string); ok {
				hostNodeID = value
			}
			continue
		}
		if key == "" {
			continue
		}
		existing[key] = entry
	}
	if hostMemberID == "" {
		// 快照里没有主持条目：不动它。补一个主持人不是这里该做的事（主持配置在
		// host_config_snapshot 与创建编排里），猜错会把讨论派给错误的节点。
		return nil, nil, nil, false
	}
	if hostNodeID == "" {
		// 退一步用主持快照里的 node_id（role_snapshot 缺 nodeId 的老数据）：只靠
		// 融云账号排除主持也够用，node_id 这一路是补强。
		var host map[string]interface{}
		if json.Unmarshal(d.HostConfigSnapshot, &host) == nil {
			hostNodeID, _ = host["node_id"].(string)
		}
	}

	kept := make([]string, 0, len(existing))
	fresh := make([]map[string]interface{}, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		entry, ok := roundtableRoleEntryFromMember(row, 0)
		if !ok {
			continue
		}
		memberID := row.NodeRcUserID.String
		if memberID == hostMemberID {
			continue // 主持节点不是普通成员
		}
		if row.BusinessNodeID.Valid && hostNodeID != "" && row.BusinessNodeID.String == hostNodeID {
			continue
		}
		if seen[memberID] {
			continue
		}
		seen[memberID] = true
		if _, isExisting := existing[memberID]; isExisting {
			kept = append(kept, memberID)
			continue
		}
		fresh = append(fresh, entry)
	}
	// 已有成员按快照里的原顺序排（rows 已经是成员表顺序，作为并列时的稳定兜底）。
	sort.SliceStable(kept, func(i, j int) bool {
		return roleSpeakingOrder(existing[kept[i]]) < roleSpeakingOrder(existing[kept[j]])
	})

	order := 0
	for _, memberID := range kept {
		entry := make(map[string]interface{}, len(existing[memberID])+1)
		for key, value := range existing[memberID] {
			entry[key] = value
		}
		// 有人被移除时顺序要压缩成 0..n-1，否则 interactiveRoles 的排序会在
		// 中间留下空洞（排序本身没错，但下发到设备端的 speaking_order 会跳号）。
		entry["speakingOrder"] = order
		next[memberID] = entry
		order++
	}
	added := make([]string, 0, len(fresh))
	for _, entry := range fresh {
		entry["speakingOrder"] = order
		next[entry["memberId"].(string)] = entry
		order++
		added = append(added, roleEntryLabel(entry))
	}
	if order == 0 {
		return nil, nil, nil, false
	}
	removed := make([]string, 0, len(existing)-len(kept))
	for memberID, entry := range existing {
		if !seen[memberID] {
			removed = append(removed, roleEntryLabel(entry))
		}
	}
	sort.Strings(removed) // map 迭代顺序随机会让事件 payload 抖动
	snapshot, err := json.Marshal(next)
	if err != nil {
		return nil, nil, nil, false
	}
	return snapshot, added, removed, true
}

// roleSpeakingOrder 读角色板条目的 speaking_order（JSON 解出来是 float64）。
func roleSpeakingOrder(entry map[string]interface{}) int {
	if value, ok := entry["speakingOrder"].(float64); ok {
		return int(value)
	}
	return 0
}

// roleEntryLabel 取角色板条目的展示名（事件 payload 用）：优先角色名，退化昵称，
// 最后才是融云账号 —— 与 interactiveStatement.label 同一套优先级。
func roleEntryLabel(entry map[string]interface{}) string {
	for _, key := range []string{"roleName", "nickname", "memberId"} {
		if value, ok := entry[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// interactiveBuildPrompt 拼出私发给设备的你的角色提示词：人设 + 本轮上下文
// （话题/目标 + 前序发言 + 本轮用户插话 + 本轮要求）。设备端（CLI）把 role_prompt
// 原样塞进 agent 提示词，所以插话与前序发言都是从这里进入 agent 的上下文
// —— 轮次指针不受影响。
func (rt *discussionRuntime) interactiveBuildPrompt(d DiscussionRecord, role interactiveRole, extra string, opinions []interactiveOpinion, history interactiveHistory) string {
	parts := make([]string, 0, 4)
	if role.RoleInstructions != "" {
		parts = append(parts, role.RoleInstructions)
	}
	if role.IsHost {
		if hostPrompt := interactiveHostPrompt(d); hostPrompt != "" {
			parts = append(parts, hostPrompt)
		}
	}
	core := []string{}
	if d.Topic != "" {
		core = append(core, "讨论话题："+d.Topic)
	}
	if d.Goal != "" {
		core = append(core, "讨论目标："+d.Goal)
	}
	// 上一轮纪元点出的「下一轮聚焦」显式抬到本轮要求里。此前它只存在于上一轮纪要
	// 的正文深处（还要看上下文预算给没给到），主持人写了也等于没传下去。
	if focus := history.focusForRound(d.CurrentRound); focus != "" {
		core = append(core, "本轮聚焦（上一轮纪要点出的问题，先回应它）："+focus)
	}
	if len(opinions) > 0 {
		lines := []string{"本轮用户插话（必须纳入你的发言）："}
		for _, opinion := range opinions {
			lines = append(lines, "- "+opinion.content)
		}
		core = append(core, strings.Join(lines, "\n"))
	}
	coreText := strings.Join(core, "\n")
	// 话题/目标/插话与本轮要求先占额度，剩下的才是前序发言的预算：宁可少给历史，
	// 也不能把本轮指令挤出上下文。截断也一律按 rune 走（旧实现按字节切片，会把
	// 多字节字符切成乱码，而且切掉的正是最新的那段）。
	budget := interactivePromptContextLimit - len([]rune(coreText)) - len([]rune(extra))
	if budget < 0 {
		budget = 0
	}
	block := coreText
	if prior := interactiveHistoryBlock(history, d.CurrentRound, role.IsHost, budget); prior != "" {
		if block != "" {
			block += "\n"
		}
		block += prior
	}
	if extra != "" {
		if block != "" {
			block += "\n"
		}
		block += extra
	}
	if block != "" {
		if len([]rune(block)) > interactivePromptContextLimit {
			block = clipRelayText(block, interactivePromptContextLimit)
		}
		parts = append(parts, "[本轮上下文]\n"+block)
	}
	// 成员名册放在预算之外：它是主持人才用得上的对照表，不该跟前序发言抢额度
	// （主持人手里那几条发言是写纪要的唯一素材）。排在上下文之后，也不会被
	// 上下文截断切掉。
	if role.IsHost {
		if ordinary, _, ok := interactiveRoles(d); ok {
			if roster := interactiveCheckpointRoster(ordinary); roster != "" {
				parts = append(parts, roster)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// interactiveRoundTask 是派给普通成员的「本轮任务」，按轮次给三种不同的发言协议：
//
//	第 1 轮        独立立论（先给结论与依据，再自曝最可能被推翻的一点）
//	第 2 轮起      交叉质询（点名回应具体论点 → 说明自己改了什么 → 说明自己坚持什么）
//	判定停滞时     只走两条路之一（带新事实，或直接收口）
//
// 为什么要把一句话拆成协议：讨论质量主要由「每轮要求成员做什么」决定，而不是由
// 轮次数量决定。只写「请给出你的分析」时，模型会输出五份互不相干的独白，第二轮
// 起又开始互相附和 —— 这是多智能体讨论最常见的失效形态。
func interactiveRoundTask(round int, stalled bool, similarity float64) string {
	if round <= 1 {
		return "这是第 1 轮（立论）。请按顺序写三件事，不要写客套话：\n" +
			"1. 结论：一句话说清你主张什么；\n" +
			"2. 依据：给出可核查的来源、数据、文件、约束条件或亲身经验；写不出具体来源就直说这是你的判断；\n" +
			"3. 最可能被推翻的一点：你最没把握、最希望别人来攻的地方。"
	}
	if stalled {
		return fmt.Sprintf("这是第 %d 轮（讨论已停滞）。上一轮与本轮的发言高度重复（相似度约 %.0f%%），"+
			"换一种说法不会产生新信息。本轮只能走下面两条路之一：\n"+
			"1. 带来上一轮没有的新材料：具体数据、文件、约束条件、真实案例；\n"+
			"2. 直接收口：给出你能接受的最终方案，并写明你放弃了哪些条件、为什么。\n"+
			"仍然要点名回应至少一位成员的具体论点，但不要再复述已达成的共识。",
			round, similarity*100)
	}
	return fmt.Sprintf("这是第 %d 轮（交叉质询）。按顺序写三件事，不要重述上一轮已经说过的内容：\n"+
		"1. 点名回应：从[前序发言]里挑至少一位成员，用角色名指出来，针对他具体哪一句话、哪个论据提出反驳或补充；"+
		"只说「有人提到」不算回应；\n"+
		"2. 你改变了什么：哪一点你被说服了、为什么被说服；\n"+
		"3. 你坚持什么：哪一点你仍然不同意，理由是什么。\n"+
		"如果[上一轮主持纪要]里写了「下一轮聚焦」，先回答那个问题，再展开自己的部分。", round)
}

// interactiveHostTask 是派给主持人的本轮纪要任务。四段结构对齐讨论类产品通行的
// 会议纪要：共识、分歧、未决、下一步。分歧段刻意排在共识之后、并明确要求「不要
// 磨平异议」—— 把分歧压成一句「大家基本同意」是最容易发生、也最难被用户发现的失真。
func interactiveHostTask(round int, stalled bool, similarity float64) string {
	// 产物是「一份持续演进的讨论文档」，不是每轮一份孤立的当轮快照：第 1 轮
	// 从头起稿，第 2 轮起主持人在上一版全文的基础上修订（改有变动的、补新信息、
	// 删已失效的），每轮落库的是同一份产物的最新版本。
	var task string
	if round <= 1 {
		task = "本轮（第 1 轮）所有成员已发言完毕，请起草讨论产物（第一版）。" +
			"这份产物会随后续轮次持续修订，请搭好能长期维护的结构。严格按下面四段写，标题照抄：\n" +
			"【已达成的共识】逐条写，一条一句，写清成立的前提；没有就写「暂无」。\n" +
			"【仍存在的分歧】写清谁坚持什么、他的理由是什么。分歧比共识更重要：不要为了好看把异议磨平，" +
			"也不要替成员编造他没说过的立场。\n" +
			"【未解决的问题】本轮没谈透、需要补材料或需要用户拍板的事。\n" +
			"【下一轮聚焦】一句话，写成可以直接当成下一轮议题的句子；如果你建议结束，就写「建议结束，可以出纪要」。\n" +
			"最后一行单独写「建议：继续」或「建议：结束」，只写这两个之一。\n" +
			"成员发言已在上文[前序发言]中逐条给出，请直接据此撰写；即便信息不完整，也请基于现有" +
			"发言给出纪要与待确认项，不要向我索要材料。"
	} else {
		task = fmt.Sprintf("本轮（第 %d 轮）所有成员已发言完毕。这份讨论产物正在持续演进："+
			"上一版全文已附在[上一版产物]中，请**在它的基础上修订**，输出**完整的新版全文**（不是只写本轮变化，也不要另起炉灶重写）：\n"+
			"1. 保留：上一版里仍然成立的内容原样保留（允许微调措辞）；\n"+
			"2. 修订：本轮发言推翻或修正了的内容，改写对应段落；\n"+
			"3. 补充：本轮出现的新信息、新共识，并入对应段落；不要为了合并而磨平分歧；\n"+
			"4. 清理：已解决的问题从【未解决的问题】移除。\n"+
			"四段结构（【已达成的共识】【仍存在的分歧】【未解决的问题】【下一轮聚焦】）保持不变；"+
			"【下一轮聚焦】写当前最新判断；最后一行单独写「建议：继续」或「建议：结束」，只写这两个之一。\n"+
			"本轮成员发言已在上文[前序发言]中逐条给出，请据此修订；即便信息不完整，也请基于现有发言"+
			"给出新版产物与待确认项，不要向我索要材料。", round)
	}
	if stalled {
		task += fmt.Sprintf("\n另外：本轮发言与上一轮高度重复（相似度约 %.0f%%），没有产生新信息。"+
			"请在【仍存在的分歧】里直接点明这一点，并把【下一轮聚焦】写成一个换角度的具体问题；"+
			"如果连续两轮如此，最后一行写「建议：结束」。", similarity*100)
	}
	// 机器可读检查点：正文仍然是给人读的四段纪要，末尾多一段给系统读的 JSON。
	// 前端「成员立场 / 共识 / 分歧 / 下一轮聚焦」那套卡片就靠它落地。
	return task + "\n" + interactiveCheckpointContract()
}

// interactiveStallThreshold 是判定「讨论停滞」的相似度阈值。取 0.85 与业界同类
// 实现（opencode-roundtable 的 loopSimilarityThreshold）一致：低于它时两轮发言
// 仍有实质差异，高于它基本只是换词重述。
const interactiveStallThreshold = 0.85

// interactiveRoundStalled 判断第 round 轮是否在重述第 round-1 轮。
//
// 规则是确定性的：把两轮的成员发言各自拼成一个字符串，取字符二元组集合，
// 算 Jaccard 相似度（交集 / 并集）；两轮都有发言才算，任一为空一律判否
// （空轮由空轮守卫处理，不能在这里当成停滞）。
func interactiveRoundStalled(h interactiveHistory, round int) (bool, float64) {
	if round <= 1 {
		return false, 0
	}
	current := renderStatementsText(h.roundStatements(round))
	previous := renderStatementsText(h.roundStatements(round - 1))
	similarity := bigramJaccard(current, previous)
	return similarity >= interactiveStallThreshold, similarity
}

// renderStatementsText 把一组发言拼成纯文本（不带署名），用于相似度计算。
func renderStatementsText(statements []interactiveStatement) string {
	var b strings.Builder
	for _, statement := range statements {
		b.WriteString(statement.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// bigramJaccard 用字符二元组集合的 Jaccard 系数衡量两段文本的重复度：
// 返回 0（毫无重叠）到 1（二元组集合完全一致）。按 rune 切分，中文不会被切坏。
func bigramJaccard(a, b string) float64 {
	setA := bigramSet(a)
	setB := bigramSet(b)
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}
	intersection := 0
	for gram := range setA {
		if _, ok := setB[gram]; ok {
			intersection++
		}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func bigramSet(text string) map[string]struct{} {
	runes := []rune(strings.TrimSpace(text))
	set := make(map[string]struct{}, len(runes))
	for i := 0; i+1 < len(runes); i++ {
		set[string(runes[i:i+2])] = struct{}{}
	}
	return set
}

// interactiveOpinion 是一条被本轮消费的用户插话：messageUID 用于回执（interjection_applied）
// 与前端气泡对账，content 用于拼进提示词。
type interactiveOpinion struct {
	messageUID string
	content    string
}

// interactiveStatement 是提示词上下文里的一条已发生发言（成员发言或主持纪要）。
type interactiveStatement struct {
	Round    int
	MemberID string
	RoleName string
	Content  string
}

// label 是这条发言在上下文里的署名：优先角色名（快照里 nickname 与 roleName 同值），
// 最后才退化到 rc 账号。
func (s interactiveStatement) label() string {
	if name := strings.TrimSpace(s.RoleName); name != "" {
		return name
	}
	return s.MemberID
}

// interactiveHistory 是一次讨论里已经发生过的发言快照，用于拼「前序发言」上下文。
type interactiveHistory struct {
	// contributions 是各轮成员发言（事件顺序；同轮同人只留最后一次，重试不重复）。
	contributions []interactiveStatement
	// summaries 是各轮主持纪要（事件顺序；同轮只留最后一次）。
	summaries []interactiveStatement
	// focus 是各轮检查点里「下一轮聚焦」的取值（轮次 → 聚焦句）。第 N 轮的检查点
	// 描述的是第 N+1 轮该谈什么，所以第 R 轮的聚焦取 focus[R-1]。
	focus map[int]string
}

// focusForRound 返回第 round 轮该聚焦的问题（来自上一轮检查点）；没有就返回空串。
func (h interactiveHistory) focusForRound(round int) string {
	if round <= 1 || h.focus == nil {
		return ""
	}
	return strings.TrimSpace(h.focus[round-1])
}

// roundStatements 返回某一轮已完成的成员发言（保持发言顺序）。
func (h interactiveHistory) roundStatements(round int) []interactiveStatement {
	out := make([]interactiveStatement, 0, len(h.contributions))
	for _, statement := range h.contributions {
		if statement.Round == round {
			out = append(out, statement)
		}
	}
	return out
}

// lastContributionIndex 返回该成员最后一次发言在 contributions 里的下标，-1 表示没有。
//
// contributions 是事件顺序升序（同轮同人只留最后一次，见 interactivePromptInputs），
// 所以反序扫到的第一处就是该成员的最新一条 —— 它正是上下文窗口的锚点。
func (h interactiveHistory) lastContributionIndex(memberID string) int {
	if memberID == "" {
		return -1
	}
	for i := len(h.contributions) - 1; i >= 0; i-- {
		if h.contributions[i].MemberID == memberID {
			return i
		}
	}
	return -1
}

// windowFor 返回「按该成员自身发言截断」后的 history 副本：
//
//   - 有锚点：contributions = contributions[idx:]（含自己那条）；
//   - 无锚点（新入组 / 首次发言）：contributions = 尾部 fallbackCount 条，至少 1 条；
//   - summaries / focus 原样保留 —— 纪要本来只取最近一条，不属于累计型上下文；
//   - 结果至少 1 条，保证窗口不退化。
//
// 为什么按「自己最后一次发言」截断：上下文全量累计时，每位成员看到的都是同一份
// 全文，模型会把别人前几轮的立场当成自己说过的话复述（表现为「越聊越像同一个人」
// 与「第 N 轮还在回应第 1 轮」）。从自己上次开口处起算，既保住了「我说过什么」的
// 连续性，也保住了这段区间里别人真正推进过的内容。
//
// 返回的 contributions 是**新切片**：直接对共享底层数组做 append 会写到别人的窗口里。
func (h interactiveHistory) windowFor(memberID string, fallbackCount int) interactiveHistory {
	out := h
	total := len(h.contributions)
	if total == 0 {
		return out
	}
	start := h.lastContributionIndex(memberID)
	if start < 0 {
		if fallbackCount <= 0 {
			fallbackCount = 1
		}
		start = total - fallbackCount
	}
	if start < 0 {
		start = 0
	}
	if start >= total {
		start = total - 1
	}
	out.contributions = append([]interactiveStatement(nil), h.contributions[start:]...)
	return out
}

// lastSummaryBefore 返回第 round 轮之前最近的一轮主持纪要。
func (h interactiveHistory) lastSummaryBefore(round int) (interactiveStatement, bool) {
	var found interactiveStatement
	ok := false
	for _, summary := range h.summaries {
		if summary.Round >= round {
			continue
		}
		if !ok || summary.Round >= found.Round {
			found = summary
			ok = true
		}
	}
	return found, ok
}

// interactivePromptInputs 一次读回该讨论的事件流，派生出「本轮消费的用户插话」与
// 「已发生的前序发言」。派发一轮要同时用这两样，一次拉取避免两处看到不同快照。
func (rt *discussionRuntime) interactivePromptInputs(d DiscussionRecord) ([]interactiveOpinion, interactiveHistory) {
	history := interactiveHistory{}
	events, err := rt.repo.ListEventsAfter(context.Background(), d.DiscussionID, 0)
	if err != nil {
		return nil, history
	}
	names := interactiveRoleNames(d)
	opinions := []interactiveOpinion{}
	contributionAt := map[string]int{}
	summaryAt := map[int]int{}
	for _, e := range events {
		var payload map[string]interface{}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		switch e.EventType {
		case "user_interjection":
			// 插话只在被投递的那一轮生效，不跨轮重复注入（与旧后端 _context 一致）。
			round, _ := payload["consumedFromRound"].(float64)
			if int(round) != d.CurrentRound {
				continue
			}
			content, _ := payload["content"].(string)
			if content == "" {
				continue
			}
			uid, _ := payload["messageUid"].(string)
			opinions = append(opinions, interactiveOpinion{messageUID: uid, content: content})
		case "contribution_completed":
			round, _ := payload["round"].(float64)
			content, _ := payload["content"].(string)
			if content == "" || int(round) <= 0 {
				continue
			}
			statement := interactiveStatement{
				Round: int(round), MemberID: e.ActorID,
				RoleName: names[e.ActorID], Content: content,
			}
			key := fmt.Sprintf("%d:%s", statement.Round, statement.MemberID)
			if index, ok := contributionAt[key]; ok {
				// 重试/整段重放：后者才是作数的发言。
				history.contributions[index] = statement
				continue
			}
			contributionAt[key] = len(history.contributions)
			history.contributions = append(history.contributions, statement)
		case "round_summary_completed":
			round, _ := payload["round"].(float64)
			content, _ := payload["content"].(string)
			if content == "" || int(round) <= 0 {
				continue
			}
			statement := interactiveStatement{
				Round: int(round), MemberID: e.ActorID,
				RoleName: names[e.ActorID], Content: content,
			}
			if index, ok := summaryAt[statement.Round]; ok {
				history.summaries[index] = statement
				continue
			}
			summaryAt[statement.Round] = len(history.summaries)
			history.summaries = append(history.summaries, statement)
		case "round_checkpoint":
			// 检查点里的「下一轮聚焦」要传回下一轮，否则主持人写的那句话只停在
			// 卡片上，下一轮成员（和主持自己）看不到它。
			round, _ := payload["round"].(float64)
			focus, _ := payload["nextFocus"].(string)
			if int(round) <= 0 || strings.TrimSpace(focus) == "" {
				continue
			}
			if history.focus == nil {
				history.focus = map[int]string{}
			}
			history.focus[int(round)] = focus
		}
	}
	return opinions, history
}

// interactiveRoleNames 取角色板「rc 账号 → 展示名」，给上下文里的发言署名用。
func interactiveRoleNames(d DiscussionRecord) map[string]string {
	names := map[string]string{}
	ordinary, host, ok := interactiveRoles(d)
	if !ok {
		return names
	}
	record := func(role interactiveRole) {
		name := strings.TrimSpace(role.RoleName)
		if name == "" {
			name = strings.TrimSpace(role.Nickname)
		}
		if name != "" {
			names[role.MemberID] = name
		}
	}
	for _, role := range ordinary {
		record(role)
	}
	record(host)
	return names
}

// interactiveHistoryBlock 把「已经发生过的发言」渲染成设备提示词里的前序上下文段。
//
// 缺了这一段：第二位成员只能凭空表态、第二轮会从零重新开始，主持人更是拿不到任何
// 成员发言 —— 它只能回「缺少前序发言记录，无法复述成员立场与分歧。请提供第 N 轮的
// 发言内容」。线上 2026-10-15 的「第二轮不延续第一轮」就是这条路径。
//
// 预算与 relayPromptContextLimit 同源（CLI prompt 走 argv 有 8000 字符上限）：按段
// 优先级装入，装不下的丢掉，只有第一段允许被截断。返回空串表示没有任何可注入的
// 前序上下文（旧报文形态逐字不变）。
func interactiveHistoryBlock(h interactiveHistory, currentRound int, forHost bool, budget int) string {
	if budget <= 0 {
		return ""
	}
	sections := make([]string, 0, 3)
	used := 0
	// take 按优先级装入一段：装得下就装，装不下就停（第一段允许截断，至少给一段上下文）。
	take := func(block string) bool {
		if block == "" {
			return true
		}
		size := len([]rune(block))
		if used+size <= budget {
			sections = append(sections, block)
			used += size
			return true
		}
		if len(sections) == 0 {
			if clipped, _ := clipToBudget(block, budget); clipped != "" {
				sections = append(sections, clipped)
			}
		}
		return false
	}
	if forHost {
		// 本轮成员发言是本轮修订的唯一增量素材，先给。
		statements := h.roundStatements(currentRound)
		take(renderInteractiveStatements(
			fmt.Sprintf("本轮（第 %d 轮）成员发言（本轮修订的全部素材，请据此修订产物）：", currentRound),
			statements, shareBudget(budget, len(statements), interactiveHostReplyClip)))
		// 上一版产物全文：修订模式下的「底稿」。任务要求在它基础上修订，所以它
		// 比什么都重要 —— 被预算挤掉主持人就只能重写而不是修订。remaining 预算
		// 全部让给它（截断只发生在极端长文时，仍优于缺席）。
		if summary, ok := h.lastSummaryBefore(currentRound); ok {
			remaining := budget - used
			if remaining > 0 {
				take("\n[上一版产物]（请在此版本基础上修订，输出完整新版全文）：\n" +
					clipRelayText(summary.Content, remaining))
			}
		}
	} else {
		// 普通成员：先给上一轮纪要（跨轮延续的主锚点），再给更早轮次的发言，最后给
		// 本轮已发言成员（承接或核验，而不是重复表态）。
		if summary, ok := h.lastSummaryBefore(currentRound); ok {
			take("\n上一轮主持纪要（据此继续推进，不要从零重开）：\n" + clipRelayText(summary.Content, interactiveSummaryClip))
		}
		for round := currentRound - 1; round >= 1; round-- {
			statements := h.roundStatements(round)
			if len(statements) == 0 {
				continue
			}
			if !take(renderInteractiveStatements(
				fmt.Sprintf("第 %d 轮成员发言：", round),
				statements, shareBudget(budget, len(statements), interactivePriorReplyClip))) {
				break
			}
		}
		statements := h.roundStatements(currentRound)
		take(renderInteractiveStatements(
			"本轮已发言成员的观点（承接或核验后再发言，不要重复）：",
			statements, shareBudget(budget, len(statements), interactivePriorReplyClip)))
	}
	return strings.Join(sections, "")
}

// shareBudget 把一段预算按条数平分，并夹在 [120, max] 之间：条数多时不至于把后面的
// 发言挤成空串，条数少时也不浪费额度。
func shareBudget(budget, count, max int) int {
	if count <= 0 {
		return 0
	}
	share := budget / (count + 1)
	if share > max {
		share = max
	}
	if share < 120 {
		share = 120
	}
	return share
}

// renderInteractiveStatements 把一批发言渲染成列表段（自带前导换行，便于直接拼接）。
func renderInteractiveStatements(header string, items []interactiveStatement, perItem int) string {
	if len(items) == 0 {
		return ""
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, fmt.Sprintf("- %s：%s", item.label(), clipRelayText(item.Content, perItem)))
	}
	return "\n" + header + "\n" + strings.Join(lines, "\n")
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
	// 本轮被消费的用户插话 + 前序发言：同一份快照同时用于提示词拼接与回执落库，
	// 避免重复全量拉事件，也避免两处看到不同快照。
	opinions, history := rt.interactivePromptInputs(d)
	action := "assignment"
	eventType := "assignment_created"
	// 本轮任务按阶段协议生成，而不是一句「请给出你的分析」：首轮立论、中段交叉
	// 质询、停滞时收口。第二轮起必须有轮次抬头，否则成员会把本轮当成一次全新讨论
	// 从零重开（线上 2026-10-15 的现象）。
	stalled, similarity := interactiveRoundStalled(history, d.CurrentRound)
	extra := interactiveRoundTask(d.CurrentRound, stalled, similarity)
	if role.IsHost {
		action = "summary"
		eventType = "host_turn_requested"
		extra = interactiveHostTask(d.CurrentRound, stalled, similarity)
		if len(history.roundStatements(d.CurrentRound)) == 0 {
			// 兜底：正常路径下空轮不会再派发主持人（见 interactiveNext 的空轮守卫），
			// 只有用户在失败态手动「重试主持任务」才会走到这里。别让它编造纪要。
			extra = fmt.Sprintf("本轮（第 %d 轮）没有任何成员产出：成员节点超时或执行失败，"+
				"你手上没有可依据的发言。请只输出一段简短说明 —— 本轮无成员发言记录、"+
				"无法复述立场与分歧，建议检查成员节点后重开讨论。不要索要材料，也不要编造成员观点。",
				d.CurrentRound)
		}
	}
	// 上下文窗口：普通成员只带「自己上次发言（含）→ 现在」，主持人**豁免**
	// （他要写本轮纪要 + 上一版产物全文，靠的就是本轮全员发言的完整素材）。
	//
	// 裁剪必须放在 stalled 判定**之后**：停滞检测是引擎级判断（第 N 轮是否在重述
	// 第 N-1 轮），要看全量；拿裁剪后的窗口去比，上一轮发言可能整段不在窗口里，
	// 相似度恒为 0，停滞协议永远不触发。
	if !role.IsHost {
		if ordinary, _, ok := interactiveRoles(d); ok {
			history = history.windowFor(role.MemberID, len(ordinary))
		}
	}
	prompt := rt.interactiveBuildPrompt(d, role, extra, opinions, history)

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
		"roundFocus":    history.focusForRound(d.CurrentRound),
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
		// 流式帧路由参数：CLI 据此把 chat_stream 帧直接广播进讨论所在的聊天室
		// 会话，web 才能看到成员「边想边说」。缺了它 CLI 会退回私聊（1）并把
		// 帧发给一个根本不存在的私聊 targetId —— 讨论组于是完全没有流式，
		// 只有轮次结束后的 contribution_completed 整包。
		//
		// 与 chatroom_relay.go 的 dispatchTurn、discussion_coordinator.go 的
		// sendYourTurn 保持同一约定：讨论一定发生在聊天室（4）。
		// requester_id 留空（正式轮次由服务端编排而非某用户发起，web 端
		// validStreamDelivery 对群/聊天室只校验 conversation_id）。
		"conversation_type": 4,
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
	// 主持人按任务要求在正文末尾追加一段机器可读检查点。先摘出来：正文存成给用户
	// 读的纪要（不含 JSON），结构化数据另发一条 round_checkpoint 事件供卡片渲染。
	checkpointWire, markdown, hasCheckpoint := parseInteractiveCheckpoint(trimmed)
	if hasCheckpoint && strings.TrimSpace(markdown) != "" {
		trimmed = strings.TrimSpace(markdown)
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
	if hasCheckpoint {
		rt.interactiveRecordCheckpoint(ctx, advanced, task, checkpointWire)
	}
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
		// 重试上限按**本次派发链**的 attempt 判，不能统计整场讨论里的
		// member_retry_scheduled 事件：一场 7 轮的讨论中，前几轮主持人偶发的
		// 一次失败重试会永久累计，到最后一轮时计数早已到顶 —— 主持人第一次
		// 超时就被判 summary_failed，连重试机会都没有（线上 2026-10-22 的
		// 「第 7 轮质疑者已跳过 → 5 分钟后讨论失败」）。attempt 只属于当前
		// 任务链，天然按轮隔离，也自动覆盖「重试主持任务」按钮开的新链。
		if interactiveHostRetryExhausted(task.Attempt) {
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

// interactiveHostRetryExhausted 报告主持人汇总的重试次数是否已用尽。
// attempt 是**本次派发链**内的序号（dispatch 时从 1 起，重试 +1），所以
// attempt 1..interactiveMaxHostRetries 都还有下一次机会，超过才判失败。
//
// 为什么不用「统计事件里的 member_retry_scheduled」：那是跨轮累计的，一场
// 多轮讨论里前几轮的零星重试会把计数吃满，最后一轮的主持人一次超时就被判
// 失败（线上 2026-10-22 的 summary_failed）。
func interactiveHostRetryExhausted(attempt int) bool {
	if attempt <= 0 {
		attempt = 1
	}
	return attempt > interactiveMaxHostRetries
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
//
// 两段：先收敛跨过绝对上限的命令（心跳也不能无限续命），再处理普通到期。
// 没有第一段，一次 agent 挂死的心跳（CLI 的 ping 定时器按轮次存活，不看
// agent 是否还在跑）会让这一轮永远不到期，整场讨论卡在 summarizing。
func (rt *discussionRuntime) interactiveSweep(ctx context.Context) {
	if !rt.interactiveReady() {
		return
	}
	handled := make(map[string]struct{}, 8)
	overdue, err := rt.queries.ListOverdueRongCloudDiscussionCommands(ctx, db.ListOverdueRongCloudDiscussionCommandsParams{
		Limit:  32,
		Cutoff: pgtype.Timestamptz{Time: time.Now().Add(-interactiveMaxTurnDuration), Valid: true},
	})
	if err != nil {
		rt.logger.Warn("discussion: sweep list overdue commands failed", "error", err)
	} else {
		for _, cmd := range overdue {
			handled[cmd.CommandID] = struct{}{}
			rt.logger.Warn("discussion: turn exceeded the absolute cap",
				"discussionId", cmd.DiscussionID, "commandId", cmd.CommandID,
				"cap", interactiveMaxTurnDuration)
			rt.interactiveExpireCommand(ctx, cmd)
		}
	}
	due, err := rt.queries.ListDueRongCloudDiscussionCommands(ctx, 32)
	if err != nil {
		rt.logger.Warn("discussion: sweep list due commands failed", "error", err)
		return
	}
	for _, cmd := range due {
		if _, seen := handled[cmd.CommandID]; seen {
			continue
		}
		rt.interactiveExpireCommand(ctx, cmd)
	}
}

// interactiveExpireCommand 把一条过期派发按超时处理（终结 outbox + 落事件 +
// 走重试/跳过/失败的分支）。
func (rt *discussionRuntime) interactiveExpireCommand(ctx context.Context, cmd db.RongcloudDiscussionCommand) {
	var task interactiveTask
	if err := json.Unmarshal(cmd.Payload, &task); err != nil {
		return
	}
	d, err := rt.repo.Get(ctx, cmd.DiscussionID)
	if err != nil {
		return
	}
	if !IsLiveDiscussionStatus(d.Status) || task.Round != d.CurrentRound {
		_, _ = rt.queries.SettleRongCloudDiscussionCommand(ctx, db.SettleRongCloudDiscussionCommandParams{
			State: "failed", CommandID: cmd.CommandID,
		})
		return
	}
	if _, err := rt.queries.SettleRongCloudDiscussionCommand(ctx, db.SettleRongCloudDiscussionCommandParams{
		State: "failed", CommandID: cmd.CommandID,
	}); err != nil {
		return
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

// interactiveTurnTimeout 是单轮等待上限：优先用系统主持配置的 timeout_seconds，
// 但绝不低于 interactiveTurnTimeoutFloor。配置项管的是「多久没回包算可疑」，
// 不是「正常轮次的上限」—— 讨论按序发言，等前一位说完等多久都不该算错。
func (rt *discussionRuntime) interactiveTurnTimeout() time.Duration {
	timeout := interactiveDefaultTurnTimeout
	if rt.systemHostLoader != nil {
		if cfg, found, err := rt.systemHostLoader(context.Background()); err == nil && found && cfg.TimeoutSeconds > 0 {
			timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
		}
	}
	if timeout < interactiveTurnTimeoutFloor {
		timeout = interactiveTurnTimeoutFloor
	}
	return timeout
}

// interactiveHeartbeatWindow 是心跳续期后给的窗口：取「单轮上限」与空闲窗口的
// 较大者。管理端配得更长时按配置走，配得更短时仍给满空闲窗口（心跳在 = 还活着）。
func (rt *discussionRuntime) interactiveHeartbeatWindow() time.Duration {
	window := rt.interactiveTurnTimeout()
	if window < interactiveHeartbeatIdleWindow {
		window = interactiveHeartbeatIdleWindow
	}
	return window
}

// interactiveNotifyStreamActivity 是讨论组的心跳入口：requestID 是设备端
// relay_ping_ 前缀剥掉之后的原始 requestId（= outbox 的 command_id）。
//
// 返回 true 表示这条消息被当作心跳消化 —— 调用方不能再把它当回包处理（否则
// 空的 command_result 会被 interactiveAcceptTurn 当成「成员发言为空」而判失败）。
//
// 只续期未终结的派发命令：已 acknowledged / failed 的（迟到心跳）返回 false，
// 由调用方继续走 relay 的未知心跳路径（那条路径恒定消化，不会漏进别处）。
func (rt *discussionRuntime) interactiveNotifyStreamActivity(ctx context.Context, requestID string) bool {
	if !rt.interactiveReady() || requestID == "" {
		return false
	}
	cmd, err := rt.queries.GetRongCloudDiscussionCommand(ctx, requestID)
	if err != nil || cmd.Kind != "dispatch" {
		return false
	}
	if cmd.State != "pending" && cmd.State != "sent" {
		return false
	}
	rows, err := rt.queries.ExtendRongCloudDiscussionCommandDeadline(ctx, db.ExtendRongCloudDiscussionCommandDeadlineParams{
		CommandID:   requestID,
		AvailableAt: pgtype.Timestamptz{Time: time.Now().Add(rt.interactiveHeartbeatWindow()), Valid: true},
	})
	if err != nil {
		rt.logger.Warn("discussion: extend dispatch deadline failed",
			"requestId", requestID, "error", err)
		return true // 心跳本身是合法的：别让它漏成回包
	}
	if rows == 0 {
		// 心跳与回包赛跑：命令刚好在这一刻被终结。心跳仍是心跳。
		return true
	}
	return true
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
		Payload:   map[string]interface{}{"round": nextRound, "roundFocus": rt.interactiveFocusForRound(ctx, d.DiscussionID, nextRound)},
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
	// 进入新轮前对齐角色板：新成员从刚开始的这一轮起被派发（轮边界，不插队）。
	// 同步失败不阻断继续 —— 用旧快照照样能跑完这一轮，本轮结束时还会再同步一次。
	dispatched := advanced
	if synced, changed, syncErr := rt.syncDiscussionRoles(ctx, advanced); syncErr != nil {
		rt.logger.Warn("discussion: role snapshot sync failed on continue",
			"discussionId", advanced.DiscussionID, "round", nextRound, "error", syncErr)
	} else if changed {
		dispatched = synced
	}
	if err := rt.interactiveNext(ctx, dispatched); err != nil {
		rt.logger.Warn("discussion: dispatch after continue failed", "error", err)
	}
	// 回包用 dispatched：角色板刚同步过，前端要立刻看到新成员在板上（版本号没变，
	// 所以这里不存在乐观锁语义上的冲突）。
	return okResult(discussionSnapshotView(dispatched, rt.latestArtifactPtr(ctx, dispatched.DiscussionID)))
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

// interactiveLifecycleActions 是本函数认领的用户动作全集。
//
// system_handler 对所有 service=discussion 命令都会先调 interactiveLifecycleAction
// （早于 action 分发），所以这里必须自己收口：只有下面六个真正的生命周期动作归 v3
// 引擎，其余命令一律放行到分发层。否则 configuring 守卫会把「读快照 / 断点续引 /
// 提交议题」这些**引导链路自己的动作**一并拦成 409 —— 空讨论组一建完，前端
// getActive 就失败，拿不到半成品快照，引导向导永远不出现（历史缺陷）。
var interactiveLifecycleActions = map[string]bool{
	"continue": true, "finish": true, "cancel": true,
	"interject": true, "retry": true, "stop": true,
}

// interactiveLifecycleAction 是用户动作（continue/finish/cancel/interject/retry/
// stop）的 v3 入口。仅当该聊天室当前讨论是 user_confirmed 且引擎就绪时接管；
// handled=false 时 system_handler 回落到 P4 的 legacy 实现。
//
// 二选一而不是并行：一个聊天室的活跃讨论要么是 v3、要么是 legacy，两个实现的
// 状态机互斥（v3 的 continue 是「等用户确认再开下一轮」，legacy 是「按 max_rounds
// 计数」），绝不能同时套上。
func (rt *discussionRuntime) interactiveLifecycleAction(ctx context.Context, action string, cmd CommandContent, actor discussionActor) (commandResult, bool) {
	if !interactiveLifecycleActions[action] {
		return commandResult{}, false
	}
	d, ok := rt.activeInteractiveDiscussion(ctx, cmd)
	if !ok {
		return commandResult{}, false
	}
	ownerRCID, roomID, err := rt.roomOwner(ctx, d.ChatroomID)
	if err != nil {
		return errResult(500, "查询聊天室失败"), true
	}
	// configuring 半成品：只允许 cancel（用户主动放弃，cancel_reason='user_abandoned'，
	// 同时回收房间释放配额，与 GC 路径同一套幂等回收逻辑）；continue/finish/interject/
	// retry/stop 对尚未确认开始的讨论无意义，一律 409。
	if d.Status == DiscussionStatusConfiguring {
		if action != "cancel" {
			return errResult(409, "讨论组仍在配置中，仅支持取消"), true
		}
		if !canControl(d, ownerRCID, actor.RcID) {
			return errResult(403, "only the discussion owner may cancel"), true
		}
		rt.reclaimSetup(ctx, d.DiscussionID, setupUserAbandonReason, actor.RcID, "user-abandon:"+d.DiscussionID)
		d2, err := rt.repo.Get(ctx, d.DiscussionID)
		if err != nil {
			return okResult(nil), true
		}
		return okResult(discussionSnapshotView(d2, nil)), true
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
