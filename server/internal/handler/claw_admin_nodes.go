// Package handler - 管理后台「设备管理」。
//
// 这一片在 Go 侧一直是空桩：GET /api/admin/nodes 返回空页、/{id} 与 /{id}/status
// 直接 404，所以管理端页面永远列不出设备，其上的启停/归属/编辑/删除按钮也点不到。
// 这里把它们换成真实实现。
//
// 数据源与判据全部复用既有集成层，避免和 C 端设备列表漂移：
//   - 可见性：rongcloud.IsDisplayNode（隐藏每台机器的内部节点与内置 ops 运维节点）
//   - 在线态：NodeService.IsOnline（服务端心跳 + 90s TTL），不是库里那个会长期停在
//     迁移时旧值的 deploy_status
//   - 昵称：rongcloud_user.name（好友列表/IM 会话渲染的就是它，改名必须同步融云）
//   - 归属：NodeService.NodeClaimants（反查融云好友边，见 node_claim.go）
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// clawAdminDescriptionKeyPrefix namespaces the admin-authored node description
// inside rongcloud_system_config. rongcloud_node has no description column and
// adding one would force a sqlc regeneration (every generated query expands
// `SELECT *` into positional scans), so the description rides the generic
// key/value store that already carries a node_id column for exactly this kind
// of per-node payload.
const clawAdminDescriptionKeyPrefix = "node_meta:"

// ---- 行加载 ----------------------------------------------------------------

