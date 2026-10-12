// Package handler - 管理后台「系统 → 讨论主持 AI」。
//
// 这组接口在旧 Python 后端里是 admin/routes/system.py 的 discussion_host_admin
// blueprint（url_prefix="/api/system/discussion-host"，另见 discussion/system_host.py），
// 迁到 Go 之后一直没被移植，导致管理端页面直接吃 404。这里按 Python 的对外契约补回来，
// 只是**路径收进 /api/admin/ 命名空间**，与其余管理接口保持一致，并由前端同步改路径。
//
// 存储沿用 Go 运行时自己的那一份：rongcloud_system_config 里 config_key='system_host'
// 的那一行（Python 用的是 im_system_config 的 discussion_system_host；线上后端是 Go，
// 只有 system_host 有意义，而且它给指定节点单开了 node_id(uuid) 列）。
//
// ⚠️ 注意：Go 的讨论运行时目前**不读**这条配置 —— 主持人取自每条聊天室自己的
// rongcloud_chatroom.host_node_id。所以这里保存的是"平台级默认主持"的声明，要等运行时
// 接入后才会真正生效。同时 eligible-nodes 会按 REQUIRED_HOST_CAPABILITIES 过滤，
// 而设备端目前上报的能力里还没有这几个值，因此列表可能为空 —— 这是数据现实，不是接口故障。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
)

const (
	clawAdminDiscussionHostKey = "system_host"

	clawAdminDiscussionHostMaxNodeID   = 128
	clawAdminDiscussionHostMaxName     = 128
	clawAdminDiscussionHostMaxAvatar   = 2048
	clawAdminDiscussionHostMaxPrompt   = 32000
	clawAdminDiscussionHostMinTimeout  = 10
	clawAdminDiscussionHostMaxTimeout  = 600
	clawAdminDiscussionHostTimeoutInit = 60
)

// clawAdminRequiredHostCapabilities mirrors REQUIRED_HOST_CAPABILITIES in the
// legacy discussion/system_host.py: a node advertising anything less than all
// four may not be designated as the platform-wide discussion host. The admin
// web applies the same gate to the dropdown, so keep the two in sync.
var clawAdminRequiredHostCapabilities = []string{
	"discussion_host",
	"discussion_roundtable",
	"discussion_model_routing",
	"discussion_role_recommendation",
}

// clawAdminDiscussionHostConfig is the wire shape GET and PUT both return; it
// matches DiscussionHostConfig in clawmessenger-admin-web/src/types.
type clawAdminDiscussionHostConfig struct {
	Enabled                  bool   `json:"enabled"`
	NodeID                   string `json:"node_id"`
	AIUserID                 string `json:"ai_user_id"`
	DisplayName              string `json:"display_name"`
	Avatar                   string `json:"avatar"`
	RoleRecommendationPrompt string `json:"role_recommendation_prompt"`
	DiscussionHostPrompt     string `json:"discussion_host_prompt"`
	TimeoutSeconds           int    `json:"timeout_seconds"`
	ConfigVersion            int    `json:"config_version"`
}

// clawAdminDiscussionHostBody is the PUT payload. Every field is a pointer so a
// missing key is distinguishable from a zero value — the legacy contract
// rejects bodies that omit any of them.
type clawAdminDiscussionHostBody struct {
	ExpectedConfigVersion    *int    `json:"expected_config_version"`
	Enabled                  *bool   `json:"enabled"`
	NodeID                   *string `json:"node_id"`
	DisplayName              *string `json:"display_name"`
	Avatar                   *string `json:"avatar"`
	RoleRecommendationPrompt *string `json:"role_recommendation_prompt"`
	DiscussionHostPrompt     *string `json:"discussion_host_prompt"`
	TimeoutSeconds           *int    `json:"timeout_seconds"`
}

