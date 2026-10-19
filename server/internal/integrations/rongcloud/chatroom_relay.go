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
	// relayPromptContextLimit 是注入进提示词的上下文（议题 + 刚才接过的话 +
	// 更早的轮次）的字符总上限，与 v3 讨论那边的 interactivePromptContextLimit
	// 同源同理由：CLI 的 prompt 走 argv 时有 8000 字符上限（agents.ts 的
	// ARGV_PROMPT_MAX_CHARS、discussion_bridge.go 的 argvPromptMaxChars），
	// 超了会退化到 stdin 模式，而 openclaw / opencode / hermes 都不支持 stdin
	// —— 代价是整轮空转。用户起头的话与指令不能被挤掉，所以只压缩上下文。
	relayPromptContextLimit = 4000
	// relayPromptTotalLimit 是整条提示词的安全上限，比 CLI 的 argv 上限
	// （8000）留出余量。上下文预算与它扣除固定文案后的余量取小：用户起的头
	// 最长可达 relayMaxTextLength（4000），加上场景/指令/身份约束之后，光靠
	// relayPromptContextLimit 兜不住 argv 上限 —— 一旦越界整轮退化成 stdin
	// 模式空转，而且用户看到的是「发了消息没人应」。
	relayPromptTotalLimit = 7000
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
	// replies 是**本轮**已发言成员的 (roleName, content) 列表（每轮开始清空）。
	// 它就是接力链条的当前段：下一位的提示词据此接住紧邻的上一位 —— 最后一条
	// 即「要接的话头」（见 lastSpeaker / relayRepliesBlock）。
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
	// totalRounds 是本次接龙要跑几轮「全员发言」。普通群聊按「AI 对话轮数」
	// 设置推进（用户起个头，AI 成员之间接着上一位的话来回接龙 N 轮）；聊天室
	// 恒为 1，保持「一轮问答」的旧行为 —— 聊天室那个设置值另有「讨论默认轮数」
	// 的语义，不能顺带当成接龙轮数用。
	totalRounds int
	// roundIndex 是当前进行到第几轮（0 基）。dispatchTurn 报给设备端的
	// round 参数与提示词里的轮次提示都用它。
	roundIndex int
	// allReplies 是本次中继累计的全部发言（跨轮），只用于收尾时并入滚动
	// 缓存；s.replies 仍是「本轮」发言，每轮开始清空。
	allReplies []relayReply
	// turnExt 从派发起每次成员发言流帧都会触碰的续期句柄：流还在动 = 成员
	// 还活着，pump 的等待计时就该重置，不能按固定超时把长思考轮掐了
	// （hermes 长思考 90~120s，默认 60s 超时一到下一位就开跑，用户看到的
	// 「上一位还在流式下一位已开跑」就是它）。nil 表示当前无轮可续。
	turnExt *turnExtension
	// pending 是「中继进行中用户又发了消息」时排下的下一段中继。旧行为是
	// 直接回 409「上一条消息的回应还在进行中」，用户那条消息就永远没人接
	// ——长思考轮下尤其明显（等 3 分钟才轮到自己说话，却说不上）。
	// 现在改为排队：当前中继跑完，接着用 pending 起一段新的。
	pending *relayPending
}

// relayPending 是中继忙时排队的用户消息（同一房间只留最新一条，多条按
// 换行拼接，不丢内容）。
type relayPending struct {
	userText      string
	requesterRCID string
	totalRounds   int
	contextRounds int
}

// enqueueLocked 把一条用户消息排进会话待办；调用方持有 h.mu。
func enqueueLocked(s *relaySession, userText, requesterRCID string, totalRounds, contextRounds int) {
	if s.pending == nil {
		s.pending = &relayPending{
			userText:      userText,
			requesterRCID: requesterRCID,
			totalRounds:   totalRounds,
			contextRounds: contextRounds,
		}
		return
	}
	merged := s.pending.userText + "\n" + userText
	if runes := []rune(merged); len(runes) > relayMaxTextLength {
		merged = string(runes[len(runes)-relayMaxTextLength:])
	}
	s.pending.userText = merged
	s.pending.requesterRCID = requesterRCID
	s.pending.totalRounds = totalRounds
	s.pending.contextRounds = contextRounds
}

