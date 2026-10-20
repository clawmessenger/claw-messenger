// Package handler - 管理后台「聊天室管理 / 讨论组管理 / 消息查询」。
//
// 这三片在 Go 侧此前整片是空桩（/api/admin/chatrooms、/api/admin/messages 恒返回
// 空页，/{id} 直接 404），所以管理端两个页面永远列不出任何数据；讨论组管理则
// 连页面都没有。这里把它们换成真实实现。
//
// 数据源（全部是 Go 侧 rongcloud_* 表，与 C 端同源）：
//   - 聊天室：rongcloud_chatroom（实体）+ rongcloud_chatroom_member（成员）
//   - rongcloud_discussion（该房间的讨论场次，用来推阶段）
//   - 讨论组：rongcloud_discussion（讨论实例：话题/目标/状态/轮次/主持快照）
//   - rongcloud_discussion_round_document（逐轮纪要）
//   - 消息：rongcloud_discussion_event（唯一持久化的 IM 消息流 —— 融云只负责
//     投递，Go 不落普通单聊/群聊消息，落库的只有讨论事件）
//
// 术语对齐 C 端（clawmessenger-web/src/lib/conversation-kind.ts）：
// conversation_kind='discussion' 的房间在 C 端叫「讨论组」，其余叫「聊天室」。
// 管理端因此把「聊天室」定义为房间实体、把「讨论组」定义为讨论场次，两者是
// 一对多，各自有独立页面，不重复列同一批数据。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// ---- 事件公共列 ------------------------------------------------------------

// clawAdminEventRoundExpr 取事件所属轮次（别名固定为 e）。
//
// 迁移 589 之后的链路只把 round 写进 payload（v3 信封），round_number 这一列是
// 旧极简链路留下的、新行恒为 0 —— 只读列会让界面上的「轮次」永远是 0/空。
// payload->>'round' 是文本，非数字时不能直接 ::int（会整条查询报错），
// 所以先用正则挡一道再转。
const clawAdminEventRoundExpr = `
	COALESCE(NULLIF(e.round_number, 0),
	         CASE WHEN COALESCE(e.payload->>'round', '') ~ '^[0-9]{1,6}$'
	              THEN (e.payload->>'round')::int ELSE 0 END)`

// ---- 阶段映射 --------------------------------------------------------------
// clawAdminStageCase 把讨论状态折叠成管理端沿用的四段阶段（旧 Python 后端的
// ADMIN_WEB_STAGE_STATUSES 是同一套映射，前端筛选器直接以这四个值做选项）。
const clawAdminStageCase = `
	CASE latest.status
		WHEN 'planning'     THEN 'intake'
		WHEN 'awaiting_user' THEN 'proposal'
		WHEN 'running'      THEN 'execution'
		WHEN 'summarizing'  THEN 'execution'
		WHEN 'synthesizing' THEN 'execution'
		WHEN 'completed'    THEN 'done'
		WHEN 'cancelled'    THEN 'done'
		WHEN 'failed'       THEN 'done'
		ELSE ''
	END`

var clawAdminChatRoomStages = map[string]bool{
	"intake": true, "proposal": true, "execution": true, "done": true,
}

