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
// ⚠️ 注意：保存配置会**强制解绑**该节点的原用户（删融云好友边，见
// clawAdminUnbindHostNode）—— 主持是平台级基础设施，不该同时挂在某个普通用户的
// 设备列表里。这与旧后端 _persist 的 DELETE FROM im_user_nodes 一致。
//
// 运行时的主持解析（P5）走 rongcloud_system_config 这一行：讨论运行时用它取
// 展示名/头像/提示词/超时，节点在线判定按融云 presence（node_presence.go）。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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

	// clawAdminDiscussionHostMaxModelSegment 是 provider / model 单段的字符上限，
	// 与设备端（xiachat-cli/src/model-catalog.ts 的 ROUTE_RE）以及 web 的
	// segment() 一致：超过这个长度设备侧本来也解析不出 route。
	clawAdminDiscussionHostMaxModelSegment = 128
	// 向主持节点要一次模型目录的等待上限。目录是设备本机的快跑查询（CLI 侧还有
	// 5 分钟缓存），8s 与 C 端浏览器的超时同值；查询接口放宽到 12s，因为那是一
	// 次显式的人工操作，而保存路径要短——离线节点不能让「改个提示词」卡 12 秒。
	clawAdminDiscussionHostCatalogTimeout  = 12 * time.Second
	clawAdminDiscussionHostVerifyTimeout   = 8 * time.Second
)

// clawAdminModelSegmentCharset 是 provider / model 单段允许的字符集。`@` 保留在
// 这里只是为了给出更准确的报错（有些网关的模型名带它），route 正则并不放行 ——
// 设备端 buildModelArgs 拆不出带 `@` 的段，所以这类值最终仍会被拒。
var clawAdminModelSegmentCharset = regexp.MustCompile(`^[A-Za-z0-9._:/@-]+$`)

// clawAdminModelRouteRE 与 rongcloud 包 modelRouteRE（discussion_bridge.go）、
// 设备端 buildModelArgs、web 的 device-default-model 同源：provider 段
// `[A-Za-z0-9][A-Za-z0-9._-]{0,127}` + model 段
// `/[A-Za-z0-9][A-Za-z0-9._:-]{0,127}`。两段都必填 —— 只有 model 会退化成裸 id，
// 设备端拆不出 provider（见 clawAdminDiscussionHostNormalizeModel）。
var clawAdminModelRouteRE = regexp.MustCompile(
	`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}/[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`,
)

// 模型核验的三种结局。目录**拿不到**（离线 / 超时 / 旧 CLI 不响应）必须放行，
// 只有「目录拿到了、里面明确没有这个模型」才拒绝——主持节点常常是一台离线
// 备用机，做成一票否决会让整个选择器在最常见的运维场景下不可用。
const (
	clawAdminModelCheckVerified   = "verified"
	clawAdminModelCheckUnverified = "unverified"
	clawAdminModelCheckRejected   = "rejected"
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
	// ModelProvider / Model 是运营为主持人指定的模型（空串 = 不指定，沿用设备
	// 默认）。两者分开存是为了让「供应商 → 模型」两级选择器原样来回读写；
	// 运行时只认拼好的 route（见 clawAdminDiscussionHostModelRoute）。
	ModelProvider string `json:"model_provider"`
	Model         string `json:"model"`
	// ModelVerified 只在 PUT 响应里出现，且只在「这次保存带了模型但没能核验」
	// 时为 false（目录拿不到 / 节点离线 / 旧 CLI 不响应）。指向 bool 是为了区分
	// 「没核验过」与「核验通过了」——GET 恒为 nil，不写这个键。
	ModelVerified *bool `json:"model_verified,omitempty"`
}

