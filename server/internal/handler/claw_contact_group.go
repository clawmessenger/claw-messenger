package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// 通讯录分组接口。给 clawmessenger-web / clawmessenger-uniapp 的通讯录左栏用。
//
// 设计要点（改之前先读，这几条都是被具体问题逼出来的）：
//
//  1. 「运维」分组是**视图**，不存成员。它按机器把全部 agent 节点聚合起来
//     （同机 agent 共享 machine_name），由前端从 /api/claw/nodes 现算。
//     服务端为它留一行只为存「用户改过的名字」。
//  2. 「我的 Agent」是**兜底分组**：`im_contact_group_member` 里没有行的设备
//     就落在这里。所以删掉一个自定义分组不需要「把成员搬回去」——删的瞬间
//     成员行随 CASCADE 消失，设备自动回到默认分组。
//  3. 归属是 **一台设备最多一个分组**（成员表主键 user_id+node_id）。微信的
//     「标签」允许多个分组，这里刻意不学：设备列表已经按机器分了层次，
//     再加多标签会让左栏出现同一台设备三遍。
//  4. 默认分组不可删（slot 非空），但可以改名 —— 用户要的「自定义名称」。
//  5. 前端在接口 404/500 时必须退回到「只有两个默认分组 + 本地缓存」的降级
//     路径。所以这里的响应格式一旦改动要两端同步，否则旧包会白屏。

const (
	contactGroupSlotMaintenance = "maintenance"
	contactGroupSlotMyAgent     = "my_agent"

	// 分组名上限与迁移里的 CHECK 必须一致，否则前端能存进空名/超长名而在
	// 落库时才炸（那时用户已经看到「保存成功」）。
	contactGroupNameMaxRunes = 24
)

// 默认分组懒创建：第一次访问分组接口时补两行。
//
// ON CONFLICT 的目标带部分索引的谓词（slot IS NOT NULL），与
// uq_im_contact_group_slot 完全一致 —— 少了 WHERE 谓词 PostgreSQL 无法推断
// 到那个索引，会直接报 "no unique or exclusion constraint matching"。
const contactGroupEnsureDefaultsSQL = `
INSERT INTO im_contact_group (user_id, name, slot, sort_order)
VALUES ($1, '运维', 'maintenance', 0), ($1, '我的 Agent', 'my_agent', 1)
ON CONFLICT (user_id, slot) WHERE slot IS NOT NULL DO NOTHING`

// 一条 SQL 取回全部分组 + 成员：分组数量是个位数，没必要 N+1。
// ORDER BY m.created_at 让成员顺序稳定（前端按设备列表顺序渲染时不会跳）。
const contactGroupListSQL = `
SELECT g.id::text,
       g.name,
       COALESCE(g.slot, ''),
       g.sort_order,
       COALESCE(
         array_agg(m.node_id ORDER BY m.created_at) FILTER (WHERE m.node_id IS NOT NULL),
         ARRAY[]::text[]
       ) AS members
FROM im_contact_group g
LEFT JOIN im_contact_group_member m ON m.group_id = g.id
WHERE g.user_id = $1
GROUP BY g.id, g.name, g.slot, g.sort_order, g.created_at
ORDER BY g.sort_order, g.created_at`

const contactGroupCreateSQL = `
INSERT INTO im_contact_group (user_id, name, slot, sort_order)
SELECT $1, $2, NULL,
       COALESCE((SELECT MAX(sort_order) FROM im_contact_group WHERE user_id = $1), 0) + 1
RETURNING id::text, name, sort_order`

const contactGroupRenameSQL = `
UPDATE im_contact_group
SET name = $3, updated_at = now()
WHERE id = $1 AND user_id = $2
RETURNING id::text, name, COALESCE(slot, ''), sort_order`

// 删除只允许自定义分组：slot IS NULL 是硬条件，默认分组哪怕 id 猜对了也删不掉。
const contactGroupDeleteSQL = `
DELETE FROM im_contact_group
WHERE id = $1 AND user_id = $2 AND slot IS NULL
RETURNING id::text`

// 把设备挪进分组。SELECT 源就是归属校验：分组不属于当前用户时没有行可插，
// RETURNING 也不会出行 —— 调用方据此回 404，而不是先 SELECT 再 INSERT
// （那中间有窗口，且多一次往返）。
const contactGroupAssignSQL = `
INSERT INTO im_contact_group_member (user_id, node_id, group_id)
SELECT $1, $2, g.id
FROM im_contact_group g
WHERE g.id = $3 AND g.user_id = $1
ON CONFLICT (user_id, node_id) DO UPDATE SET group_id = EXCLUDED.group_id
RETURNING node_id`

const contactGroupUnassignSQL = `
DELETE FROM im_contact_group_member
WHERE user_id = $1 AND node_id = $2
RETURNING node_id`