// clawAdminDiscussionHostWorkspaceID resolves which workspace the config row
// belongs to: the one already holding a row for this key, else whatever
// workspace the other rongcloud_system_config rows use, else the oldest
// workspace. Mirrors the fallback ClawAdminSystemConfigPut already relies on.
func (h *Handler) clawAdminDiscussionHostWorkspaceID(ctx context.Context) (string, error) {
	var wsID string
	if err := h.DB.QueryRow(ctx, `
		SELECT workspace_id::text FROM rongcloud_system_config
		WHERE config_key = $1 ORDER BY updated_at DESC LIMIT 1`,
		clawAdminDiscussionHostKey).Scan(&wsID); err == nil {
		return wsID, nil
	}
	if err := h.DB.QueryRow(ctx, `
		SELECT workspace_id::text FROM rongcloud_system_config
		ORDER BY updated_at DESC LIMIT 1`).Scan(&wsID); err == nil {
		return wsID, nil
	}
	err := h.DB.QueryRow(ctx,
		`SELECT id::text FROM workspace ORDER BY created_at LIMIT 1`).Scan(&wsID)
	return wsID, err
}

// clawAdminDiscussionHostLoad reads the stored config. found=false means the
// key has never been written, which the frontend renders as "尚未配置".
func (h *Handler) clawAdminDiscussionHostLoad(ctx context.Context) (clawAdminDiscussionHostConfig, bool, error) {
	var (
		raw     []byte
		version int
		nodeID  string
	)
	err := h.DB.QueryRow(ctx, `
		SELECT COALESCE(config, '{}'::jsonb), config_version, COALESCE(node_id::text, '')
		FROM rongcloud_system_config
		WHERE config_key = $1
		ORDER BY updated_at DESC
		LIMIT 1`, clawAdminDiscussionHostKey).Scan(&raw, &version, &nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return clawAdminDiscussionHostConfig{}, false, nil
	}
	if err != nil {
		return clawAdminDiscussionHostConfig{}, false, err
	}

	cfg := clawAdminDiscussionHostConfig{
		NodeID:         nodeID,
		ConfigVersion:  version,
		TimeoutSeconds: clawAdminDiscussionHostTimeoutInit,
	}
	// config 列是 jsonb：本接口写对象；历史行可能被包成 {"value": "..."}，
	// 那种情况只保留 node_id / 版本号，其余回落默认值。
	if len(raw) > 0 {
		var stored map[string]interface{}
		if err := json.Unmarshal(raw, &stored); err == nil {
			applyClawAdminDiscussionHostStored(stored, &cfg)
		}
	}
	if cfg.NodeID != "" {
		if aiUserID, _, _, found, err := h.clawAdminDiscussionHostNodeByUUID(ctx, cfg.NodeID); err == nil && found {
			cfg.AIUserID = aiUserID
		}
	}
	return cfg, true, nil
}

// ClawDiscussionHostPersona hands the operator-written 「人设提示词」 to the
// discussion coordinator; it is wired in as rongcloud.HostPersonaLoader.
//
// Before this existed the field was stored, echoed back to the admin page, and
// then read by nobody — writing it had no effect on the host's behaviour. It is
// resolved through clawAdminDiscussionHostLoad so the same workspace fallback
// the admin API uses applies here too. Any failure degrades to "no persona"
// rather than blocking the discussion.
func (h *Handler) ClawDiscussionHostPersona(ctx context.Context) string {
	cfg, found, err := h.clawAdminDiscussionHostLoad(ctx)
	if err != nil || !found {
		return ""
	}
	return cfg.DiscussionHostPrompt
}

// applyClawAdminDiscussionHostStored copies the recognised fields out of the
// stored jsonb object, ignoring anything of the wrong type so a hand-edited row
// degrades to defaults instead of failing the whole read.
func applyClawAdminDiscussionHostStored(stored map[string]interface{}, cfg *clawAdminDiscussionHostConfig) {
	if v, ok := stored["enabled"].(bool); ok {
		cfg.Enabled = v
	}
	if v, ok := stored["display_name"].(string); ok {
		cfg.DisplayName = v
	}
	if v, ok := stored["avatar"].(string); ok {
		cfg.Avatar = v
	}
	if v, ok := stored["role_recommendation_prompt"].(string); ok {
		cfg.RoleRecommendationPrompt = v
	}
	if v, ok := stored["discussion_host_prompt"].(string); ok {
		cfg.DiscussionHostPrompt = v
	}
	if v, ok := stored["timeout_seconds"].(float64); ok && v > 0 {
		cfg.TimeoutSeconds = int(v)
	}
	if v, ok := stored["node_id"].(string); ok && v != "" && cfg.NodeID == "" {
		cfg.NodeID = v
	}
}