// clawAdminNodeRecord is the subset of rongcloud_node the admin surface needs.
type clawAdminNodeRecord struct {
	ID              pgtype.UUID
	NodeID          string
	RongcloudUserID string
	AIType          string
	MachineID       string
	Capabilities    []byte
	DeployStatus    string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// dbNode projects the record onto the shape rongcloud.IsDisplayNode expects.
func (r clawAdminNodeRecord) dbNode() db.RongcloudNode {
	var node db.RongcloudNode
	node.ID = r.ID
	node.MachineID = r.MachineID
	node.RongcloudUserID = r.RongcloudUserID
	node.AiType = pgtype.Text{String: r.AIType, Valid: r.AIType != ""}
	node.Capabilities = r.Capabilities
	return node
}

const clawAdminNodeColumns = `
	id, node_id, rongcloud_user_id, COALESCE(ai_type, ''), COALESCE(machine_id, ''),
	COALESCE(capabilities, '[]'::jsonb), deploy_status, created_at, updated_at`

func scanClawAdminNode(row pgx.Row) (clawAdminNodeRecord, error) {
	var rec clawAdminNodeRecord
	err := row.Scan(&rec.ID, &rec.NodeID, &rec.RongcloudUserID, &rec.AIType, &rec.MachineID,
		&rec.Capabilities, &rec.DeployStatus, &rec.CreatedAt, &rec.UpdatedAt)
	return rec, err
}

func (h *Handler) clawAdminListNodeRecords(ctx context.Context) ([]clawAdminNodeRecord, error) {
	rows, err := h.DB.Query(ctx, `SELECT `+clawAdminNodeColumns+` FROM rongcloud_node ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]clawAdminNodeRecord, 0, 16)
	for rows.Next() {
		rec, err := scanClawAdminNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// clawAdminNodeByPathID resolves the {id} path segment. The admin list publishes
// the text node_id (node_xxx), which is what the C 端 contract and the frontend
// both key on; the row's uuid is accepted too so a stale bookmark still works.
func (h *Handler) clawAdminNodeByPathID(ctx context.Context, rawID string) (clawAdminNodeRecord, bool, error) {
	rawID = strings.TrimSpace(rawID)
	if rawID == "" {
		return clawAdminNodeRecord{}, false, nil
	}
	row := h.DB.QueryRow(ctx, `SELECT `+clawAdminNodeColumns+`
		FROM rongcloud_node WHERE node_id = $1 OR id::text = $1 LIMIT 1`, rawID)
	rec, err := scanClawAdminNode(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return clawAdminNodeRecord{}, false, nil
	}
	if err != nil {
		return clawAdminNodeRecord{}, false, err
	}
	return rec, true, nil
}

// clawAdminNodeUserNames maps rongcloud_user_id -> display name + the reserved
// flag, for the whole table. Both are joined in Go rather than SQL so the list
// can reuse rongcloud.IsDisplayNode instead of re-deriving machineIdentityKey
// (a sha256 branch) in SQL, where it would silently drift from isMachineNode.
func (h *Handler) clawAdminNodeUserNames(ctx context.Context) (names map[string]string, reserved map[string]bool, err error) {
	rows, err := h.DB.Query(ctx, `
		SELECT rongcloud_user_id, COALESCE(name, ''), is_system_reserved
		FROM rongcloud_user`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	names = make(map[string]string)
	reserved = make(map[string]bool)
	for rows.Next() {
		var rcID, name string
		var isReserved bool
		if err := rows.Scan(&rcID, &name, &isReserved); err != nil {
			return nil, nil, err
		}
		names[rcID] = name
		reserved[rcID] = isReserved
	}
	return names, reserved, rows.Err()
}

// clawAdminNodeDescriptions reads the admin-authored descriptions written by
// clawAdminUpdateNode (see clawAdminDescriptionKeyPrefix).
func (h *Handler) clawAdminNodeDescriptions(ctx context.Context) (map[string]string, error) {
	wsID, err := h.clawAdminDiscussionHostWorkspaceID(ctx)
	if err != nil {
		return map[string]string{}, nil
	}
	// starts_with 而不是 LIKE：clawAdminDescriptionKeyPrefix 里的下划线在 LIKE 里是
	// 单字符通配符，会让 'node_meta:' 顺带匹配 'nodeXmeta:' 这类键。
	rows, err := h.DB.Query(ctx, `
		SELECT config_key, COALESCE(config->>'description', '')
		FROM rongcloud_system_config
		WHERE workspace_id = $1::uuid AND starts_with(config_key, $2)`, wsID, clawAdminDescriptionKeyPrefix)
	if err != nil {
		return map[string]string{}, nil
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var key, description string
		if err := rows.Scan(&key, &description); err != nil {
			return out, nil
		}
		out[strings.TrimPrefix(key, clawAdminDescriptionKeyPrefix)] = description
	}
	return out, nil
}

// clawAdminDiscussionHostNodeKey returns the node currently designated as the
// platform discussion host, as a text node_id. The admin device list marks it
// reserved so the UI refuses to re-home it — its own tooltip already says
// "系统保留节点请在讨论主持设置中管理".
func (h *Handler) clawAdminReservedHostNodeKeys(ctx context.Context) map[string]bool {
	out := make(map[string]bool)
	var nodeUUID string
	err := h.DB.QueryRow(ctx, `
		SELECT COALESCE(node_id::text, '') FROM rongcloud_system_config
		WHERE config_key = $1 ORDER BY updated_at DESC LIMIT 1`,
		clawAdminDiscussionHostKey).Scan(&nodeUUID)
	if err != nil || nodeUUID == "" {
		return out
	}
	if rec, found, err := h.clawAdminNodeByPathID(ctx, nodeUUID); err == nil && found {
		out[rec.NodeID] = true
	}
	return out
}

// clawAdminNodeClaimants resolves the IM owner of every supplied node. Failures
// degrade to "unclaimed" rather than failing the page: the 归属 column is
// informational and RongCloud may be unreachable.
func (h *Handler) clawAdminNodeClaimants(ctx context.Context, rcUserIDs []string) map[string]string {
	if h.RongCloudNode == nil || len(rcUserIDs) == 0 {
		return map[string]string{}
	}
	accounts, err := h.clawAdminIMAccountIDs(ctx)
	if err != nil || len(accounts) == 0 {
		return map[string]string{}
	}
	all, err := h.RongCloudNode.NodeClaimants(ctx, accounts)
	if err != nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(rcUserIDs))
	for _, rcID := range rcUserIDs {
		if claimant, ok := all[rcID]; ok {
			out[rcID] = claimant
		}
	}
	return out
}

// clawAdminIMAccountIDs lists the claw/IM accounts whose friend edges can claim
// a node, with their display names.
func (h *Handler) clawAdminIMAccounts(ctx context.Context) (map[string]string, error) {
	rows, err := h.DB.Query(ctx, `
		SELECT user_id, COALESCE(nickname, username, '') FROM claw_im_users WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var userID, name string
		if err := rows.Scan(&userID, &name); err != nil {
			return nil, err
		}
		out[userID] = name
	}
	return out, rows.Err()
}

func (h *Handler) clawAdminIMAccountIDs(ctx context.Context) ([]string, error) {
	accounts, err := h.clawAdminIMAccounts(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(accounts))
	for id := range accounts {
		ids = append(ids, id)
	}
	return ids, nil
}

// ---- 线上语义 —— 状态与投影 -------------------------------------------------

type clawAdminNodeRow struct {
	NodeID           string `json:"node_id"`
	Name             string `json:"name"`
	UserID           string `json:"user_id,omitempty"`
	Username         string `json:"username,omitempty"`
	NodeType         string `json:"node_type"`
	DeployType       string `json:"deploy_type,omitempty"`
	DeployStatus     string `json:"deploy_status"`
	Description      string `json:"description,omitempty"`
	CreatedAt        string `json:"created_at,omitempty"`
	UpdatedAt        string `json:"updated_at,omitempty"`
	IsSystemReserved bool   `json:"is_system_reserved"`
}

// clawAdminNodeStatus reports the derived liveness of a node. `detail` explains
// where the value comes from so an operator is not left wondering why it
// disagrees with the stored deploy_status column.
func (h *Handler) clawAdminNodeStatus(rec clawAdminNodeRecord) (string, string) {
	if h.RongCloudNode == nil {
		return "offline", "心跳表不可用"
	}
	if h.RongCloudNode.IsOnline(rec.ID) {
		return "online", "心跳正常（90 秒内）"
	}
	return "offline", "90 秒内无心跳"
}

func (h *Handler) clawAdminNodeRowFor(rec clawAdminNodeRecord, name, description, claimant, claimantName string, reserved bool) clawAdminNodeRow {
	status, _ := h.clawAdminNodeStatus(rec)
	row := clawAdminNodeRow{
		NodeID:           rec.NodeID,
		Name:             name,
		NodeType:         rec.AIType,
		DeployStatus:     status,
		Description:      description,
		CreatedAt:        rec.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:        rec.UpdatedAt.UTC().Format(time.RFC3339),
		IsSystemReserved: reserved,
	}
	if claimant != "" {
		row.UserID = claimant
		row.Username = claimantName
	}
	if row.Name == "" {
		row.Name = rec.AIType
	}
	return row
}

// clawAdminNodeMatches applies the list filters. keyword matches what the page's
// placeholder promises (节点ID、名称或描述); node_type is the ai_type.
func clawAdminNodeMatches(rec clawAdminNodeRecord, name, description, keyword, nodeType string) bool {
	if nodeType != "" && rec.AIType != nodeType {
		return false
	}
	if keyword == "" {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{
		rec.NodeID, rec.RongcloudUserID, rec.MachineID, rec.AIType, name, description,
	}, "\n"))
	return strings.Contains(haystack, strings.ToLower(keyword))
}

