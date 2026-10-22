package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Device-code pairing: a terminal client (clawmessenger pair without
// --ticket) anonymously creates a pairing session and gets back an 8-char
// user code plus a verify URL. The logged-in human opens the URL, reviews
// the machine's hostname and reported agent CLIs, and authorizes binding.
// The ticket never needs to be copied by a human again.

// defaultClawWebBaseURL is used when CLAW_WEB_BASE_URL is unset. It points
// at the deployed web product so a locally-built server still emits working
// verify links.
const defaultClawWebBaseURL = "https://newsradar.dreamdt.cn/claw-web"

// devicePairRateWindow / devicePairRateMax cap anonymous session creation
// per machine_id: 3 sessions per 10 minutes. Enough for retries, hostile to
// row spam.
const (
	devicePairRateWindow = 10 * time.Minute
	devicePairRateMax    = 3
)

// newDevicePairLimiter keeps the wiring in one place for the Handler literal.
func newDevicePairLimiter() *rongcloud.DevicePairLimiter {
	return rongcloud.NewDevicePairLimiter(devicePairRateMax, devicePairRateWindow)
}

// clawDevicePairingView is the anonymous poll response for the terminal: it
// exposes only binding progress, never the user code or workspace.
type clawDevicePairingView struct {
	Status      string                 `json:"status"`
	ExpiresAt   string                 `json:"expires_at"`
	BoundAgents []clawDeviceBoundAgent `json:"bound_agents,omitempty"`
}

type clawDeviceBoundAgent struct {
	Agent  string `json:"agent"`
	NodeID string `json:"node_id"`
}

// clawPairingByCodeView is the logged-in verify-page response: enough to
// render the authorization form, minus machine_id/ticket/claim keys.
type clawPairingByCodeView struct {
	UserCode    string                 `json:"user_code"`
	Status      string                 `json:"status"`
	Hostname    string                 `json:"hostname"`
	Platform    string                 `json:"platform"`
	Agents      []string               `json:"agents"`
	BoundAgents []clawDeviceBoundAgent `json:"bound_agents"`
	ExpiresAt   string                 `json:"expires_at"`
}

func clawWebBaseURL() string {
	if base := strings.TrimRight(os.Getenv("CLAW_WEB_BASE_URL"), "/"); base != "" {
		return base
	}
	return defaultClawWebBaseURL
}

func deviceBoundAgentsFromSession(session db.RongcloudPairingSession) []clawDeviceBoundAgent {
	out := []clawDeviceBoundAgent{}
	if len(session.BoundAgents) == 0 {
		return out
	}
	var bound []rongcloud.BoundAgentResult
	if err := json.Unmarshal(session.BoundAgents, &bound); err != nil {
		return out
	}
	for _, b := range bound {
		out = append(out, clawDeviceBoundAgent{Agent: b.Agent, NodeID: b.NodeID})
	}
	return out
}

// ClawCreateDeviceSession POST /api/claw/pairing/device — anonymous.
// The terminal reports machine identity + discovered agent CLIs and receives
// the pairing ticket (kept internal), the 8-char user code and the verify
// URL to show the user.
func (h *Handler) ClawCreateDeviceSession(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudPairing == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	var req struct {
		MachineID string   `json:"machine_id"`
		Hostname  string   `json:"hostname"`
		Platform  string   `json:"platform"`
		Agents    []string `json:"agents"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		clawJSON(w, 400, 400, "请求体无法解析", nil)
		return
	}
	req.MachineID = strings.TrimSpace(req.MachineID)
	if req.MachineID == "" {
		clawJSON(w, 400, 400, "缺少 machine_id", nil)
		return
	}
	if !h.devicePairLimiter.Allow(req.MachineID) {
		clawJSON(w, 429, 429, "请求过于频繁，请稍后再试", nil)
		return
	}
	wsID, ok := h.clawSingleInstallWorkspace(r.Context())
	if !ok {
		clawJSON(w, 503, 503, "RongCloud 渠道未安装", nil)
		return
	}
	session, err := h.RongCloudPairing.CreateDeviceSession(r.Context(), rongcloud.DeviceSessionParams{
		WorkspaceID: wsID,
		MachineID:   req.MachineID,
		Hostname:    req.Hostname,
		Platform:    req.Platform,
		Agents:      req.Agents,
		ExpiresIn:   15 * time.Minute,
	})
	if err != nil {
		slog.Warn("claw: create device pairing session failed", "error", err, "machine_prefix", req.MachineID[:min(6, len(req.MachineID))])
		clawJSON(w, 500, 500, "创建配对会话失败", nil)
		return
	}
	verifyURL := clawWebBaseURL() + "/pair?code=" + session.UserCode.String
	clawJSON(w, 200, 200, "ok", map[string]interface{}{
		"ticket":           session.Ticket,
		"user_code":        session.UserCode.String,
		"verify_url":       verifyURL,
		"expires_at":       session.ExpiresAt.Time.Format(time.RFC3339),
		"interval_seconds": 2,
	})
}

// ClawGetDeviceSession GET /api/claw/pairing/device/{ticket} — anonymous.
// The terminal polls this while waiting for the human to authorize.
func (h *Handler) ClawGetDeviceSession(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudPairing == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	ticket := chi.URLParam(r, "ticket")
	session, err := h.RongCloudPairing.GetSession(r.Context(), ticket)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			clawJSON(w, 404, 404, "配对会话不存在", nil)
			return
		}
		clawJSON(w, 500, 500, "查询配对会话失败", nil)
		return
	}
	clawJSON(w, 200, 200, "ok", clawDevicePairingView{
		Status:      rongcloud.SessionEffectiveStatus(session, time.Now()),
		ExpiresAt:   session.ExpiresAt.Time.Format(time.RFC3339),
		BoundAgents: deviceBoundAgentsFromSession(session),
	})
}

// ClawGetPairingByCode GET /api/claw/pairing/code/{code} — Bearer auth via
// clawRequireClawUser. Backs the browser verify page.
func (h *Handler) ClawGetPairingByCode(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	if h.RongCloudPairing == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	code := chi.URLParam(r, "code")
	session, err := h.RongCloudPairing.GetSessionByUserCode(r.Context(), code)
	if err != nil {
		if errors.Is(err, rongcloud.ErrUnknownPairingTicket) {
			clawJSON(w, 404, 404, "配对码无效或已过期", nil)
			return
		}
		slog.Warn("claw: pairing by code lookup failed", "error", err, "userId", userID)
		clawJSON(w, 500, 500, "查询配对会话失败", nil)
		return
	}
	hostname, platform := clawDeviceSessionInfo(session)
	agents := []string{}
	if len(session.ReportedAgents) > 0 {
		_ = json.Unmarshal(session.ReportedAgents, &agents)
	}
	clawJSON(w, 200, 200, "ok", clawPairingByCodeView{
		UserCode:    session.UserCode.String,
		Status:      rongcloud.SessionEffectiveStatus(session, time.Now()),
		Hostname:    hostname,
		Platform:    platform,
		Agents:      agents,
		BoundAgents: deviceBoundAgentsFromSession(session),
		ExpiresAt:   session.ExpiresAt.Time.Format(time.RFC3339),
	})
}

// ClawBindPairingByCode POST /api/claw/pairing/code/{code}/bind — Bearer
// auth. The human selects the reported agents to bind; each becomes its own
// node with a friend edge to the authorizing user, exactly like the legacy
// ticket bind dialog.
func (h *Handler) ClawBindPairingByCode(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.clawRequireClawUser(w, r)
	if !ok {
		return
	}
	if h.RongCloudPairing == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	code := chi.URLParam(r, "code")
	session, err := h.RongCloudPairing.GetSessionByUserCode(r.Context(), code)
	if err != nil {
		if errors.Is(err, rongcloud.ErrUnknownPairingTicket) {
			clawJSON(w, 404, 404, "配对码无效或已过期", nil)
			return
		}
		slog.Warn("claw: pairing by code lookup failed on bind", "error", err, "userId", userID)
		clawJSON(w, 500, 500, "查询配对会话失败", nil)
		return
	}
	if rongcloud.SessionEffectiveStatus(session, time.Now()) == "expired" {
		clawJSON(w, 410, 410, "链接已过期，请在终端重新运行 clawmessenger pair", nil)
		return
	}
	if session.Status != "claimed" {
		clawJSON(w, 409, 409, "设备尚未连接，请先在终端运行 clawmessenger pair", nil)
		return
	}
	var req struct {
		Agents []string `json:"agents"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || len(req.Agents) == 0 {
		clawJSON(w, 400, 400, "请至少选择一个智能体", nil)
		return
	}
	// Only agents the device itself reported may be bound: the verify page
	// renders from reported_agents, so this closes the loop against a
	// hand-crafted bind of arbitrary agent names.
	reported := map[string]bool{}
	if len(session.ReportedAgents) > 0 {
		var names []string
		if err := json.Unmarshal(session.ReportedAgents, &names); err == nil {
			for _, a := range names {
				reported[a] = true
			}
		}
	}
	for _, a := range req.Agents {
		if !reported[a] {
			clawJSON(w, 400, 400, "智能体未在设备上检测到："+a, nil)
			return
		}
	}
	// Checked here rather than at the top so the validation verdicts above are
	// still reachable when the node service is absent (tests, partial wiring).
	if h.RongCloudNode == nil {
		clawJSON(w, 503, 503, "RongCloud 集成未配置", nil)
		return
	}
	results, err := h.RongCloudNode.BindAgents(r.Context(), session, req.Agents, userID)
	if err != nil {
		switch {
		case errors.Is(err, rongcloud.ErrPairingSessionNotClaimed):
			clawJSON(w, 409, 409, "设备尚未完成配对", nil)
		case errors.Is(err, rongcloud.ErrPairingSessionExpired):
			clawJSON(w, 410, 410, "链接已过期，请在终端重新运行 clawmessenger pair", nil)
		default:
			slog.Warn("claw: bind pairing by code failed", "error", err, "userId", userID)
			clawJSON(w, 500, 500, "绑定智能体失败", nil)
		}
		return
	}
	out := make([]clawDeviceBoundAgent, 0, len(results))
	for _, b := range results {
		out = append(out, clawDeviceBoundAgent{Agent: b.Agent, NodeID: b.NodeID})
	}
	clawJSON(w, 200, 200, "授权完成", map[string]interface{}{"bound": out})
}

// clawDeviceSessionInfo extracts hostname/platform from the device_info
// JSONB column; missing or malformed info degrades to empty strings.
func clawDeviceSessionInfo(session db.RongcloudPairingSession) (string, string) {
	if len(session.DeviceInfo) == 0 {
		return "", ""
	}
	var info map[string]string
	if err := json.Unmarshal(session.DeviceInfo, &info); err != nil {
		return "", ""
	}
	return info["hostname"], info["platform"]
}
