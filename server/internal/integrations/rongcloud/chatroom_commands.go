package rongcloud

// chatroom_commands.go implements the service=chatroom command channel.
//
// The web speaks to chatrooms over the IM command channel (service=chatroom,
// action=create|getInfo|update|destroy|setHost|setMemberRole|setDiscussionTeam|
// join|leave|invite|removeMember|getMembers|getList), not over REST. The legacy
// Python backend's contract_handler.py routed these to a ChatroomRepository
// keyed on the RongCloud chatroom id (TEXT). P1 made the Go side reply in the
// web's envelope and degrade unknown actions to a 404 the web matches on; P3
// fills in the real behavior so the C-end's room lifecycle works end to end.
//
// Key shape: every action keys on rongcloud_chatroom_id (TEXT), not the
// internal UUID. The owner/host/member identities returned to the web are the
// RongCloud-side ids (TEXT) the web actually compares against, not Go UUIDs.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/quota"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Capabilities the roundtable host / members must declare. Mirrors the legacy
// backend's ROUNDTABLE_HOST_CAPABILITIES / ROUNDTABLE_MEMBER_CAPABILITIES.
var (
	roundtableHostCaps = []string{"discussion_host", "discussion_roundtable", "discussion_model_routing"}
	roundtableMembCaps = []string{"discussion_participant", "discussion_roundtable", "discussion_model_routing"}
)

const (
	roundtableHostRole        = "中立主持人"
	roundtableHostInstruction = "保持中立，控制讨论节奏；每轮汇总共识、分歧与开放问题，不替普通成员表达立场。"
)

// commandResult is what each action returns: the data payload on success, or
// (nil, code, message) on failure. handleChatroomCommand translates this into
// replyOK / replyErr.
type commandResult struct {
	data    interface{}
	code    int
	message string
}

func okResult(data interface{}) commandResult      { return commandResult{data: data, code: 200} }
func errResult(code int, msg string) commandResult { return commandResult{code: code, message: msg} }

// resolveActor maps the IM sender (a RongCloud user id) to the rongcloud_user
// row. Every ownership / membership check goes through this.
//
// 双域解析，与讨论域的 resolveActor 保持同一语义：先查 rongcloud_user
// （节点 / 安装账号）；miss 后查 claw_im_users —— C 端真人注册时融云 id 就是
// user_id 本身，但注册链路（clawRong）直接调融云 API、不写 rongcloud_user
// 镜像，所以必须兜底，命中后幂等补镜像行再返回。
//
// 缺了这一兜底，真人发来的 chatroom 命令会一律 401 sender not recognized：
// 前端表现为聊天室列表/元数据加载失败（chatroom/getList 拿不到房间），
// 且房间生命周期写动作（create/join/setHost…）全部不可用。
func (h *systemHandler) resolveActor(ctx context.Context, fromUserID string) (db.RongcloudUser, error) {
	if h.queries == nil {
		return db.RongcloudUser{}, errors.New("database not available")
	}
	if fromUserID == "" {
		return db.RongcloudUser{}, errors.New("missing sender")
	}
	if u, err := h.queries.GetRongCloudUserByRongCloudID(ctx, fromUserID); err == nil {
		return u, nil
	}
	row, err := h.queries.GetClawIMUserByUserID(ctx, fromUserID)
	if err != nil {
		return db.RongcloudUser{}, fmt.Errorf("rongcloud: resolve actor %s: %w", fromUserID, err)
	}
	name := stringOrNull(row.Nickname)
	if name == "" {
		name = row.Username
	}
	return mirrorClawUserToRongCloudUser(ctx, h.queries, row.UserID, name, stringOrNull(row.PortraitUri))
}

// mirrorClawUserToRongCloudUser 幂等补建 claw 真人的 rongcloud_user 镜像行并
// 返回该行。claw 注册链路不写镜像，但 chatroom 的写动作（create/join 等）拿
// actor 的 UUID 做 owner_user_id 外键，所以第一次进命令通道时补上。
// workspace 走单安装归属（已有镜像行则直接返回，不重复插入）。
func mirrorClawUserToRongCloudUser(ctx context.Context, queries *db.Queries, rcID, name, portrait string) (db.RongcloudUser, error) {
	if queries == nil {
		return db.RongcloudUser{}, errors.New("database not available")
	}
	if u, err := queries.GetRongCloudUserByRongCloudID(ctx, rcID); err == nil {
		return u, nil
	}
	wsID, err := queries.GetFirstActiveRongCloudInstallationWorkspace(ctx)
	if err != nil {
		return db.RongcloudUser{}, fmt.Errorf("rongcloud: resolve install workspace: %w", err)
	}
	mirror, err := queries.CreateRongCloudUser(ctx, db.CreateRongCloudUserParams{
		WorkspaceID:     wsID,
		RongcloudUserID: rcID,
		Name:            textOrNull(name),
		// claw_im_users.portrait_uri 可空：镜像行也绝不能空，否则聊天室成员列表
		// 头像恒空白（客户端只能各显各的兜底图）。
		PortraitUri: pgText(DefaultPortraitIfEmpty(portrait)),
		NodeType:    "human",
	})
	if err == nil {
		return mirror, nil
	}
	// 并发下另一路已插入：查回即可；仍失败才把原始错误抛出去。
	if u, gErr := queries.GetRongCloudUserByRongCloudID(ctx, rcID); gErr == nil {
		return u, nil
	}
	return db.RongcloudUser{}, err
}

// getStringList extracts a string slice from a params value, trimming and
// dropping empties.
func getStringList(v interface{}) []string {
	if arr, ok := v.([]interface{}); ok {
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			if s, ok := item.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					out = append(out, t)
				}
			}
		}
		return out
	}
	return nil
}

// hasCapability reports whether a JSONB capabilities blob declares the given
// capability. Stored shape is a JSON array of strings ("discussion_host" …).
func hasCapability(caps []byte, want string) bool {
	if len(caps) == 0 {
		return false
	}
	var arr []string
	if err := json.Unmarshal(caps, &arr); err != nil {
		return false
	}
	for _, c := range arr {
		if c == want {
			return true
		}
	}
	return false
}

// missingCapabilities returns the capabilities in `required` that the node's
// JSONB capabilities blob does not declare. Empty = all present.
func missingCapabilities(caps []byte, required []string) []string {
	var missing []string
	for _, c := range required {
		if !hasCapability(caps, c) {
			missing = append(missing, c)
		}
	}
	return missing
}

