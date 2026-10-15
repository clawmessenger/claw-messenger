package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 讨论组闲聊的「按序发言」中继。
//
// 背景：讨论组（聊天室）在非讨论态下，用户发一条消息如果直接扇出给全部
// AI 成员，每个节点都会跑一轮 agent 并抢答（一窝蜂）。这里改成服务端仲裁：
// 用户消息经 POST /api/claw/chatrooms/{id}/relay 进来，按聊天室角色板的
// speaking_order 依次给每个 enabled 的 AI 成员私发 your_turn（设备端零改动），
// 收到回包后以该节点自己的融云账号把内容广播回聊天室（RC:TxtMsg，正常群聊
// 气泡），再派下一位。超时（interactiveTurnTimeout 同源配置）跳过该成员。
//
// 会话是进程内存态：闲聊不落库，服务重启时在途的一轮作废（用户重发即可）。
// 每个聊天室同时只允许一个中继会话——正在进行 v3 讨论的房间不会走到这里
//（前端在讨论 live 状态下走插话通道，不调 relay）。

const (
	relayTurnKind      = "relay"
	relayMaxTextLength = 4000

	// defaultRelayContextRounds 是会话没设置（或设成 0）时携带的历史轮数。
	// 与 Web 端「AI 对话轮数」的默认值、旧后端的默认值保持一致。
	defaultRelayContextRounds = 20

	// defaultGroupRelayRounds 是普通群聊没设置轮数时，一条用户消息触发后 AI
	// 成员之间自动接力的轮数（1 = 每人各答一次，即旧行为）。
	defaultGroupRelayRounds = 3
	// maxGroupRelayRounds 是群聊接力的轮数上限。每一轮都是「全员各跑一次
	// agent」，M 个成员 × N 轮就是 M×N 次 agent 调用，所以必须封顶：设置项里
	// 存的 20/50/100 是「上下文窗口」时代的量级，直接拿来当接力轮数会把设备
	// 与融云发送配额一起打爆。
	maxGroupRelayRounds = 10
	// maxRelayContextRounds 是 Web 端允许设置的上限（ConversationCreationFields
	// 的 ROUND_MAX），超过就按上限截断。
	maxRelayContextRounds = 100
	// maxRetainedRelayRounds 是内存里为单个会话保留的轮数上限。它是
	// 「上下文窗口」而不是历史存档，多留几轮只是为了让用户临时把上限调大时
	// 立即有料可用，超出即丢。
	maxRetainedRelayRounds = 100
	// relayPromptContextLimit 是注入进提示词的上下文（议题 + 本次已发言成员 +
	// 历史轮次）的字符总上限，与 v3 讨论那边的 interactivePromptContextLimit
	// 同源同理由：CLI 的 prompt 走 argv 时有 8000 字符上限（agents.ts 的
	// ARGV_PROMPT_MAX_CHARS、discussion_bridge.go 的 argvPromptMaxChars），
	// 超了会退化到 stdin 模式，而 openclaw / opencode / hermes 都不支持 stdin
	// —— 代价是整轮空转。用户消息和角色人设不能被挤掉，所以只压缩上下文。
	relayPromptContextLimit = 4000
	// relayReplyClip 是「本次已发言成员」单条发言的截断长度上限（实际额度还要
	// 按成员数平分，见 relayRepliesBlock）。
	relayReplyClip = 1200
	// relayHistoryReplyClip 是历史记录里单条消息的截断长度：上下文里的旧发言
	// 只需要「说了什么」，不必是完整长文，否则一两轮就把预算吃光。
	relayHistoryReplyClip = 300
)

type relayMember struct {
	rcID      string
	roleName  string
	roleInstr string
	model     string
	order     int
}