// ---- GET /api/admin/nodes -------------------------------------------------

func (h *Handler) ClawAdminListNodes(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("node:read", w, r) == nil {
		return
	}
	ctx := r.Context()
	records, err := h.clawAdminListNodeRecords(ctx)
	if err != nil {
		clawAdminFail(w, 500, "获取设备列表失败")
		return
	}
	names, reserved, err := h.clawAdminNodeUserNames(ctx)
	if err != nil {
		clawAdminFail(w, 500, "获取设备列表失败")
		return
	}
	descriptions, _ := h.clawAdminNodeDescriptions(ctx)
	hostNodes := h.clawAdminReservedHostNodeKeys(ctx)

	page, pageSize := clawAdminPagination(r)
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	nodeType := strings.TrimSpace(r.URL.Query().Get("node_type"))

	// 先按可见性过滤再分页：可见性判据是 Go 侧的 isHiddenNode（含 sha256 分支的
	// machineIdentityKey），在 SQL 里重写一遍必然与它漂移；workspace 的节点数由
	// 设备数决定，量级很小。LegacyNodeRecords 也是同样的取全量再过滤的写法。
	visible := make([]clawAdminNodeRecord, 0, len(records))
	for _, rec := range records {
		if !rongcloud.IsDisplayNode(rec.dbNode()) {
			continue
		}
		if !clawAdminNodeMatches(rec, names[rec.RongcloudUserID], descriptions[rec.NodeID], keyword, nodeType) {
			continue
		}
		visible = append(visible, rec)
	}

	total := len(visible)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	pageRecords := visible[start:end]

	rcIDs := make([]string, 0, len(pageRecords))
	for _, rec := range pageRecords {
		rcIDs = append(rcIDs, rec.RongcloudUserID)
	}
	claimants := h.clawAdminNodeClaimants(ctx, rcIDs)
	accountNames, _ := h.clawAdminIMAccounts(ctx)

	items := make([]clawAdminNodeRow, 0, len(pageRecords))
	for _, rec := range pageRecords {
		claimant := claimants[rec.RongcloudUserID]
		items = append(items, h.clawAdminNodeRowFor(
			rec,
			names[rec.RongcloudUserID],
			descriptions[rec.NodeID],
			claimant,
			accountNames[claimant],
			reserved[rec.RongcloudUserID] || hostNodes[rec.NodeID],
		))
	}
	clawAdminOK(w, map[string]interface{}{
		"items": items, "total": total, "page": page, "page_size": pageSize,
	})
}