// validateDiscussionModel mirrors the legacy backend's provider/model check.
func validateDiscussionModel(value interface{}) (string, string) {
	s := strings.TrimSpace(fmt.Sprintf("%v", value))
	if s == "" {
		return "", ""
	}
	parts := strings.Split(s, "/")
	if len(s) > 256 || len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
		parts[0] != strings.TrimSpace(parts[0]) || parts[1] != strings.TrimSpace(parts[1]) ||
		strings.ContainsAny(parts[0], " \t\n") || strings.ContainsAny(parts[1], " \t\n") {
		return "", "模型必须使用 provider/model 格式"
	}
	return s, ""
}

// chatroomMemberView is the shape the web expects for a member: userId /
// memberType / nodeId (TEXT business id, not UUID) / nickname / portraitUri /
// roleName / roleInstructions / model / speakingOrder / capabilities / enabled.
func chatroomMemberView(m db.ListRongCloudChatroomMembersWithNodeRow) map[string]interface{} {
	memberID := ""
	if m.MemberID.Valid {
		memberID = m.MemberID.String
	}
	var nodeID interface{}
	if m.BusinessNodeID.Valid && m.BusinessNodeID.String != "" {
		nodeID = m.BusinessNodeID.String
	}
	rec := map[string]interface{}{
		"userId":           memberID,
		"memberType":       m.MemberType,
		"nodeId":           nodeID,
		"nickname":         m.Nickname,
		"portraitUri":      m.PortraitUri,
		"roleName":         textStr(m.RoleName),
		"roleInstructions": textStr(m.RoleInstructions),
		"capabilities":     capsToInterface(m.Capabilities),
		"enabled":          m.Enabled,
	}
	if m.DiscussionModel.Valid {
		rec["model"] = m.DiscussionModel.String
	} else if m.Model.Valid {
		rec["model"] = m.Model.String
	}
	if m.SpeakingOrder.Valid {
		rec["speakingOrder"] = m.SpeakingOrder.Int32
	}
	return rec
}

// textStr returns the string of a pgtype.Text, "" when invalid.
func textStr(t pgtype.Text) string {
	if t.Valid {
		return t.String
	}
	return ""
}

// capsToInterface turns a JSONB capabilities blob into a value the JSON
// encoder can emit directly. An empty/invalid blob becomes an empty array so
// the web's normalizeCapabilities gets [] not {}.
func capsToInterface(caps []byte) interface{} {
	if len(caps) == 0 {
		return []string{}
	}
	var arr []interface{}
	if err := json.Unmarshal(caps, &arr); err == nil {
		return arr
	}
	// Not an array; let the web's normalizeCapabilities parse the string.
	return json.RawMessage(caps)
}

// chatroomViewData builds the response record from a chatroom "view" row (one
// that already carries owner_rc_user_id / host_business_node_id via JOIN).
//
// viewerRCID 是**观察者**（本条命令的发送者）的融云 id；isOwner 是相对视角的
// 字段，同一间房对不同调用者答案不同，所以必须逐次按 viewer 判定。
//
// 为什么要由服务端下发（与群聊 groupProfileView 同一取舍）：客户端原先是拿
// `ownerId` 与本地两套 id（平台 userId / 融云 imUserId）做字符串比对来猜群主。
// `owner_rc_user_id` 在这一列被 JOIN 出来之前是 NULL，`ownerRCID()` 返回空串 ——
// 这些老聊天室/讨论组上，真正的房主会被判成非房主，「移除成员」入口整块消失，
// 而「添加成员」不判权限所以还在，用户看到的就是「群主没有移除成员的功能」。
//
// 重要边界：isOwner 只用于**显示侧**开关（决定要不要把入口画出来）。真正的写
// 操作仍由服务端校验 —— `actionChatroomRemoveMember` 用 `room.OwnerUserID !=
// actor.ID` 挡 403，`actionChatroomSetHost` 同理。前端不得把 isOwner 当授权凭据。
//
// viewerRCID 为空（内部调用、取不到发送者）时一律 false：没有观察者就无从判定，
// 绝不能因为 owner 也空就把 `"" == ""` 当成「观察者就是房主」。
func chatroomViewData(room chatroomView, members []map[string]interface{}, includeMembers bool, viewerRCID string) map[string]interface{} {
	// ownerId 保留原值（可能是空串）：别的端/老客户端还靠它显示「房主：xxx」，
	// 抹成空或改写成 viewer 只会让它们更困惑。判定只走 ownerViewpoint，
	// 与群聊共用一个实现（理由见 ownerViewpoint 的注释）。
	rec := map[string]interface{}{
		"chatroomId":        room.rcChatroomID(),
		"chatroomName":      room.name(),
		"portraitUri":       room.portraitUri(),
		"ownerId":           room.ownerRCID(),
		"isOwner":           ownerViewpoint(room.ownerRCID(), viewerRCID),
		"hostNodeId":        room.hostBusinessNodeID(),
		"openclawMaxRounds": room.maxRounds(),
		"createTime":        room.createdAt(),
		"updateTime":        room.updatedAt(),
	}
	if k := room.conversationKind(); k != "" {
		rec["conversationKind"] = k
	}
	if includeMembers {
		aiMembers := make([]map[string]interface{}, 0)
		for _, m := range members {
			if m["memberType"] == "ai" {
				aiMembers = append(aiMembers, m)
			}
		}
		rec["memberCount"] = len(members)
		rec["members"] = members
		rec["aiMembers"] = aiMembers
	}
	return rec
}

// chatroomView is the narrow interface chatroomViewData needs. Both view-row
// types satisfy it.
type chatroomView interface {
	rcChatroomID() string
	name() string
	portraitUri() string
	ownerRCID() string
	hostBusinessNodeID() interface{}
	maxRounds() int32
	createdAt() interface{}
	updatedAt() interface{}
	conversationKind() string
}

type viewRow struct {
	db.GetActiveRongCloudChatroomViewByRongCloudIDRow
}