type relaySession struct {
	chatroomRCID string
	members      []relayMember
	index        int
	userText     string
	// isGroup 区分普通群组（融云 group）与聊天室（chatroom）：二者复用同一
	// 套按序派发状态机，但回包广播端点不同（/message/group/publish.json vs
	// /message/chatroom/publish.json），提示词语气也不同。
	isGroup bool
	// 每个进行中的成员一轮一个 done channel（pump 在派发前换新），避免
	// 「超时后迟到的回包 close 同一个 chan」把下一轮误推进。
	currentDone chan struct{}
	// requestID 对应 currentDone 所代表的成员派发；仅 AcceptResponse 读，
	// pump 写。写发生在派发前且 pump 单线程，读侧加锁即可。
	requestID string
	// senderRCID 是 currentDone 轮的预期回包者。
	senderRCID string
	// topic 是本讨论组的常驻议题（取最近一次讨论的议题；无历史讨论时为空）。
	// 守门型角色强依赖它判定「本轮缺少必要输入」，缺失会让整轮空转。
	topic string
	// replies 是已发言成员的 (roleName, content) 列表：后续成员的提示词带上
	// 前序发言，否则序号 2 看不到序号 1 说了什么，只能凭空表态。
	replies []relayReply
	// requesterRCID 是发起这次闲聊中继的成员（web 用户）；流式帧的
	// requester_id 字段用它，web 端 validStreamDelivery 据此校验。
	requesterRCID string
	// history 是本次中继开始前该会话已发生的最近若干轮对话，供每个成员的
	// 提示词携带上下文。它由 contextRounds（会话设置里的「AI 对话轮数」）决定
	// 长度，更早的轮次被截断 —— 这正是那个设置项的实际语义。
	history []relayRound
	// contextRounds 是本次中继携带的历史轮数上限（已归一化到
	// 1..maxRelayContextRounds）。
	contextRounds int
	// totalRounds 是本次中继要跑几轮「全员发言」。普通群聊按「AI 对话轮数」
	// 设置推进（用户发一条消息，AI 成员之间接力 N 轮）；聊天室恒为 1，保持
	// 「一轮问答」的旧行为 —— 聊天室那个设置值另有「讨论默认轮数」的语义，
	// 不能顺带当成接力轮数用。
	totalRounds int
	// roundIndex 是当前进行到第几轮（0 基）。dispatchTurn 报给设备端的
	// round 参数与提示词里的轮次提示都用它。
	roundIndex int
	// allReplies 是本次中继累计的全部发言（跨轮），只用于收尾时并入滚动
	// 缓存；s.replies 仍是「本轮」发言，每轮开始清空。
	allReplies []relayReply
}

// relayRound 是一轮已完成的对话：一条用户消息 + 该轮各位 AI 的回应。
// 只在上下文的粒度上使用，不落库。
type relayRound struct {
	userText string
	replies  []relayReply
}

type relayReply struct {
	roleName string
	content  string
}

type chatroomRelayHub struct {
	mu       sync.Mutex
	sessions map[string]*relaySession // key: chatroom/group rc id
	// rounds 是每个会话已完成的对话轮次（键与 sessions 相同），喂给下一次中继
	// 当上下文用。刻意不落库：服务重启后上下文从头开始，与「闲聊不落库」
	// （见文件头注释）保持一致。
	rounds map[string][]relayRound
	rt     *discussionRuntime
	// groupMirror 提供普通群组的成员镜像读取（relayGroupMessage 用）。
	groupMirror *GroupMirror
}

func newChatroomRelayHub(rt *discussionRuntime) *chatroomRelayHub {
	return &chatroomRelayHub{sessions: map[string]*relaySession{}, rounds: map[string][]relayRound{}, rt: rt}
}

// normalizeContextRounds 把会话设置里的轮数收敛到可用区间。0/负数（库里没
// 存过、或旧数据）按默认值处理 —— 那个设置项从来不是「0 轮上下文」，而是
// 「没设置」。
func normalizeContextRounds(value int) int {
	if value <= 0 {
		return defaultRelayContextRounds
	}
	if value > maxRelayContextRounds {
		return maxRelayContextRounds
	}
	return value
}