// clawAdminChatRoomSource 是聊天室列表与详情共用的投影。所有统计都写成相关子查询：
// 房间量级由设备/用户数决定（线上个位数），一次全量扫比在页面循环里逐条查便宜得多，
// 也避免把「最近一个讨论」的判据散到多处。
//
// 两个易错点：
//   - rongcloud_chatroom_member.chatroom_id 指向 rongcloud_chatroom.id（内部 UUID），
//     不是 rongcloud_chatroom_id（融云 id），所以带成员统计的子查询都拿 c.id 做键；
//     而 rongcloud_discussion.chatroom_id 存的恰恰是融云 id，两者不能混用。
//   - 「用户」取讨论的 created_by（真实 IM 用户 id）而不是 c.owner_user_id：后者是
//     multica 账号 UUID，和 IM 账号是两个命名空间，直接显示会让人对不上号。
const clawAdminChatRoomSource = `
	SELECT c.rongcloud_chatroom_id                    AS room_id,
	       COALESCE(c.name, '')                       AS title,
	       COALESCE(c.conversation_kind, '')          AS conversation_kind,
	       c.status                                   AS room_status,
	       c.max_rounds                               AS max_rounds,
	       COALESCE(latest.status, '')                AS discussion_status,
	       COALESCE(latest.discussion_id, '')         AS discussion_id,
	       COALESCE(` + clawAdminStageCase + `, '')   AS stage,
	       COALESCE((
	           SELECT d0.created_by FROM rongcloud_discussion d0
	           WHERE d0.chatroom_id = c.rongcloud_chatroom_id
	           ORDER BY d0.created_at DESC LIMIT 1
	       ), COALESCE((
	           SELECT m0.member_id FROM rongcloud_chatroom_member m0
	           WHERE m0.chatroom_id = c.id AND m0.member_type = 'user'
	           ORDER BY m0.created_at ASC LIMIT 1
	       ), ''))                                    AS user_id,
	       COALESCE((
	           SELECT n.rongcloud_user_id FROM rongcloud_node n WHERE n.id = c.host_node_id
	       ), '')                                     AS assigned_executor,
	       (SELECT COUNT(*) FROM rongcloud_chatroom_member m WHERE m.chatroom_id = c.id)
	                                                  AS participant_count,
	       (SELECT COUNT(*) FROM rongcloud_discussion_event e
	        JOIN rongcloud_discussion d ON d.discussion_id = e.discussion_id
	        WHERE d.chatroom_id = c.rongcloud_chatroom_id)
	                                                  AS message_count,
	       (SELECT MAX(e.created_at) FROM rongcloud_discussion_event e
	        JOIN rongcloud_discussion d ON d.discussion_id = e.discussion_id
	        WHERE d.chatroom_id = c.rongcloud_chatroom_id)
	                                                  AS last_message_at,
	       (SELECT COUNT(*) FROM rongcloud_discussion d
	        WHERE d.chatroom_id = c.rongcloud_chatroom_id)
	                                                  AS discussion_count,
	       c.created_at                               AS created_at,
	       c.updated_at                               AS updated_at
	FROM rongcloud_chatroom c
	LEFT JOIN LATERAL (
	    SELECT d.status, d.discussion_id FROM rongcloud_discussion d
	    WHERE d.chatroom_id = c.rongcloud_chatroom_id
	    ORDER BY d.created_at DESC LIMIT 1
	) latest ON TRUE`

const clawAdminChatRoomColumns = `
	room_id, title, conversation_kind, stage, user_id, assigned_executor,
	participant_count, message_count, last_message_at, discussion_count,
	created_at, updated_at`