func (v viewRow) rcChatroomID() string { return v.RongcloudChatroomID }
func (v viewRow) name() string         { return v.Name }
func (v viewRow) portraitUri() string  { return v.PortraitUri }
func (v viewRow) ownerRCID() string {
	if v.OwnerRcUserID.Valid {
		return v.OwnerRcUserID.String
	}
	return ""
}
func (v viewRow) hostBusinessNodeID() interface{} {
	if v.HostBusinessNodeID.Valid && v.HostBusinessNodeID.String != "" {
		return v.HostBusinessNodeID.String
	}
	return nil
}
func (v viewRow) maxRounds() int32       { return v.MaxRounds }
func (v viewRow) createdAt() interface{} { return v.CreatedAt }
func (v viewRow) updatedAt() interface{} { return v.UpdatedAt }
func (v viewRow) conversationKind() string {
	if v.ConversationKind.Valid {
		return v.ConversationKind.String
	}
	return ""
}

type listViewRow struct {
	db.ListRongCloudChatroomViewsForUserRow
}

func (v listViewRow) rcChatroomID() string { return v.RongcloudChatroomID }
func (v listViewRow) name() string         { return v.Name }
func (v listViewRow) portraitUri() string  { return v.PortraitUri }
func (v listViewRow) ownerRCID() string {
	if v.OwnerRcUserID.Valid {
		return v.OwnerRcUserID.String
	}
	return ""
}
func (v listViewRow) hostBusinessNodeID() interface{} {
	if v.HostBusinessNodeID.Valid && v.HostBusinessNodeID.String != "" {
		return v.HostBusinessNodeID.String
	}
	return nil
}
func (v listViewRow) maxRounds() int32       { return v.MaxRounds }
func (v listViewRow) createdAt() interface{} { return v.CreatedAt }
func (v listViewRow) updatedAt() interface{} { return v.UpdatedAt }
func (v listViewRow) conversationKind() string {
	if v.ConversationKind.Valid {
		return v.ConversationKind.String
	}
	return ""
}

// loadMembers fetches enabled members with their node business ids and builds
// the response records. Returns nil for an empty room (callers that pass
// includeMembers=false should skip this).
func (h *systemHandler) loadMembers(ctx context.Context, chatroomID pgtype.UUID) ([]map[string]interface{}, error) {
	rows, err := h.queries.ListRongCloudChatroomMembersWithNode(ctx, chatroomID)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, len(rows))
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		out = append(out, chatroomMemberView(r))
	}
	return out, nil
}

// ---- actions ----

func (h *systemHandler) actionChatroomGetList(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	page := 1
	if v, ok := cmd.Params["page"].(float64); ok && v >= 1 {
		page = int(v)
	}
	pageSize := 20
	if v, ok := cmd.Params["pageSize"].(float64); ok && v >= 1 {
		pageSize = int(v)
		if pageSize > 100 {
			pageSize = 100
		}
	}
	memberID := actor.RongcloudUserID
	rooms, err := h.queries.ListRongCloudChatroomViewsForUser(ctx, db.ListRongCloudChatroomViewsForUserParams{
		OwnerUserID: actor.ID,
		MemberID:    pgtype.Text{String: memberID, Valid: memberID != ""},
	})
	if err != nil {
		h.logger.Error("rongcloud: chatroom getList", "error", err)
		return errResult(500, "获取聊天室列表失败")
	}
	total := len(rooms)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	pageRooms := rooms[start:end]
	list := make([]map[string]interface{}, 0, len(pageRooms))
	for _, r := range pageRooms {
		list = append(list, chatroomViewData(listViewRow{r}, nil, false, actor.RongcloudUserID))
	}
	// 附上每间房「最后一条消息」（见 chatroom_last_message.go）：聊天室不存历史，
	// 会话列表没有它就只剩占位文案。批量查一页，失败不影响列表本身。
	h.applyChatroomLastEvents(ctx, pageRooms, list)
	return okResult(map[string]interface{}{
		"list":     list,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
	})
}

// actionChatroomGetInfo 是只读接口：不校验身份、不解析 actor（getInfo 对谁都能读），
// 但**仍然需要知道观察者是谁**才能下发 isOwner —— 所以这里收的是发送者的融云 id，
// 而不是 db.RongcloudUser（不需要查库，也不需要它一定是个已注册用户）。
func (h *systemHandler) actionChatroomGetInfo(ctx context.Context, cmd CommandContent, viewerRCID string) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	if rcID == "" {
		return errResult(400, "聊天室 ID 不能为空")
	}
	room, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return errResult(404, "聊天室不存在")
	}
	// Ensure the RongCloud-side room still exists (it auto-destroys after the
	// inactivity window); rebuild it if not. Best-effort: a failure here does
	// not block the business record.
	if h.client != nil {
		_ = h.client.ensureChatroom(ctx, rcID, room.Name)
	}
	members, err := h.loadMembers(ctx, room.ID)
	if err != nil {
		h.logger.Error("rongcloud: chatroom getInfo load members", "error", err)
		return errResult(500, "获取聊天室信息失败")
	}
	return okResult(chatroomViewData(viewRow{room}, members, true, viewerRCID))
}

func (h *systemHandler) actionChatroomUpdate(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	if rcID == "" {
		return errResult(400, "聊天室 ID 不能为空")
	}
	room, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return errResult(404, "聊天室不存在")
	}
	if room.OwnerUserID != actor.ID {
		return errResult(403, "只有聊天室创建者可以修改聊天室设置")
	}
	name := room.Name
	if v, ok := cmd.Params["chatroomName"].(string); ok {
		if t := strings.TrimSpace(v); t != "" {
			name = t
		}
	}
	// 只有非空 patch 才覆盖，与群设置（COALESCE(NULLIF(...))）语义一致：客户端
	// 显式传 portraitUri:"" 不能把已有头像清空。两边都为空时按房间类型落默认图
	// （讨论组=圆桌三席 / 聊天室=两个气泡），不再落真人那张灰人形。
	// 注意：聊天室头像**只写本地库** —— 融云聊天室 API 没有头像字段，这里不推融云，
	// 也不存在「已同步到融云」这回事。
	portrait := room.PortraitUri
	if v, ok := cmd.Params["portraitUri"].(string); ok {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			portrait = trimmed
		}
	}
	portrait = GroupPortraitIfEmpty(ChatroomPortraitKind(room.ConversationKind.String), portrait)
	maxRounds := room.MaxRounds
	if v, ok := cmd.Params["openclawMaxRounds"]; ok {
		switch t := v.(type) {
		case float64:
			maxRounds = int32(t)
		case int:
			maxRounds = int32(t)
		default:
			return errResult(400, "对话轮数必须是 1-100 的整数")
		}
		if maxRounds < 1 || maxRounds > 100 {
			return errResult(400, "对话轮数必须是 1-100 的整数")
		}
	}
	updated, err := h.queries.UpdateRongCloudChatroomProfileByRongCloudID(ctx, db.UpdateRongCloudChatroomProfileByRongCloudIDParams{
		RongcloudChatroomID: rcID,
		Name:                name,
		PortraitUri:         portrait,
		MaxRounds:           maxRounds,
	})
	if err != nil {
		return errResult(500, "更新聊天室失败")
	}
	// Re-fetch the view to get owner/host business ids for the response.
	view, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		view = db.GetActiveRongCloudChatroomViewByRongCloudIDRow{
			ID: updated.ID, RongcloudChatroomID: updated.RongcloudChatroomID,
			Name: updated.Name, PortraitUri: updated.PortraitUri,
			OwnerUserID: updated.OwnerUserID, HostNodeID: updated.HostNodeID,
			MaxRounds: updated.MaxRounds, ConversationKind: updated.ConversationKind,
			Status: updated.Status, CreatedAt: updated.CreatedAt, UpdatedAt: updated.UpdatedAt,
		}
	}
	members, _ := h.loadMembers(ctx, view.ID)
	return okResult(chatroomViewData(viewRow{view}, members, true, actor.RongcloudUserID))
}