// normalizeGroupRelayRounds 把群聊「AI 对话轮数」收敛成接力轮数。0/负数
// （库里没存过、或旧数据）按默认值处理；超过 maxGroupRelayRounds 一律截到
// 上限 —— 这个设置项历史上是「上下文窗口」，存着 20/50/100 的老群不该因为
// 语义变化突然跑几十轮。
func normalizeGroupRelayRounds(value int) int {
	if value <= 0 {
		return defaultGroupRelayRounds
	}
	if value > maxGroupRelayRounds {
		return maxGroupRelayRounds
	}
	return value
}

// attachContextLocked 在持有 h.mu 的前提下，把该会话最近 contextRounds 轮
// 对话快照进 session.history。取值后即与 hub 的滚动缓存解耦（深拷贝），
// 后续轮次的写入不会影响在途的提示词。
func (h *chatroomRelayHub) attachContextLocked(s *relaySession) {
	if s.contextRounds <= 0 {
		return
	}
	all := h.rounds[s.chatroomRCID]
	if len(all) == 0 {
		return
	}
	if len(all) > s.contextRounds {
		all = all[len(all)-s.contextRounds:]
	}
	history := make([]relayRound, len(all))
	for i, round := range all {
		replies := make([]relayReply, len(round.replies))
		copy(replies, round.replies)
		history[i] = relayRound{userText: round.userText, replies: replies}
	}
	s.history = history
}

// recordRound 把一轮已完成的对话并入滚动缓存。空轮（没有任何成员回包）不占
// 上下文额度。h.mu 必须在 h.rounds 初始化之前就存在；map 懒初始化，兼容测试
// 里直接构造的零值 hub。
func (h *chatroomRelayHub) recordRound(key string, round relayRound) {
	if strings.TrimSpace(round.userText) == "" && len(round.replies) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rounds == nil {
		h.rounds = map[string][]relayRound{}
	}
	all := append(h.rounds[key], round)
	if len(all) > maxRetainedRelayRounds {
		trimmed := make([]relayRound, maxRetainedRelayRounds)
		copy(trimmed, all[len(all)-maxRetainedRelayRounds:])
		all = trimmed
	}
	h.rounds[key] = all
}

// StartRelay 是 web 走 IM 命令（discussion/relayMessage）进来的入口：校验
// 发起者是聊天室成员，读角色板，然后按 speaking_order 依次派发。
func (h *chatroomRelayHub) StartRelay(ctx context.Context, chatroomRCID, requesterRCID, text string) commandResult {
	if h.rt == nil || !h.rt.interactiveReady() {
		return errRelayResult(503, "讨论服务尚未初始化")
	}
	if strings.TrimSpace(text) == "" {
		return errRelayResult(400, "消息内容不能为空")
	}
	if len([]rune(text)) > relayMaxTextLength {
		return errRelayResult(400, "消息内容过长")
	}
	room, err := h.rt.queries.GetRongCloudChatroomViewByRongCloudID(ctx, chatroomRCID)
	if err != nil || !room.ID.Valid {
		return errRelayResult(404, "聊天室不存在")
	}
	memberRows, err := h.rt.queries.ListRongCloudChatroomMembersWithNode(ctx, room.ID)
	if err != nil {
		return errRelayResult(500, "查询成员失败")
	}
	// 发起者必须是聊天室成员（真人用户或其绑定 AI 的主人语义都过 member 行判定）。
	isMember := false
	for _, row := range memberRows {
		if row.MemberID.Valid && row.MemberID.String == requesterRCID {
			isMember = true
			break
		}
	}
	if !isMember {
		return errRelayResult(403, "仅聊天室成员可以发起对话")
	}
	members := relayMembersFor(memberRows)
	if len(members) == 0 {
		return errRelayResult(409, "该聊天室没有可发言的 AI 成员")
	}
	// 讨论组的议题对闲聊同样有效：后续成员「知道这个组在聊什么」是基本上下文。
	// 必须用融云 chatroom id（chatroomRCID）查：rongcloud_discussion.chatroom_id
	// 列存的是融云 id（创建时写入的 d.ChatroomID），不是 chatroom 表的 UUID
	// 主键 —— 传主键会永远查不到讨论、议题静默丢失。
	// 取最近一条带议题的讨论（不限 policy，legacy 议题同样有效）；新组还没有
	// 讨论行时退回房间名。
	topic := ""
	if discussions, err := h.rt.repo.ListByChatroom(ctx, chatroomRCID); err == nil {
		for _, d := range discussions {
			if d.Topic != "" {
				topic = d.Topic
				break
			}
		}
	}
	if topic == "" {
		topic = strings.TrimSpace(room.Name)
	}
	// HTTP 请求返回后中继要继续跑：换独立 context。
	bgCtx := context.WithoutCancel(ctx)
	if !h.Start(bgCtx, chatroomRCID, members, text, topic, requesterRCID, int(room.MaxRounds)) {
		return errRelayResult(409, "上一条消息的回应还在进行中，请稍候")
	}
	return commandResult{code: 200, data: map[string]interface{}{
		"ok":         true,
		"chatroomId": chatroomRCID,
		"speakers":   len(members),
	}}
}

