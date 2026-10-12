package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

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
func (h *Handler) ClawDeviceControl(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	var req struct {
		NodeID       string `json:"nodeId"`
		CredentialID string `json:"credentialId"`
		Secret       string `json:"secret"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil ||
		req.NodeID == "" || req.CredentialID == "" || req.Secret == "" {
		clawJSON(w, 400, 400, "缺少设备凭据", nil)
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
	clawJSON(w, 200, 200, "", map[string]interface{}{"agents": states})
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