// startQueued 在当前中继收尾后接着跑排队的那条用户消息。复用同一份成员
// 列表与房间形态（群/聊天室、议题），只把 userText/requester 换成新的。
func (h *chatroomRelayHub) startQueued(ctx context.Context, prev *relaySession, p *relayPending) {
	next := &relaySession{
		chatroomRCID:  prev.chatroomRCID,
		members:       prev.members,
		userText:      p.userText,
		isGroup:       prev.isGroup,
		topic:         prev.topic,
		requesterRCID: p.requesterRCID,
		contextRounds: p.contextRounds,
		totalRounds:   p.totalRounds,
	}
	h.mu.Lock()
	h.attachContextLocked(next)
	h.sessions[prev.chatroomRCID] = next
	h.mu.Unlock()
	h.rt.logger.Info("relay: queued message started",
		"chatroom", prev.chatroomRCID, "chars", len([]rune(p.userText)))
	go h.pump(ctx, next)
}

// turnExtension 是当前轮的可重置计时：NotifyStreamActivity 收到心跳时把
// deadline 重置，pump 的等待循环每次醒来都看它。互斥量就是 relayHub 的 h.mu。
type turnExtension struct {
	deadline time.Time
}

// minDuration 返回较小者，pump 等待循环用它限制单次睡眠。
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
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
	// silent 标记这不是成员的真实发言，而是「本轮没交出正文」的占位。
	// 它必须留在**本轮**接力链上（下一位得知道上一位空手，否则以为自己第一个
	// 接、或者接错人），但不能进历史存档 —— 用户下次发消息时，上下文里不该
	// 出现「（X 本轮没有产出内容）」这种噪音。
	silent bool
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
	started, queued := h.StartOrQueue(bgCtx, chatroomRCID, members, text, topic, requesterRCID, int(room.MaxRounds))
	if !started && !queued {
		return errRelayResult(409, "当前无法发起对话，请稍后重试")
	}
	return commandResult{code: 200, data: map[string]interface{}{
		"ok":         true,
		"chatroomId": chatroomRCID,
		"speakers":   len(members),
		"queued":     queued,
	}}
}

func errRelayResult(code int, msg string) commandResult {
	return commandResult{code: code, message: msg}
}

// Start 为聊天室发起一次按序中继。返回 false 表示该房间已有会话在跑（前端
// 可以提示「上一条消息的回应还在进行中」）。contextRounds 是该房间的
// 「AI 对话轮数」设置：每个成员的提示词最多携带这么多轮历史对话。
func (h *chatroomRelayHub) Start(ctx context.Context, chatroomRCID string, members []relayMember, userText, topic, requesterRCID string, contextRounds int) bool {
	started, _ := h.StartOrQueue(ctx, chatroomRCID, members, userText, topic, requesterRCID, contextRounds)
	return started
}