func errRelayResult(code int, msg string) commandResult {
	return commandResult{code: code, message: msg}
}

// Start 为聊天室发起一次按序中继。返回 false 表示该房间已有会话在跑（前端
// 可以提示「上一条消息的回应还在进行中」）。contextRounds 是该房间的
// 「AI 对话轮数」设置：每个成员的提示词最多携带这么多轮历史对话。
func (h *chatroomRelayHub) Start(ctx context.Context, chatroomRCID string, members []relayMember, userText, topic, requesterRCID string, contextRounds int) bool {
	if len(members) == 0 {
		return false
	}
	h.mu.Lock()
	if _, busy := h.sessions[chatroomRCID]; busy {
		h.mu.Unlock()
		return false
	}
	session := &relaySession{
		chatroomRCID:  chatroomRCID,
		members:       members,
		userText:      userText,
		topic:         topic,
		requesterRCID: requesterRCID,
		contextRounds: normalizeContextRounds(contextRounds),
		// 聊天室闲聊恒为一轮：这个房间的 max_rounds 兼作「发起讨论时的默认
		// 轮数」（见 conversation-kind / InfoPanel 文案），把它顺带解释成
		// 闲聊接力轮数会让同一份配置有两个互相打架的含义。
		totalRounds: 1,
	}
	h.attachContextLocked(session)
	h.sessions[chatroomRCID] = session
	h.mu.Unlock()

	go h.pump(ctx, session)
	return true
}