// clawAdminDiscussionHostNodeByUUID looks a node up by its primary key (the id
// the runtime stores in rongcloud_system_config.node_id, which is a uuid).
// Returns the node's rongcloud user id, the capabilities it advertises, and
// whether some user currently owns it.
func (h *Handler) clawAdminDiscussionHostNodeByUUID(ctx context.Context, nodeUUID string) (aiUserID string, capabilities []string, userBound bool, found bool, err error) {
	var caps []byte
	err = h.DB.QueryRow(ctx, `
		SELECT n.rongcloud_user_id,
		       COALESCE(n.capabilities, '[]'::jsonb),
		       (n.owner_user_id IS NOT NULL)
		FROM rongcloud_node n
		WHERE n.id::text = $1`, nodeUUID).Scan(&aiUserID, &caps, &userBound)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, false, false, nil
	}
	if err != nil {
		return "", nil, false, false, err
	}
	return aiUserID, clawAdminParseCapabilities(caps), userBound, true, nil
}

// clawAdminParseCapabilities normalises the jsonb capabilities column. The
// column is documented as a JSON array, but a bare string is accepted the same
// way the web client tolerates legacy comma-separated values.
func clawAdminParseCapabilities(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return clawAdminDedupeCapabilities(list)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return clawAdminDedupeCapabilities(strings.FieldsFunc(text, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n'
		}))
	}
	return nil
}

func clawAdminDedupeCapabilities(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

// clawAdminHasHostCapabilities reports whether the node advertises every
// capability the platform host requires.
func clawAdminHasHostCapabilities(capabilities []string) bool {
	if len(capabilities) == 0 {
		return false
	}
	have := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		have[capability] = struct{}{}
	}
	for _, required := range clawAdminRequiredHostCapabilities {
		if _, ok := have[required]; !ok {
			return false
		}
	}
	return true
}

func clawAdminMissingHostCapabilities(capabilities []string) []string {
	have := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		have[capability] = struct{}{}
	}
	missing := make([]string, 0, len(clawAdminRequiredHostCapabilities))
	for _, required := range clawAdminRequiredHostCapabilities {
		if _, ok := have[required]; !ok {
			missing = append(missing, required)
		}
	}
	return missing
}

// ---- GET /api/admin/system/discussion-host ----

func (h *Handler) ClawAdminDiscussionHostGet(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("system:read", w, r) == nil {
		return
	}
	cfg, found, err := h.clawAdminDiscussionHostLoad(r.Context())
	if err != nil {
		clawAdminFail(w, 503, "discussion host config unavailable")
		return
	}
	// 从未配置过时返回空对象：前端的 getDiscussionHost 以 node_id 是否存在
	// 判定"是否已配置"，返回 {} 才能落到"尚未配置"分支而不是报错。
	if !found {
		clawAdminOK(w, map[string]interface{}{})
		return
	}
	clawAdminOK(w, cfg)
}

// ---- GET /api/admin/system/discussion-host/eligible-nodes ----
//
// The candidate list is built from the same node service the device list uses,
// so the visibility contract (machine infrastructure nodes and the ops
// maintenance agent stay internal) and the online judgement (server heartbeat,
// not the stale deploy_status column) cannot drift from the rest of the平台.
func (h *Handler) ClawAdminDiscussionHostEligibleNodes(w http.ResponseWriter, r *http.Request) {
	if h.requireClawAdmin("system:read", w, r) == nil {
		return
	}
	if h.RongCloudNode == nil {
		clawAdminOK(w, map[string]interface{}{"nodes": []interface{}{}})
		return
	}
	wsID, err := h.clawAdminDiscussionHostWorkspaceID(r.Context())
	if err != nil {
		// 工作区都取不到时返回空列表，让下拉框空着而不是整页报错。
		clawAdminOK(w, map[string]interface{}{"nodes": []interface{}{}})
		return
	}
	var wsUUID pgtype.UUID
	if err := wsUUID.Scan(wsID); err != nil {
		clawAdminOK(w, map[string]interface{}{"nodes": []interface{}{}})
		return
	}

	nodes, err := h.RongCloudNode.ListNodes(r.Context(), wsUUID)
	if err != nil {
		clawAdminFail(w, 503, "node list unavailable")
		return
	}

	out := []interface{}{}
	for _, node := range nodes {
		if !rongcloud.IsDisplayNode(node) {
			continue
		}
		capabilities := clawAdminParseCapabilities(node.Capabilities)
		// 与旧后端一致：不具备全部主持能力的节点不进候选集。
		if !clawAdminHasHostCapabilities(capabilities) {
			continue
		}
		name := node.AiType.String
		if u, err := h.RongCloudNode.GetUserByRongCloudID(r.Context(), node.RongcloudUserID); err == nil && u.Name.Valid && u.Name.String != "" {
			name = u.Name.String
		}
		status := "offline"
		if h.RongCloudNode.IsOnline(node.ID) {
			status = "online"
		}
		out = append(out, map[string]interface{}{
			"node_id":       node.ID.String(),
			"ai_user_id":    node.RongcloudUserID,
			"name":          name,
			"deploy_status": status,
			// 旧后端的 is_user_bound 用来提醒"指定主持会从普通用户名下抢走设备"。
			// Go 侧的主持配置是 rongcloud_system_config 里独立的一行，指定主持
			// 不改动 rongcloud_node.owner_user_id，不存在抢占，因此恒为 false ——
			// 否则前端会一直显示一条并不成立的"强制解绑"警告。
			"is_user_bound": false,
			"capabilities":  capabilities,
		})
	}
	clawAdminOK(w, map[string]interface{}{"nodes": out})
}