// ---- GET /api/admin/nodes/status/all --------------------------------------

func (h *Handler) ClawAdminAllNodeStatuses(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("node:read", w, r) == nil {
		return
	}
	records, err := h.clawAdminListNodeRecords(r.Context())
	if err != nil {
		clawAdminFail(w, 500, "获取设备状态失败")
		return
	}
	out := make(map[string]map[string]string, len(records))
	for _, rec := range records {
		status, detail := h.clawAdminNodeStatus(rec)
		out[rec.NodeID] = map[string]string{"status": status, "detail": detail}
	}
	clawAdminOK(w, out)
}

// ---- GET /api/admin/nodes/{id} 与 /{id}/status -----------------------------

func (h *Handler) ClawAdminGetNode(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("node:read", w, r) == nil {
		return
	}
	ctx := r.Context()
	rec, found, err := h.clawAdminNodeByPathID(ctx, chi.URLParam(r, "id"))
	if err != nil {
		clawAdminFail(w, 500, "获取设备失败")
		return
	}
	if !found {
		clawAdminFail(w, 404, "设备不存在")
		return
	}
	names, reserved, _ := h.clawAdminNodeUserNames(ctx)
	descriptions, _ := h.clawAdminNodeDescriptions(ctx)
	hostNodes := h.clawAdminReservedHostNodeKeys(ctx)
	claimants := h.clawAdminNodeClaimants(ctx, []string{rec.RongcloudUserID})
	accountNames, _ := h.clawAdminIMAccounts(ctx)
	claimant := claimants[rec.RongcloudUserID]

	row := h.clawAdminNodeRowFor(
		rec, names[rec.RongcloudUserID], descriptions[rec.NodeID], claimant,
		accountNames[claimant], reserved[rec.RongcloudUserID] || hostNodes[rec.NodeID],
	)
	clawAdminOK(w, row)
}

func (h *Handler) ClawAdminGetNodeStatus(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("node:read", w, r) == nil {
		return
	}
	rec, found, err := h.clawAdminNodeByPathID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		clawAdminFail(w, 500, "获取设备状态失败")
		return
	}
	if !found {
		clawAdminFail(w, 404, "设备不存在")
		return
	}
	status, detail := h.clawAdminNodeStatus(rec)
	clawAdminOK(w, map[string]string{"status": status, "detail": detail})
}

// ---- PUT /api/admin/nodes/{id} -------------------------------------------

// clawAdminUpdateNodeBody 只声明真正可写的字段。deploy_status 不是可写字段：
// 线上在线态由心跳表（90s TTL）推导，库里那一列会长期停在迁移时的旧值，写它只会
// 造成"提示成功但状态没变"的假象。
type clawAdminUpdateNodeBody struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