// pump 依次派发成员。ctx 是请求生命周期之外的独立 context（WithoutCancel），
// 保证 HTTP 200 之后中继仍继续跑。
//
// 多轮：一轮 = 全员各发言一次。跑完一轮后若 totalRounds 还没到，就把本轮发言
// 并进历史、清空本轮发言、回到第一位成员继续 —— 这正是普通群聊「AI 之间自动
// 聊 N 轮」的实现。聊天室 totalRounds 恒为 1，循环只走一圈，行为与从前一致。
func (h *chatroomRelayHub) pump(ctx context.Context, s *relaySession) {
	timeout := h.rt.interactiveTurnTimeout()
	defer func() {
		h.mu.Lock()
		replies := append([]relayReply(nil), s.allReplies...)
		if cur, ok := h.sessions[s.chatroomRCID]; ok && cur == s {
			delete(h.sessions, s.chatroomRCID)
		}
		h.mu.Unlock()
		// 本轮结束后并入滚动缓存，供下一条用户消息携带上下文。
		h.recordRound(s.chatroomRCID, relayRound{userText: s.userText, replies: replies})
	}()
	for s.roundIndex < s.totalRounds {
		for s.index < len(s.members) {
			member := s.members[s.index]
			requestID := fmt.Sprintf("relay_%d_%d_%d", time.Now().UnixNano(), s.roundIndex, member.order)
			done := make(chan struct{})
			h.mu.Lock()
			s.requestID = requestID
			s.senderRCID = member.rcID
			s.currentDone = done
			h.mu.Unlock()
			if err := h.dispatchTurn(ctx, s, member, requestID); err != nil {
				h.rt.logger.Warn("relay: dispatch failed",
					"chatroom", s.chatroomRCID, "target", member.rcID,
					"round", s.roundIndex+1, "error", err)
				// 派发失败视同该成员本轮跳过，继续下一位。
				s.index++
				continue
			}
			select {
			case <-done:
				// 回包已处理（broadcast 已在 AcceptResponse 里完成），前进。
			case <-time.After(timeout):
				h.rt.logger.Warn("relay: turn timed out",
					"chatroom", s.chatroomRCID, "target", member.rcID,
					"round", s.roundIndex+1)
				// 超时后作废当前轮：迟到的回包因 requestID 已不匹配被丢弃。
				h.mu.Lock()
				if s.requestID == requestID {
					s.requestID = ""
				}
				h.mu.Unlock()
			case <-ctx.Done():
				return
			}
			s.index++
		}
		// 本轮全员发言结束：把这一轮并进历史，供下一轮的提示词使用。
		h.closeRoundLocked(s)
		if s.roundIndex+1 >= s.totalRounds {
			return
		}
		s.roundIndex++
		s.index = 0
	}
}

// closeRoundLocked 在持锁前提下收尾当前一轮：把本轮发言并进会话历史（下一轮
// 的提示词据此承接）、并入累计发言，然后清空本轮发言。本轮无人回包时不留
// 空轮，免得历史里多出一段「什么都没有」的记录。
func (h *chatroomRelayHub) closeRoundLocked(s *relaySession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(s.replies) == 0 {
		return
	}
	// 首轮带用户原话；后续轮次是成员之间的接力，没有新的用户消息，
	// userText 留空（renderRelayRound 会省略「用户：」那一行）。
	userText := ""
	if s.roundIndex == 0 {
		userText = s.userText
	}
	round := relayRound{userText: userText}
	round.replies = append(round.replies, s.replies...)
	s.history = append(s.history, round)
	s.allReplies = append(s.allReplies, s.replies...)
	s.replies = nil
}

// dispatchTurn 私发 your_turn 给成员（CLI 零改动的既有协议）。
func (h *chatroomRelayHub) dispatchTurn(ctx context.Context, s *relaySession, member relayMember, requestID string) error {
	prompt := h.buildPrompt(s, member)
	convType := 4
	if s.isGroup {
		convType = 3
	}
	params := map[string]interface{}{
		"round":          s.roundIndex + 1,
		"speaking_order": member.order,
		"role_name":      member.roleName,
		"chatroom_id":    s.chatroomRCID,
		"role_prompt":    prompt,
		// 流式帧路由参数：CLI 据此把 chat_stream 帧直接广播进群/聊天室
		// 会话（web 端 validStreamDelivery 校验 conversation 一致性）。
		"conversation_type": convType,
		"requester_id":      s.requesterRCID,
	}
	if member.model != "" {
		params["model"] = member.model
	}
	content, _ := json.Marshal(map[string]interface{}{
		"request_id": requestID,
		"service":    "discussion",
		"action":     "your_turn",
		"params":     params,
	})
	return h.rt.sendPrivateJSON(ctx, member.rcID, content)
}