func (h *systemHandler) actionChatroomDestroy(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	if rcID == "" {
		return errResult(400, "聊天室 ID 不能为空")
	}
	room, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return errResult(404, "聊天室不存在")
	}
	if room.OwnerUserID != actor.ID {
		return errResult(403, "只有聊天室创建者可以销毁聊天室")
	}
	if h.client != nil {
		if err := h.client.destroyChatroom(ctx, rcID); err != nil {
			h.logger.Warn("rongcloud: destroyChatroom failed, continuing with DB delete",
				"chatroom_id", rcID, "error", err)
		}
	}
	_ = h.queries.SoftDeleteRongCloudChatroomByRongCloudID(ctx, rcID)
	return okResult(map[string]interface{}{"ok": true})
}

func (h *systemHandler) actionChatroomJoin(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	if rcID == "" {
		return errResult(400, "聊天室 ID 不能为空")
	}
	room, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return errResult(404, "聊天室不存在")
	}
	if h.client != nil {
		_ = h.client.ensureChatroom(ctx, rcID, room.Name)
	}
	nickname := actor.Name.String
	if !actor.Name.Valid || nickname == "" {
		nickname = actor.RongcloudUserID
	}
	portrait := DefaultPortraitIfEmpty(actor.PortraitUri.String)
	_, _ = h.queries.UpsertRongCloudChatroomMemberByMemberID(ctx, db.UpsertRongCloudChatroomMemberByMemberIDParams{
		ChatroomID:   room.ID,
		MemberType:   "user",
		Capabilities: []byte("[]"),
		Enabled:      true,
		MemberID:     pgtype.Text{String: actor.RongcloudUserID, Valid: true},
		Nickname:     nickname,
		PortraitUri:  portrait,
	})
	return okResult(map[string]interface{}{"ok": true})
}

func (h *systemHandler) actionChatroomLeave(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	if rcID == "" {
		return errResult(400, "聊天室 ID 不能为空")
	}
	room, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return errResult(404, "聊天室不存在")
	}
	_ = h.queries.DeleteRongCloudChatroomMemberByMemberID(ctx, db.DeleteRongCloudChatroomMemberByMemberIDParams{
		ChatroomID: room.ID,
		MemberID:   pgtype.Text{String: actor.RongcloudUserID, Valid: true},
	})
	return okResult(map[string]interface{}{"ok": true})
}

func (h *systemHandler) actionChatroomInvite(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	if rcID == "" {
		return errResult(400, "聊天室 ID 不能为空")
	}
	userIDs := getStringList(cmd.Params["userIds"])
	if len(userIDs) == 0 {
		return errResult(400, "聊天室 ID 和邀请用户不能为空")
	}
	room, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return errResult(404, "聊天室不存在")
	}
	// The inviter must already be a member.
	members, err := h.loadMembers(ctx, room.ID)
	if err != nil {
		return errResult(500, "邀请失败")
	}
	isMember := false
	for _, m := range members {
		if s, ok := m["userId"].(string); ok && s == actor.RongcloudUserID {
			isMember = true
			break
		}
	}
	if !isMember {
		return errResult(403, "请先加入聊天室再邀请成员")
	}
	for _, uid := range userIDs {
		// Look up the invitee's profile for nickname/portrait. 真人在首次发聊天室
		// 命令前没有 rongcloud_user 镜像行，AI 节点也可能还没写过资料 —— 所以
		// 镜像行 miss 时回退 claw_im_users，仍为空才落统一默认图。
		nickname, portrait := uid, ""
		if prof, err := h.queries.GetRongCloudUserByRongCloudID(ctx, uid); err == nil {
			if prof.Name.Valid && prof.Name.String != "" {
				nickname = prof.Name.String
			}
			portrait = strings.TrimSpace(prof.PortraitUri.String)
		}
		if portrait == "" {
			if claw, err := h.queries.GetClawIMUserByUserID(ctx, uid); err == nil {
				portrait = strings.TrimSpace(stringOrNull(claw.PortraitUri))
			}
		}
		// Upsert 的 ON CONFLICT 无条件写 EXCLUDED.portrait_uri，没有新值时必须沿用
		// 成员行已有的头像，否则一次「邀请」就会把已设好的头像清空。
		if portrait == "" {
			if existing, err := h.queries.GetRongCloudChatroomMemberByChatroomAndMemberID(ctx, db.GetRongCloudChatroomMemberByChatroomAndMemberIDParams{
				ChatroomID: room.ID,
				MemberID:   pgtype.Text{String: uid, Valid: true},
			}); err == nil {
				portrait = strings.TrimSpace(existing.PortraitUri)
			}
		}
		_, _ = h.queries.UpsertRongCloudChatroomMemberByMemberID(ctx, db.UpsertRongCloudChatroomMemberByMemberIDParams{
			ChatroomID:   room.ID,
			MemberType:   "user",
			Capabilities: []byte("[]"),
			Enabled:      true,
			MemberID:     pgtype.Text{String: uid, Valid: true},
			Nickname:     nickname,
			PortraitUri:  DefaultPortraitIfEmpty(portrait),
		})
	}
	members, _ = h.loadMembers(ctx, room.ID)
	return okResult(map[string]interface{}{"members": members})
}

