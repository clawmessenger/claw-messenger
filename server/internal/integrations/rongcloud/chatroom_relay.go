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
	// 每个进行中的成员一轮一个 done channel（pump 在派发前换新），避免
	// 「超时后迟到的回包 close 同一个 chan」把下一轮误推进。
	currentDone chan struct{}
	// requestID 对应 currentDone 所代表的成员派发；仅 AcceptResponse 读，
	// pump 写。写发生在派发前且 pump 单线程，读侧加锁即可。
	requestID string
	// senderRCID 是 currentDone 轮的预期回包者。
	senderRCID string
}

type chatroomRelayHub struct {
	mu       sync.Mutex
	sessions map[string]*relaySession // key: chatroom rc id
	rt       *discussionRuntime
}

func newChatroomRelayHub(rt *discussionRuntime) *chatroomRelayHub {
	return &chatroomRelayHub{sessions: map[string]*relaySession{}, rt: rt}
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
	// HTTP 请求返回后中继要继续跑：换独立 context。
	bgCtx := context.WithoutCancel(ctx)
	if !h.Start(bgCtx, chatroomRCID, members, text) {
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
// 可以提示「上一条消息的回应还在进行中」）。
func (h *chatroomRelayHub) Start(ctx context.Context, chatroomRCID string, members []relayMember, userText string) bool {
	if len(members) == 0 {
		return false
	}
	h.mu.Lock()
	if _, busy := h.sessions[chatroomRCID]; busy {
		h.mu.Unlock()
		return false
	}
	session := &relaySession{
		chatroomRCID: chatroomRCID,
		members:      members,
		userText:     userText,
	}
	h.sessions[chatroomRCID] = session
	h.mu.Unlock()

	go h.pump(ctx, session)
	return true
}

// pump 依次派发成员。ctx 是请求生命周期之外的独立 context（WithoutCancel），
// 保证 HTTP 200 之后中继仍继续跑。
func (h *chatroomRelayHub) pump(ctx context.Context, s *relaySession) {
	timeout := h.rt.interactiveTurnTimeout()
	defer func() {
		h.mu.Lock()
		if cur, ok := h.sessions[s.chatroomRCID]; ok && cur == s {
			delete(h.sessions, s.chatroomRCID)
		}
		h.mu.Unlock()
	}()
	for s.index < len(s.members) {
		member := s.members[s.index]
		requestID := fmt.Sprintf("relay_%d_%d", time.Now().UnixNano(), member.order)
		done := make(chan struct{})
		h.mu.Lock()
		s.requestID = requestID
		s.senderRCID = member.rcID
		s.currentDone = done
		h.mu.Unlock()
		if err := h.dispatchTurn(ctx, s, member, requestID); err != nil {
			h.rt.logger.Warn("relay: dispatch failed",
				"chatroom", s.chatroomRCID, "target", member.rcID, "error", err)
			// 派发失败视同该成员本轮跳过，继续下一位。
			s.index++
			continue
		}
		select {
		case <-done:
			// 回包已处理（broadcast 已在 AcceptResponse 里完成），前进。
		case <-time.After(timeout):
			h.rt.logger.Warn("relay: turn timed out",
				"chatroom", s.chatroomRCID, "target", member.rcID)
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
}

// dispatchTurn 私发 your_turn 给成员（CLI 零改动的既有协议）。
func (h *chatroomRelayHub) dispatchTurn(ctx context.Context, s *relaySession, member relayMember, requestID string) error {
	prompt := h.buildPrompt(s, member)
	params := map[string]interface{}{
		"round":          1,
		"speaking_order": member.order,
		"role_name":      member.roleName,
		"chatroom_id":    s.chatroomRCID,
		"role_prompt":    prompt,
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

// buildPrompt 组装闲聊提示词：群聊上下文 + 用户消息 + 角色人设。
func (h *chatroomRelayHub) buildPrompt(s *relaySession, member relayMember) string {
	instr := ""
	if member.roleInstr != "" {
		instr = "\n" + member.roleInstr + "\nStay in character."
	}
	return fmt.Sprintf(
		"[group chat] 这是讨论组的一次自由对话。用户刚在聊天室发言：\n\n%s\n\n"+
			"请以「%s」的身份回应用户。回应会被广播回聊天室，请只输出回应正文，不要输出多余格式。%s",
		s.userText, member.roleName, instr,
	)
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
	if _, err := h.rt.client.sendChatroomMessage(
		ctx, member.rcID, s.chatroomRCID, objectNameText, string(textPayload),
	); err != nil {
		h.rt.logger.Warn("relay: chatroom broadcast failed",
			"chatroom", s.chatroomRCID, "from", member.rcID, "error", err)
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