// buildPrompt 组装闲聊提示词：群聊上下文 + 用户消息 + 角色人设 + 本组议题 +
// 前序成员发言。守门型/承接型角色（如「守门人」）没有议题与前序发言会直接
// 拒绝实质回应，整轮空转。
func (h *chatroomRelayHub) buildPrompt(s *relaySession, member relayMember) string {
	instr := ""
	if member.roleInstr != "" {
		instr = "\n" + member.roleInstr + "\nStay in character."
	}
	venue := "聊天室"
	scope := "这是讨论组的一次自由对话。用户刚在聊天室发言"
	if s.isGroup {
		venue = "群聊"
		scope = "这是普通群聊的一次自由对话。用户刚在群里发言"
	}
	// 接力轮次（普通群聊 > 1 轮时）：从第二轮起用户并没有再发言，是成员之间
	// 接着聊。抬头必须换掉，否则 agent 会以为用户又提了一遍同样的问题，把
	// 上一轮的回答原样再输出一遍。
	roundNote := ""
	if s.totalRounds > 1 {
		if s.roundIndex > 0 {
			scope = "这是普通群聊的一次自由讨论。成员们在接着上一轮的发言继续聊下去，用户这一轮没有再发言"
			roundNote = fmt.Sprintf("\n\n现在是第 %d/%d 轮接力：请顺着上面成员们的观点继续推进（补充、质疑、收敛都行），不要重复你已经说过的内容。",
				s.roundIndex+1, s.totalRounds)
		} else {
			roundNote = fmt.Sprintf("\n\n用户发一条消息后，你们会接力讨论 %d 轮；现在从你开始。", s.totalRounds)
		}
	}
	// 本组议题（来自最近一次讨论）：讨论组的闲聊不是无根之水，成员应知道
	// 这个组围绕什么展开。无历史讨论时整段省略。
	contextBlock := ""
	if s.topic != "" {
		contextBlock = fmt.Sprintf("\n\n本讨论组的议题：%s", s.topic)
	}
	// 注入的上下文（本次已发言成员 + 历史轮次）共享一个字符预算，与 v3 讨论
	// 那边的 interactivePromptContextLimit 同源：CLI 的 prompt 走 argv 有 8000
	// 字符上限，超了会退化到 stdin，而 openclaw/opencode/hermes 都不支持 stdin
	// ——整轮空转。用户消息与角色人设必须留下，所以压缩的只能是上下文。
	budget := relayPromptContextLimit - len([]rune(contextBlock)) - len([]rune(instr))
	if budget < 0 {
		budget = 0
	}
	// 前序成员发言（本次中继已广播的）：后续成员承接而非凭空重复表态。优先级
	// 高于历史轮次，先按成员数平分预算。
	if len(s.replies) > 0 {
		block, used := relayRepliesBlock(s.replies, budget)
		contextBlock += block
		budget -= used
	}
	// 历史上下文：「AI 对话轮数」设置决定最多携带几轮，更早的已截断；装不进
	// 剩余预算的旧轮次也会被丢掉。设备端的 agent 会话只有 openclaw 跨界保留，
	// 其余每轮都是新会话 —— 没有这一段，除了第一个发言者以外所有人都不记得
	// 上一轮聊过什么。
	contextBlock += relayHistoryBlock(s.history, budget)
	return fmt.Sprintf(
		"[group chat] %s：\n\n%s%s%s\n\n"+
			"请以「%s」的身份回应用户。回应会被广播回%s，请只输出回应正文，不要输出多余格式。%s",
		scope, s.userText, contextBlock, roundNote, member.roleName, venue, instr,
	)
}

// clipRelayText 截断超长文本，保留开头并注明截断。
func clipRelayText(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…（已截断）"
}

// clipToBudget 把 text 压进 budget 个字符，返回结果与实际占用的额度。
func clipToBudget(text string, budget int) (string, int) {
	if budget <= 0 || text == "" {
		return "", 0
	}
	runes := []rune(text)
	if len(runes) <= budget {
		return text, len(runes)
	}
	return string(runes[:budget]) + "…（已截断）", budget
}