func (h *systemHandler) actionChatroomRemoveMember(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	memberID := getStringAny(cmd.Params, "member_id", "memberId")
	if rcID == "" || memberID == "" {
		return errResult(400, "聊天室 ID 和成员 ID 不能为空")
	}
	room, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return errResult(404, "聊天室不存在")
	}
	if room.OwnerUserID != actor.ID {
		return errResult(403, "只有聊天室创建者可以移除成员")
	}
	if memberID == actor.RongcloudUserID {
		return errResult(400, "不能移除自己，请使用退出聊天室")
	}
	target, err := h.queries.GetRongCloudChatroomMemberByChatroomAndMemberID(ctx, db.GetRongCloudChatroomMemberByChatroomAndMemberIDParams{
		ChatroomID: room.ID,
		MemberID:   pgtype.Text{String: memberID, Valid: true},
	})
	if err != nil {
		return errResult(404, "成员不存在")
	}
	// Discussion-host removal guard: kept as a no-op until P6 wires the runtime.
	_ = target
	_ = h.queries.DeleteRongCloudChatroomMemberByMemberID(ctx, db.DeleteRongCloudChatroomMemberByMemberIDParams{
		ChatroomID: room.ID,
		MemberID:   pgtype.Text{String: memberID, Valid: true},
	})
	members, _ := h.loadMembers(ctx, room.ID)
	return okResult(map[string]interface{}{"members": members})
}

func (h *systemHandler) actionChatroomGetMembers(ctx context.Context, cmd CommandContent) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	if rcID == "" {
		return errResult(400, "聊天室 ID 不能为空")
	}
	room, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return errResult(404, "聊天室不存在")
	}
	if h.client != nil {
		_ = h.client.ensureChatroom(ctx, rcID, room.Name)
	}
	members, err := h.loadMembers(ctx, room.ID)
	if err != nil {
		return errResult(500, "获取成员列表失败")
	}
	return okResult(map[string]interface{}{
		"users": members,
		"total": len(members),
	})
}

// aiMemberInput is the web's aiMembers[] item shape.
type aiMemberInput struct {
	NodeID           string `json:"nodeId"`
	RoleName         string `json:"roleName"`
	RoleInstructions string `json:"roleInstructions"`
	Model            string `json:"model"`
}

func parseAIMembers(v interface{}) []aiMemberInput {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]aiMemberInput, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil
		}
		out = append(out, aiMemberInput{
			NodeID:           strings.TrimSpace(getString(m, "nodeId")),
			RoleName:         strings.TrimSpace(getString(m, "roleName")),
			RoleInstructions: getString(m, "roleInstructions"),
			Model:            getString(m, "model"),
		})
	}
	return out
}