// contactGroupDTO 是给前端的形状。slot 用 omitempty：自定义分组不该在
// JSON 里出现空字符串字段，前端 `if (group.slot)` 才不会把 '' 当成有效值。
type contactGroupDTO struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Slot      string   `json:"slot,omitempty"`
	SortOrder int      `json:"sortOrder"`
	Members   []string `json:"members"`
}

// normalizeContactGroupName 统一按「去首尾空白 + 按字符数（不是字节数）计长」
// 校验。中文名一个字符三字节，用 len() 会让「我的智能体小分队」这种 8 字的
// 名字被判超限。
func normalizeContactGroupName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", false
	}
	if utf8.RuneCountInString(name) > contactGroupNameMaxRunes {
		return "", false
	}
	return name, true
}

// parseContactGroupID 把路径参数转成 uuid。非法 uuid 走 400 而不是让
// PostgreSQL 报 invalid input syntax（那会是 500，日志里看着像服务端故障）。
func parseContactGroupID(raw string) (string, bool) {
	parsed, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	return parsed.String(), true
}

// ensureContactGroupDefaults 幂等地补上两个默认分组。任何写接口前都要调一次：
// 用户第一次进通讯录可能直接改「运维」的名字，那时两行还不存在。
func (h *Handler) ensureContactGroupDefaults(r *http.Request, userID string) bool {
	if _, err := h.DB.Exec(r.Context(), contactGroupEnsureDefaultsSQL, userID); err != nil {
		slog.Warn("claw: ensure contact group defaults failed", "error", err)
		return false
	}
	return true
}

// ClawListContactGroups GET /api/claw/contact-groups
func (h *Handler) ClawListContactGroups(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	if h.DB == nil {
		clawJSON(w, 503, 503, "数据库未配置", nil)
		return
	}
	if !h.ensureContactGroupDefaults(r, userID) {
		clawJSON(w, 500, 500, "获取分组失败", nil)
		return
	}
	rows, err := h.DB.Query(r.Context(), contactGroupListSQL, userID)
	if err != nil {
		slog.Warn("claw: list contact groups failed", "error", err)
		clawJSON(w, 500, 500, "获取分组失败", nil)
		return
	}
	defer rows.Close()

	groups := make([]contactGroupDTO, 0, 4)
	for rows.Next() {
		var (
			group   contactGroupDTO
			members []string
		)
		if err := rows.Scan(&group.ID, &group.Name, &group.Slot, &group.SortOrder, &members); err != nil {
			slog.Warn("claw: scan contact group failed", "error", err)
			clawJSON(w, 500, 500, "获取分组失败", nil)
			return
		}
		// nil 会被 json 编码成 null，前端 .map 会炸。给空数组。
		if members == nil {
			members = []string{}
		}
		group.Members = members
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("claw: iterate contact groups failed", "error", err)
		clawJSON(w, 500, 500, "获取分组失败", nil)
		return
	}
	clawJSON(w, 200, 200, "ok", map[string]interface{}{"groups": groups})
}

// ClawCreateContactGroup POST /api/claw/contact-groups  {name}
func (h *Handler) ClawCreateContactGroup(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	if h.DB == nil {
		clawJSON(w, 503, 503, "数据库未配置", nil)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		clawJSON(w, 400, 400, "请求格式有误", nil)
		return
	}
	name, valid := normalizeContactGroupName(req.Name)
	if !valid {
		clawJSON(w, 400, 400, "分组名不能为空，且不超过 24 个字", nil)
		return
	}
	if !h.ensureContactGroupDefaults(r, userID) {
		clawJSON(w, 500, 500, "创建分组失败", nil)
		return
	}
	group := contactGroupDTO{Members: []string{}}
	err := h.DB.QueryRow(r.Context(), contactGroupCreateSQL, userID, name).
		Scan(&group.ID, &group.Name, &group.SortOrder)
	if err != nil {
		if isUniqueViolation(err) {
			clawJSON(w, 409, 409, "分组名已存在", nil)
			return
		}
		slog.Warn("claw: create contact group failed", "error", err)
		clawJSON(w, 500, 500, "创建分组失败", nil)
		return
	}
	clawJSON(w, 200, 200, "ok", map[string]interface{}{"group": group})
}

