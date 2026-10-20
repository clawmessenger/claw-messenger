// C 端配额与半成品列表 REST（讨论组引导式创建与配额）。
//
//	GET /api/quota/me                     当前用户三类资源的 limit/used/remaining
//	GET /api/my/discussions?status=configuring  当前用户的配置中半成品（会话列表合并用）
//
// 鉴权与 P4 讨论 REST 一致：Bearer = claw_im_users.rongcloud_token。
package handler

import (
	"net/http"
	"time"
)

// ClawGetMyQuota GET /api/quota/me
//
// data 形状（service 层转 camelCase 前的 snake 契约，web services/quota.ts 负责映射）：
//
//	{discussion:{limit,used,remaining}, group:{...}, chatroom:{...}}
//
// limit=-1 表示不限；remaining=-1 表示不限。配额未启用（融云集成未装配）时 503，
// 前端按「不限制」降级处理。
func (h *Handler) ClawGetMyQuota(w http.ResponseWriter, r *http.Request) {
	if h.ClawQuota == nil {
		clawJSON(w, 503, 503, "配额服务尚未初始化", nil)
		return
	}
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	usage, err := h.ClawQuota.Usage(r.Context(), userID)
	if err != nil {
		clawJSON(w, 500, 500, "查询配额失败", nil)
		return
	}
	clawJSON(w, 200, 200, "", usage)
}

// ClawListMyDiscussions GET /api/my/discussions?status=configuring
//
// 目前只支持 status=configuring（半成品列表）；缺省与未知值按 400 处理，
// 避免日后扩 status 语义时旧客户端拿到误解的数据。
func (h *Handler) ClawListMyDiscussions(w http.ResponseWriter, r *http.Request) {
	if h.Queries == nil {
		clawJSON(w, 503, 503, "讨论服务尚未初始化", nil)
		return
	}
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	if status := r.URL.Query().Get("status"); status != "configuring" {
		clawJSON(w, 400, 400, "status 仅支持 configuring", nil)
		return
	}
	rows, err := h.Queries.ListMyConfiguringRongCloudDiscussions(r.Context(), userID)
	if err != nil {
		clawJSON(w, 500, 500, "查询失败", nil)
		return
	}
	items := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		// 房间名取自聊天室行（半成品数量小，逐条查代价可忽略）；已删房（并发
		// GC）时名字留空，前端仍有 step/topic 可渲染。
		name := ""
		if room, gErr := h.Queries.GetRongCloudChatroomViewByRongCloudID(r.Context(), row.ChatroomID); gErr == nil {
			name = room.Name
		}
		var lastInteractionAt int64
		if row.LastInteractionAt.Valid {
			lastInteractionAt = row.LastInteractionAt.Time.UTC().UnixMilli()
		}
		var createdAt string
		if row.CreatedAt.Valid {
			createdAt = row.CreatedAt.Time.UTC().Format(time.RFC3339)
		}
		items = append(items, map[string]interface{}{
			"chatroomId":        row.ChatroomID,
			"discussionId":      row.DiscussionID,
			"name":              name,
			"status":            row.Status,
			"step":              row.SetupStep,
			"topic":             row.Topic,
			"lastInteractionAt": lastInteractionAt,
			"createdAt":         createdAt,
		})
	}
	clawJSON(w, 200, 200, "", items)
}