// relayRepliesBlock 渲染「本次对话中已发言成员的观点」段。预算按成员数平分，
// 免得前几位把额度吃光、后面发言的人根本看不到前面说了什么。
func relayRepliesBlock(replies []relayReply, budget int) (string, int) {
	if len(replies) == 0 || budget <= 0 {
		return "", 0
	}
	perReply := budget / len(replies)
	if perReply > relayReplyClip {
		perReply = relayReplyClip
	}
	lines := make([]string, 0, len(replies))
	for _, reply := range replies {
		lines = append(lines, fmt.Sprintf("- %s：%s", reply.roleName, clipRelayText(reply.content, perReply)))
	}
	return clipToBudget(fmt.Sprintf("\n\n本次对话中已发言成员的观点（承接或核验后再发言，不要凭空表态）：\n%s",
		strings.Join(lines, "\n")), budget)
}

// relayHistoryBlock 把最近若干轮已完成对话渲染成提示词里的上下文段。空历史、
// 零预算返回空串（整段省略，旧报文形态逐字不变）。只渲染装得进 budget 的
// **最新**轮次：装不下的旧轮次直接丢弃 —— 宁可少给上下文，也不能把提示词撑过
// CLI 的 argv 上限（见 relayPromptContextLimit）。
func relayHistoryBlock(history []relayRound, budget int) string {
	if len(history) == 0 || budget <= 0 {
		return ""
	}
	blocks := make([]string, 0, len(history))
	used := 0
	// 从最近一轮往前装，装不下就停。
	for i := len(history) - 1; i >= 0; i-- {
		block := renderRelayRound(history[i])
		length := len([]rune(block))
		if used+length > budget {
			if len(blocks) == 0 {
				// 最新一轮自己就超预算：截断它，至少给一段上下文。
				clipped, spent := clipToBudget(block, budget)
				if clipped != "" {
					blocks = append(blocks, clipped)
					used += spent
				}
			}
			break
		}
		blocks = append(blocks, block)
		used += length
	}
	if len(blocks) == 0 {
		return ""
	}
	// 翻回时间顺序（旧 → 新）。
	for i, j := 0, len(blocks)-1; i < j; i, j = i+1, j-1 {
		blocks[i], blocks[j] = blocks[j], blocks[i]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n最近的对话记录（只保留最近 %d 轮，更早的已截断；仅作背景，不要复述）：",
		len(blocks))
	for _, block := range blocks {
		b.WriteString(block)
	}
	return b.String()
}

// renderRelayRound 渲染一轮记录：用户的提问 + 该轮各位成员的回应。接力讨论
// 的第二轮起没有新的用户消息，那一段整行省略（只留成员的发言）。
func renderRelayRound(round relayRound) string {
	var b strings.Builder
	if strings.TrimSpace(round.userText) != "" {
		b.WriteString("\n用户：")
		b.WriteString(clipRelayText(round.userText, relayHistoryReplyClip))
	}
	for _, reply := range round.replies {
		fmt.Fprintf(&b, "\n- %s：%s", reply.roleName, clipRelayText(reply.content, relayHistoryReplyClip))
	}
	return b.String()
}