func (h *systemHandler) actionChatroomCreate(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID := strings.TrimSpace(getStringAny(cmd.Params, "chatroom_id", "chatroomId"))
	name := strings.TrimSpace(getStringAny(cmd.Params, "chatroom_name", "chatroomName"))
	portrait := GroupPortraitIfEmpty(GroupPortraitKindChatroom, getStringAny(cmd.Params, "portrait_uri", "portraitUri"))
	hostNodeID := strings.TrimSpace(getStringAny(cmd.Params, "host_node_id", "hostNodeId"))
	aiMembers := parseAIMembers(cmd.Params["aiMembers"])
	maxRounds := int32(20)
	if v, ok := cmd.Params["openclawMaxRounds"]; ok {
		switch t := v.(type) {
		case float64:
			maxRounds = int32(t)
		case int:
			maxRounds = int32(t)
		}
	}
	if maxRounds < 1 || maxRounds > 100 {
		maxRounds = 20
	}
	if rcID == "" || name == "" {
		return errResult(400, "聊天室 ID 和名称不能为空")
	}
	if hostNodeID == "" || len(aiMembers) == 0 {
		return errResult(422, "新聊天室必须配置 AI 主持人")
	}
	// Exactly one member must carry the host node.
	hostEntries := 0
	for _, m := range aiMembers {
		if m.NodeID == hostNodeID {
			hostEntries++
		}
	}
	if hostEntries != 1 {
		return errResult(422, "AI 成员节点不能重复")
	}
	ordinary := make([]aiMemberInput, 0, len(aiMembers))
	for _, m := range aiMembers {
		if m.NodeID != hostNodeID {
			ordinary = append(ordinary, m)
		}
	}
	if len(ordinary) < 1 || len(ordinary) > 8 {
		return errResult(422, "普通讨论成员数量必须为 1 到 8 个")
	}
	// Room must not already exist.
	if existing, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID); err == nil && existing.ID.Valid {
		return errResult(409, "聊天室 ID 已存在")
	}
	// 创建配额硬校验（普通聊天室口径；超限：429 QUOTA_EXCEEDED{kind,limit,used}）。
	if qErr := h.quotaCheck(ctx, actor.RongcloudUserID, quota.KindChatroom); qErr != nil {
		return commandResult{code: 429, message: quota.CodeQuotaExceeded, data: qErr.Data()}
	}
	// Resolve host node.
	hostNode, err := h.resolveNode(ctx, hostNodeID)
	if err != nil {
		return errResult(422, fmt.Sprintf("AI 节点未注册: %s", hostNodeID))
	}
	// 在线判定走融云 presence（见 node_presence.go）：deploy_status 列在 Go 侧
	// 没有任何写入者，读它会把所有节点判成离线。
	if !nodeIsOnline(ctx, h.client, hostNode) {
		return errResult(422, "主持人节点必须在线")
	}
	if missing := missingCapabilities(hostNode.Capabilities, roundtableHostCaps); len(missing) > 0 {
		return errResult(422, "主持人节点缺少能力: "+strings.Join(missing, ", "))
	}
	// Resolve ordinary members.
	seen := map[string]bool{hostNodeID: true}
	type resolvedMember struct {
		node  db.RongcloudNode
		role  string
		instr string
		model string
		order int32
	}
	resolved := make([]resolvedMember, 0, len(ordinary))
	seenMemberIDs := map[string]bool{hostNode.RongcloudUserID: true}
	for i, m := range ordinary {
		if m.NodeID == "" || seen[m.NodeID] {
			return errResult(422, "AI 成员节点不能为空或重复")
		}
		seen[m.NodeID] = true
		node, err := h.resolveNode(ctx, m.NodeID)
		if err != nil {
			return errResult(422, fmt.Sprintf("AI 节点未注册: %s", m.NodeID))
		}
		if !nodeIsOnline(ctx, h.client, node) {
			return errResult(422, fmt.Sprintf("AI 节点必须在线: %s", m.NodeID))
		}
		if missing := missingCapabilities(node.Capabilities, roundtableMembCaps); len(missing) > 0 {
			return errResult(422, fmt.Sprintf("AI 节点缺少能力: %s", strings.Join(missing, ", ")))
		}
		// 配置期权限校验（迁移 607）：拉人进讨论组时就挡住，别等派发时才静默跳过。
		//
		// 为什么非要在这一步：派发侧的 fail-closed 只保证「不说话」，而用户的观感
		// 是「人拉进来了、图标也在，就是不吭声」—— 无从判断是权限还是故障。在
		// 配置期回 422 并说清「让主人去开」，用户才知道下一步点哪里。
		//
		// 只校验普通成员，不校验主持人：派发侧同样不对主持人做权限闸门（主持工作
		// 由创建者显式选定），两侧口径必须一致，否则会出现「能建不能跑」。
		//
		// **未认领节点放行**：权限位是「主人对自己 agent 的授权」，而
		// claimed_by_user_id 为空意味着这台节点还没有 C 端主人 —— 没有能点「开启」
		// 的人。对它回 422 会把节点永久锁在讨论组之外（用户手上没有任何可操作的
		// 入口）。这类节点沿用迁移前的准入口径：能力 + 在线 + 注册即通过；真正
		// 需要保护的是「已认领」的 agent —— 那正是被分享出去、主人以为还在自己
		// 手里的那些。
		//
		// 判据是「聊天室 ∪ 讨论组 至少开一个」，不是单看讨论组：加人的这一刻还
		// 不知道这个房间以后会不会被用作讨论组（讨论组是房间建好之后才由
		// createRoundtableShell 起的），只认 allow_discussion 会误拒一大批「只想
		// 在聊天室里聊」的 agent。派发侧仍按房间当时的实际用途分别判定。
		if !node.AllowChatroom && !node.AllowDiscussion && nodeIsClaimed(node) {
			return errResult(422, agentPermissionDeniedMessage("聊天室/讨论组", m.NodeID))
		}
		if m.RoleName == "" {
			return errResult(422, "AI 成员角色不能为空")
		}
		if len(m.RoleInstructions) > 8000 {
			return errResult(422, "AI 成员角色说明不能超过 8000 个字符")
		}
		model, modelErr := validateDiscussionModel(m.Model)
		if modelErr != "" {
			return errResult(422, modelErr)
		}
		if seenMemberIDs[node.RongcloudUserID] {
			return errResult(422, "AI 成员账号不能重复")
		}
		seenMemberIDs[node.RongcloudUserID] = true
		resolved = append(resolved, resolvedMember{node: node, role: m.RoleName, instr: m.RoleInstructions, model: model, order: int32(i)})
	}
	// Create the RongCloud-side room.
	if h.client != nil {
		if err := h.client.createChatroom(ctx, rcID); err != nil {
			h.logger.Error("rongcloud: createChatroom", "error", err)
			return errResult(500, "融云聊天室创建失败")
		}
	}
	// Insert the chatroom row.
	room, err := h.queries.CreateRongCloudChatroom(ctx, db.CreateRongCloudChatroomParams{
		WorkspaceID:         actor.WorkspaceID,
		RongcloudChatroomID: rcID,
		OwnerUserID:         actor.ID,
		HostNodeID:          hostNode.ID,
		MaxRounds:           maxRounds,
		ConversationKind:    pgtype.Text{String: "chatroom", Valid: true},
		Config:              []byte("{}"),
		Status:              "active",
		Name:                name,
		PortraitUri:         portrait,
	})
	if err != nil {
		// Roll back the RongCloud room.
		if h.client != nil {
			_ = h.client.destroyChatroom(ctx, rcID)
		}
		h.logger.Error("rongcloud: insert chatroom", "error", err)
		return errResult(500, "创建聊天室失败")
	}
	// Owner as a human member.
	nickname := actor.Name.String
	if !actor.Name.Valid || nickname == "" {
		nickname = actor.RongcloudUserID
	}
	_, _ = h.queries.UpsertRongCloudChatroomMemberByMemberID(ctx, db.UpsertRongCloudChatroomMemberByMemberIDParams{
		ChatroomID:   room.ID,
		MemberType:   "user",
		Capabilities: []byte("[]"),
		Enabled:      true,
		MemberID:     pgtype.Text{String: actor.RongcloudUserID, Valid: true},
		Nickname:     nickname,
		PortraitUri:  DefaultPortraitIfEmpty(actor.PortraitUri.String),
	})
	// Host as an AI member.
	_, _ = h.queries.UpsertRongCloudChatroomMemberByMemberID(ctx, db.UpsertRongCloudChatroomMemberByMemberIDParams{
		ChatroomID:       room.ID,
		NodeID:           hostNode.ID,
		MemberType:       "ai",
		RoleName:         pgtype.Text{String: roundtableHostRole, Valid: true},
		RoleInstructions: pgtype.Text{String: roundtableHostInstruction, Valid: true},
		Capabilities:     hostNode.Capabilities,
		Enabled:          true,
		MemberID:         pgtype.Text{String: hostNode.RongcloudUserID, Valid: true},
		Nickname:         hostNodeNickname(hostNode),
		// AI 成员头像不能写死空串：聊天室成员列表直接读这一列。
		PortraitUri: DefaultUserPortrait(),
	})
	// Ordinary AI members.
	for _, m := range resolved {
		model := pgtype.Text{}
		if m.model != "" {
			model = pgtype.Text{String: m.model, Valid: true}
		}
		_, _ = h.queries.UpsertRongCloudChatroomMemberByMemberID(ctx, db.UpsertRongCloudChatroomMemberByMemberIDParams{
			ChatroomID:       room.ID,
			NodeID:           m.node.ID,
			MemberType:       "ai",
			RoleName:         pgtype.Text{String: m.role, Valid: true},
			RoleInstructions: pgtype.Text{String: m.instr, Valid: true},
			Capabilities:     m.node.Capabilities,
			SpeakingOrder:    pgtype.Int4{Int32: m.order, Valid: true},
			Enabled:          true,
			DiscussionModel:  model,
			MemberID:         pgtype.Text{String: m.node.RongcloudUserID, Valid: true},
			Nickname:         hostNodeNickname(m.node),
			PortraitUri:      DefaultUserPortrait(),
		})
	}
	// Sync the AI members into the RongCloud room.
	if h.client != nil {
		rcUserIDs := []string{hostNode.RongcloudUserID}
		for _, m := range resolved {
			rcUserIDs = append(rcUserIDs, m.node.RongcloudUserID)
		}
		_ = h.client.joinChatroom(ctx, rcID, rcUserIDs)
	}
	view, _ := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	members, _ := h.loadMembers(ctx, room.ID)
	if view.ID.Valid {
		return okResult(chatroomViewData(viewRow{view}, members, true, actor.RongcloudUserID))
	}
	return okResult(map[string]interface{}{"chatroomId": rcID, "chatroomName": name})
}