// ---- PUT /api/admin/system/discussion-host ----

func (h *Handler) ClawAdminDiscussionHostPut(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("system:write", w, r)
	if ident == nil {
		return
	}

	var body clawAdminDiscussionHostBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body); err != nil {
		clawAdminFail(w, 400, "JSON object is required")
		return
	}
	if body.ExpectedConfigVersion == nil || *body.ExpectedConfigVersion < 0 {
		clawAdminFail(w, 400, "expected_config_version must be a non-negative integer")
		return
	}
	if body.Enabled == nil {
		clawAdminFail(w, 400, "enabled is required")
		return
	}
	if body.TimeoutSeconds == nil || *body.TimeoutSeconds < clawAdminDiscussionHostMinTimeout ||
		*body.TimeoutSeconds > clawAdminDiscussionHostMaxTimeout {
		clawAdminFail(w, 400, "timeout_seconds must be between 10 and 600")
		return
	}
	nodeID, ok := clawAdminRequiredField(body.NodeID, "node_id", clawAdminDiscussionHostMaxNodeID)
	if !ok {
		clawAdminFail(w, 400, "node_id is required")
		return
	}
	displayName, ok := clawAdminRequiredField(body.DisplayName, "display_name", clawAdminDiscussionHostMaxName)
	if !ok {
		clawAdminFail(w, 400, "display_name is required")
		return
	}
	avatar, ok := clawAdminRequiredField(body.Avatar, "avatar", clawAdminDiscussionHostMaxAvatar)
	if !ok {
		clawAdminFail(w, 400, "avatar is required")
		return
	}
	rolePrompt, ok := clawAdminRequiredField(body.RoleRecommendationPrompt, "role_recommendation_prompt", clawAdminDiscussionHostMaxPrompt)
	if !ok {
		clawAdminFail(w, 400, "role_recommendation_prompt is required")
		return
	}
	hostPrompt, ok := clawAdminRequiredField(body.DiscussionHostPrompt, "discussion_host_prompt", clawAdminDiscussionHostMaxPrompt)
	if !ok {
		clawAdminFail(w, 400, "discussion_host_prompt is required")
		return
	}

	// 节点必须真实存在，且自报能力覆盖主持所需的全部能力 —— 旧后端把这一条
	// 放在保存时校验，前端下拉框也用同一套门槛。
	aiUserID, capabilities, _, found, err := h.clawAdminDiscussionHostNodeByUUID(r.Context(), nodeID)
	if err != nil {
		clawAdminFail(w, 503, "node lookup unavailable")
		return
	}
	if !found {
		clawAdminFail(w, 400, "node not found")
		return
	}
	if missing := clawAdminMissingHostCapabilities(capabilities); len(missing) > 0 {
		clawAdminFail(w, 400, "missing required capabilities: "+strings.Join(missing, ", "))
		return
	}

	wsID, err := h.clawAdminDiscussionHostWorkspaceID(r.Context())
	if err != nil {
		clawAdminFail(w, 500, "no workspace available")
		return
	}

	// 乐观锁：行已存在就必须带上正确的版本号，否则说明别人先改了。
	current, exists, err := h.clawAdminDiscussionHostLoad(r.Context())
	if err != nil {
		clawAdminFail(w, 503, "discussion host config unavailable")
		return
	}
	if exists {
		if current.ConfigVersion != *body.ExpectedConfigVersion {
			clawAdminFail(w, 409, "Configuration version conflict")
			return
		}
	} else if *body.ExpectedConfigVersion != 0 {
		clawAdminFail(w, 409, "Configuration version conflict")
		return
	}

	configJSON, err := json.Marshal(map[string]interface{}{
		"enabled":                    *body.Enabled,
		"node_id":                    nodeID,
		"ai_user_id":                 aiUserID,
		"display_name":               displayName,
		"avatar":                     avatar,
		"role_recommendation_prompt": rolePrompt,
		"discussion_host_prompt":     hostPrompt,
		"timeout_seconds":            *body.TimeoutSeconds,
	})
	if err != nil {
		clawAdminFail(w, 400, "invalid payload")
		return
	}

	// node_id 单列出来（uuid），运行时 UpdateRongCloudSystemHost 就是按它写；
	// jsonb 里再存一份完整对象，供管理端来回读。
	var version int
	err = h.DB.QueryRow(r.Context(), `
		INSERT INTO rongcloud_system_config (workspace_id, config_key, node_id, config, config_version)
		VALUES ($1, $2, $3, $4, 1)
		ON CONFLICT (workspace_id, config_key)
		DO UPDATE SET node_id = $3,
		              config = $4,
		              config_version = rongcloud_system_config.config_version + 1,
		              updated_at = now()
		RETURNING config_version`,
		wsID, clawAdminDiscussionHostKey, nodeID, configJSON).Scan(&version)
	if err != nil {
		clawAdminFail(w, 500, "database error")
		return
	}

	h.clawAdminAudit(r, ident, "admin_system_discussion_host_update", "system_config",
		clawAdminDiscussionHostKey, map[string]interface{}{
			"node_id":          nodeID,
			"config_version":   version,
			"expected_version": *body.ExpectedConfigVersion,
			"enabled":          *body.Enabled,
		})

	clawAdminOK(w, clawAdminDiscussionHostConfig{
		Enabled:                  *body.Enabled,
		NodeID:                   nodeID,
		AIUserID:                 aiUserID,
		DisplayName:              displayName,
		Avatar:                   avatar,
		RoleRecommendationPrompt: rolePrompt,
		DiscussionHostPrompt:     hostPrompt,
		TimeoutSeconds:           *body.TimeoutSeconds,
		ConfigVersion:            version,
	})
}

