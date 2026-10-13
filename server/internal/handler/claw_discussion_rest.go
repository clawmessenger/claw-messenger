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
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

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

// ClawListDiscussionRoundDocuments GET /api/discussions/{discussionId}/round-documents
//
// 只回元数据（不含正文）—— C 端 services/discussion.ts 的 documentMetadata 就只读
// 这些键，正文由 getRoundDocument 单独取。键名是 snake_case，别改成 camelCase。
func (h *Handler) ClawListDiscussionRoundDocuments(w http.ResponseWriter, r *http.Request) {
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
	if access, aErr := h.clawDiscussionRoomAccess(r, d.ChatroomID, userID); aErr != nil || !access {
		clawJSON(w, 404, 404, "资源不存在", nil)
		return
	}
	documents, err := repo.ListRoundDocuments(r.Context(), discussionID)
	if err != nil {
		clawJSON(w, 500, 500, "查询失败", nil)
		return
	}
	data := make([]map[string]interface{}, 0, len(documents))
	for _, doc := range documents {
		data = append(data, rongcloud.RoundDocumentView(doc, false))
	}
	clawJSON(w, 200, 200, "", data)
}

// ClawGetRoundDocument GET /api/round-documents/{documentId}
func (h *Handler) ClawGetRoundDocument(w http.ResponseWriter, r *http.Request) {
	doc, ok := h.clawRoundDocumentContext(w, r)
	if !ok {
		return
	}
	clawJSON(w, 200, 200, "", rongcloud.RoundDocumentView(doc, true))
}

// ClawDownloadRoundDocument GET /api/round-documents/{documentId}/download
//
// 直接回文件本体（不是 {code,message,data} 信封）——C 端 api.download 读的是
// Content-Disposition 里的文件名 + blob 内容。文件名与 Python 一致：
// discussion-<safeId>-round-<round>-r<revision>.md，安全化后仍是纯 basename。
func (h *Handler) ClawDownloadRoundDocument(w http.ResponseWriter, r *http.Request) {
	doc, ok := h.clawRoundDocumentContext(w, r)
	if !ok {
		return
	}
	safeID := clawSafeFilenamePart(doc.DiscussionID)
	if safeID == "" {
		safeID = "unknown"
	}
	filename := fmt.Sprintf("discussion-%s-round-%d-r%d.md", safeID, doc.Round, doc.Revision)
	writeClawDownload(w, filename, "text/markdown; charset=utf-8", []byte(doc.Content))
}

// ClawGetArtifact GET /api/artifacts/{artifactId}
//
// 产物正文只经这条路径下发（IM 的 artifact 更新事件只带元数据）：C 端
// loadArtifactIfNewer 靠它把本地版本追到事件里的 version。
func (h *Handler) ClawGetArtifact(w http.ResponseWriter, r *http.Request) {
	repo := h.clawDiscussionRepo()
	if repo == nil {
		clawJSON(w, 503, 503, "讨论服务尚未初始化", nil)
		return
	}
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	artifactID := chi.URLParam(r, "artifactId")
	artifact, err := repo.GetArtifact(r.Context(), artifactID)
	if errors.Is(err, rongcloud.ErrArtifactNotFound) {
		clawJSON(w, 404, 404, "资源不存在", nil)
		return
	}
	if err != nil {
		clawJSON(w, 500, 500, "查询失败", nil)
		return
	}
	d, err := repo.Get(r.Context(), artifact.DiscussionID)
	if err != nil {
		clawJSON(w, 404, 404, "资源不存在", nil)
		return
	}
	if access, aErr := h.clawDiscussionRoomAccess(r, d.ChatroomID, userID); aErr != nil || !access {
		clawJSON(w, 404, 404, "资源不存在", nil)
		return
	}
	clawJSON(w, 200, 200, "", rongcloud.ArtifactView(artifact))
}

// ---- 内部辅助 ------------------------------------------------------------

// clawRoundDocumentContext 是两条逐轮纪要读取路径共用的前置：鉴权 → 文档存在 →
// 该文档所属讨论的聊天室成员资格。聊天室访问失败一律 404（不区分不存在与无权，
// 与 Python _round_document_api_context 一致，避免泄露讨论 id 的存在性）。
func (h *Handler) clawRoundDocumentContext(w http.ResponseWriter, r *http.Request) (rongcloud.RoundDocumentRecord, bool) {
	repo := h.clawDiscussionRepo()
	if repo == nil {
		clawJSON(w, 503, 503, "讨论服务尚未初始化", nil)
		return rongcloud.RoundDocumentRecord{}, false
	}
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return rongcloud.RoundDocumentRecord{}, false
	}
	doc, err := repo.GetRoundDocument(r.Context(), chi.URLParam(r, "documentId"))
	if errors.Is(err, rongcloud.ErrRoundDocumentNotFound) {
		clawJSON(w, 404, 404, "资源不存在", nil)
		return rongcloud.RoundDocumentRecord{}, false
	}
	if err != nil {
		clawJSON(w, 500, 500, "查询失败", nil)
		return rongcloud.RoundDocumentRecord{}, false
	}
	d, err := repo.Get(r.Context(), doc.DiscussionID)
	if err != nil {
		clawJSON(w, 404, 404, "资源不存在", nil)
		return rongcloud.RoundDocumentRecord{}, false
	}
	if access, aErr := h.clawDiscussionRoomAccess(r, d.ChatroomID, userID); aErr != nil || !access {
		clawJSON(w, 404, 404, "资源不存在", nil)
		return rongcloud.RoundDocumentRecord{}, false
	}
	return doc, true
}

// clawSafeFilenamePart 把 id 安全化成文件名片段（对齐 Python
// re.sub(r"[^A-Za-z0-9_-]+", "_", ...)[:128].strip("_-")），保证回给客户端的
// 永远是纯 basename。
func clawSafeFilenamePart(value string) string {
	if len(value) > 128 {
		value = value[:128]
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_-")
}

// writeClawDownload 写一个附件下载响应。Content-Disposition 同时给 ASCII 安全名与
// RFC 5987 的 filename*（前端优先读后者），并显式 nosniff —— 产物内容里可能有
// HTML，不能让浏览器把它当页面执行。
func writeClawDownload(w http.ResponseWriter, filename, contentType string, body []byte) {
	ascii := clawSafeFilenamePart(filename)
	if ascii == "" {
		ascii = "download"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", ascii, url.PathEscape(filename)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

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