func (h *Handler) ClawAdminUpdateNode(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("node:write", w, r)
	if ident == nil {
		return
	}
	ctx := r.Context()
	rec, found, err := h.clawAdminNodeByPathID(ctx, chi.URLParam(r, "id"))
	if err != nil {
		clawAdminFail(w, 500, "设备查找失败")
		return
	}
	if !found {
		clawAdminFail(w, 404, "设备不存在")
		return
	}

	var body clawAdminUpdateNodeBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		clawAdminFail(w, 400, "请求参数错误")
		return
	}

	// 节点昵称渲染在好友列表与 IM 会话标题里，权威值是 rongcloud_user.name；
	// 改完必须推融云（refreshUser），否则前端看到的仍是旧名字。
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			clawAdminFail(w, 400, "设备名称不能为空")
			return
		}
		if len([]rune(name)) > 128 {
			clawAdminFail(w, 400, "设备名称过长")
			return
		}
		if h.RongCloudNode != nil {
			user, err := h.RongCloudNode.GetUserByRongCloudID(ctx, rec.RongcloudUserID)
			if err != nil {
				clawAdminFail(w, 503, "设备账号不存在")
				return
			}
			if err := h.RongCloudNode.UpdateUserProfile(ctx, user.ID, name, ""); err != nil {
				clawAdminFail(w, 502, "同步融云昵称失败")
				return
			}
		}
	}

	// 描述走 rongcloud_system_config 的 node_meta:<node_id>，见文件头注释。
	if body.Description != nil {
		description := strings.TrimSpace(*body.Description)
		if len([]rune(description)) > 500 {
			clawAdminFail(w, 400, "描述过长")
			return
		}
		wsID, err := h.clawAdminDiscussionHostWorkspaceID(ctx)
		if err != nil {
			clawAdminFail(w, 500, "缺少可用工作区")
			return
		}
		payload, err := json.Marshal(map[string]string{"description": description})
		if err != nil {
			clawAdminFail(w, 400, "请求参数错误")
			return
		}
		if _, err := h.DB.Exec(ctx, `
			INSERT INTO rongcloud_system_config (workspace_id, config_key, config, config_version)
			VALUES ($1::uuid, $2, $3, 1)
			ON CONFLICT (workspace_id, config_key)
			DO UPDATE SET config = $3,
			              config_version = rongcloud_system_config.config_version + 1,
			              updated_at = now()`,
			wsID, clawAdminDescriptionKeyPrefix+rec.NodeID, payload); err != nil {
			clawAdminFail(w, 500, "保存描述失败")
			return
		}
	}

	h.clawAdminAudit(r, ident, "node_updated", "node", rec.NodeID, map[string]interface{}{
		"name":        body.Name != nil,
		"description": body.Description != nil,
	})

	// 回读，保证返回值就是页面接下来会渲染的那一份。
	names, reserved, _ := h.clawAdminNodeUserNames(ctx)
	descriptions, _ := h.clawAdminNodeDescriptions(ctx)
	hostNodes := h.clawAdminReservedHostNodeKeys(ctx)
	claimants := h.clawAdminNodeClaimants(ctx, []string{rec.RongcloudUserID})
	accountNames, _ := h.clawAdminIMAccounts(ctx)
	claimant := claimants[rec.RongcloudUserID]
	clawAdminOK(w, h.clawAdminNodeRowFor(
		rec, names[rec.RongcloudUserID], descriptions[rec.NodeID], claimant,
		accountNames[claimant], reserved[rec.RongcloudUserID] || hostNodes[rec.NodeID],
	))
}

// ---- DELETE /api/admin/nodes/{id} ----------------------------------------

// ClawAdminDeleteNode 解除绑定：删节点行 + 它的模型目录 + 融云好友边。复用
// UnbindNode，与 C 端「取消绑定」同一套语义（融云账号本身保留，重新绑定会复用，
// 免得同一个 agent 出现两个账号）。
func (h *Handler) ClawAdminDeleteNode(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("node:write", w, r)
	if ident == nil {
		return
	}
	ctx := r.Context()
	rec, found, err := h.clawAdminNodeByPathID(ctx, chi.URLParam(r, "id"))
	if err != nil {
		clawAdminFail(w, 500, "设备查找失败")
		return
	}
	if !found {
		clawAdminFail(w, 404, "设备不存在")
		return
	}
	if h.RongCloudNode == nil {
		clawAdminFail(w, 503, "RongCloud 集成未配置")
		return
	}

	claimants := h.clawAdminNodeClaimants(ctx, []string{rec.RongcloudUserID})
	if err := h.RongCloudNode.UnbindNode(ctx, rec.dbNode(), claimants[rec.RongcloudUserID]); err != nil {
		clawAdminFail(w, 500, "删除设备失败")
		return
	}
	h.RongCloudNode.InvalidateNodeClaimants()

	h.clawAdminAudit(r, ident, "node_deleted", "node", rec.NodeID, map[string]interface{}{
		"rongcloud_id": rec.RongcloudUserID,
		"ai_type":      rec.AIType,
	})
	clawAdminOK(w, map[string]interface{}{"node_id": rec.NodeID})
}