// clawAdminDiscussionHostBody is the PUT payload. Every field is a pointer so a
// missing key is distinguishable from a zero value — the legacy contract
// rejects bodies that omit any of them.
//
// ModelProvider / Model 例外：它们**可选**。旧客户端不带这两个键时必须仍然
// 能保存（行为与改造前完全一致），所以既不能设成必填，也不能把缺失当成清空
// 之外的语义 —— nil 与空串在此等价，都表示「不指定模型」。
type clawAdminDiscussionHostBody struct {
	ExpectedConfigVersion    *int    `json:"expected_config_version"`
	Enabled                  *bool   `json:"enabled"`
	NodeID                   *string `json:"node_id"`
	DisplayName              *string `json:"display_name"`
	Avatar                   *string `json:"avatar"`
	RoleRecommendationPrompt *string `json:"role_recommendation_prompt"`
	DiscussionHostPrompt     *string `json:"discussion_host_prompt"`
	TimeoutSeconds           *int    `json:"timeout_seconds"`
	ModelProvider            *string `json:"model_provider"`
	Model                    *string `json:"model"`
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
	if v, ok := stored["model_provider"].(string); ok {
		cfg.ModelProvider = strings.TrimSpace(v)
	}
	if v, ok := stored["model"].(string); ok {
		cfg.Model = strings.TrimSpace(v)
	}
	// 读路径兜底（M3）：写入口不止管理端 PUT 一个 —— 旁路
	// `PUT /api/workspaces/{id}/rongcloud/system-host` 会把 config 整块覆盖，
	// 不做任何字段校验。模型串最终会进角色快照、IM payload 和设备 argv，所以在
	// 读的这一侧统一用同一套规则 clamp 一次：非法值丢弃并回到「不指定模型」，
	// 运行时永远只可能吃到合法 route。
	if provider, model, fail := clawAdminDiscussionHostNormalizeModel(cfg.ModelProvider, cfg.Model); fail != "" {
		// 只告警一次：这个函数在每次加载配置时都会跑（含每场讨论），
		// 每次刷日志会把真正的故障信号淹掉。
		clawAdminInvalidStoredModelOnce.Do(func() {
			slog.Warn("claw admin: dropping invalid stored discussion host model",
				"reason", fail,
				"model_provider", cfg.ModelProvider,
				"model", cfg.Model)
		})
		cfg.ModelProvider, cfg.Model = "", ""
	} else {
		cfg.ModelProvider, cfg.Model = provider, model
	}
	if v, ok := stored["node_id"].(string); ok && v != "" && cfg.NodeID == "" {
		cfg.NodeID = v
	}
}

// clawAdminInvalidStoredModelOnce 让「丢弃非法存量模型」的告警只打一次。
var clawAdminInvalidStoredModelOnce sync.Once

// clawAdminDiscussionHostModelRoute 把两级选择拼成 `provider/slug`。
//
// 两个键同进同出（归一化保证了这一点），所以这里没有「只有 model」的分支 ——
// 那个形态在 normalize 阶段就被 400 拒了，运行时永远看不到裸 id。
func clawAdminDiscussionHostModelRoute(provider, model string) string {
	if provider == "" || model == "" {
		return ""
	}
	return provider + "/" + model
}

// clawAdminDiscussionHostModelFields 读 PUT body 里的两个可选字段并校验。
func clawAdminDiscussionHostModelFields(providerRaw, modelRaw *string) (provider, model, fail string) {
	return clawAdminDiscussionHostNormalizeModel(
		strings.TrimSpace(derefString(providerRaw)),
		strings.TrimSpace(derefString(modelRaw)),
	)
}

