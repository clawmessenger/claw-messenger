// 管理后台「群组管理」，以及 `service=group` 命令所需的本地镜像实现。
//
// 这组接口在旧 Python 后端里是 admin/routes/groups.py 的 admin_groups blueprint
// （权限 group:read / group:write），迁到 Go 之后一直是空桩：/api/admin/groups 恒
// 返回空页、/{id} 与 /{id}/members 返回 JSON 404。
//
// 数据源：旧后端读的是 im_groups / im_group_members 镜像表（融云没有「列出全部
// 群」的接口，只能按群 ID 或按用户查，所以必须本地留一份）。Go 侧此前没有任何群
// 表，本次按同样语义新建 rongcloud_group / rongcloud_group_member（迁移 588）。
//
// 群从哪来：C 端 web 的「建群」走系统命令通道 service=group，action=create，
// 由 group_command.go 处理（调融云 API 建群，再写这张镜像表）。也就是说镜像里的
// 行都源自融云，本管理端只读改本地投影 —— 与旧后端一致：旧的管理端也不碰融云，
// 群改名走 C 端的 group/update（那条路径会先刷融云）。
package handler

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
)

const (
	clawAdminGroupMaxName        = 128
	clawAdminGroupMaxDescription = 4000
)

// ---- service=group 的本地镜像 ------------------------------------------------

// ClawGroupMirror 组装 rongcloud.GroupMirror：融云 API 在集成包里，数据库在这里。
// 未启用融云集成（RongCloudNode 为 nil）时返回 nil，命令通道会回 503。
func (h *Handler) ClawGroupMirror() *rongcloud.GroupMirror {
	return &rongcloud.GroupMirror{
		AllocateID:    h.clawGenerateGroupID,
		SaveGroup:     h.clawSaveGroup,
		OwnerOf:       h.clawGroupOwner,
		UpdateMeta:    h.clawUpdateGroupMeta,
		FallbackAgent: h.clawGroupFallbackAgent,
	}
}