// clawAdminRequiredField trims and bounds a required string field.
func clawAdminRequiredField(value *string, _ string, maximum int) (string, bool) {
	if value == nil {
		return "", false
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" || len([]rune(trimmed)) > maximum {
		return "", false
	}
	return trimmed, true
}

// ---- POST /api/admin/system/discussion-host/test-recommendation ----
//
// 把「测试话题」发给已配置的主持节点，请它返回一组讨论角色，供页面预览。
// 这是一次真实的往返：服务端经融云 command 通道下发 recommend_roles，设备端
// CLI 调起本机 agent 产出角色 JSON 后回包（见 node_request_broker.go）。
//
// 旧后端的错误码（DISCUSSION_HOST_PUBLIC_ERRORS）原样保留，前端已有对应文案；
// 未配置/被停用/节点离线属于 400 + 具名错误码，服务本身不可用才是 503。
const (
	clawAdminRecommendMaxTopic = 4000
	clawAdminRecommendMaxGoal  = 8000
)

func (h *Handler) ClawAdminDiscussionHostTestRecommendation(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("system:write", w, r)
	if ident == nil {
		return
	}

	var body struct {
		Topic    *string `json:"topic"`
		Goal     *string `json:"goal"`
		MaxRoles *int    `json:"max_roles"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		clawAdminFail(w, 400, "JSON object is required")
		return
	}
	topic, ok := clawAdminRequiredField(body.Topic, "topic", clawAdminRecommendMaxTopic)
	if !ok {
		clawAdminFail(w, 400, "topic is required")
		return
	}
	goal := ""
	if body.Goal != nil {
		goal = strings.TrimSpace(*body.Goal)
		if len([]rune(goal)) > clawAdminRecommendMaxGoal {
			clawAdminFail(w, 400, "goal has invalid length")
			return
		}
	}
	maxRoles := 3
	if body.MaxRoles != nil {
		maxRoles = *body.MaxRoles
	}
	if maxRoles < 1 || maxRoles > rongcloud.RecommendRolesMaxRoles {
		clawAdminFail(w, 400, "max_roles must be between 1 and 8")
		return
	}

	cfg, found, err := h.clawAdminDiscussionHostLoad(r.Context())
	if err != nil {
		clawAdminFail(w, 503, "discussion host config unavailable")
		return
	}
	if !found || cfg.NodeID == "" {
		h.clawAdminRecommendFail(w, r, ident, rongcloud.NodeRequestCodeHostNotConfigured, maxRoles)
		return
	}
	if !cfg.Enabled {
		h.clawAdminRecommendFail(w, r, ident, rongcloud.NodeRequestCodeHostDisabled, maxRoles)
		return
	}
	if cfg.AIUserID == "" {
		// 保存时已校验节点存在；读回来却没有融云 id，说明节点被删了。
		h.clawAdminRecommendFail(w, r, ident, rongcloud.NodeRequestCodeHostNotConfigured, maxRoles)
		return
	}
	if h.RongCloudNodeRequests == nil {
		clawAdminFail(w, 503, "Role recommendation service is unavailable")
		return
	}

	host := rongcloud.NodeRequestHost{
		AIUserID:             cfg.AIUserID,
		DisplayName:          cfg.DisplayName,
		Avatar:               cfg.Avatar,
		RecommendationPrompt: cfg.RoleRecommendationPrompt,
		TimeoutSeconds:       cfg.TimeoutSeconds,
		ConfigVersion:        cfg.ConfigVersion,
	}

	// 等待期间不要被服务端自己的读超时打断：设备端可能要跑完一次完整的
	// agent 调用。上限仍由 host.TimeoutSeconds 决定（broker 内计时）。
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(cfg.TimeoutSeconds+5)*time.Second)
	defer cancel()

	payload, code := h.RongCloudNodeRequests.RecommendRoles(ctx, host, topic, goal, maxRoles)
	if code != "" {
		h.clawAdminRecommendFail(w, r, ident, code, maxRoles)
		return
	}

	h.clawAdminAudit(r, ident, "test_discussion_host_recommendation", "system_config",
		clawAdminDiscussionHostKey, map[string]interface{}{
			"max_roles":         maxRoles,
			"recommendation_id": payload["recommendation_id"],
			"result_role_count": len(payload["roles"].([]map[string]interface{})),
		})
	clawAdminOK(w, payload)
}

// clawAdminRecommendFail renders a public role-recommendation error. The codes
// the legacy backend returned as `400 {code,message}` keep that shape so the
// admin page's existing error copy still applies.
func (h *Handler) clawAdminRecommendFail(w http.ResponseWriter, r *http.Request, ident *clawAdminIdentity, code string, maxRoles int) {
	h.clawAdminAudit(r, ident, "test_discussion_host_recommendation", "system_config",
		clawAdminDiscussionHostKey, map[string]interface{}{
			"max_roles":  maxRoles,
			"error_code": code,
		})
	clawAdminFail(w, 400, code)
}

// ClawDiscussionSystemHostConfig 供圆桌运行时（P4）读取管理端配置的 system
// host 展示信息。复用 clawAdminDiscussionHostLoad 的解析（含 workspace 回落），
// 翻译成 rongcloud 包的轻量形状。
func (h *Handler) ClawDiscussionSystemHostConfig(ctx context.Context) (rongcloud.DiscussionSystemHostConfig, bool, error) {
	cfg, found, err := h.clawAdminDiscussionHostLoad(ctx)
	if err != nil || !found {
		return rongcloud.DiscussionSystemHostConfig{}, found, err
	}
	return rongcloud.DiscussionSystemHostConfig{
		NodeID:               cfg.NodeID,
		DisplayName:          cfg.DisplayName,
		Avatar:               cfg.Avatar,
		ConfigVersion:        strconv.Itoa(cfg.ConfigVersion),
		HostPrompt:           cfg.DiscussionHostPrompt,
		Enabled:              cfg.Enabled,
		TimeoutSeconds:       cfg.TimeoutSeconds,
		RecommendationPrompt: cfg.RoleRecommendationPrompt,
	}, true, nil
}