// ---- POST /api/admin/nodes/{id}/start & /stop ------------------------------
//
// 启停是异步语义，页面按钮的文案就是「启动指令已发送」。能真正拉起/杀掉 agent 的
// 是设备上的 supervisor 主进程，它按固定间隔轮询 /api/claw/device/control 并把自己的
// 子进程集合收敛到期望状态（见 claw_device_control.go 的注释：为什么不能走 IM）。
//
// 这里刻意不写 rongcloud_node.deploy_status：IsOnline 读的是服务端心跳表，不是这一
// 列，写它只会让页面标签变一下而设备毫无变化 —— 正是这次要消除的那种「假成功」。

func (h *Handler) ClawAdminStartNode(w http.ResponseWriter, r *http.Request) {
	h.clawAdminSetNodeDesiredState(w, r, rongcloud.NodeDesiredRunning)
}

func (h *Handler) ClawAdminStopNode(w http.ResponseWriter, r *http.Request) {
	h.clawAdminSetNodeDesiredState(w, r, rongcloud.NodeDesiredStopped)
}

func (h *Handler) clawAdminSetNodeDesiredState(w http.ResponseWriter, r *http.Request, state string) {
	ident := h.requireClawAdmin("node:write", w, r)
	if ident == nil {
		return
	}
	if h.DB == nil {
		clawAdminFail(w, 503, "数据库不可用")
		return
	}
	ctx := r.Context()
	rec, found, err := h.clawAdminNodeByPathID(ctx, chi.URLParam(r, "id"))
	if err != nil {
		clawAdminFail(w, 500, "设备查找失败")
		return
	}
	if !found {
		clawAdminFail(w, 404, "设备不存在")
		return
	}
	// 机器主节点与内置 ops 节点没有可启停的 agent 进程（IsDisplayNode 与设备列表
	// 用的是同一判据），对它们下发指令只会留下一条无人消费的记录。
	if !rongcloud.IsDisplayNode(rec.dbNode()) {
		clawAdminFail(w, 409, "该节点不是可启停的设备")
		return
	}
	version, err := h.clawAdminUpsertNodeControl(ctx, rec.ID, state, ident.AdminID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			clawAdminFail(w, 404, "设备不存在")
			return
		}
		clawAdminFail(w, 500, "下发指令失败")
		return
	}
	action := "node_started"
	if state == rongcloud.NodeDesiredStopped {
		action = "node_stopped"
	}
	h.clawAdminAudit(r, ident, action, "node", rec.NodeID, map[string]interface{}{
		"desired_state": state,
		"version":       version,
	})
	clawAdminOK(w, map[string]interface{}{
		"node_id":       rec.NodeID,
		"desired_state": state,
		"version":       version,
	})
}

// clawAdminUpsertNodeControl records operator intent and returns the row's new
// version. The version only advances when the state actually changes, so
// pressing 停止 twice is not two distinct commands and cannot re-kill a node the
// operator just started again.
func (h *Handler) clawAdminUpsertNodeControl(ctx context.Context, nodeUUID pgtype.UUID, state, actor string) (int64, error) {
	var version int64
	err := h.DB.QueryRow(ctx, `
		INSERT INTO rongcloud_node_control (node_id, workspace_id, desired_state, desired_version, changed_by)
		SELECT n.id, n.workspace_id, $2, 1, $3
		  FROM rongcloud_node n
		 WHERE n.id = $1
		ON CONFLICT (node_id) DO UPDATE
		   SET desired_state = EXCLUDED.desired_state,
		       desired_version = rongcloud_node_control.desired_version
		                         + CASE WHEN rongcloud_node_control.desired_state IS DISTINCT FROM EXCLUDED.desired_state
		                                THEN 1 ELSE 0 END,
		       changed_by = EXCLUDED.changed_by,
		       updated_at = now()
		RETURNING desired_version`,
		nodeUUID, state, pgtype.Text{String: actor, Valid: actor != ""}).Scan(&version)
	return version, err
}

// ---- PUT /api/admin/nodes/{id}/owner ---------------------------------------

type clawAdminTransferOwnerBody struct {
	UserID         *string `json:"user_id"`
	ExpectedUserID *string `json:"expected_user_id"`
}