// clawGenerateGroupID 生成 6 位起、在用户/群组/节点三张表之间都不重复的纯数字 ID。
// 旧后端 generate_unique_id 就是在这三张表里联合查重 —— 因为融云侧的 userId 与
// groupId 共用一个命名空间下的可见性，重复会让人分不清是用户还是群。
func (h *Handler) clawGenerateGroupID(ctx context.Context) (string, error) {
	for i := 0; i < 20; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(900000))
		if err != nil {
			return "", err
		}
		id := fmt.Sprintf("%d", 100000+n.Int64())
		var exists bool
		err = h.DB.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM rongcloud_group WHERE group_id = $1)
			    OR EXISTS(SELECT 1 FROM claw_im_users   WHERE user_id  = $1)
			    OR EXISTS(SELECT 1 FROM rongcloud_node  WHERE node_id  = $1)`, id).Scan(&exists)
		if err != nil {
			return "", err
		}
		if !exists {
			return id, nil
		}
	}
	return "", fmt.Errorf("无法生成唯一群组ID")
}

// clawSaveGroup upserts the mirrored group and replaces its member list in one
// transaction, so member_count can never disagree with the rows beneath it.
func (h *Handler) clawSaveGroup(ctx context.Context, group rongcloud.GroupRecord) error {
	if h.TxStarter == nil {
		return errors.New("事务不可用")
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var maxRounds *int
	if group.MaxRounds > 0 {
		value := group.MaxRounds
		maxRounds = &value
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO rongcloud_group
		    (group_id, group_name, owner_id, portrait_uri, description, member_count, openclaw_max_rounds)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (group_id) DO UPDATE SET
		    group_name          = EXCLUDED.group_name,
		    owner_id            = EXCLUDED.owner_id,
		    portrait_uri        = EXCLUDED.portrait_uri,
		    description         = EXCLUDED.description,
		    member_count        = EXCLUDED.member_count,
		    openclaw_max_rounds = COALESCE(EXCLUDED.openclaw_max_rounds, rongcloud_group.openclaw_max_rounds),
		    updated_at          = now()`,
		group.GroupID, group.GroupName, group.OwnerID, group.PortraitURI, group.Description,
		len(group.MemberIDs), maxRounds); err != nil {
		return err
	}
	// 成员表按「本次上传的清单为准」重建：群刚建好时这是唯一来源，
	// 上传的清单本身就是去重后的结果。
	if _, err := tx.Exec(ctx, `DELETE FROM rongcloud_group_member WHERE group_id = $1`, group.GroupID); err != nil {
		return err
	}
	for _, userID := range group.MemberIDs {
		role := "member"
		if userID == group.OwnerID {
			role = "owner"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO rongcloud_group_member (group_id, user_id, role)
			VALUES ($1, $2, $3)
			ON CONFLICT (group_id, user_id) DO UPDATE SET role = EXCLUDED.role, updated_at = now()`,
			group.GroupID, userID, role); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (h *Handler) clawGroupOwner(ctx context.Context, groupID string) (string, bool, error) {
	var owner string
	err := h.DB.QueryRow(ctx, `SELECT owner_id FROM rongcloud_group WHERE group_id = $1`, groupID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return owner, true, nil
}

// clawUpdateGroupMeta applies a partial settings change; an empty string or nil
// pointer leaves that column untouched (old contract used COALESCE the same way).
func (h *Handler) clawUpdateGroupMeta(ctx context.Context, groupID, groupName, portraitURI, description string, maxRounds *int, stopped *bool) error {
	_, err := h.DB.Exec(ctx, `
		UPDATE rongcloud_group
		SET group_name          = COALESCE(NULLIF($2, ''), group_name),
		    portrait_uri        = COALESCE(NULLIF($3, ''), portrait_uri),
		    description         = COALESCE(NULLIF($4, ''), description),
		    openclaw_max_rounds = COALESCE($5, openclaw_max_rounds),
		    openclaw_stopped    = COALESCE($6, openclaw_stopped),
		    updated_at          = now()
		WHERE group_id = $1`,
		groupID, groupName, portraitURI, description, maxRounds, stopped)
	return err
}

// clawGroupFallbackAgent picks an agent node to seed a group with when the
// creator selected none, otherwise the group's messages reach only humans.
//
// The legacy backend looked for a node owned by the creator. That is not
// expressible here: rongcloud_node.owner_user_id points at a multica account
// (every row shares the one installer) and shares no id space with
// claw_im_users, so "the creator's nodes" cannot be resolved. Any
// display-visible agent node is used instead, preferring one that is online.
func (h *Handler) clawGroupFallbackAgent(ctx context.Context) (string, bool, error) {
	if h.RongCloudNode == nil {
		return "", false, nil
	}
	wsID, err := h.clawAdminDiscussionHostWorkspaceID(ctx)
	if err != nil {
		return "", false, err
	}
	var wsUUID pgtype.UUID
	if err := wsUUID.Scan(wsID); err != nil {
		return "", false, err
	}
	nodes, err := h.RongCloudNode.ListNodes(ctx, wsUUID)
	if err != nil {
		return "", false, err
	}
	fallback := ""
	fallbackOnline := ""
	for _, node := range nodes {
		if !rongcloud.IsDisplayNode(node) {
			continue
		}
		if node.RongcloudUserID == "" {
			continue
		}
		if fallback == "" {
			fallback = node.RongcloudUserID
		}
		if h.RongCloudNode.IsOnline(node.ID) {
			fallbackOnline = node.RongcloudUserID
			break
		}
	}
	if fallbackOnline != "" {
		return fallbackOnline, true, nil
	}
	if fallback != "" {
		return fallback, true, nil
	}
	return "", false, nil
}

// ---- GET /api/admin/groups ---------------------------------------------------

func (h *Handler) ClawAdminGroups(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("group:read", w, r) == nil {
		return
	}
	page, pageSize := clawAdminPagination(r)
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))

	where := ""
	args := []interface{}{}
	if keyword != "" {
		where = ` WHERE group_id ILIKE $1 OR group_name ILIKE $1`
		args = append(args, clawAdminLikePattern(keyword))
	}

	var total int
	if err := h.DB.QueryRow(r.Context(), `SELECT COUNT(*) FROM rongcloud_group`+where, args...).Scan(&total); err != nil {
		clawAdminFail(w, 503, "群组列表不可用")
		return
	}

	items := []map[string]interface{}{}
	rows, err := h.DB.Query(r.Context(), `
		SELECT group_id, group_name, owner_id, member_count, description, created_at, updated_at
		FROM rongcloud_group`+where+`
		ORDER BY created_at DESC
		LIMIT $`+fmt.Sprint(len(args)+1)+` OFFSET $`+fmt.Sprint(len(args)+2),
		append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		clawAdminFail(w, 503, "群组列表不可用")
		return
	}
	defer rows.Close()
	for rows.Next() {
		group, err := clawAdminScanGroup(rows)
		if err != nil {
			continue
		}
		items = append(items, group)
	}
	if err := rows.Err(); err != nil {
		clawAdminFail(w, 503, "群组列表不可用")
		return
	}

	clawAdminOK(w, map[string]interface{}{"items": items, "total": total, "page": page, "page_size": pageSize})
}

// ---- GET /api/admin/groups/{id} ---------------------------------------------

func (h *Handler) ClawAdminGroupDetail(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("group:read", w, r) == nil {
		return
	}
	group, err := h.clawAdminGroupByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		clawAdminFail(w, 503, "群组信息不可用")
		return
	}
	if group == nil {
		clawAdminFail(w, 404, "群组不存在")
		return
	}
	clawAdminOK(w, group)
}

// ---- PUT /api/admin/groups/{id} ---------------------------------------------

// clawAdminUpdateGroupBody 只声明真正可写的字段，且每个都是指针：旧契约要求
// 至少给一个字段，缺字段与给了空值要能区分开。
type clawAdminUpdateGroupBody struct {
	GroupName   *string `json:"group_name"`
	Description *string `json:"description"`
}

func (h *Handler) ClawAdminUpdateGroup(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("group:write", w, r)
	if ident == nil {
		return
	}
	groupID := chi.URLParam(r, "id")

	var body clawAdminUpdateGroupBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		clawAdminFail(w, 400, "JSON object is required")
		return
	}
	if body.GroupName == nil && body.Description == nil {
		clawAdminFail(w, 400, "At least one field is required")
		return
	}
	groupName := ""
	if body.GroupName != nil {
		groupName = strings.TrimSpace(*body.GroupName)
		if groupName == "" || len([]rune(groupName)) > clawAdminGroupMaxName {
			clawAdminFail(w, 400, "group_name has invalid length")
			return
		}
	}
	description := ""
	if body.Description != nil {
		description = strings.TrimSpace(*body.Description)
		if len([]rune(description)) > clawAdminGroupMaxDescription {
			clawAdminFail(w, 400, "description has invalid length")
			return
		}
	}

	changed := []string{}
	if body.GroupName != nil {
		changed = append(changed, "group_name")
	}
	if body.Description != nil {
		changed = append(changed, "description")
	}

	updated, err := h.clawAdminGroupByID(r.Context(), groupID)
	if err != nil {
		clawAdminFail(w, 503, "群组信息不可用")
		return
	}
	if updated == nil {
		clawAdminFail(w, 404, "群组不存在")
		return
	}
	if err := h.clawUpdateGroupMeta(r.Context(), groupID, groupName, "", description, nil, nil); err != nil {
		clawAdminFail(w, 503, "群组更新失败")
		return
	}
	h.clawAdminAudit(r, ident, "group_updated", "group", groupID, map[string]interface{}{
		"changed_fields": changed,
	})

	group, err := h.clawAdminGroupByID(r.Context(), groupID)
	if err != nil || group == nil {
		clawAdminFail(w, 503, "群组信息不可用")
		return
	}
	clawAdminOK(w, group)
}

// ---- DELETE /api/admin/groups/{id} ------------------------------------------

func (h *Handler) ClawAdminDeleteGroup(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("group:write", w, r)
	if ident == nil {
		return
	}
	groupID := chi.URLParam(r, "id")

	// 成员行靠外键 ON DELETE CASCADE 一起清掉；count 用来区分 404 与成功。
	tag, err := h.DB.Exec(r.Context(), `DELETE FROM rongcloud_group WHERE group_id = $1`, groupID)
	if err != nil {
		clawAdminFail(w, 503, "群组删除失败")
		return
	}
	if tag.RowsAffected() == 0 {
		clawAdminFail(w, 404, "群组不存在")
		return
	}
	h.clawAdminAudit(r, ident, "group_deleted", "group", groupID, map[string]interface{}{})
	clawAdminOK(w, map[string]interface{}{"deleted": true})
}

// ---- GET /api/admin/groups/{id}/members -------------------------------------

func (h *Handler) ClawAdminGroupMembers(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("group:read", w, r) == nil {
		return
	}
	groupID := chi.URLParam(r, "id")
	if _, found, err := h.clawGroupOwner(r.Context(), groupID); err != nil {
		clawAdminFail(w, 503, "群组成员不可用")
		return
	} else if !found {
		clawAdminFail(w, 404, "群组不存在")
		return
	}

	items, err := h.clawAdminGroupMembers(r.Context(), groupID)
	if err != nil {
		clawAdminFail(w, 503, "群组成员不可用")
		return
	}
	clawAdminOK(w, items)
}

// ---- DELETE /api/admin/groups/{id}/members/{user_id} ------------------------

func (h *Handler) ClawAdminRemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("group:write", w, r)
	if ident == nil {
		return
	}
	groupID := chi.URLParam(r, "id")
	userID := chi.URLParam(r, "user_id")

	tag, err := h.DB.Exec(r.Context(),
		`DELETE FROM rongcloud_group_member WHERE group_id = $1 AND user_id = $2`, groupID, userID)
	if err != nil {
		clawAdminFail(w, 503, "移除成员失败")
		return
	}
	if tag.RowsAffected() == 0 {
		clawAdminFail(w, 404, "成员不存在")
		return
	}
	// 成员数由成员表自己算，避免手工累加漂移（旧后端同样重算）。
	if _, err := h.DB.Exec(r.Context(), `
		UPDATE rongcloud_group
		SET member_count = (SELECT COUNT(*) FROM rongcloud_group_member WHERE group_id = $1),
		    updated_at   = now()
		WHERE group_id = $1`, groupID); err != nil {
		clawAdminFail(w, 503, "移除成员失败")
		return
	}
	h.clawAdminAudit(r, ident, "group_member_removed", "group", groupID, map[string]interface{}{
		"user_id": userID,
	})
	clawAdminOK(w, map[string]interface{}{"removed": true})
}

// ---- 行加载 ----------------------------------------------------------------

func (h *Handler) clawAdminGroupByID(ctx context.Context, groupID string) (map[string]interface{}, error) {
	row := h.DB.QueryRow(ctx, `
		SELECT group_id, group_name, owner_id, member_count, description, created_at, updated_at
		FROM rongcloud_group WHERE group_id = $1`, groupID)
	group, err := clawAdminScanGroup(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return group, nil
}

// clawAdminGroupMembers 的 username/nickname 取自 claw_im_users（人类账号）；
// 融云节点成员不是 IM 账号，两列留空，前端会显示成 "-"。
func (h *Handler) clawAdminGroupMembers(ctx context.Context, groupID string) ([]map[string]interface{}, error) {
	rows, err := h.DB.Query(ctx, `
		SELECT m.user_id, m.group_id,
		       COALESCE(u.username, ''),
		       COALESCE(NULLIF(m.nickname, ''), u.nickname, ''),
		       m.role, m.created_at
		FROM rongcloud_group_member m
		LEFT JOIN claw_im_users u ON u.user_id = m.user_id
		WHERE m.group_id = $1
		ORDER BY m.created_at DESC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]interface{}{}
	for rows.Next() {
		var (
			userID, group, username, nickname, role string
			createdAt                               time.Time
		)
		if err := rows.Scan(&userID, &group, &username, &nickname, &role, &createdAt); err != nil {
			continue
		}
		items = append(items, map[string]interface{}{
			"user_id":    userID,
			"group_id":   group,
			"username":   username,
			"nickname":   nickname,
			"role":       role,
			"created_at": createdAt.UTC().Format(time.RFC3339),
			// 前端成员表两处都读 joined_at（旧契约只有 created_at），别名一份省得它显示空。
			"joined_at": createdAt.UTC().Format(time.RFC3339),
		})
	}
	return items, rows.Err()
}

// clawAdminScanGroup reads the seven group columns shared by the list and detail
// queries. Takes a pgx.Row so both call sites can reuse it.
func clawAdminScanGroup(row pgx.Row) (map[string]interface{}, error) {
	var (
		groupID, groupName, ownerID, description string
		memberCount                              int
		createdAt, updatedAt                     time.Time
	)
	if err := row.Scan(&groupID, &groupName, &ownerID, &memberCount, &description, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"group_id":     groupID,
		"group_name":   groupName,
		"owner_id":     ownerID,
		"member_count": memberCount,
		"description":  description,
		"created_at":   createdAt.UTC().Format(time.RFC3339),
		"updated_at":   updatedAt.UTC().Format(time.RFC3339),
	}, nil
}
