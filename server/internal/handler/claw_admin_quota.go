// 管理后台「用户配额」API（讨论组引导式创建与配额，R-09）。
//
//	GET /api/admin/users/{id}/quota   读用户的覆盖值 + 生效值 + 当前用量
//	PUT /api/admin/users/{id}/quota   保存覆盖值（保存即生效，下一次 Check 读到新值）
//
// 配额语义：-1 不限 / 0 禁止 / N 上限 / null 跟随默认（清除覆盖）。
// 权限：user:read / user:write（与用户详情页同一粒度）。
package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/quota"
)

// clawAdminQuotaView 是 GET/PUT 共用的响应 data 形状（admin-web QuotaSection 渲染）。
type clawAdminQuotaView struct {
	// Override 是 claw_im_user_quota 行；没有行（从未覆盖）时三类全 null。
	Override quota.OverrideLimits `json:"override"`
	// Effective 是覆盖>套餐>内置默认解析后的生效上限（-1 不限）。
	Effective quota.Limits `json:"effective"`
	// Usage 是当前用量（status='active' 房间计数，讨论组不双计）。
	Usage quota.Usage `json:"usage"`
	// UpdatedBy / UpdatedAt 是最小审计（谁最后改的）；无覆盖行为空。
	UpdatedBy string `json:"updated_by"`
	UpdatedAt string `json:"updated_at"`
}

// clawAdminQuotaPayload 解析 PUT 请求体：指针为 nil 表示「跟随默认」（清除该类的覆盖）。
type clawAdminQuotaPayload struct {
	DiscussionLimit *int `json:"discussion_limit"`
	GroupLimit      *int `json:"group_limit"`
	ChatroomLimit   *int `json:"chatroom_limit"`
}

// validate 校验取值域：-1 不限 / 0 禁止 / 正整数上限；其余非法。
func (p clawAdminQuotaPayload) validate() bool {
	for _, v := range []*int{p.DiscussionLimit, p.GroupLimit, p.ChatroomLimit} {
		if v != nil && *v < -1 {
			return false
		}
	}
	return true
}

// clawUserExists 404 守门：配额行挂在 claw_im_users 上，目标用户不存在时直接 404，
// 避免给不存在的 user_id 落一条孤儿覆盖行。
func (h *Handler) clawUserExists(w http.ResponseWriter, r *http.Request, userID string) bool {
	var exists bool
	if err := h.DB.QueryRow(r.Context(),
		"SELECT EXISTS(SELECT 1 FROM claw_im_users WHERE user_id=$1)", userID).Scan(&exists); err != nil {
		clawAdminFail(w, 500, "database error")
		return false
	}
	if !exists {
		clawAdminFail(w, 404, "user not found")
		return false
	}
	return true
}

// buildClawAdminQuotaView 组装 GET/PUT 共用的响应：覆盖 + 生效 + 用量 + 最小审计。
func (h *Handler) buildClawAdminQuotaView(r *http.Request, userID string) (clawAdminQuotaView, error) {
	ctx := r.Context()
	var view clawAdminQuotaView
	// 覆盖行（含审计列）：没有行时 Override 保持全 null。
	if row, err := h.Queries.GetClawIMUserQuota(ctx, userID); err == nil {
		if row.DiscussionLimit.Valid {
			v := int(row.DiscussionLimit.Int32)
			view.Override.DiscussionLimit = &v
		}
		if row.GroupLimit.Valid {
			v := int(row.GroupLimit.Int32)
			view.Override.GroupLimit = &v
		}
		if row.ChatroomLimit.Valid {
			v := int(row.ChatroomLimit.Int32)
			view.Override.ChatroomLimit = &v
		}
		view.UpdatedBy = row.UpdatedBy
		if row.UpdatedAt.Valid {
			view.UpdatedAt = row.UpdatedAt.Time.UTC().Format(time.RFC3339)
		}
	}
	effective, err := h.ClawQuota.Resolve(ctx, userID)
	if err != nil {
		return view, err
	}
	view.Effective = effective
	usage, err := h.ClawQuota.Usage(ctx, userID)
	if err != nil {
		return view, err
	}
	view.Usage = usage
	return view, nil
}

// ClawAdminGetUserQuota GET /api/admin/users/{id}/quota
func (h *Handler) ClawAdminGetUserQuota(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("user:read", w, r) == nil {
		return
	}
	if h.ClawQuota == nil || h.Queries == nil {
		clawAdminFail(w, 503, "quota service is unavailable")
		return
	}
	userID := chi.URLParam(r, "id")
	if !h.clawUserExists(w, r, userID) {
		return
	}
	view, err := h.buildClawAdminQuotaView(r, userID)
	if err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}
	clawAdminOK(w, view)
}

// ClawAdminPutUserQuota PUT /api/admin/users/{id}/quota
//
// 保存即生效：UpsertOverride 落库后，下一次 Check（三入口共用同一 Service 实例）
// 立刻读到新值；updated_by 记当前管理员账号。
func (h *Handler) ClawAdminPutUserQuota(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("user:write", w, r)
	if ident == nil {
		return
	}
	if h.ClawQuota == nil || h.Queries == nil {
		clawAdminFail(w, 503, "quota service is unavailable")
		return
	}
	userID := chi.URLParam(r, "id")
	if !h.clawUserExists(w, r, userID) {
		return
	}
	var body clawAdminQuotaPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		clawAdminFail(w, 400, "invalid json body")
		return
	}
	if !body.validate() {
		clawAdminFail(w, 400, "limit must be -1 (unlimited), 0 (forbidden) or a positive integer")
		return
	}
	if err := h.ClawQuota.UpsertOverride(r.Context(), userID, quota.OverrideLimits{
		DiscussionLimit: body.DiscussionLimit,
		GroupLimit:      body.GroupLimit,
		ChatroomLimit:   body.ChatroomLimit,
	}, ident.Username); err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}
	view, err := h.buildClawAdminQuotaView(r, userID)
	if err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}
	clawAdminOK(w, view)
}