// AcceptResponse 处理设备对 relay your_turn 的回包。返回 false 表示这条
// command_result 不属于任何 relay 会话（webhook 继续找别的消费方）。
func (h *chatroomRelayHub) AcceptResponse(ctx context.Context, senderID, requestID, content string, failed bool, errMsg string) bool {
	if requestID == "" || !strings.HasPrefix(requestID, "relay_") {
		return false
	}
	h.mu.Lock()
	var done chan struct{}
	var session *relaySession
	var member relayMember
	var memberFound bool
	for _, s := range h.sessions {
		if s.requestID == requestID {
			session = s
			if s.senderRCID == senderID && s.index < len(s.members) {
				member = s.members[s.index]
				memberFound = true
				done = s.currentDone
			}
			break
		}
	}
	h.mu.Unlock()
	if session == nil {
		// requestID 前缀匹配但找不到会话：会话已结束或被超时作废。吞掉
		// 这条迟到回包（不再广播），也不再让它流向旧协调器刷 Warn。
		return true
	}
	if !memberFound || done == nil {
		// 回包者不是当前轮的预期成员：吞掉，不推进。
		return true
	}
	if failed {
		h.rt.logger.Warn("relay: member turn failed",
			"chatroom", session.chatroomRCID, "target", senderID, "error", errMsg)
	} else if content != "" {
		h.broadcastMemberSpeech(ctx, session, &member, content)
		// 积累进会话上下文：后续成员的提示词会带上这条发言。
		h.mu.Lock()
		if s := session; s.requestID == requestID {
			s.replies = append(s.replies, relayReply{roleName: member.roleName, content: content})
		}
		h.mu.Unlock()
	} else {
		// 回包成功但正文为空。以前这里直接静默跳过 —— 用户看到的是「发了消息
		// 却一条回复都没有」，完全无从判断是没在跑还是跑了没产出。设备端只把
		// agent 的 stdout 当正文，而「生成个 HTML」这类请求多数是把文件写进
		// 工作目录、stdout 近乎为空，于是空回包成了常态而非异常。
		// 这里补一条占位气泡：中继照常推进，但空产出对用户可见。
		h.rt.logger.Warn("relay: member returned empty content",
			"chatroom", session.chatroomRCID, "target", senderID,
			"round", session.roundIndex+1)
		h.broadcastMemberSpeech(ctx, session, &member,
			fmt.Sprintf("（%s 本轮没有产出内容）", member.roleName))
	}
	// 作废当前轮的 requestID（防重复回包重复推进），再唤醒 pump。
	h.mu.Lock()
	if s := session; s.requestID == requestID {
		s.requestID = ""
	}
	h.mu.Unlock()
	close(done)
	return true
}

// broadcastMemberSpeech 以成员节点自己的融云账号把回应广播回聊天室，
// 呈现为一条正常的群聊文本气泡。
func (h *chatroomRelayHub) broadcastMemberSpeech(ctx context.Context, s *relaySession, member *relayMember, content string) {
	if h.rt.client == nil {
		return
	}
	trimmed := content
	if len(trimmed) > relayMaxTextLength {
		trimmed = trimmed[:relayMaxTextLength]
	}
	textPayload, _ := json.Marshal(map[string]interface{}{"content": trimmed})
	var sendErr error
	if s.isGroup {
		_, sendErr = h.rt.client.sendGroupMessage(
			ctx, member.rcID, s.chatroomRCID, objectNameText, string(textPayload),
		)
	} else {
		_, sendErr = h.rt.client.sendChatroomMessage(
			ctx, member.rcID, s.chatroomRCID, objectNameText, string(textPayload),
		)
	}
	if sendErr != nil {
		h.rt.logger.Warn("relay: chatroom broadcast failed",
			"chatroom", s.chatroomRCID, "from", member.rcID, "error", sendErr)
	}
}

// relayMembersFor 从聊天室成员行提取按序发言的 AI 成员列表。
func relayMembersFor(rows []db.ListRongCloudChatroomMembersWithNodeRow) []relayMember {
	members := make([]relayMember, 0, len(rows))
	order := 0
	for _, row := range rows {
		if row.MemberType != "ai" || !row.Enabled || !row.NodeRcUserID.Valid || row.NodeRcUserID.String == "" {
			continue
		}
		members = append(members, relayMember{
			rcID:      row.NodeRcUserID.String,
			roleName:  relayRoleName(row),
			roleInstr: row.RoleInstructions.String,
			model:     relayModel(row),
			order:     order,
		})
		order++
	}
	return members
}

func relayRoleName(row db.ListRongCloudChatroomMembersWithNodeRow) string {
	if row.RoleName.Valid && row.RoleName.String != "" {
		return row.RoleName.String
	}
	if row.Nickname != "" {
		return row.Nickname
	}
	return row.NodeRcUserID.String
}

func relayModel(row db.ListRongCloudChatroomMembersWithNodeRow) string {
	if row.DiscussionModel.Valid && row.DiscussionModel.String != "" {
		return row.DiscussionModel.String
	}
	return row.Model.String
}