func (h *systemHandler) actionChatroomSetHost(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	hostNodeID := getStringAny(cmd.Params, "host_node_id", "hostNodeId")
	if rcID == "" || hostNodeID == "" {
		return errResult(400, "参数不完整")
	}
	room, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return errResult(404, "聊天室不存在")
	}
	if room.OwnerUserID != actor.ID {
		return errResult(403, "只有聊天室创建者可以设置主持人")
	}
	if room.ConversationKind.Valid && room.ConversationKind.String == "discussion" {
		return errResult(403, "系统主持人由管理后台配置，不能在讨论组中修改")
	}
	node, err := h.resolveNode(ctx, hostNodeID)
	if err != nil {
		return errResult(422, "AI 节点未注册")
	}
	if !nodeIsOnline(ctx, h.client, node) {
		return errResult(422, "主持人节点必须在线")
	}
	if missing := missingCapabilities(node.Capabilities, roundtableHostCaps); len(missing) > 0 {
		return errResult(422, "主持人节点缺少能力: "+strings.Join(missing, ", "))
	}
	// The host must already be an AI member of the room.
	rawMembers, err := h.queries.ListRongCloudChatroomMembersWithNode(ctx, room.ID)
	if err != nil {
		return errResult(500, "设置主持人失败")
	}
	isMember := false
	for _, m := range rawMembers {
		if m.BusinessNodeID.Valid && m.BusinessNodeID.String == hostNodeID && m.MemberType == "ai" && m.Enabled {
			isMember = true
			break
		}
	}
	if !isMember {
		return errResult(422, "主持人必须是聊天室 AI 成员")
	}
	updated, err := h.queries.SetRongCloudChatroomHostByRongCloudID(ctx, db.SetRongCloudChatroomHostByRongCloudIDParams{
		RongcloudChatroomID: rcID,
		HostNodeID:          node.ID,
	})
	if err != nil {
		return errResult(500, "设置主持人失败")
	}
	view, _ := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if !view.ID.Valid {
		view = db.GetActiveRongCloudChatroomViewByRongCloudIDRow{
			ID: updated.ID, RongcloudChatroomID: updated.RongcloudChatroomID,
			Name: updated.Name, PortraitUri: updated.PortraitUri,
			OwnerUserID: updated.OwnerUserID, HostNodeID: updated.HostNodeID,
			MaxRounds: updated.MaxRounds, ConversationKind: updated.ConversationKind,
			Status: updated.Status, CreatedAt: updated.CreatedAt, UpdatedAt: updated.UpdatedAt,
		}
	}
	members, _ := h.loadMembers(ctx, view.ID)
	return okResult(chatroomViewData(viewRow{view}, members, true, actor.RongcloudUserID))
}

func (h *systemHandler) actionChatroomSetMemberRole(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	nodeID := getStringAny(cmd.Params, "node_id", "nodeId")
	if rcID == "" || nodeID == "" {
		return errResult(400, "参数不完整")
	}
	room, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return errResult(404, "聊天室不存在")
	}
	if room.OwnerUserID != actor.ID {
		return errResult(403, "只有聊天室创建者可以设置 AI 角色")
	}
	node, err := h.resolveNode(ctx, nodeID)
	if err != nil {
		return errResult(422, "AI 节点未注册")
	}
	roleName := getStringAny(cmd.Params, "role_name", "roleName")
	roleInstructions := getStringAny(cmd.Params, "role_instructions", "roleInstructions")
	enabled := true
	if v, ok := cmd.Params["enabled"]; ok {
		if b, ok := v.(bool); ok {
			enabled = b
		}
	}
	if !enabled && room.HostBusinessNodeID.Valid && room.HostBusinessNodeID.String == nodeID {
		return errResult(409, "请先重新指派主持人")
	}
	updated, err := h.queries.UpdateRongCloudChatroomMemberRoleByChatroomAndNodeID(ctx, db.UpdateRongCloudChatroomMemberRoleByChatroomAndNodeIDParams{
		ChatroomID:       room.ID,
		NodeID:           node.ID,
		RoleName:         pgtype.Text{String: roleName, Valid: roleName != ""},
		RoleInstructions: pgtype.Text{String: roleInstructions, Valid: roleInstructions != ""},
		Enabled:          enabled,
	})
	if err != nil {
		return errResult(404, "成员不存在")
	}
	return okResult(map[string]interface{}{
		"nodeId":           nodeID,
		"roleName":         roleName,
		"roleInstructions": roleInstructions,
		"model":            textStr(updated.DiscussionModel),
		"speakingOrder":    updated.SpeakingOrder.Int32,
		"capabilities":     capsToInterface(node.Capabilities),
		"enabled":          enabled,
	})
}