// ClawUpdateContactGroup PUT /api/claw/contact-groups/{id}  {name}
//
// 只能改名。默认分组也能改 —— 那正是「运维分组支持自定义名称」那条需求。
func (h *Handler) ClawUpdateContactGroup(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	if h.DB == nil {
		clawJSON(w, 503, 503, "数据库未配置", nil)
		return
	}
	groupID, valid := parseContactGroupID(chi.URLParam(r, "id"))
	if !valid {
		clawJSON(w, 400, 400, "分组 ID 无效", nil)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		clawJSON(w, 400, 400, "请求格式有误", nil)
		return
	}
	name, valid := normalizeContactGroupName(req.Name)
	if !valid {
		clawJSON(w, 400, 400, "分组名不能为空，且不超过 24 个字", nil)
		return
	}
	var group contactGroupDTO
	group.Members = []string{}
	err := h.DB.QueryRow(r.Context(), contactGroupRenameSQL, groupID, userID, name).
		Scan(&group.ID, &group.Name, &group.Slot, &group.SortOrder)
	if err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			clawJSON(w, 404, 404, "分组不存在", nil)
		case isUniqueViolation(err):
			clawJSON(w, 409, 409, "分组名已存在", nil)
		default:
			slog.Warn("claw: rename contact group failed", "error", err)
			clawJSON(w, 500, 500, "重命名失败", nil)
		}
		return
	}
	clawJSON(w, 200, 200, "ok", map[string]interface{}{"group": group})
}

// ClawDeleteContactGroup DELETE /api/claw/contact-groups/{id}
//
// 默认分组删不掉（SQL 里 slot IS NULL 硬拦），这里额外区分 404 与 400，
// 让前端能给出「默认分组不能删除」这种准确提示，而不是笼统的「删除失败」。
func (h *Handler) ClawDeleteContactGroup(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	if h.DB == nil {
		clawJSON(w, 503, 503, "数据库未配置", nil)
		return
	}
	groupID, valid := parseContactGroupID(chi.URLParam(r, "id"))
	if !valid {
		clawJSON(w, 400, 400, "分组 ID 无效", nil)
		return
	}
	var slot sql.NullString
	err := h.DB.QueryRow(r.Context(),
		`SELECT slot FROM im_contact_group WHERE id = $1 AND user_id = $2`,
		groupID, userID).Scan(&slot)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			clawJSON(w, 404, 404, "分组不存在", nil)
			return
		}
		slog.Warn("claw: lookup contact group failed", "error", err)
		clawJSON(w, 500, 500, "删除分组失败", nil)
		return
	}
	if slot.Valid && slot.String != "" {
		clawJSON(w, 400, 400, "默认分组不能删除，可以改名", nil)
		return
	}
	var deletedID string
	if err := h.DB.QueryRow(r.Context(), contactGroupDeleteSQL, groupID, userID).Scan(&deletedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 查到了但删不掉：并发下被别的请求删了。按已删除成功处理（幂等）。
			clawJSON(w, 200, 200, "ok", map[string]interface{}{"id": groupID})
			return
		}
		slog.Warn("claw: delete contact group failed", "error", err)
		clawJSON(w, 500, 500, "删除分组失败", nil)
		return
	}
	clawJSON(w, 200, 200, "ok", map[string]interface{}{"id": deletedID})
}

// ClawAssignContactGroup PUT /api/claw/contact-groups/assignment  {nodeId, groupId?}
//
// groupId 为空 = 移出所属分组，设备回到默认「我的 Agent」。
// 幂等：重复移动同一分组、移出一个本来就没分组的设备，都返回成功。
func (h *Handler) ClawAssignContactGroup(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	if h.DB == nil {
		clawJSON(w, 503, 503, "数据库未配置", nil)
		return
	}
	var req struct {
		NodeID  string `json:"nodeId"`
		GroupID string `json:"groupId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		clawJSON(w, 400, 400, "请求格式有误", nil)
		return
	}
	nodeID := strings.TrimSpace(req.NodeID)
	if nodeID == "" {
		clawJSON(w, 400, 400, "缺少设备 ID", nil)
		return
	}
	groupIDRaw := strings.TrimSpace(req.GroupID)
	if groupIDRaw == "" {
		err := h.DB.QueryRow(r.Context(), contactGroupUnassignSQL, userID, nodeID).Scan(&nodeID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("claw: unassign contact group failed", "error", err)
			clawJSON(w, 500, 500, "移出分组失败", nil)
			return
		}
		clawJSON(w, 200, 200, "ok", map[string]interface{}{"nodeId": nodeID, "groupId": ""})
		return
	}
	groupID, valid := parseContactGroupID(groupIDRaw)
	if !valid {
		clawJSON(w, 400, 400, "分组 ID 无效", nil)
		return
	}
	var assignedNode string
	err := h.DB.QueryRow(r.Context(), contactGroupAssignSQL, userID, nodeID, groupID).Scan(&assignedNode)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 分组不存在，或存在但属于别人 —— 两种情况都当「查无此组」，
			// 不泄露「这个 id 存在但不归你」。
			clawJSON(w, 404, 404, "分组不存在", nil)
			return
		}
		slog.Warn("claw: assign contact group failed", "error", err)
		clawJSON(w, 500, 500, "移入分组失败", nil)
		return
	}
	clawJSON(w, 200, 200, "ok", map[string]interface{}{"nodeId": assignedNode, "groupId": groupID})
}
