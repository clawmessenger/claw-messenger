// Package handler —— C 端圆桌扁平 REST（P4）。
//
// 对齐旧 Python 后端 app.py:3042-3182 的路由与响应契约：
//
//	GET /api/chatrooms/{chatroomId}/discussions        列表
//	GET /api/discussions/{discussionId}                快照
//	GET /api/discussions/{discussionId}/events?afterSeq= 事件尾读
//
// 鉴权与 Python 一致：Bearer = claw_im_users.rongcloud_token；成员资格按
// member_id（融云侧 TEXT id）判定。响应为 {code, message, data} 包装，data 的
// 形状对齐 Python snapshot_json / event_json（camelCase）。
package handler

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
)

// clawDiscussionRepo 懒构造 P2 的讨论仓库层（router 未配置融云集成时为 nil → 503）。
func (h *Handler) clawDiscussionRepo() *rongcloud.DiscussionRepository {
	if h.RongCloudDiscussionRepo == nil {
		return nil
	}
	return h.RongCloudDiscussionRepo
}

// ClawListChatroomDiscussions GET /api/chatrooms/{chatroomId}/discussions
func (h *Handler) ClawListChatroomDiscussions(w http.ResponseWriter, r *http.Request) {
	repo := h.clawDiscussionRepo()
	if repo == nil {
		clawJSON(w, 503, 503, "讨论服务尚未初始化", nil)
		return
	}
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	chatroomID := chi.URLParam(r, "chatroomId")
	access, err := h.clawDiscussionRoomAccess(r, chatroomID, userID)
	if err != nil || !access {
		clawJSON(w, 404, 404, "资源不存在", nil)
		return
	}
	list, err := repo.ListByChatroom(r.Context(), chatroomID)
	if err != nil {
		clawJSON(w, 500, 500, "查询失败", nil)
		return
	}
	data := make([]map[string]interface{}, 0, len(list))
	for _, d := range list {
		data = append(data, rongcloud.DiscussionSnapshotView(d, h.clawLatestArtifact(r, repo, d.DiscussionID)))
	}
	clawJSON(w, 200, 200, "", data)
}

// ClawGetDiscussion GET /api/discussions/{discussionId}
func (h *Handler) ClawGetDiscussion(w http.ResponseWriter, r *http.Request) {
	repo := h.clawDiscussionRepo()
	if repo == nil {
		clawJSON(w, 503, 503, "讨论服务尚未初始化", nil)
		return
	}
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	discussionID := chi.URLParam(r, "discussionId")
	d, err := repo.Get(r.Context(), discussionID)
	if errors.Is(err, rongcloud.ErrDiscussionNotFound) {
		clawJSON(w, 404, 404, "资源不存在", nil)
		return
	}
	if err != nil {
		clawJSON(w, 500, 500, "查询失败", nil)
		return
	}
	access, err := h.clawDiscussionRoomAccess(r, d.ChatroomID, userID)
	if err != nil || !access {
		clawJSON(w, 404, 404, "资源不存在", nil)
		return
	}
	clawJSON(w, 200, 200, "", rongcloud.DiscussionSnapshotView(d, h.clawLatestArtifact(r, repo, discussionID)))
}

// ClawGetDiscussionEvents GET /api/discussions/{discussionId}/events?afterSeq=
func (h *Handler) ClawGetDiscussionEvents(w http.ResponseWriter, r *http.Request) {
	repo := h.clawDiscussionRepo()
	if repo == nil {
		clawJSON(w, 503, 503, "讨论服务尚未初始化", nil)
		return
	}
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	discussionID := chi.URLParam(r, "discussionId")
	rawAfter := r.URL.Query().Get("afterSeq")
	if !regexp.MustCompile(`^\d+$`).MatchString(rawAfter) {
		clawJSON(w, 400, 400, "afterSeq 必须是非负整数", nil)
		return
	}
	afterSeq, err := strconv.ParseInt(rawAfter, 10, 64)
	if err != nil || afterSeq > 9_223_372_036_854_775_807 {
		clawJSON(w, 400, 400, "afterSeq 必须是非负整数", nil)
		return
	}
	d, err := repo.Get(r.Context(), discussionID)
	if errors.Is(err, rongcloud.ErrDiscussionNotFound) {
		clawJSON(w, 404, 404, "资源不存在", nil)
		return
	}
	if err != nil {
		clawJSON(w, 500, 500, "查询失败", nil)
		return
	}
	access, err := h.clawDiscussionRoomAccess(r, d.ChatroomID, userID)
	if err != nil || !access {
		clawJSON(w, 404, 404, "资源不存在", nil)
		return
	}
	events, err := repo.ListEventsAfter(r.Context(), discussionID, afterSeq)
	if err != nil {
		clawJSON(w, 500, 500, "查询失败", nil)
		return
	}
	data := make([]map[string]interface{}, 0, len(events))
	for _, e := range events {
		data = append(data, rongcloud.DiscussionEventView(e))
	}
	clawJSON(w, 200, 200, "", data)
}

// ---- 内部辅助 ------------------------------------------------------------

// clawDiscussionRoomAccess 对齐 Python _discussion_room_access：房间存在且
// 用户是启用成员（owner 也算）。
func (h *Handler) clawDiscussionRoomAccess(r *http.Request, chatroomID, userID string) (bool, error) {
	return h.RongCloudDiscussionAccess.CheckMember(r.Context(), chatroomID, userID)
}

// clawLatestArtifact 读取讨论的最新产物（无则 nil）。
func (h *Handler) clawLatestArtifact(r *http.Request, repo *rongcloud.DiscussionRepository, discussionID string) *rongcloud.ArtifactRecord {
	a, err := repo.LatestArtifact(r.Context(), discussionID)
	if err != nil {
		return nil
	}
	return &a
}
