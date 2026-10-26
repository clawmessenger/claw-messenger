package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
)

// clawDeviceControlState is one bound agent's run-state, as squeezed out of
// the control table for the asking device.
//
// Version is 0 for an agent that has no control row at all — the state the
// supervisor already assumes on a fresh bind. Sending 0 lets the device tell
// "nothing has ever been asked of me here" from "someone pressed stop", without
// the server having to materialise a row for every node the moment it binds.
type clawDeviceControlState struct {
	Agent        string `json:"agent"`
	NodeID       string `json:"nodeId"`
	DesiredState string `json:"desiredState"`
	Version      int64  `json:"version"`
}

// ClawDeviceControl POST /api/claw/device/control —— clawmessenger supervisor 轮询
// 本机各 agent 的期望运行状态。认证与 device/nodes、device/heartbeat 一致（机器节点
// device credential）。
//
// 这是启停指令唯一的投递路径。走 IM 不可行：能真正拉起/杀掉 agent 的是 supervisor
// 主进程，而它不持有融云连接（imlib 是模块级单例，一个 IM 用户只能有一个进程登录），
// 持连接的是它拉起的每个 agent 子进程；且一旦子进程被停掉，「启动」就没有任何进程能
// 收到。所以让始终在线的 supervisor 主动来拉，而不是服务端去找它。
//
// 同一次往返还带 exe 升级：请求体里的 version/platform 是「这台机器的安装版本」，
// 响应体里的 upgrade 是「该不该升级、升到哪个版本、从哪下」。合成一次往返的理由见
// claw_device_upgrade.go 文件头 —— 版本是机器属性，而这是唯一的机器级定时轮询。
func (h *Handler) ClawDeviceControl(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	var req struct {
		NodeID       string `json:"nodeId"`
		CredentialID string `json:"credentialId"`
		Secret       string `json:"secret"`
		// 旧 CLI 不带这两个字段：那时 version 为空，版本列显示「未知」，
		// upgrade 分支自然不会被触发（服务端不会给没上报过平台的机器下发
		// Windows 升级指令）。
		Version  string `json:"version"`
		Platform string `json:"platform"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil ||
		req.NodeID == "" || req.CredentialID == "" || req.Secret == "" {
		clawJSON(w, 400, 400, "缺少设备凭据", nil)
		return
	}
	// 先拿机器节点：升级行是按机器存的，需要它的 uuid/workspace/machine_id。
	// 这条查一次与 AgentNodesForDevice 内部的校验重复，但那是已索引的主键查询，
	// 每 10 秒一次可以接受；把机器维度拆出来比让 AgentNodesForDevice 顺手返回
	// 一个调用方多半不关心的结构更不容易出错。
	machineNode, err := h.RongCloudNode.AuthorizeDevice(r.Context(), req.NodeID, req.CredentialID, req.Secret)
	if err != nil {
		if errors.Is(err, rongcloud.ErrInvalidDeviceCredential) {
			clawJSON(w, 401, 401, "设备凭据无效", nil)
			return
		}
		slog.Warn("claw: device control machine lookup failed", "nodeId", req.NodeID, "error", err)
		clawJSON(w, 500, 500, "查询设备控制状态失败", nil)
		return
	}
	refs, err := h.RongCloudNode.AgentNodesForDevice(r.Context(), req.NodeID, req.CredentialID, req.Secret)
	if err != nil {
		if errors.Is(err, rongcloud.ErrInvalidDeviceCredential) {
			clawJSON(w, 401, 401, "设备凭据无效", nil)
			return
		}
		slog.Warn("claw: device control lookup failed", "nodeId", req.NodeID, "error", err)
		clawJSON(w, 500, 500, "查询设备控制状态失败", nil)
		return
	}
	states, err := h.clawAdminDeviceControlStates(r.Context(), refs)
	if err != nil {
		slog.Warn("claw: device control state read failed", "nodeId", req.NodeID, "error", err)
		clawJSON(w, 500, 500, "查询设备控制状态失败", nil)
		return
	}
	payload := map[string]interface{}{"agents": states}
	if h.DB != nil {
		version := strings.TrimSpace(req.Version)
		platform := strings.TrimSpace(req.Platform)
		if version != "" || platform != "" {
			// 版本上报失败绝不影响启停：下一轮再报就是了。
			if err := h.clawDeviceRecordUpgradeState(r.Context(), machineNode, version, platform, 0); err != nil {
				slog.Warn("claw: device version report failed",
					"machine", machineIDForDevice(machineNode), "error", err)
			}
		}
		intent, err := h.clawDeviceUpgradeIntent(r.Context(), machineNode, version)
		if err != nil {
			slog.Warn("claw: device upgrade intent failed",
				"machine", machineIDForDevice(machineNode), "error", err)
		}
		if intent != nil {
			payload["upgrade"] = intent
		}
	}
	clawJSON(w, 200, 200, "", payload)
}

func (h *Handler) clawAdminDeviceControlStates(ctx context.Context, refs []rongcloud.AgentNodeRef) ([]clawDeviceControlState, error) {
	overrides, err := h.clawAdminDeviceControlOverrides(ctx, refs)
	if err != nil {
		return nil, err
	}
	return applyDeviceControlOverrides(refs, overrides), nil
}

// clawAdminDeviceControlOverrides reads the stored control rows for refs, keyed
// by node uuid text.
func (h *Handler) clawAdminDeviceControlOverrides(ctx context.Context, refs []rongcloud.AgentNodeRef) (map[string]clawDeviceControlState, error) {
	overrides := make(map[string]clawDeviceControlState, len(refs))
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.ID.Valid {
			ids = append(ids, ref.ID.String())
		}
	}
	if len(ids) == 0 || h.DB == nil {
		return overrides, nil
	}
	// node_id::text rather than = ANY($1::uuid[]): the caller already holds the
	// uuids as text, and comparing as text avoids depending on how pgx infers
	// the array element type from a []string.
	rows, err := h.DB.Query(ctx, `
		SELECT node_id::text, desired_state, desired_version
		FROM rongcloud_node_control
		WHERE node_id::text = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, state string
		var version int64
		if err := rows.Scan(&id, &state, &version); err != nil {
			return nil, err
		}
		overrides[id] = clawDeviceControlState{DesiredState: state, Version: version}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return overrides, nil
}

// applyDeviceControlOverrides merges stored rows over the "running by default"
// baseline. Agents with no row default to running: the supervisor's baseline is
// "start everything bound", and only an explicit operator decision overrides
// it. Pure, so that rule is unit-testable without a database.
func applyDeviceControlOverrides(refs []rongcloud.AgentNodeRef, overrides map[string]clawDeviceControlState) []clawDeviceControlState {
	states := make([]clawDeviceControlState, 0, len(refs))
	for _, ref := range refs {
		state := clawDeviceControlState{
			Agent:        ref.Agent,
			NodeID:       ref.NodeID,
			DesiredState: rongcloud.NodeDesiredRunning,
		}
		if ref.ID.Valid {
			if row, ok := overrides[ref.ID.String()]; ok {
				if row.DesiredState != "" {
					state.DesiredState = row.DesiredState
				}
				state.Version = row.Version
			}
		}
		states = append(states, state)
	}
	return states
}

// ---- 按账号启停（设备端 / 运维助手） ---------------------------------------
//
// 管理端一直是按节点启停（/api/admin/nodes/{id}/start|stop），但设备侧的运维
// 助手只有整机级的 `clawmessenger stop/restart` —— 一句话就能把同机其他账号
// 的智能体一起拉下来。这条接口把「启停某一个智能体」的能力下放到设备凭据，
// 让运维助手（以及后台）用同一张 control 表、同一套收敛逻辑，粒度收窄到账号。

// controlActor 是发起启停的节点身份。
type controlActor struct {
	NodeID          string
	RongcloudUserID string
	AIType          string
	// IsMachine 真 = 机器主节点（supervisor 的凭据）。它代表整台设备，可以
	// 操作本机任意 agent —— 与后台管理端同一语义。
	IsMachine bool
}

// controlTargetAllowed 决定 actor 能不能启停 target。规则按优先级：
//
//  1. 机器主节点：本机任意 agent（supervisor / 后台语义）；
//  2. per-agent 运维节点（"<ownerRC>_ops"）：只允许它服务的那个账号 ——
//     「每个设备的运维助手只准操作本账号」在服务端的落点。CLI 的
//     CLAWMESSENGER_OPS_SCOPE 是第一道，绕不过这里这一道；
//  3. 机器级 ops（旧 CLI，去掉 "_ops" 不命中本机任何 agent）：全机兼容；
//  4. 其他 agent 节点：只允许操作自己。
//
// 纯函数，便于单测（不碰 DB / IM）。
func controlTargetAllowed(actor controlActor, refs []rongcloud.AgentNodeRef, target rongcloud.AgentNodeRef) bool {
	if actor.IsMachine {
		return true
	}
	if owner, perAgent := perAgentOpsOwner(actor.RongcloudUserID, refs); perAgent {
		return target.RongcloudUserID != "" && target.RongcloudUserID == owner
	}
	if actor.AIType == rongcloud.OpsAgentType {
		return true
	}
	return actor.NodeID != "" && actor.NodeID == target.NodeID
}

// matchControlTarget 在本机 agent 清单里找 req.Target 指向的那一个。
//
// 三种写法都认：agent 名（hermes）、节点 uuid、融云 id（rc_node_<key>_hermes）。
//
// 认融云 id 是给老设备的兜底：0.2.45 及更早的 CLI 从 <machineRC>_<agent> 里
// 取 agent 名时算错，会把 rc_node_<key> 当 target 发上来。只认名字的话它们一律
// 404「本机没有这个智能体」，而 CLI 又把 404 译成「接口还没上线」——用户看到
// 的是「服务端要升级」，实际是名字算错了（2026-10-20 12:43 的现场）。
// 认了 rc id，这些设备不升级也能按账号启停；权限仍由 controlTargetAllowed 判。
func matchControlTarget(refs []rongcloud.AgentNodeRef, target string) *rongcloud.AgentNodeRef {
	if target == "" {
		return nil
	}
	for i := range refs {
		if refs[i].NodeID == target || refs[i].Agent == target || refs[i].RongcloudUserID == target {
			return &refs[i]
		}
	}
	return nil
}

// perAgentOpsOwner 判定 rc 是否 per-agent 运维账号，并返回它服务的 agent rc。
//
// 旧机器级 ops 也叫 "rc_node_<key>_ops"，同样以 "_ops" 结尾，所以不能只看
// 后缀：去掉后缀后必须命中本机某个 agent 的融云 id，才算 per-agent。
func perAgentOpsOwner(rcUserID string, refs []rongcloud.AgentNodeRef) (string, bool) {
	if !strings.HasSuffix(rcUserID, "_ops") {
		return "", false
	}
	owner := strings.TrimSuffix(rcUserID, "_ops")
	for _, ref := range refs {
		if ref.RongcloudUserID == owner {
			return owner, true
		}
	}
	return "", false
}

// ClawDeviceControlSet POST /api/claw/device/control/set —— 设备端按账号启停。
//
// 认证与 device/control 一致（设备凭据）。action: start | stop | restart。
// restart 复用 running 状态，但把 desired_version 强制 +1；supervisor 看到
// 「状态没变、版本变了」就杀进程重拉 —— 只重启这一个 agent，不碰别人。
func (h *Handler) ClawDeviceControlSet(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	if h.DB == nil {
		clawJSON(w, 503, 503, "数据库不可用", nil)
		return
	}
	var req struct {
		NodeID       string `json:"nodeId"`
		CredentialID string `json:"credentialId"`
		Secret       string `json:"secret"`
		Target       string `json:"target"`
		Action       string `json:"action"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil ||
		req.NodeID == "" || req.CredentialID == "" || req.Secret == "" || req.Target == "" {
		clawJSON(w, 400, 400, "缺少设备凭据或目标智能体", nil)
		return
	}

	desired := rongcloud.NodeDesiredRunning
	forceRestart := false
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "", "start":
		desired = rongcloud.NodeDesiredRunning
	case "stop":
		desired = rongcloud.NodeDesiredStopped
	case "restart":
		desired = rongcloud.NodeDesiredRunning
		forceRestart = true
	default:
		clawJSON(w, 400, 400, "action 只能是 start / stop / restart", nil)
		return
	}

	ctx := r.Context()
	actorNode, err := h.RongCloudNode.AuthorizeDevice(ctx, req.NodeID, req.CredentialID, req.Secret)
	if err != nil {
		if errors.Is(err, rongcloud.ErrInvalidDeviceCredential) {
			clawJSON(w, 401, 401, "设备凭据无效", nil)
			return
		}
		slog.Warn("claw: device control actor lookup failed", "nodeId", req.NodeID, "error", err)
		clawJSON(w, 500, 500, "校验设备凭据失败", nil)
		return
	}
	refs, err := h.RongCloudNode.AgentNodesForDevice(ctx, req.NodeID, req.CredentialID, req.Secret)
	if err != nil {
		if errors.Is(err, rongcloud.ErrInvalidDeviceCredential) {
			clawJSON(w, 401, 401, "设备凭据无效", nil)
			return
		}
		slog.Warn("claw: device control refs failed", "nodeId", req.NodeID, "error", err)
		clawJSON(w, 500, 500, "查询本机智能体失败", nil)
		return
	}

	target := matchControlTarget(refs, req.Target)
	if target == nil {
		clawJSON(w, 404, 404, "本机没有这个智能体", nil)
		return
	}
	if !target.ID.Valid {
		clawJSON(w, 409, 409, "该智能体还没有可控制的节点记录", nil)
		return
	}

	actorIsMachine := true
	for _, ref := range refs {
		if ref.ID == actorNode.ID {
			actorIsMachine = false
			break
		}
	}
	actor := controlActor{
		NodeID:          actorNode.NodeID,
		RongcloudUserID: actorNode.RongcloudUserID,
		AIType:          actorNode.AiType.String,
		IsMachine:       actorIsMachine,
	}
	if !controlTargetAllowed(actor, refs, *target) {
		clawJSON(w, 403, 403, "只能操作本账号的智能体", nil)
		return
	}

	version, err := h.clawDeviceSetNodeControl(ctx, target.ID, desired, "device:"+actorNode.NodeID, forceRestart)
	if err != nil {
		slog.Warn("claw: device control write failed",
			"nodeId", req.NodeID, "target", target.NodeID, "error", err)
		clawJSON(w, 500, 500, "下发启停指令失败", nil)
		return
	}
	clawJSON(w, 200, 200, "", map[string]interface{}{
		"agent":        target.Agent,
		"nodeId":       target.NodeID,
		"desiredState": desired,
		"version":      version,
		"restart":      forceRestart,
	})
}

// clawDeviceSetNodeControl writes operator intent. forceVersion=true 时无论
// 状态是否变化都推进版本（restart 的语义：状态仍是 running，但要求重拉）。
func (h *Handler) clawDeviceSetNodeControl(
	ctx context.Context,
	nodeUUID pgtype.UUID,
	state, actor string,
	forceVersion bool,
) (int64, error) {
	var version int64
	err := h.DB.QueryRow(ctx, `
		INSERT INTO rongcloud_node_control (node_id, workspace_id, desired_state, desired_version, changed_by)
		SELECT n.id, n.workspace_id, $2, 1, $3
		  FROM rongcloud_node n
		 WHERE n.id = $1
		ON CONFLICT (node_id) DO UPDATE
		   SET desired_state = EXCLUDED.desired_state,
		       desired_version = rongcloud_node_control.desired_version
		                         + CASE WHEN $4 OR rongcloud_node_control.desired_state IS DISTINCT FROM EXCLUDED.desired_state
		                                THEN 1 ELSE 0 END,
		       changed_by = EXCLUDED.changed_by,
		       updated_at = now()
		RETURNING desired_version`,
		nodeUUID, state, pgtype.Text{String: actor, Valid: actor != ""}, forceVersion).Scan(&version)
	return version, err
}