// ClawAdminTransferNodeOwner 变更设备归属。归属的真相是融云好友边 —— 旧站把它当
// 「谁的好友列表里有这台设备」，融云也没有「谁加了我」的查询，所以只能按已知的 IM
// 账号逐个反查（node_claim.go）。变更即重建边：从原归属摘掉，挂到新归属。
//
// 不写 rongcloud_node.owner_user_id：实测这一列 12 行全部指向同一个 multica 安装
// 账号（resolveWorkspaceAttribution 取 workspace 的第一个 manager），与 claw_im_users
// 的账号体系 0 匹配且没有映射字段，写它等于写入一个语义错误的归属。
func (h *Handler) ClawAdminTransferNodeOwner(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("node:write", w, r)
	if ident == nil {
		return
	}
	if h.RongCloudNode == nil {
		clawAdminFail(w, 503, "RongCloud 集成未配置")
		return
	}
	ctx := r.Context()
	rec, found, err := h.clawAdminNodeByPathID(ctx, chi.URLParam(r, "id"))
	if err != nil {
		clawAdminFail(w, 500, "设备查找失败")
		return
	}
	if !found {
		clawAdminFail(w, 404, "设备不存在")
		return
	}
	var body clawAdminTransferOwnerBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		clawAdminFail(w, 400, "请求参数错误")
		return
	}
	if body.UserID == nil || strings.TrimSpace(*body.UserID) == "" {
		clawAdminFail(w, 400, "缺少目标用户")
		return
	}
	target := strings.TrimSpace(*body.UserID)
	if target == rec.RongcloudUserID {
		clawAdminFail(w, 400, "目标用户不能是设备自身")
		return
	}
	accounts, err := h.clawAdminIMAccounts(ctx)
	if err != nil {
		clawAdminFail(w, 500, "用户查询失败")
		return
	}
	if _, ok := accounts[target]; !ok {
		clawAdminFail(w, 400, "目标用户不存在或未启用")
		return
	}

	names, reserved, _ := h.clawAdminNodeUserNames(ctx)
	hostNodes := h.clawAdminReservedHostNodeKeys(ctx)
	isReserved := reserved[rec.RongcloudUserID] || hostNodes[rec.NodeID]
	if isReserved {
		clawAdminFail(w, 409, "系统保留节点不支持变更归属")
		return
	}

	// 先丢掉服务端缓存，保证 current 反映的是当前好友边而不是 15 秒内的旧快照。
	h.RongCloudNode.InvalidateNodeClaimants()
	current := h.clawAdminNodeClaimants(ctx, []string{rec.RongcloudUserID})[rec.RongcloudUserID]
	if body.ExpectedUserID != nil {
		if expected := strings.TrimSpace(*body.ExpectedUserID); expected != current {
			clawAdminFail(w, 409, "设备归属已被其他操作变更，请刷新后重试")
			return
		}
	}

	if current != target {
		if current != "" {
			if err := h.RongCloudNode.RevokeFriendship(ctx, current, rec.RongcloudUserID); err != nil {
				clawAdminFail(w, 502, "解除原归属失败")
				return
			}
		}
		if _, err := h.RongCloudNode.GrantFriendship(ctx, target, rec.RongcloudUserID, rec.NodeID); err != nil {
			// 尽力把原归属补回去，免得设备在两处都不可见。
			if current != "" {
				_, _ = h.RongCloudNode.GrantFriendship(ctx, current, rec.RongcloudUserID, rec.NodeID)
			}
			clawAdminFail(w, 502, "设置新归属失败")
			return
		}
		h.RongCloudNode.InvalidateNodeClaimants()
		h.clawAdminAudit(r, ident, "node_owner_changed", "node", rec.NodeID, map[string]interface{}{
			"from": current,
			"to":   target,
		})
	}

	claimants := h.clawAdminNodeClaimants(ctx, []string{rec.RongcloudUserID})
	accountNames, _ := h.clawAdminIMAccounts(ctx)
	descriptions, _ := h.clawAdminNodeDescriptions(ctx)
	claimant := claimants[rec.RongcloudUserID]
	clawAdminOK(w, h.clawAdminNodeRowFor(
		rec, names[rec.RongcloudUserID], descriptions[rec.NodeID], claimant,
		accountNames[claimant], isReserved,
	))
}
