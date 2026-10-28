package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 绑定智能体的使用权限（私聊 / 群聊 / 讨论组 / 聊天室 / 读写文件 + 行为规范）。
//
// 为什么要有这一层：agent 被「分享」出去之后，服务端所有「是不是我的」判定只认
// 融云好友边（见 discussion_roundtable_stages.go 的 ownedAndEligibleNodes），
// 好友边存在即视为可支配 —— 于是被分享者的群/讨论组可以直接拉它干活。权限位
// 是唯一能表达「主人允许它被谁那样用」的维度，且必须落在服务端（派发在服务端）。
//
// 谁能读写：只有 rongcloud_node.claimed_by_user_id == 当前登录用户（设备的 C 端
// 认领者）。这条判定刻意**不看好友边** —— 好友边正是本次要绕开的那个缺口：
// 被分享出去的 agent 在好友边视角下也算「我的」（node_service.go 的 binding_role
// 恒按好友边算，设备列表里甚至恒为 "owner"），拿它做闸门等于把主人的配置交给对方。
//
// 读取同样要过这道闸门：权限位与行为规范是主人的配置，不是面向被分享者的公开
// 资料。行为规范里常写着主人自己的边界（能碰哪些数据、能不能对外发），一读就是
// 提示词全文。非主人一律 403，与写同一句文案。
//
// 契约（与 web / CLI 逐字对齐，改字段名要三端同时发版）：
//
//	GET  /api/claw/nodes/{nodeId}/permissions
//	PUT  /api/claw/nodes/{nodeId}/permissions
//
//	nodeId 是业务节点 id（rongcloud_node.node_id，与 /api/claw/nodes 同源），
//	不是 rongcloud_node 的 uuid 主键，也不是融云 user id —— 与同前缀的
//	/nodes/{nodeId}/status 保持一致。

// defaultAgentBehaviorPrompt 是「没有自定义行为规范」时每轮注入的默认模板。
//
// 文案的唯一来源在 rongcloud 包（派发侧要自己拼 prompt，而 handler 不能反向被
// 依赖）。这里只做别名，保证 GET/PUT 响应里的 defaultBehaviorPrompt 与真正注入
// 的那段文字永远一致 —— 两处各存一份文案迟早漂移。
const defaultAgentBehaviorPrompt = rongcloud.DefaultAgentBehaviorPrompt

// agentBehaviorPromptMaxRunes 是自定义行为规范的长度上限。
//
// 这道校验是边界防护，不是产品限制：行为规范在讨论组 v3 里注入在 prompt 预算
// **之外**（见 discussion_interactive.go 的 interactiveBuildPrompt），而 CLI 的
// argv 提示词上限是 8000 字符。不封顶的话，一条超长规范会让整条 prompt 越界、
// 退化到 CLI 不支持的 stdin 模式 —— 表现是「讨论突然没人说话」，很难查。
const agentBehaviorPromptMaxRunes = 4000

// agentPermissionDTO 是 GET / PUT 的响应形状（PUT 请求体是其子集，
// 不含 defaultBehaviorPrompt）。
//
// behaviorPrompt 返回**库里的原值**（空串 = 用默认），defaultBehaviorPrompt 恒为
// 模板：前端靠这两个字段区分「没设过」与「设成了和模板一样」，也才能在用户点
// 「恢复默认」时把输入框清空而不是填入模板正文。
type agentPermissionDTO struct {
	AllowPrivate          bool   `json:"allowPrivate"`
	AllowGroup            bool   `json:"allowGroup"`
	AllowDiscussion       bool   `json:"allowDiscussion"`
	AllowChatroom         bool   `json:"allowChatroom"`
	AllowFs               bool   `json:"allowFs"`
	BehaviorPrompt        string `json:"behaviorPrompt"`
	DefaultBehaviorPrompt string `json:"defaultBehaviorPrompt"`
}

// agentPermissionView 把节点行折叠成响应体。纯函数，便于单测。
func agentPermissionView(node db.RongcloudNode) agentPermissionDTO {
	return agentPermissionDTO{
		AllowPrivate:          node.AllowPrivate,
		AllowGroup:            node.AllowGroup,
		AllowDiscussion:       node.AllowDiscussion,
		AllowChatroom:         node.AllowChatroom,
		AllowFs:               node.AllowFs,
		BehaviorPrompt:        node.BehaviorPrompt,
		DefaultBehaviorPrompt: defaultAgentBehaviorPrompt,
	}
}

// agentPermissionForbiddenMessage 是「你不是这台设备的主人」的唯一文案。
//
// GET 与 PUT 共用同一句：前端只按状态码 403 展示这一条提示，两种文案会让用户
// 以为是两种不同的问题（一个「不能看」一个「不能改」），而实际原因只有一个。
const agentPermissionForbiddenMessage = "仅设备主人可以修改这台设备的权限"

// agentPermissionNodeReader 只抽 GetNodeByNodeID 一个方法。
//
// 抽出来是为了可测：rongcloud.NodeService 在 handler 包外构造不出来（融云 HTTP
// 客户端参数未导出），而「非 owner 读不到 behaviorPrompt」这条断言必须能在不连
// 数据库、不连融云的情况下跑。生产路径恒为 h.RongCloudNode。
type agentPermissionNodeReader interface {
	GetNodeByNodeID(ctx context.Context, nodeIDText string) (db.RongcloudNode, error)
}