// clawAdminChatRoomRow is the wire shape. Kept as a struct (not a map) so the
// list and the detail endpoint cannot drift apart field by field.
type clawAdminChatRoomRow struct {
	RoomID           string     `json:"room_id"`
	Title            string     `json:"title"`
	ConversationKind string     `json:"conversation_kind"`
	Stage            string     `json:"stage"`
	UserID           string     `json:"user_id"`
	AssignedExecutor string     `json:"assigned_executor"`
	ParticipantCount int        `json:"participant_count"`
	MessageCount     int        `json:"message_count"`
	LastMessageAt    *time.Time `json:"last_message_at"`
	DiscussionCount  int        `json:"discussion_count"`
	DiscussionID     string     `json:"discussion_id,omitempty"`
	DiscussionStatus string     `json:"discussion_status,omitempty"`
	MaxRounds        int        `json:"max_rounds,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	// 房间状态只在包含已删除房间时才有意义（默认列表只给 active）。
	RoomStatus string `json:"room_status,omitempty"`
}

func scanClawAdminChatRoom(row pgx.Row) (clawAdminChatRoomRow, error) {
	var out clawAdminChatRoomRow
	err := row.Scan(
		&out.RoomID, &out.Title, &out.ConversationKind, &out.Stage, &out.UserID,
		&out.AssignedExecutor, &out.ParticipantCount, &out.MessageCount,
		&out.LastMessageAt, &out.DiscussionCount, &out.CreatedAt, &out.UpdatedAt,
	)
	return out, err
}

// ---- GET /api/admin/chatrooms ----------------------------------------------

func (h *Handler) ClawAdminListChatRooms(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("group:read", w, r) == nil {
		return
	}
	page, pageSize := clawAdminPagination(r)
	q := r.URL.Query()
	keyword := strings.TrimSpace(q.Get("keyword"))
	stage := strings.TrimSpace(q.Get("stage"))
	kind := strings.TrimSpace(q.Get("conversation_kind"))
	status := strings.TrimSpace(q.Get("status"))

	if stage != "" && !clawAdminChatRoomStages[stage] {
		clawAdminFail(w, 400, "stage has an unsupported value")
		return
	}
	// conversation_kind 三个取值：discussion=讨论组房间；normal=普通聊天室
	// （投影里 COALESCE 成空串，只能用 = '' 表达，直接传空串又会被当成
	// 「不过滤」，所以用 normal 这个字面量）；缺省/空 = 全部。
	if kind != "" && kind != "normal" && kind != "discussion" {
		clawAdminFail(w, 400, "conversation_kind has an unsupported value")
		return
	}

	where := []string{}
	args := []interface{}{}
	// 默认只看未删除的房间；status=all 才会带出软删除的。
	switch status {
	case "all":
	case "deleted":
		where = append(where, "room_status = 'deleted'")
	default:
		where = append(where, "room_status = 'active'")
	}
	if keyword != "" {
		args = append(args, clawAdminLikePattern(keyword))
		where = append(where, "(room_id ILIKE $1 ESCAPE '\\' OR title ILIKE $1 ESCAPE '\\' OR user_id ILIKE $1 ESCAPE '\\' OR assigned_executor ILIKE $1 ESCAPE '\\')")
	}
	if stage != "" {
		args = append(args, stage)
		where = append(where, "stage = $"+strconv.Itoa(len(args)))
	}
	switch kind {
	case "normal":
		where = append(where, "conversation_kind = ''")
	case "discussion":
		args = append(args, kind)
		where = append(where, "conversation_kind = $"+strconv.Itoa(len(args)))
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}

	source := " FROM (" + clawAdminChatRoomSource + ") AS room" + whereSQL

	var total int
	if err := h.DB.QueryRow(r.Context(), "SELECT COUNT(*)"+source, args...).Scan(&total); err != nil {
		clawAdminFail(w, 503, "聊天室列表不可用")
		return
	}

	rows, err := h.DB.Query(r.Context(),
		"SELECT "+clawAdminChatRoomColumns+source+
			" ORDER BY CASE WHEN last_message_at IS NULL THEN 1 ELSE 0 END,"+
			" last_message_at DESC, updated_at DESC LIMIT $"+strconv.Itoa(len(args)+1)+" OFFSET $"+strconv.Itoa(len(args)+2),
		append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		clawAdminFail(w, 503, "聊天室列表不可用")
		return
	}
	defer rows.Close()
	items := []clawAdminChatRoomRow{}
	for rows.Next() {
		room, err := scanClawAdminChatRoom(rows)
		if err != nil {
			clawAdminFail(w, 503, "聊天室列表不可用")
			return
		}
		items = append(items, room)
	}
	if err := rows.Err(); err != nil {
		clawAdminFail(w, 503, "聊天室列表不可用")
		return
	}
	clawAdminOK(w, map[string]interface{}{"items": items, "total": total, "page": page, "page_size": pageSize})
}

// ---- GET /api/admin/chatrooms/{id} -----------------------------------------

func (h *Handler) ClawAdminGetChatRoom(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("group:read", w, r) == nil {
		return
	}
	room, found, err := h.clawAdminChatRoomByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		clawAdminFail(w, 503, "聊天室信息不可用")
		return
	}
	if !found {
		clawAdminFail(w, 404, "聊天室不存在")
		return
	}
	clawAdminOK(w, room)
}

func (h *Handler) clawAdminChatRoomByID(ctx context.Context, roomID string) (clawAdminChatRoomRow, bool, error) {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return clawAdminChatRoomRow{}, false, nil
	}
	row := h.DB.QueryRow(ctx,
		"SELECT "+clawAdminChatRoomColumns+" FROM ("+clawAdminChatRoomSource+") AS room WHERE room_id = $1 LIMIT 1", roomID)
	room, err := scanClawAdminChatRoom(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return clawAdminChatRoomRow{}, false, nil
	}
	if err != nil {
		return clawAdminChatRoomRow{}, false, err
	}
	return room, true, nil
}

// ---- GET /api/admin/chatrooms/{id}/messages --------------------------------

// clawAdminChatRoomMessage 对应前端的 ChatRoomMessage。author_label 依次尝试
// IM 账号昵称 → 登录名 → 房间里的角色名 → 融云用户资料名，最后回落到 actor id：
// 讨论事件里的 actor 既可能是人类用户（claw_im_users），也可能是节点账号
// （rongcloud_user），两条来源都要覆盖，否则 AI 发言会显示成一串 rc_node_*。
type clawAdminChatRoomMessage struct {
	MessageID     string    `json:"message_id"`
	RoomID        string    `json:"room_id"`
	Kind          string    `json:"kind"`
	AuthorRole    string    `json:"author_role"`
	AuthorLabel   string    `json:"author_label"`
	ParticipantID string    `json:"participant_id"`
	Content       string    `json:"content"`
	Round         int       `json:"round"`
	CreatedAt     time.Time `json:"created_at"`
}

func (h *Handler) ClawAdminChatRoomMessages(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("group:read", w, r) == nil {
		return
	}
	roomID := strings.TrimSpace(chi.URLParam(r, "id"))
	if _, found, err := h.clawAdminChatRoomByID(r.Context(), roomID); err != nil {
		clawAdminFail(w, 503, "聊天室信息不可用")
		return
	} else if !found {
		clawAdminFail(w, 404, "聊天室不存在")
		return
	}

	rows, err := h.DB.Query(r.Context(), `
		SELECT e.id::text,
		       e.event_type,
		       COALESCE(e.actor_id, ''),
		       COALESCE(m.role_name, ''),
		       COALESCE(NULLIF(u.nickname, ''), NULLIF(u.username, ''),
		                NULLIF(m.role_name, ''), NULLIF(ru.name, ''), COALESCE(e.actor_id, '')),
		       COALESCE(e.payload->>'content', ''),
		       `+clawAdminEventRoundExpr+`,
		       e.created_at
		FROM rongcloud_discussion_event e
		JOIN rongcloud_discussion d ON d.discussion_id = e.discussion_id
		LEFT JOIN rongcloud_chatroom c ON c.rongcloud_chatroom_id = d.chatroom_id
		LEFT JOIN rongcloud_chatroom_member m ON m.chatroom_id = c.id AND m.member_id = e.actor_id
		LEFT JOIN claw_im_users u ON u.user_id = e.actor_id
		LEFT JOIN rongcloud_user ru ON ru.rongcloud_user_id = e.actor_id
		WHERE d.chatroom_id = $1
		ORDER BY e.created_at ASC, e.seq ASC
		LIMIT 200`, roomID)
	if err != nil {
		clawAdminFail(w, 503, "聊天室消息不可用")
		return
	}
	defer rows.Close()

	items := []clawAdminChatRoomMessage{}
	for rows.Next() {
		var item clawAdminChatRoomMessage
		if err := rows.Scan(&item.MessageID, &item.Kind, &item.ParticipantID, &item.AuthorRole,
			&item.AuthorLabel, &item.Content, &item.Round, &item.CreatedAt); err != nil {
			clawAdminFail(w, 503, "聊天室消息不可用")
			return
		}
		item.RoomID = roomID
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		clawAdminFail(w, 503, "聊天室消息不可用")
		return
	}
	clawAdminOK(w, items)
}

// ---- DELETE /api/admin/chatrooms/{id} --------------------------------------

// ClawAdminDeleteChatRoom 走软删除（status='deleted'）而不是物理删除：
// rongcloud_chatroom 的 status 列本身就是为这件事设计的（CHECK 只允许
// active/deleted），而物理删会连带丢掉讨论场次与事件归档 —— 那正是消息查询页
// 的数据源。融云侧的房间不在这里销毁（旧 Python 后端同样不碰融云，房间名改写
// 一律走 C 端的 command 通道先刷融云）。
func (h *Handler) ClawAdminDeleteChatRoom(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("group:write", w, r)
	if ident == nil {
		return
	}
	roomID := strings.TrimSpace(chi.URLParam(r, "id"))
	tag, err := h.DB.Exec(r.Context(),
		`UPDATE rongcloud_chatroom SET status = 'deleted', updated_at = now()
		 WHERE rongcloud_chatroom_id = $1 AND status = 'active'`, roomID)
	if err != nil {
		clawAdminFail(w, 503, "聊天室删除失败")
		return
	}
	if tag.RowsAffected() == 0 {
		clawAdminFail(w, 404, "聊天室不存在")
		return
	}
	h.clawAdminAudit(r, ident, "chatroom_deleted", "chatroom", roomID, map[string]interface{}{"mode": "soft"})
	clawAdminOK(w, map[string]interface{}{"deleted": true})
}

// ---- GET /api/admin/messages -----------------------------------------------

// 消息查询的数据源只有一样：rongcloud_discussion_event。融云负责投递，Go 不落
// 普通单聊/群聊消息（单聊转发、群聊中继都是即时的，只有讨论事件进库），所以这里
// 的 conversation_type 只会是 chatroom —— 但字段本身保留旧契约的取值域，
// 将来补上别的存档时前端不用改。
const clawAdminMessageSource = `
	SELECT 'evt:' || e.id::text                       AS message_id,
	       COALESCE(d.chatroom_id, e.discussion_id)   AS conversation_id,
	       e.discussion_id                            AS discussion_id,
	       COALESCE(e.actor_id, '')                   AS sender_id,
	       COALESCE(e.target_id, '')                  AS target_id,
	       COALESCE(e.payload->>'content', '')        AS content,
	       'jsonb'                                    AS content_type,
	       e.event_type                               AS message_type,
	       'chatroom'                                 AS conversation_type,
	       e.payload                                  AS metadata,
	       e.created_at                               AS created_at
	FROM rongcloud_discussion_event e
	LEFT JOIN rongcloud_discussion d ON d.discussion_id = e.discussion_id`

const clawAdminMessageColumns = `
	message_id, conversation_id, discussion_id, sender_id, target_id, content,
	content_type, message_type, conversation_type, metadata, created_at`

type clawAdminMessageRow struct {
	MessageID        string    `json:"message_id"`
	ConversationID   string    `json:"conversation_id"`
	DiscussionID     string    `json:"discussion_id"`
	SenderID         string    `json:"sender_id"`
	TargetID         string    `json:"target_id"`
	Content          string    `json:"content"`
	ContentType      string    `json:"content_type"`
	MessageType      string    `json:"message_type"`
	ConversationType string    `json:"conversation_type"`
	Metadata         []byte    `json:"metadata"`
	CreatedAt        time.Time `json:"created_at"`
}

func (h *Handler) ClawAdminMessages(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("message:read", w, r) == nil {
		return
	}
	page, pageSize := clawAdminPagination(r)
	q := r.URL.Query()
	senderID := strings.TrimSpace(q.Get("sender_id"))
	targetID := strings.TrimSpace(q.Get("target_id"))
	conversationID := strings.TrimSpace(q.Get("conversation_id"))
	conversationType := strings.ToLower(strings.TrimSpace(q.Get("conversation_type")))
	messageType := strings.TrimSpace(q.Get("message_type"))
	keyword := strings.TrimSpace(q.Get("keyword"))
	startDate := strings.TrimSpace(q.Get("start_date"))
	endDate := strings.TrimSpace(q.Get("end_date"))

	if len(senderID) > 256 || len(targetID) > 256 || len(conversationID) > 256 {
		clawAdminFail(w, 400, "Invalid identifier filter")
		return
	}
	if len(messageType) > 128 || len(keyword) > 4000 {
		clawAdminFail(w, 400, "Invalid message filter")
		return
	}
	// conversation_type 目前只有 chatroom 有存档；给了别的值仍然按「查不到」
	// 处理（而不是 400），免得前端旧书签直接报错。
	if conversationType != "" && conversationType != "chatroom" {
		clawAdminOK(w, map[string]interface{}{"items": []interface{}{}, "total": 0, "page": page, "page_size": pageSize})
		return
	}

	where := []string{}
	args := []interface{}{}
	add := func(clause string, value interface{}) {
		args = append(args, value)
		where = append(where, strings.Replace(clause, "?", "$"+strconv.Itoa(len(args)), 1))
	}
	if senderID != "" {
		add("sender_id = ?", senderID)
	}
	if targetID != "" {
		add("target_id = ?", targetID)
	}
	if conversationID != "" {
		// 会话 ID 既可能是融云聊天室 id（rt_*），也可能是讨论组 id；两者都放行，
		// 免得运维要先去讨论组页把 id 换个形状再来查。
		args = append(args, conversationID)
		where = append(where, "(conversation_id = $"+strconv.Itoa(len(args))+" OR discussion_id = $"+strconv.Itoa(len(args))+")")
	}
	if messageType != "" {
		add("message_type = ?", messageType)
	}
	if keyword != "" {
		args = append(args, clawAdminLikePattern(keyword))
		where = append(where, "(content ILIKE $"+strconv.Itoa(len(args))+` ESCAPE '\'`+
			" OR message_type ILIKE $"+strconv.Itoa(len(args))+` ESCAPE '\'`+
			" OR sender_id ILIKE $"+strconv.Itoa(len(args))+` ESCAPE '\')`)
	}
	if startDate != "" {
		add("created_at >= ?", startDate)
	}
	if endDate != "" {
		add("created_at <= ?", endDate)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}
	source := " FROM (" + clawAdminMessageSource + ") AS messages" + whereSQL

	var total int
	if err := h.DB.QueryRow(r.Context(), "SELECT COUNT(*)"+source, args...).Scan(&total); err != nil {
		clawAdminFail(w, 503, "消息归档不可用")
		return
	}
	rows, err := h.DB.Query(r.Context(),
		"SELECT "+clawAdminMessageColumns+source+
			" ORDER BY created_at DESC, message_id DESC LIMIT $"+strconv.Itoa(len(args)+1)+" OFFSET $"+strconv.Itoa(len(args)+2),
		append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		clawAdminFail(w, 503, "消息归档不可用")
		return
	}
	defer rows.Close()

	items := make([]map[string]interface{}, 0, pageSize)
	for rows.Next() {
		var row clawAdminMessageRow
		if err := rows.Scan(&row.MessageID, &row.ConversationID, &row.DiscussionID, &row.SenderID,
			&row.TargetID, &row.Content, &row.ContentType, &row.MessageType,
			&row.ConversationType, &row.Metadata, &row.CreatedAt); err != nil {
			clawAdminFail(w, 503, "消息归档不可用")
			return
		}
		item := map[string]interface{}{
			"message_id":        row.MessageID,
			"conversation_id":   row.ConversationID,
			"discussion_id":     row.DiscussionID,
			"sender_id":         row.SenderID,
			"target_id":         row.TargetID,
			"content":           row.Content,
			"content_type":      row.ContentType,
			"message_type":      row.MessageType,
			"conversation_type": row.ConversationType,
			"created_at":        row.CreatedAt.UTC().Format(time.RFC3339),
			// metadata 原样透出 JSON 对象（前端有个「元数据」抽屉直接展示）。
			"metadata": rawJSONOrNull(row.Metadata),
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		clawAdminFail(w, 503, "消息归档不可用")
		return
	}
	clawAdminOK(w, map[string]interface{}{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func rawJSONOrNull(raw []byte) interface{} {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	return json.RawMessage(raw)
}

// ---- GET /api/admin/discussions --------------------------------------------

const clawAdminDiscussionSource = `
	SELECT d.discussion_id,
	       d.chatroom_id,
	       COALESCE(c.name, '')            AS chatroom_name,
	       d.topic,
	       d.goal,
	       d.status,
	       d.mode,
	       d.current_round,
	       d.max_rounds,
	       d.round_policy,
	       d.created_by,
	       d.version,
	       COALESCE((SELECT COUNT(*) FROM rongcloud_discussion_event e
	                 WHERE e.discussion_id = d.discussion_id), 0) AS event_count,
	       COALESCE((SELECT COUNT(*) FROM rongcloud_discussion_round_document rd
	                 WHERE rd.discussion_id = d.discussion_id), 0) AS document_count,
	       (SELECT MAX(e.created_at) FROM rongcloud_discussion_event e
	        WHERE e.discussion_id = d.discussion_id) AS last_event_at,
	       d.created_at,
	       d.updated_at
	FROM rongcloud_discussion d
	LEFT JOIN rongcloud_chatroom c ON c.rongcloud_chatroom_id = d.chatroom_id`

var clawAdminDiscussionStatuses = map[string]bool{
	"planning": true, "running": true, "summarizing": true, "awaiting_user": true,
	"synthesizing": true, "completed": true, "cancelled": true, "failed": true,
}

type clawAdminDiscussionRow struct {
	DiscussionID  string     `json:"discussion_id"`
	ChatroomID    string     `json:"chatroom_id"`
	ChatroomName  string     `json:"chatroom_name"`
	Topic         string     `json:"topic"`
	Goal          string     `json:"goal"`
	Status        string     `json:"status"`
	Mode          string     `json:"mode"`
	CurrentRound  int        `json:"current_round"`
	MaxRounds     *int       `json:"max_rounds"`
	RoundPolicy   string     `json:"round_policy"`
	CreatedBy     string     `json:"created_by"`
	Version       int        `json:"version"`
	EventCount    int        `json:"event_count"`
	DocumentCount int        `json:"document_count"`
	LastEventAt   *time.Time `json:"last_event_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

const clawAdminDiscussionColumns = `
	discussion_id, chatroom_id, chatroom_name, topic, goal, status, mode,
	current_round, max_rounds, round_policy, created_by, version, event_count,
	document_count, last_event_at, created_at, updated_at`

func scanClawAdminDiscussion(row pgx.Row) (clawAdminDiscussionRow, error) {
	var out clawAdminDiscussionRow
	err := row.Scan(
		&out.DiscussionID, &out.ChatroomID, &out.ChatroomName, &out.Topic, &out.Goal,
		&out.Status, &out.Mode, &out.CurrentRound, &out.MaxRounds, &out.RoundPolicy,
		&out.CreatedBy, &out.Version, &out.EventCount, &out.DocumentCount,
		&out.LastEventAt, &out.CreatedAt, &out.UpdatedAt,
	)
	return out, err
}

func (h *Handler) ClawAdminListDiscussions(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("group:read", w, r) == nil {
		return
	}
	page, pageSize := clawAdminPagination(r)
	q := r.URL.Query()
	keyword := strings.TrimSpace(q.Get("keyword"))
	status := strings.TrimSpace(q.Get("status"))
	if status != "" && !clawAdminDiscussionStatuses[status] {
		clawAdminFail(w, 400, "status has an unsupported value")
		return
	}

	where := []string{}
	args := []interface{}{}
	if keyword != "" {
		args = append(args, clawAdminLikePattern(keyword))
		p := "$" + strconv.Itoa(len(args))
		where = append(where, "(discussion_id ILIKE "+p+` ESCAPE '\'`+
			" OR chatroom_id ILIKE "+p+` ESCAPE '\'`+
			" OR chatroom_name ILIKE "+p+` ESCAPE '\'`+
			" OR topic ILIKE "+p+` ESCAPE '\'`+
			" OR created_by ILIKE "+p+` ESCAPE '\')`)
	}
	if status != "" {
		args = append(args, status)
		where = append(where, "status = $"+strconv.Itoa(len(args)))
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}
	source := " FROM (" + clawAdminDiscussionSource + ") AS disc" + whereSQL

	var total int
	if err := h.DB.QueryRow(r.Context(), "SELECT COUNT(*)"+source, args...).Scan(&total); err != nil {
		clawAdminFail(w, 503, "讨论组列表不可用")
		return
	}
	rows, err := h.DB.Query(r.Context(),
		"SELECT "+clawAdminDiscussionColumns+source+
			" ORDER BY created_at DESC LIMIT $"+strconv.Itoa(len(args)+1)+" OFFSET $"+strconv.Itoa(len(args)+2),
		append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		clawAdminFail(w, 503, "讨论组列表不可用")
		return
	}
	defer rows.Close()
	items := []clawAdminDiscussionRow{}
	for rows.Next() {
		item, err := scanClawAdminDiscussion(rows)
		if err != nil {
			clawAdminFail(w, 503, "讨论组列表不可用")
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		clawAdminFail(w, 503, "讨论组列表不可用")
		return
	}
	clawAdminOK(w, map[string]interface{}{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func (h *Handler) ClawAdminGetDiscussion(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("group:read", w, r) == nil {
		return
	}
	discussion, found, err := h.clawAdminDiscussionByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		clawAdminFail(w, 503, "讨论组信息不可用")
		return
	}
	if !found {
		clawAdminFail(w, 404, "讨论组不存在")
		return
	}
	clawAdminOK(w, discussion)
}

func (h *Handler) clawAdminDiscussionByID(ctx context.Context, discussionID string) (clawAdminDiscussionRow, bool, error) {
	discussionID = strings.TrimSpace(discussionID)
	if discussionID == "" {
		return clawAdminDiscussionRow{}, false, nil
	}
	row := h.DB.QueryRow(ctx,
		"SELECT "+clawAdminDiscussionColumns+" FROM ("+clawAdminDiscussionSource+") AS disc WHERE discussion_id = $1 LIMIT 1",
		discussionID)
	discussion, err := scanClawAdminDiscussion(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return clawAdminDiscussionRow{}, false, nil
	}
	if err != nil {
		return clawAdminDiscussionRow{}, false, err
	}
	return discussion, true, nil
}

// ---- GET /api/admin/discussions/{id}/events --------------------------------

// clawAdminDiscussionEvent 是讨论时间线的一行。payload 原样透出（v3 信封就存在
// 里面），前端按 event_type 决定怎么渲染，管理端不再复制一份协议解析。
type clawAdminDiscussionEvent struct {
	EventID   string `json:"event_id"`
	Seq       *int64 `json:"seq"`
	EventType string `json:"event_type"`
	ActorID   string `json:"actor_id"`
	TargetID  string `json:"target_id"`
	Round     int    `json:"round"`
	Content   string `json:"content"`
	Payload   []byte `json:"payload"`
	CreatedAt string `json:"created_at"`
}

func (h *Handler) ClawAdminDiscussionEvents(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("group:read", w, r) == nil {
		return
	}
	discussionID := strings.TrimSpace(chi.URLParam(r, "id"))
	if _, found, err := h.clawAdminDiscussionByID(r.Context(), discussionID); err != nil {
		clawAdminFail(w, 503, "讨论组信息不可用")
		return
	} else if !found {
		clawAdminFail(w, 404, "讨论组不存在")
		return
	}
	rows, err := h.DB.Query(r.Context(), `
		SELECT e.id::text, e.seq, e.event_type, COALESCE(e.actor_id, ''), COALESCE(e.target_id, ''),
		       `+clawAdminEventRoundExpr+`, COALESCE(e.payload->>'content', ''), e.payload, e.created_at
		FROM rongcloud_discussion_event e
		WHERE e.discussion_id = $1
		ORDER BY e.seq ASC, e.created_at ASC
		LIMIT 500`, discussionID)
	if err != nil {
		clawAdminFail(w, 503, "讨论事件不可用")
		return
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0, 64)
	for rows.Next() {
		var (
			event   clawAdminDiscussionEvent
			created time.Time
			payload []byte
		)
		if err := rows.Scan(&event.EventID, &event.Seq, &event.EventType, &event.ActorID,
			&event.TargetID, &event.Round, &event.Content, &payload, &created); err != nil {
			clawAdminFail(w, 503, "讨论事件不可用")
			return
		}
		items = append(items, map[string]interface{}{
			"event_id":   event.EventID,
			"seq":        event.Seq,
			"event_type": event.EventType,
			"actor_id":   event.ActorID,
			"target_id":  event.TargetID,
			"round":      event.Round,
			"content":    event.Content,
			"payload":    rawJSONOrNull(payload),
			"created_at": created.UTC().Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		clawAdminFail(w, 503, "讨论事件不可用")
		return
	}
	clawAdminOK(w, items)
}

// ---- GET /api/admin/discussions/{id}/round-documents -----------------------

func (h *Handler) ClawAdminDiscussionRoundDocuments(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("group:read", w, r) == nil {
		return
	}
	discussionID := strings.TrimSpace(chi.URLParam(r, "id"))
	if _, found, err := h.clawAdminDiscussionByID(r.Context(), discussionID); err != nil {
		clawAdminFail(w, 503, "讨论组信息不可用")
		return
	} else if !found {
		clawAdminFail(w, 404, "讨论组不存在")
		return
	}
	rows, err := h.DB.Query(r.Context(), `
		SELECT document_id, round, revision, title, content, created_at
		FROM rongcloud_discussion_round_document
		WHERE discussion_id = $1
		ORDER BY round ASC, revision DESC`, discussionID)
	if err != nil {
		clawAdminFail(w, 503, "逐轮纪要不可用")
		return
	}
	defer rows.Close()
	items := make([]map[string]interface{}, 0, 8)
	for rows.Next() {
		var (
			documentID, title, content string
			round, revision            int
			created                    time.Time
		)
		if err := rows.Scan(&documentID, &round, &revision, &title, &content, &created); err != nil {
			clawAdminFail(w, 503, "逐轮纪要不可用")
			return
		}
		items = append(items, map[string]interface{}{
			"document_id": documentID,
			"round":       round,
			"revision":    revision,
			"title":       title,
			"content":     content,
			"created_at":  created.UTC().Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		clawAdminFail(w, 503, "逐轮纪要不可用")
		return
	}
	clawAdminOK(w, items)
}