func (h *systemHandler) actionChatroomSetDiscussionTeam(ctx context.Context, cmd CommandContent, actor db.RongcloudUser) commandResult {
	rcID := getStringAny(cmd.Params, "chatroom_id", "chatroomId")
	if rcID == "" {
		return errResult(400, "参数不完整")
	}
	room, err := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	if err != nil {
		return errResult(404, "聊天室不存在")
	}
	if room.OwnerUserID != actor.ID {
		return errResult(403, "只有聊天室创建者可以配置讨论团队")
	}
	requestedHost := getStringAny(cmd.Params, "host_node_id", "hostNodeId")
	currentHost := ""
	if room.HostBusinessNodeID.Valid {
		currentHost = room.HostBusinessNodeID.String
	}
	if requestedHost != "" && requestedHost != currentHost {
		return errResult(409, "主持节点与已创建的讨论组不一致，请恢复原主持节点后重试")
	}
	values := parseAIMembers(cmd.Params["aiMembers"])
	if values == nil {
		values = parseAIMembers(cmd.Params["members"])
	}
	if len(values) < 1 || len(values) > 8 {
		return errResult(422, "普通讨论成员数量必须为 1 到 8 个")
	}
	// Resolve and validate each member.
	seen := map[string]bool{}
	type resolved struct {
		node  db.RongcloudNode
		role  string
		instr string
		model string
	}
	resolvedMembers := make([]resolved, 0, len(values))
	for _, m := range values {
		if m.NodeID == "" || seen[m.NodeID] {
			return errResult(422, "AI 成员节点不能为空或重复")
		}
		if m.NodeID == currentHost {
			return errResult(422, "主持人不能同时作为普通讨论成员")
		}
		seen[m.NodeID] = true
		node, err := h.resolveNode(ctx, m.NodeID)
		if err != nil {
			return errResult(422, fmt.Sprintf("AI 节点未注册: %s", m.NodeID))
		}
		if !nodeIsOnline(ctx, h.client, node) {
			return errResult(422, fmt.Sprintf("AI 节点必须在线: %s", m.NodeID))
		}
		if missing := missingCapabilities(node.Capabilities, roundtableMembCaps); len(missing) > 0 {
			return errResult(422, fmt.Sprintf("AI 节点缺少能力: %s", strings.Join(missing, ", ")))
		}
		// 同 actionChatroomCreate：配置讨论团队时就挡住一个权限都没开的 agent。
		// 判据同样是「聊天室 ∪ 讨论组 至少开一个」（理由见上）。
		// 未认领节点放行，理由见 actionChatroomCreate 里的长注释。
		if !node.AllowChatroom && !node.AllowDiscussion && nodeIsClaimed(node) {
			return errResult(422, agentPermissionDeniedMessage("聊天室/讨论组", m.NodeID))
		}
		if m.RoleName == "" {
			return errResult(422, "AI 成员角色不能为空")
		}
		if len(m.RoleInstructions) > 8000 {
			return errResult(422, "AI 成员角色说明不能超过 8000 个字符")
		}
		model, modelErr := validateDiscussionModel(m.Model)
		if modelErr != "" {
			return errResult(422, modelErr)
		}
		resolvedMembers = append(resolvedMembers, resolved{node: node, role: m.RoleName, instr: m.RoleInstructions, model: model})
	}
	// Replace the non-host AI members: delete all AI members except the host,
	// then insert the new team.
	members, err := h.queries.ListRongCloudChatroomMembersWithNode(ctx, room.ID)
	if err != nil {
		return errResult(500, "配置讨论团队失败")
	}
	for _, m := range members {
		if m.MemberType == "ai" && (!m.BusinessNodeID.Valid || m.BusinessNodeID.String != currentHost) {
			_ = h.queries.DeleteRongCloudChatroomMemberByMemberID(ctx, db.DeleteRongCloudChatroomMemberByMemberIDParams{
				ChatroomID: room.ID,
				MemberID:   m.MemberID,
			})
		}
	}
	for i, m := range resolvedMembers {
		model := pgtype.Text{}
		if m.model != "" {
			model = pgtype.Text{String: m.model, Valid: true}
		}
		_, _ = h.queries.UpsertRongCloudChatroomMemberByMemberID(ctx, db.UpsertRongCloudChatroomMemberByMemberIDParams{
			ChatroomID:       room.ID,
			NodeID:           m.node.ID,
			MemberType:       "ai",
			RoleName:         pgtype.Text{String: m.role, Valid: true},
			RoleInstructions: pgtype.Text{String: m.instr, Valid: true},
			Capabilities:     m.node.Capabilities,
			SpeakingOrder:    pgtype.Int4{Int32: int32(i), Valid: true},
			Enabled:          true,
			DiscussionModel:  model,
			MemberID:         pgtype.Text{String: m.node.RongcloudUserID, Valid: true},
			Nickname:         hostNodeNickname(m.node),
			PortraitUri:      DefaultUserPortrait(),
		})
	}
	// Sync join/quit with RongCloud.
	if h.client != nil {
		var add []string
		for _, m := range resolvedMembers {
			add = append(add, m.node.RongcloudUserID)
		}
		if len(add) > 0 {
			_ = h.client.joinChatroom(ctx, rcID, add)
		}
	}
	view, _ := h.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, rcID)
	updatedMembers, _ := h.loadMembers(ctx, room.ID)
	if view.ID.Valid {
		return okResult(chatroomViewData(viewRow{view}, updatedMembers, true, actor.RongcloudUserID))
	}
	return okResult(map[string]interface{}{"chatroomId": rcID})
}

// resolveNode maps a business node_id (TEXT) to the rongcloud_node row.
func (h *systemHandler) resolveNode(ctx context.Context, nodeID string) (db.RongcloudNode, error) {
	if nodeID == "" {
		return db.RongcloudNode{}, errors.New("empty node id")
	}
	return h.queries.GetRongCloudNodeByNodeID(ctx, nodeID)
}

// hostNodeNickname returns the display name for an AI node, defaulting to its
// business node_id when the rongcloud_user name is unset.
func hostNodeNickname(node db.RongcloudNode) string {
	return node.NodeID
}

// agentPermissionDeniedMessage 是「智能体权限没开」的统一 422 文案。
//
// 必须点名场景（群聊/讨论组/聊天室）与节点：主人可能同时有多台设备，只说
// 「权限不足」等于让用户自己去猜是哪个 agent 的哪个开关。后半句给出动作
// （去设备名片里开），否则用户只会看到一条无法处理的错误。
func agentPermissionDeniedMessage(scene, nodeID string) string {
	return fmt.Sprintf("该智能体未开启%s权限，请让主人在设备名片里开启: %s", scene, nodeID)
}

// nodeIsClaimed 报告这个节点是否已经有 C 端主人（claimed_by_user_id 非空）。
//
// 权限位是「主人对自己 agent 的授权」，没有认领者就没有能点开关的人：这类节点
// （历史数据、或绑定流程还没走完的中间态）在配置期不做权限拦截，否则它会被永久
// 锁在讨论组之外，而用户手上没有任何可操作的入口。
func nodeIsClaimed(node db.RongcloudNode) bool {
	return node.ClaimedByUserID.Valid && node.ClaimedByUserID.String != ""
}