// clawAdminDiscussionHostNormalizeModel 是「供应商 + 模型」的**唯一**校验/归一化点。
//
// 三个调用方共用它，因此「什么算合法模型」只有一处定义：
//   - PUT（写入口）；
//   - applyClawAdminDiscussionHostStored（**读路径**兜底：旁路写入口
//     PUT /api/workspaces/{id}/rongcloud/system-host 会整块覆盖 config、不做任何
//     字段校验，读路径不 clamp 的话非法串会一路进快照、进 IM payload、进设备 argv）；
//   - 测试。
//
// 两个键要么都有要么都没有：只剩 model 会退化成裸 id，设备端 buildModelArgs 拆不出
// provider（codex 会用自己配置里的 provider），表现为「选了但模型不对」。
func clawAdminDiscussionHostNormalizeModel(provider, model string) (normProvider, normModel, fail string) {
	if provider == "" && model == "" {
		return "", "", ""
	}
	if provider == "" {
		return "", "", "选择模型时必须同时指定供应商"
	}
	if model == "" {
		return "", "", "选择供应商后必须选择模型"
	}
	// 逐段校验。文案统一用中文且带字段名：这几个 400 只可能出现在「手填」形态，
	// 是运营直接看到的话，别把开发者英文（+ 裸字段名）摊给ta。
	for _, field := range []struct{ label, value string }{
		{"供应商", provider},
		{"模型", model},
	} {
		if len([]rune(field.value)) > clawAdminDiscussionHostMaxModelSegment {
			return "", "", field.label + "最多 128 个字符"
		}
		if !clawAdminModelSegmentCharset.MatchString(field.value) {
			return "", "", field.label + "只能包含字母、数字和 . _ : - 这些字符"
		}
	}
	// 拼起来必须是一条设备端认得的 route：CLI 的 buildModelArgs 会按它拆出
	// provider（codex / hermes 需要分别传 -c model_provider 与 --provider），
	// 形状不对时模型会被整条当成裸 id 传下去，表现为「选择生效但模型不对」。
	if !clawAdminModelRouteRE.MatchString(clawAdminDiscussionHostModelRoute(provider, model)) {
		return "", "", "供应商或模型的写法无法拼成“供应商/模型”形式"
	}
	return provider, model, ""
}