// StartOrQueue 与 Start 相同，差别只在房间忙时的处理：不拒绝，而是把这条
// 用户消息排进当前会话的待办（pump 收尾后自动接着跑）。返回 (started, queued)：
// started=false && queued=true 表示已排队；两者都 false 表示没有可发言成员。
//
// 为什么要排队：群/聊天室的中继一轮要等每位成员跑完 agent（长思考 1~3 分钟），
// 用户在等待期间发消息是极自然的动作。旧行为回 409，消息就永远没有回应，
// 用户在 UI 上只看到「发出去了没人理」。
func (h *chatroomRelayHub) StartOrQueue(ctx context.Context, chatroomRCID string, members []relayMember, userText, topic, requesterRCID string, contextRounds int) (bool, bool) {
	if len(members) == 0 {
		return false, false
	}
	h.mu.Lock()
	if existing, busy := h.sessions[chatroomRCID]; busy {
		enqueueLocked(existing, userText, requesterRCID, existing.totalRounds,
			normalizeContextRounds(contextRounds))
		h.mu.Unlock()
		return false, true
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
	return true, false
}

// pump 依次派发成员。ctx 是请求生命周期之外的独立 context（WithoutCancel），
// 保证 HTTP 200 之后中继仍继续跑。
//
// 多轮：一轮 = 全员各发言一次。跑完一轮后若 totalRounds 还没到，就把本轮发言
// 并进历史、清空本轮发言、回到第一位成员继续 —— 这正是普通群聊「AI 之间自动
// 聊 N 轮」的实现。聊天室 totalRounds 恒为 1，循环只走一圈，行为与从前一致。
func (h *chatroomRelayHub) pump(ctx context.Context, s *relaySession) {
	timeout := h.rt.interactiveTurnTimeout()
	// 保底：中继单轮等待不低于 relayTurnTimeoutFloor（见常量注释）。
	extended := false
	if timeout < relayTurnTimeoutFloor {
		timeout = relayTurnTimeoutFloor
		extended = true
	}
	if extended {
		h.rt.logger.Info("relay: turn timeout raised to floor",
			"chatroom", s.chatroomRCID, "timeout", timeout)
	}
	defer func() {
		h.mu.Lock()
		replies := append([]relayReply(nil), s.allReplies...)
		pending := s.pending
		if cur, ok := h.sessions[s.chatroomRCID]; ok && cur == s {
			delete(h.sessions, s.chatroomRCID)
		}
		h.mu.Unlock()
		// 本轮结束后并入滚动缓存，供下一条用户消息携带上下文。
		h.recordRound(s.chatroomRCID, relayRound{userText: s.userText, replies: replies})
		// 用户在中继期间发的消息：现在接着跑，不再静默丢弃。
		if pending != nil {
			h.startQueued(ctx, s, pending)
		}
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
			// 新一轮派发：从现在起计时；流帧续期从此句柄进入。
			ext := &turnExtension{deadline: time.Now().Add(timeout)}
			s.turnExt = ext
			h.mu.Unlock()
			if err := h.dispatchTurn(ctx, s, member, requestID); err != nil {
				h.rt.logger.Warn("relay: dispatch failed",
					"chatroom", s.chatroomRCID, "target", member.rcID,
					"round", s.roundIndex+1, "error", err)
				// 派发失败视同该成员本轮跳过，继续下一位。
				h.mu.Lock()
				if s.turnExt == ext {
					s.turnExt = nil
				}
				h.mu.Unlock()
				s.index++
				continue
			}
			// 可重置的等待：成员每发一个流帧就把 deadline 往后推 —— 流还在
			// 动说明它还在产出，绝不能按固定超时把长思考轮掐掉。轮询间隔
			// 500ms，与派发粒度同量级；「deadline 后仍无回执」才判超时。
		waitLoop:
			for {
				h.mu.Lock()
				now := time.Now()
				remaining := ext.deadline.Sub(now)
				if s.turnExt != ext || s.requestID != requestID {
					h.mu.Unlock()
					break waitLoop // 轮已被作废/推进，别再等
				}
				if remaining <= 0 {
					h.mu.Unlock()
					// 超时后作废当前轮：迟到的回包因 requestID 已不匹配被丢弃。
					h.rt.logger.Warn("relay: turn timed out",
						"chatroom", s.chatroomRCID, "target", member.rcID,
						"round", s.roundIndex+1, "timeout", timeout)
					h.mu.Lock()
					stale := s.requestID != requestID
					if !stale {
						s.requestID = ""
					}
					if s.turnExt == ext {
						s.turnExt = nil
					}
					h.mu.Unlock()
					// 超时不能只写日志：前端那条「正在思考」的气泡在等一条
					// 结束信号，群里什么都不发就会永远转圈（且下一位已经开口，
					// 用户看到的是「上一位卡住、下一位抢话」）。补一条占位。
					if !stale {
						h.noteSkippedMember(ctx, s, &member, relayTimeoutNote(member.roleName))
					}
					break waitLoop
				}
				h.mu.Unlock()
				select {
				case <-done:
					break waitLoop // 回包已处理（broadcast 在 AcceptResponse 里完成），前进。
				case <-ctx.Done():
					return
				case <-time.After(minDuration(remaining, 500*time.Millisecond)):
					// 回头重新检查 deadline（可能已被流帧推后）。
				}
			}
			h.mu.Lock()
			if s.turnExt == ext {
				s.turnExt = nil
			}
			h.mu.Unlock()
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

// closeRoundLocked 在持锁前提下收尾当前一轮：把本轮真实发言并进会话历史（下一
// 轮的提示词据此承接）、并入累计发言，然后清空本轮发言。本轮无人回包时不留空轮，
// 免得历史里多出一段「什么都没有」的记录。
//
// 占位（silent）只活在**本轮**接力链上，不并入历史与累计发言：它们是「某位成员
// 这轮空手」的提示，进了存档就变成用户下次对话里的噪音。
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
	for _, reply := range s.replies {
		if reply.silent {
			continue
		}
		round.replies = append(round.replies, reply)
		s.allReplies = append(s.allReplies, reply)
	}
	s.history = append(s.history, round)
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

// lastSpeaker 返回这轮该「接住」的那条发言：本轮已发言者里的最后一位 —— 接龙
// 接的是紧邻的上一位，不是所有人。本轮还没人说话时（每轮的第一位）回看历史最后
// 一轮的最后一条，这样第二轮的第一位接的仍是上一轮的收尾，而不是从头重来。
// 返回 false 表示这是整场聊天的第一位发言者，只有用户起的头可接。
//
// 只在 pump goroutine 内调用（与 buildPrompt 同侧）。
func (s *relaySession) lastSpeaker() (relayReply, bool) {
	if n := len(s.replies); n > 0 {
		return s.replies[n-1], true
	}
	for i := len(s.history) - 1; i >= 0; i-- {
		if n := len(s.history[i].replies); n > 0 {
			return s.history[i].replies[n-1], true
		}
	}
	return relayReply{}, false
}

// buildPrompt 组装一次发言的提示词。
//
// 语义是「接龙」：用户只起了个头，之后是成员之间接着上一位的话往下聊。因此
// 指令必须指向**紧邻的上一位**（「接住「X」的话」），而不是笼统的「回应用户」
// —— 旧措辞把 agent 钉在「对用户表态」上，于是每位成员都独立喊一句客套话，
// 谁也不接谁（线上表现为「各说各的」）。
func (h *chatroomRelayHub) buildPrompt(s *relaySession, member relayMember) string {
	instr := ""
	if member.roleInstr != "" {
		instr = "\n" + member.roleInstr + "\nStay in character."
	}
	venue := "聊天室"
	room := "讨论组"
	if s.isGroup {
		venue = "群聊"
		room = "群"
	}
	prev, hasPrev := s.lastSpeaker()

	scope := fmt.Sprintf("这是%s里的一次接龙聊天：用户起了个头，成员们依次接着上一位的话往下聊。", venue)
	// 用户起的头是整场聊天的话题锚点。它是接力链条的起点，历史段可能因为预算
	// 被截掉，所以这一段始终保留、不参与上下文压缩。
	opening := ""
	if t := strings.TrimSpace(s.userText); t != "" {
		opening = "\n\n用户起的头：" + t
	}
	// 本轮要做的事：接住谁的话。第一位（只有用户起头可接）是「开接」。
	turn := "\n\n现在轮到你开接：顺着用户起的头说下去，说完把话头留给下一位。"
	if hasPrev {
		if prev.silent {
			// 上一位空手（「生成个文件」这类任务 stdout 近空是常态）：没有话可接，
			// 让下一位自己把话头续上 —— 否则它会对着「（X 本轮没有产出内容）」
			// 硬接一句，或者干脆又去重复用户的话。
			turn = fmt.Sprintf(
				"\n\n现在轮到你：「%s」这轮没交出发言正文，请你自己把话头续下去（顺着上面的对话往下说就行），说完把话头留给下一位；不要重复别人已经说过的内容。",
				prev.roleName,
			)
		} else {
			turn = fmt.Sprintf(
				"\n\n现在轮到你：请接住「%s」刚才的话往下说（赞同、补充、追问、反驳都行），说完把话头留给下一位；不要重复别人已经说过的内容。",
				prev.roleName,
			)
		}
	}
	if s.totalRounds > 1 {
		turn += fmt.Sprintf("（第 %d/%d 轮接龙）", s.roundIndex+1, s.totalRounds)
	}
	tail := fmt.Sprintf("\n\n请以「%s」的身份发言，只输出发言正文，不要输出多余格式。%s", member.roleName, instr)

	// 固定文案先占额度，剩下的才是上下文（议题 + 刚才接过的话 + 更早的轮次）的
	// 预算 —— 用户起的头与本轮指令绝不能被上下文挤掉，同时整条提示词不越
	// argv 上限（越界即退化 stdin，整轮空转）。
	fixed := len([]rune(scope + opening + turn + tail))
	budget := relayPromptContextLimit
	if room := relayPromptTotalLimit - fixed; room < budget {
		budget = room
	}
	if budget < 0 {
		budget = 0
	}

	contextBlock := ""
	if s.topic != "" && budget > 0 {
		if block, used := clipToBudget(fmt.Sprintf("\n\n本%s的议题：%s", room, s.topic), budget); block != "" {
			contextBlock = block
			budget -= used
		}
	}
	// 刚才接过的话（本次中继已广播的）：接力链条上最近的一段，优先级高于更早的
	// 轮次 —— 接不上紧邻的上一位，比看不到早几轮严重得多。
	if len(s.replies) > 0 && budget > 0 {
		block, used := relayRepliesBlock(s.replies, budget)
		contextBlock += block
		budget -= used
	}
	contextBlock += relayHistoryBlock(s.history, budget)

	return "[group chat] " + scope + opening + contextBlock + turn + tail
}

// relayClipToRoom 把 text 压进 room 个字符，**结果长度保证不超过 room** ——
// 含截断标记。clipToBudget 做不到这点：它返回「budget 个字符 + 标记」，调用方
// 按它返回的额度扣预算就会超支。预算紧张的接力段必须用这个。
func relayClipToRoom(text string, room int) string {
	if room <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= room {
		return text
	}
	if keep := room - len([]rune(relayClipSuffix)); keep > 0 {
		return string(runes[:keep]) + relayClipSuffix
	}
	// 连标记都放不下：只留正文前缀，同样不越界。
	return string(runes[:room])
}

// relayEmptyOutputNote 是成员本轮没交出正文时的占位文案：既作为气泡广播给
// 用户（否则用户看到的是「发了消息却没人应」），也记进本轮发言 —— 接力链条上
// 下一位必须知道上一位「说过了但没内容」，才不会以为前面没人说话。
func relayEmptyOutputNote(roleName string) string {
	return fmt.Sprintf("（%s 本轮没有产出内容）", roleName)
}

// relayTimeoutNote / relayTurnFailedNote 是「这一轮该成员没交出发言」的两条
// 占位。它们的存在不是为了内容，而是**给前端一个结束信号**：成员的「正在
// 思考」气泡靠一条属于该成员的消息（或流的终帧）收尾，超时/失败时服务端若
// 什么都不发，卡片就永远停在转圈状态 —— 而下一轮成员已经开口，用户看到的
// 就是「上一位卡住不动，下一位抢话」。
func relayTimeoutNote(roleName string) string {
	return fmt.Sprintf("（%s 本轮思考超时，已跳过）", roleName)
}

func relayTurnFailedNote(roleName string) string {
	return fmt.Sprintf("（%s 本轮未能发言）", roleName)
}

// noteSkippedMember 广播「本轮没交出发言」的占位，并记进本轮发言（silent：
// 不进历史与上下文，避免下次对话里出现一堆噪音）。
// 不持 h.mu 调用 —— broadcastMemberSpeech 自己会取锁。
func (h *chatroomRelayHub) noteSkippedMember(ctx context.Context, s *relaySession, member *relayMember, note string) {
	h.broadcastMemberSpeech(ctx, s, member, note)
	h.mu.Lock()
	defer h.mu.Unlock()
	s.replies = append(s.replies, relayReply{roleName: member.roleName, content: note, silent: true})
}

// relayClipSuffix 是截断标记。单列成常量是因为 relayRepliesBlock 必须能为它
// 预留位置：截断后的真实长度是「额度 + 后缀」，调用方按额度扣预算会悄悄超支。
const relayClipSuffix = "…（已截断）"

// clipRelayText 截断超长文本，保留开头并注明截断。
func clipRelayText(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + relayClipSuffix
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
	return string(runes[:budget]) + relayClipSuffix, budget
}

// relayRepliesBlock 渲染「刚才已经接过的话」段。接龙场景里要紧的是**紧邻的上
// 一位**，所以先用完整额度保住最后一条（relayReplyClip），更早的发言再按剩余
// 预算平分 —— 接不上上一位比看不到更早的人严重得多。
//
// 返回段落与它实际占用的额度，且保证 (header + 正文) 不超过 budget（含换行
// 分隔符）：调用方按返回值扣减预算，少算一个字符都会挤掉后面的历史段。
func relayRepliesBlock(replies []relayReply, budget int) (string, int) {
	if len(replies) == 0 || budget <= 0 {
		return "", 0
	}
	const header = "\n\n刚才已经接过的话（接住最后一位，不要重复别人的内容）：\n"
	headerLen := len([]rune(header))
	if headerLen >= budget {
		return clipToBudget(header, budget)
	}
	// 最后一条 = 这轮要接住的话头。
	last := replies[len(replies)-1]
	lastLine := relayClipToRoom(relayReplyLine(last, relayReplyClip), budget-headerLen)
	// 更早的发言按剩余额度平分；末尾还要留一个换行给「要接的那位」。
	remaining := budget - headerLen - len([]rune(lastLine)) - 1
	lines := make([]string, 0, len(replies))
	if earlier := replies[:len(replies)-1]; len(earlier) > 0 && remaining > 0 {
		perReply := remaining / len(earlier)
		if perReply > relayReplyClip {
			perReply = relayReplyClip
		}
		for _, reply := range earlier {
			line := relayReplyLine(reply, perReply)
			sep := 0
			if len(lines) > 0 {
				sep = 1 // 与上一条之间的换行
			}
			if l := len([]rune(line)); l+sep <= remaining {
				lines = append(lines, line)
				remaining -= l + sep
			}
		}
	}
	// 顺序保持先来后到，要接的那位放最后 —— 它就是接力锚点。
	body := lastLine
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n" + lastLine
	}
	return header + body, headerLen + len([]rune(body))
}

// relayReplyLine 渲染接力段里的一行。空产出的成员换一句独立说法：直接贴占位
// 气泡的文案会读成「hermes：（hermes 本轮没有产出内容）」，角色名重复一遍。
func relayReplyLine(reply relayReply, limit int) string {
	if reply.silent {
		return fmt.Sprintf("- %s：（本轮没有产出内容）", reply.roleName)
	}
	return fmt.Sprintf("- %s：%s", reply.roleName, clipRelayText(reply.content, limit))
}

// relayHistoryBlock 把最近若干轮已完成对话渲染成提示词里的上下文段。空历史、
// 零预算返回空串（整段省略）。只渲染装得进 budget 的**最新**轮次：装不下的旧
// 轮次直接丢弃 —— 宁可少给上下文，也不能把提示词撑过 CLI 的 argv 上限（见
// relayPromptContextLimit）。
//
// 措辞是「把话头往下带」而不是「不要复述」：接龙本来就要求承接前文，明说
// 「仅作背景、不要复述」会把 agent 推回「各自表态」的老路。
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
	fmt.Fprintf(&b, "\n\n更早的对话（只保留最近 %d 轮，更早的已截断；顺着往下聊，不要重复）：",
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

// relayPingPrefix 是设备端流式心跳 command_result 的 request_id 前缀：
// `relay_ping_<原轮次 request_id>`。CLI 在流式期间每隔 15s 发一条心跳（复用
// command 通道，不加新消息类型）；服务端拦下它只做续期，绝不当成完成回包。
// 流帧本身是群/聊天室广播、不经服务端 webhook —— 这是服务端唯一能感知
// 「成员还在产出」的信号。老设备不发心跳 → 行为与从前一致（纯固定超时）。
const relayPingPrefix = "relay_ping_"

// relayTurnTimeoutFloor 是群聊/聊天室中继单轮的**保底**等待时长，不受管理端
// 「系统主持 timeout_seconds」（初始 60s）影响。群聊里长思考的运行时（openclaw
// 这类不流式的）单轮动辄 90~120s，60s 一到服务端就放行下一位，用户看到的就是
// 「上一位还在思考，下一位已经开跑」。心跳能续期，但心跳链路的任何一环出问题
// 都不该让房间越序 —— 保底值兜住常见的长轮，心跳负责更长的情况。
//
// 180s → 300s：17:13 实测 hermes 的深思考轮超过 3 分钟（reasoning 上万字），
// 180s 到点照样被跳过，表现为「hermes 还在思考，openclaw 已经开跑」。保底值
// 与 CLI 的 agent 单轮上限（run.ts turnTimeoutMs）取同一量级，避免「服务端
// 比设备端先放弃」——那种情况下设备还在产出，服务端却已经派人接棒。
const relayTurnTimeoutFloor = 300 * time.Second

// NotifyStreamActivity 心跳入口：按前缀还原真实轮次 request_id 并续期。
// 返回 true 表示这条消息被当作心跳消化（调用方不要再当回包处理）。
func (h *chatroomRelayHub) NotifyStreamActivity(requestID string) bool {
	if !strings.HasPrefix(requestID, relayPingPrefix) {
		return false
	}
	turnID := strings.TrimPrefix(requestID, relayPingPrefix)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if s.requestID == turnID && s.turnExt != nil {
			window := h.rt.interactiveTurnTimeout()
			if window < relayTurnTimeoutFloor {
				window = relayTurnTimeoutFloor
			}
			s.turnExt.deadline = time.Now().Add(window)
			// 心跳是排查「轮次越序」的关键证据，按 Info 留痕（每 15s 一条，
			// 量可控）；连续出现即证明心跳链路通。
			h.rt.logger.Info("relay: turn extended by heartbeat",
				"chatroom", s.chatroomRCID, "turn", turnID)
			return true
		}
	}
	h.rt.logger.Warn("relay: heartbeat for unknown/stale turn",
		"turn", turnID)
	// 心跳晚到（轮已推进/超时作废）：同样按心跳消化，别往下漏。
	return true
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
		// 失败同样要留下一条属于该成员的消息：设备端报错时往往已经流过若干
		// 帧，前端的气泡正等收尾。有正文就照发，没有就发占位。
		note := content
		if note == "" {
			note = relayTurnFailedNote(member.roleName)
		}
		h.broadcastMemberSpeech(ctx, session, &member, note)
		h.mu.Lock()
		if s := session; s.requestID == requestID {
			s.replies = append(s.replies, relayReply{roleName: member.roleName, content: note, silent: true})
		}
		h.mu.Unlock()
	} else if content != "" {
		h.broadcastMemberSpeech(ctx, session, &member, content)
		// 积累进会话上下文：后续成员的提示词会带上这条发言。
		h.mu.Lock()
		if s := session; s.requestID == requestID {
			s.replies = append(s.replies, relayReply{roleName: member.roleName, content: content})
		}
		h.mu.Unlock()
	} else {
		// 回包成功但正文为空。设备端只把 agent 的 stdout 当正文，而「生成个
		// HTML」这类请求多数是把文件写进工作目录、stdout 近乎为空，于是空回包
		// 成了常态而非异常。
		// 补一条占位气泡，并且**记进本轮发言**：接力链条上后面的人必须知道
		// 上一位没交出正文。以前只广播不记录，下一位从发言列表里看不到任何
		// 前序，只能凭空表态 —— 正是「各说各的」的来源之一。
		note := relayEmptyOutputNote(member.roleName)
		h.rt.logger.Warn("relay: member returned empty content",
			"chatroom", session.chatroomRCID, "target", senderID,
			"round", session.roundIndex+1)
		h.broadcastMemberSpeech(ctx, session, &member, note)
		h.mu.Lock()
		if s := session; s.requestID == requestID {
			s.replies = append(s.replies, relayReply{roleName: member.roleName, content: note, silent: true})
		}
		h.mu.Unlock()
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