// agentPermissionNodes 返回权限接口用的节点读取器（nil = 融云渠道未启用）。
func (h *Handler) agentPermissionNodes() agentPermissionNodeReader {
	if h.clawNodeOpsForTest != nil {
		return h.clawNodeOpsForTest
	}
	if h.RongCloudNode == nil {
		return nil
	}
	return h.RongCloudNode
}

// clawLoadPermissionNode 取 URL 里的 nodeId 对应的节点行，失败时已经写好响应。
func (h *Handler) clawLoadPermissionNode(w http.ResponseWriter, r *http.Request) (db.RongcloudNode, bool) {
	nodes := h.agentPermissionNodes()
	if nodes == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return db.RongcloudNode{}, false
	}
	nodeID := strings.TrimSpace(chi.URLParam(r, "nodeId"))
	if nodeID == "" {
		clawJSON(w, 400, 400, "缺少节点 ID", nil)
		return db.RongcloudNode{}, false
	}
	node, err := nodes.GetNodeByNodeID(r.Context(), nodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			clawJSON(w, 404, 404, "节点不存在", nil)
			return db.RongcloudNode{}, false
		}
		slog.Warn("claw: agent permission node lookup failed", "nodeId", nodeID, "error", err)
		clawJSON(w, 500, 500, "查询节点失败", nil)
		return db.RongcloudNode{}, false
	}
	return node, true
}

// clawIsAgentOwner 报告 userID 是不是这台设备的主人。
//
// 判据只有 claimed_by_user_id：好友边会把「被分享者」也算成拥有者
// （node_service.go 的 binding_role 就是这么算的），拿它做闸门等于把主人的配置
// 交给对方看。
func clawIsAgentOwner(node db.RongcloudNode, userID string) bool {
	return node.ClaimedByUserID.Valid && node.ClaimedByUserID.String != "" && node.ClaimedByUserID.String == userID
}

// ClawGetNodePermission GET /api/claw/nodes/{nodeId}/permissions
//
// 只有主人可读。权限位与行为规范是**主人的配置**，不是面向被分享者的公开资料：
// 行为规范里常常写着主人自己的边界（能碰哪些数据、能不能对外发），给被分享者看
// 等于泄露提示词全文。非 owner 一律 403，与 PUT 同一句文案。
func (h *Handler) ClawGetNodePermission(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	node, ok := h.clawLoadPermissionNode(w, r)
	if !ok {
		return
	}
	if !clawIsAgentOwner(node, userID) {
		clawJSON(w, 403, 403, agentPermissionForbiddenMessage, nil)
		return
	}
	clawJSON(w, 200, 200, "ok", agentPermissionView(node))
}

// ClawUpdateNodePermission PUT /api/claw/nodes/{nodeId}/permissions
//
// 只有主人（claimed_by_user_id）可写，其余 403。判定必须用这一个字段：
// 好友边会把「被分享者」也算成拥有者，拿它做闸门等于把权限交给对方。
func (h *Handler) ClawUpdateNodePermission(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	node, ok := h.clawLoadPermissionNode(w, r)
	if !ok {
		return
	}
	if !clawIsAgentOwner(node, userID) {
		clawJSON(w, 403, 403, agentPermissionForbiddenMessage, nil)
		return
	}
	if h.Queries == nil {
		clawJSON(w, 503, 503, "数据库未配置", nil)
		return
	}
	var req struct {
		AllowPrivate    bool   `json:"allowPrivate"`
		AllowGroup      bool   `json:"allowGroup"`
		AllowDiscussion bool   `json:"allowDiscussion"`
		AllowChatroom   bool   `json:"allowChatroom"`
		AllowFs         bool   `json:"allowFs"`
		BehaviorPrompt  string `json:"behaviorPrompt"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		clawJSON(w, 400, 400, "请求体格式错误", nil)
		return
	}
	// 空串/纯空白 = 恢复默认。落库统一存空串，读取时由
	// EffectiveAgentBehaviorPrompt 回退到模板（见上方 const 的说明）。
	prompt := strings.TrimSpace(req.BehaviorPrompt)
	if utf8.RuneCountInString(prompt) > agentBehaviorPromptMaxRunes {
		clawJSON(w, 400, 400, "行为规范不能超过 4000 个字符", nil)
		return
	}
	updated, err := h.Queries.UpdateRongCloudNodeAgentPermission(r.Context(), db.UpdateRongCloudNodeAgentPermissionParams{
		NodeID:          node.NodeID,
		AllowPrivate:    req.AllowPrivate,
		AllowGroup:      req.AllowGroup,
		AllowDiscussion: req.AllowDiscussion,
		AllowChatroom:   req.AllowChatroom,
		AllowFs:         req.AllowFs,
		BehaviorPrompt:  prompt,
	})
	if err != nil {
		slog.Warn("claw: agent permission update failed", "nodeId", node.NodeID, "error", err)
		clawJSON(w, 500, 500, "保存权限失败", nil)
		return
	}
	slog.Info("claw: agent permission updated",
		"nodeId", updated.NodeID, "by", userID,
		"private", updated.AllowPrivate, "group", updated.AllowGroup,
		"discussion", updated.AllowDiscussion, "chatroom", updated.AllowChatroom,
		"fs", updated.AllowFs)
	clawJSON(w, 200, 200, "ok", agentPermissionView(updated))
}