// derefString 读一个可选字符串字段（nil 视为空串）。
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// clawAdminDiscussionHostNodeByUUID looks a node up by its primary key (the id
// the runtime stores in rongcloud_system_config.node_id, which is a uuid).
// Returns the node's rongcloud user id, the capabilities it advertises, and
// whether some user currently owns it.
//
// userBound 读的是 owner_user_id 这一列的遗留语义，**不是**设备归属的真相：
// 归属看的是融云好友边（clawAdminNodeClaimants / node_claim.go），那才是设备列表
// 的依据。调用方要判断"是否已绑定用户"请用后者。
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
//
// is_user_bound 由「融云好友边」判定（node_claim.go 的反查），与设备列表用的是
// 同一份归属真相：指定主持会把该节点从原用户名下摘下来，前端据此显示
// 「已绑定普通用户 → 强制解绑并保存」的提示。
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

	type hostCandidate struct {
		node         db.RongcloudNode
		name         string
		capabilities []string
	}
	candidates := make([]hostCandidate, 0, len(nodes))
	rcUserIDs := make([]string, 0, len(nodes))
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
		candidates = append(candidates, hostCandidate{node: node, name: name, capabilities: capabilities})
		rcUserIDs = append(rcUserIDs, node.RongcloudUserID)
	}
	// 一次反查算出所有候选的归属（内部按 15s 记忆化，不会每个节点各扫一遍）。
	claimants := h.clawAdminNodeClaimants(r.Context(), rcUserIDs)

	out := []interface{}{}
	for _, candidate := range candidates {
		node := candidate.node
		status := "offline"
		if h.RongCloudNode.IsOnline(node.ID) {
			status = "online"
		}
		out = append(out, map[string]interface{}{
			"node_id":       node.ID.String(),
			"ai_user_id":    node.RongcloudUserID,
			"name":          candidate.name,
			"deploy_status": status,
			"is_user_bound": claimants[node.RongcloudUserID] != "",
			"capabilities":  candidate.capabilities,
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
	// 模型可选：两个字段都缺省 = 不指定（与改造前完全一致）。格式校验放在
	// 查库之前，非法输入不该先付一次数据库往返。
	modelProvider, model, modelFail := clawAdminDiscussionHostModelFields(body.ModelProvider, body.Model)
	if modelFail != "" {
		clawAdminFail(w, 400, modelFail)
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
	// 上面按 id 查到了节点，说明 node_id 就是本表主键的文本形式，这里必然可解析。
	nodeUUID, err := uuid.Parse(nodeID)
	if err != nil {
		clawAdminFail(w, 400, "node_id is not a valid node id")
		return
	}
	hostNodeUUID := pgtype.UUID{Bytes: nodeUUID, Valid: true}

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

	// 模型存在性核验：目录**取得到**时才判定，取不到（离线 / 超时 / 旧 CLI
	// 不响应）一律放行并标记未核验 —— 主持节点常常是一台离线备用机，做成一票
	// 否决会让「改个提示词」也被卡死。
	//
	// 模型一个字都没改时直接跳过这次往返：运维最常见的动作是「只改人设提示词」，
	// 那不该为此多等一次设备往返（最多 8s），也不该每次都在页面上弹「未能核验」。
	modelUnchanged := exists && current.ModelProvider == modelProvider && current.Model == model
	var modelVerified *bool
	if model != "" && !modelUnchanged {
		check, reason := h.clawAdminDiscussionHostCheckModel(r.Context(), hostNodeUUID, aiUserID, modelProvider, model)
		switch check {
		case clawAdminModelCheckRejected:
			clawAdminFail(w, 400, rongcloud.NodeRequestCodeModelNotAvailable)
			return
		case clawAdminModelCheckUnverified:
			slog.Info("claw admin: discussion host model saved unverified",
				"node_id", nodeID, "model", clawAdminDiscussionHostModelRoute(modelProvider, model),
				"reason", reason)
			unverified := false
			modelVerified = &unverified
		default:
			verified := true
			modelVerified = &verified
		}
	}

	stored := map[string]interface{}{
		"enabled":                    *body.Enabled,
		"node_id":                    nodeID,
		"ai_user_id":                 aiUserID,
		"display_name":               displayName,
		"avatar":                     avatar,
		"role_recommendation_prompt": rolePrompt,
		"discussion_host_prompt":     hostPrompt,
		"timeout_seconds":            *body.TimeoutSeconds,
	}
	// 未指定模型时**不写**这两个键（而不是写空串）：存量配置的形状不变，
	// 读回来 cfg.Model == ""，三处 host 快照都不写 model，设备端 buildModelArgs
	// 的 if (!model) 让行为与改造前逐字节一致。
	if model != "" {
		stored["model"] = model
		if modelProvider != "" {
			stored["model_provider"] = modelProvider
		}
	}
	configJSON, err := json.Marshal(stored)
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

	// 指定主持即从原用户名下摘走这台设备（旧后端 _persist 的 DELETE FROM
	// im_user_nodes）。放在落库之后：配置一旦生效，主持节点就不该再出现在
	// 普通用户的设备列表里。解绑失败要如实上报，不能假装成功。
	unboundFrom, unbindErr := h.clawAdminUnbindHostNode(r.Context(), hostNodeUUID)
	if unbindErr != nil {
		// 这一条同时写日志与审计：502 是「配置已落库但解绑失败」的中间态，
		// 只回报表不写日志的话，线上只能靠翻库猜原因（2026-10-24 实测踩过）。
		slog.Warn("claw admin: unbind discussion host node failed",
			"node_id", nodeID, "ai_user_id", aiUserID, "error", unbindErr)
		h.clawAdminAudit(r, ident, "admin_system_discussion_host_unbind_failed", "system_config",
			clawAdminDiscussionHostKey, map[string]interface{}{
				"node_id": nodeID, "error": unbindErr.Error(),
			})
		clawAdminFail(w, 502, "配置已保存，但解除该节点原用户绑定失败，请重试")
		return
	}

	h.clawAdminAudit(r, ident, "admin_system_discussion_host_update", "system_config",
		clawAdminDiscussionHostKey, map[string]interface{}{
			"node_id":          nodeID,
			"config_version":   version,
			"expected_version": *body.ExpectedConfigVersion,
			"enabled":          *body.Enabled,
			"unbound_from":     unboundFrom,
			"model":            clawAdminDiscussionHostModelRoute(modelProvider, model),
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
		ModelProvider:            modelProvider,
		Model:                    model,
		ModelVerified:            modelVerified,
	})
}

// ---- 模型目录：GET /api/admin/system/discussion-host/model-catalog ----
//
// 「供应商 → 模型」两级选择器的数据源。管理端没有 IM SDK（package.json 只有
// @rongcloud/embed-console-sdk），做不到 C 端那样浏览器直连设备，因此由服务端
// 代理这一次往返：节点请求 broker 下发 `discussion_model_catalog_request`
// （protocolVersion 2），设备端 CLI 已经有应答器，**无需任何改动**。
//
// 返回形状与 C 端的 NodeModelCatalog 一致，前端因此可以 1:1 抄「设备详情」页的
// 级联选择器（clawmessenger-web/src/sections/DeviceDetail.tsx:390-430）。
//
// 取不到目录时的语义：**一律** 503 + 同一个 code（MODEL_CATALOG_UNAVAILABLE），
// 页面据此降级成两个可手填的输入框。具体原因（节点不存在 / 不是主持节点 /
// 不可展示 / 离线 / 超时 / 设备应答不合协议）只写服务端日志与审计，不回给调用方：
// 这个接口会以系统账号向一台设备私发 IM 并让它跑一次本机探测，属于**有副作用**
// 的操作，不能同时充当「某个 node_id 是否存在 / 是否在线」的探测预言机。
//
// 注意门控是 system:write 而不是 system:read：只读管理员不该能动用这条有副作用的通道。

// clawAdminDiscussionHostCatalog 是目录接口的响应体。
type clawAdminDiscussionHostCatalog struct {
	ProtocolVersion int                           `json:"protocolVersion"`
	NodeID          string                        `json:"node_id"`
	DefaultModel    string                        `json:"default_model"`
	Providers       []clawAdminDiscussionHostProv `json:"providers"`
	FetchedAt       string                        `json:"fetched_at"`
	Source          string                        `json:"source"`
}

type clawAdminDiscussionHostProv struct {
	ID     string                       `json:"id"`
	Name   string                       `json:"name"`
	Models []clawAdminDiscussionHostMdl `json:"models"`
}

type clawAdminDiscussionHostMdl struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (h *Handler) ClawAdminDiscussionHostModelCatalog(w http.ResponseWriter, r *http.Request) {
	ident := h.requireClawAdmin("system:write", w, r)
	if ident == nil {
		return
	}

	nodeID := strings.TrimSpace(r.URL.Query().Get("node_id"))
	// 不传 node_id 时回落到已保存配置的节点：页面首屏就是「已配置的节点」，
	// 让它可以不带参数地先拉一次。这一段不泄露任何东西（配置本身 GET 就能读）。
	if nodeID == "" {
		cfg, found, err := h.clawAdminDiscussionHostLoad(r.Context())
		if err != nil {
			clawAdminFail(w, 503, "discussion host config unavailable")
			return
		}
		if !found || cfg.NodeID == "" {
			h.auditCatalogFetch(r, ident, "", rongcloud.NodeRequestCodeHostNotConfigured)
			clawAdminFail(w, 400, rongcloud.NodeRequestCodeHostNotConfigured)
			return
		}
		nodeID = cfg.NodeID
	}

	// clawAdminCatalogUnavailable 是这条接口唯一的失败出口：对外的码不区分原因
	// （见 B2），但审计里要留下「谁、对哪台节点、因为什么」—— 收紧到
	// system:write 之后，审计是枚举这类行为的**唯一**证据。
	unavailable := func(reason string, detail ...interface{}) {
		slog.Warn("claw admin: discussion host model catalog unavailable",
			append([]interface{}{"reason", reason}, detail...)...)
		h.auditCatalogFetch(r, ident, nodeID, rongcloud.NodeRequestCodeCatalogUnavailable, "reason", reason)
		clawAdminFail(w, 503, rongcloud.NodeRequestCodeCatalogUnavailable)
	}

	nodeUUID, err := uuid.Parse(nodeID)
	if err != nil {
		unavailable("node_id is not a valid node id")
		return
	}
	if h.Queries == nil {
		// 半装配的 Handler（只有 DB、没有 Queries）：当作目录不可用，别 panic。
		unavailable("node queries not wired")
		return
	}
	nodeUUIDValue := pgtype.UUID{Bytes: nodeUUID, Valid: true}
	node, err := h.Queries.GetRongCloudNodeByID(r.Context(), nodeUUIDValue)
	if err != nil {
		unavailable("node lookup failed")
		return
	}
	// 与 eligible-nodes / PUT 同一套闸门：只有「可展示 + 能力位齐备」的节点才允许
	// 被点名。缺了这道门，任何已存在的 rongcloud_node.id 都能被套出 provider/model
	// 清单，还会顺带让它跑一次本机 agent 探测（机器基础设施节点、运维维护 agent、
	// 其它 workspace 的节点都在范围内 —— 与 node_capabilities.go 的展示契约冲突）。
	if !rongcloud.IsDisplayNode(node) {
		unavailable("node is not displayable", "node_id", nodeID)
		return
	}
	if missing := clawAdminMissingHostCapabilities(clawAdminParseCapabilities(node.Capabilities)); len(missing) > 0 {
		unavailable("node is not a discussion host", "node_id", nodeID, "missing", missing)
		return
	}

	catalog, code := h.clawAdminDiscussionHostFetchCatalog(
		r.Context(), nodeUUIDValue, node.RongcloudUserID, clawAdminDiscussionHostCatalogTimeout)
	if code != "" {
		unavailable(code, "node_id", nodeID)
		return
	}

	providers := make([]clawAdminDiscussionHostProv, 0, len(catalog.Providers))
	for _, provider := range catalog.Providers {
		models := make([]clawAdminDiscussionHostMdl, 0, len(provider.Models))
		for _, entry := range provider.Models {
			models = append(models, clawAdminDiscussionHostMdl{ID: entry.ID, Name: entry.Name})
		}
		providers = append(providers, clawAdminDiscussionHostProv{
			ID: provider.ID, Name: provider.Name, Models: models,
		})
	}
	h.auditCatalogFetch(r, ident, nodeID, "", "source", "device",
		"provider_count", len(providers))
	clawAdminOK(w, clawAdminDiscussionHostCatalog{
		ProtocolVersion: 2,
		NodeID:          nodeID,
		DefaultModel:    catalog.DefaultModel,
		Providers:       providers,
		FetchedAt:       time.Now().UTC().Format(time.RFC3339),
		Source:          "device",
	})
}

// auditCatalogFetch 记一条「拉取主持节点模型目录」的审计。
//
// errorCode 为空表示成功。之所以连成功也记：这条接口会让一台设备跑一次本机
// agent 探测（有副作用），收紧到 system:write 之后，审计是唯一能回答「谁在
// 什么时候枚举了哪台节点」的地方。
func (h *Handler) auditCatalogFetch(
	r *http.Request, ident *clawAdminIdentity, nodeID, errorCode string, extra ...interface{},
) {
	detail := map[string]interface{}{"node_id": nodeID}
	if errorCode != "" {
		detail["error_code"] = errorCode
	}
	for i := 0; i+1 < len(extra); i += 2 {
		key, ok := extra[i].(string)
		if !ok {
			continue
		}
		detail[key] = extra[i+1]
	}
	h.clawAdminAudit(r, ident, "fetch_discussion_host_model_catalog", "system_config",
		clawAdminDiscussionHostKey, detail)
}

// clawAdminDiscussionHostFetchCatalog 向主持节点要一次模型目录。
// 返回空 code 表示拿到了；否则是可直接回给前端的具名错误码。
func (h *Handler) clawAdminDiscussionHostFetchCatalog(
	ctx context.Context, nodeUUID pgtype.UUID, aiUserID string, timeout time.Duration,
) (rongcloud.DiscussionModelCatalog, string) {
	if aiUserID == "" {
		return rongcloud.DiscussionModelCatalog{}, rongcloud.NodeRequestCodeHostNotConfigured
	}
	if h.RongCloudNodeRequests == nil {
		// broker 未装配：当作「目录不可用」，不是 500 —— 前端降级手填即可。
		return rongcloud.DiscussionModelCatalog{}, rongcloud.NodeRequestCodeHostOffline
	}
	// 在线判定走融云 presence（与 eligible-nodes 同一份真相）。节点服务未装配
	// 时判不了，只能尝试一次 —— broker 发不出去会立刻回 HOST_OFFLINE，不阻塞。
	if h.RongCloudNode != nil && !h.RongCloudNode.IsOnline(nodeUUID) {
		return rongcloud.DiscussionModelCatalog{}, rongcloud.NodeRequestCodeHostOffline
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	host := rongcloud.NodeRequestHost{
		AIUserID:       aiUserID,
		TimeoutSeconds: int(timeout / time.Second),
	}
	return h.RongCloudNodeRequests.FetchModelCatalog(ctx, host)
}

// clawAdminDiscussionHostCheckModel 判断刚保存的模型在主持节点上是否可用。
//
// 三态：verified / unverified / rejected。只有「目录确实拿到了、且里面没有这个
// model」才 rejected；离线、超时、目录为空、broker 未装配一律 unverified（放行）。
func (h *Handler) clawAdminDiscussionHostCheckModel(
	ctx context.Context, nodeUUID pgtype.UUID, aiUserID, provider, model string,
) (string, string) {
	if model == "" {
		return clawAdminModelCheckVerified, ""
	}
	catalog, code := h.clawAdminDiscussionHostFetchCatalog(
		ctx, nodeUUID, aiUserID, clawAdminDiscussionHostVerifyTimeout)
	if code != "" {
		return clawAdminModelCheckUnverified, code
	}
	if len(catalog.Providers) == 0 {
		// 设备答了「我没有可用模型」：这不构成拒绝的理由（可能只是 agent 配置
		// 读不出来），按未核验处理并让页面提示一句。
		return clawAdminModelCheckUnverified, "empty catalog"
	}
	// 走到这里 provider 与 model 一定都非空（NormalizeModel 保证），所以目录里
	// 一定能按 `provider/model` 定位到具体条目，不存在「裸 id 无法定位」的兜底。
	if !catalog.CatalogHasRoute(provider, model) {
		return clawAdminModelCheckRejected, "model is not available on the host node"
	}
	return clawAdminModelCheckVerified, ""
}

// clawAdminUnbindHostNode 把被指定为系统主持的节点从原用户名下摘掉。
//
// 设备归属的唯一真相是「融云好友边」（node_claim.go 的反查）：C 端 /api/claw/nodes
// 只列出调用者好友列表里的节点，所以只要删掉这条边，该节点就不再出现在该用户的
// 设备列表里 —— 这正是旧 Python 后端 _persist 里
// `DELETE FROM im_user_nodes WHERE node_id = …` 那一步，Go 移植时漏掉了。
// 同时把该节点标为系统保留（同一时刻只有一个主持），与旧后端
// `is_system_reserved = TRUE` 对齐。
// 旧后端那句 `user_id = NULL` **不移植**：rongcloud_node.owner_user_id 在 564 号
// 迁移里是 UUID NOT NULL，且它存的是 multica 工作区管理员而不是 C 端认领者。
//
// 返回被解绑的账号 id（空串表示本来就没绑定）。调用方在失败时按 502 上报：
// 配置已经落库，但「强制解绑」这一半没做成，不能假装成功。
func (h *Handler) clawAdminUnbindHostNode(ctx context.Context, nodeUUID pgtype.UUID) (string, error) {
	if h.RongCloudNode == nil {
		return "", nil
	}
	var aiUserID string
	if err := h.DB.QueryRow(ctx,
		`SELECT rongcloud_user_id FROM rongcloud_node WHERE id = $1`, nodeUUID).Scan(&aiUserID); err != nil {
		return "", err
	}
	if aiUserID == "" {
		return "", nil
	}

	// 归属来自 15s 记忆化的好友边反查，先失效缓存才能看到当前真实归属。
	h.RongCloudNode.InvalidateNodeClaimants()
	owner := h.clawAdminNodeClaimants(ctx, []string{aiUserID})[aiUserID]
	if owner != "" {
		if err := h.RongCloudNode.RevokeFriendship(ctx, owner, aiUserID); err != nil {
			return "", err
		}
		h.RongCloudNode.InvalidateNodeClaimants()
	}
	// 好友边已撤（或本来就没有），claimed_by_user_id 投影必须跟着清。owner 为空
	// 时也清：主持节点被平台收回，任何残留认领都是错的。
	h.clawAttachNodeClaimDB()
	if err := h.RongCloudNode.ClearNodeClaim(ctx, aiUserID, owner); err != nil {
		slog.Warn("claw admin: clear node claim for discussion host failed",
			"node_rc", aiUserID, "owner", owner, "error", err)
	}

	// 这里**不写** rongcloud_node.owner_user_id：564 号迁移把它定义为
	// `owner_user_id UUID NOT NULL`，赋 NULL 会直接违反约束；而且它指向的是
	// multica 工作区管理员（安装账号）—— 全库所有节点行都是同一个值，从来不是
	// C 端认领者（见 node_claim.go 顶部注释、claw_admin_nodes.go 的实测记录）。
	// 早期移植照抄旧 Python 的 `user_id = NULL`，导致每次保存都在这一步撞
	// not-null 约束：配置已经落库，却恒定回 502「解除原用户绑定失败」。
	// 设备归属的真相是融云好友边 + claimed_by_user_id 投影，两者上面都已清空，
	// 对 C 端 /api/claw/nodes 来说该节点已经不再属于任何人。

	// 同一时刻只有一个主持：把上一个主持的保留标记摘掉（只动 AI 节点，
	// 别碰真人账号）。
	if _, err := h.DB.Exec(ctx, `
		UPDATE rongcloud_user SET is_system_reserved = TRUE
		WHERE rongcloud_user_id = $1 AND is_ai_node = TRUE`, aiUserID); err != nil {
		return owner, err
	}
	if _, err := h.DB.Exec(ctx, `
		UPDATE rongcloud_user SET is_system_reserved = FALSE
		WHERE is_system_reserved = TRUE AND is_ai_node = TRUE AND rongcloud_user_id <> $1`,
		aiUserID); err != nil {
		return owner, err
	}
	return owner, nil
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

	payload, code := h.RongCloudNodeRequests.RecommendRoles(ctx, host, topic, goal, maxRoles, "")
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
		// 运行时只认拼好的 route（`provider/slug`）：三处 host 快照把它原样
		// 写进角色板的 host 条目，v3 引擎读成 role.Model 后随 your_turn 的
		// params.model 下发，设备端 buildModelArgs 再按 `/` 拆回两段。
		Model: clawAdminDiscussionHostModelRoute(cfg.ModelProvider, cfg.Model),
	}, true, nil
}
