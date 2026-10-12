package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// NodeRequestBroker brokers server-initiated requests that a device answers over
// the IM `command` channel. The first (and currently only) client is the admin
// 「系统 → 讨论主持 AI → 测试角色推荐」 button, but the shape is general:
// start → send → await → validate.
//
// The legacy Python backend persisted these to `node_requests` /
// `role_recommendations` because it ran multi-process behind gunicorn, so the
// webhook receiving the answer could land on a different worker than the one
// waiting for it. The Go server runs the admin API and the RongCloud webhook in
// the same process, so the answer reaches the waiting goroutine directly and a
// table would only add a schema and a failure mode. Recommendations are also
// pure previews here: nothing in the Go discussion runtime consumes them
// (speakers come from rongcloud_chatroom state), so there is nothing to persist
// for later.
type NodeRequestBroker struct {
	logger *slog.Logger

	mu      sync.Mutex
	send    NodeRequestSender
	pending map[string]*pendingNodeRequest
	now     func() time.Time
}

// NodeRequestSender posts a `command` message to one node's RongCloud user id,
// as the system node. Bound by the RongCloud channel once it has resolved its
// credentials; nil until then, which makes a request fail fast instead of
// hanging until the timeout.
type NodeRequestSender func(ctx context.Context, toUserID, content string) error

// NodeRequestHost is the slice of the platform discussion-host configuration the
// broker needs. Defined here so the integration package does not have to import
// the admin handler's config type.
type NodeRequestHost struct {
	AIUserID             string
	DisplayName          string
	Avatar               string
	RecommendationPrompt string
	TimeoutSeconds       int
	ConfigVersion        int
}

// Public error codes the admin page is allowed to see. Mirrors
// DISCUSSION_HOST_PUBLIC_ERRORS in the legacy admin/routes/system.py, so the
// frontend's existing copy keeps working.
const (
	NodeRequestCodeHostNotConfigured = "SYSTEM_HOST_NOT_CONFIGURED"
	NodeRequestCodeHostDisabled      = "SYSTEM_HOST_DISABLED"
	NodeRequestCodeHostOffline       = "SYSTEM_HOST_OFFLINE"
	NodeRequestCodeRecommendTimeout  = "ROLE_RECOMMENDATION_TIMEOUT"
	NodeRequestCodeRecommendInvalid  = "ROLE_RECOMMENDATION_INVALID"
)

// The discussion runtime's role-recommendation request, as it rides the shared
// `{request_id, service, action, params}` command envelope. Keep in sync with
// the `recommend_roles` branch in packages/xiachat-cli/src/im.ts.
const (
	recommendRolesService   = "discussion"
	recommendRolesAction    = "recommend_roles"
	recommendRolesMaxName   = 80
	recommendRolesMaxPrompt = 8000
	recommendRolesTTL       = 7 * 24 * time.Hour

	// RecommendRolesMaxRoles bounds how many roles a single recommendation may
	// return. Shared with the admin handler so the request and the validation
	// agree on the ceiling.
	RecommendRolesMaxRoles = 8
)

type pendingNodeRequest struct {
	requestID string
	kind      string
	host      NodeRequestHost
	maxRoles  int

	targetAIUserID string
	expiresAt      time.Time
	done           chan struct{}

	// Terminal state. Written once, under the broker mutex, before done closes;
	// read only after done is closed.
	settled   bool
	status    string
	errorCode string
	roles     []map[string]interface{}
}

func NewNodeRequestBroker(logger *slog.Logger) *NodeRequestBroker {
	if logger == nil {
		logger = slog.Default()
	}
	return &NodeRequestBroker{
		logger:  logger,
		pending: make(map[string]*pendingNodeRequest),
		now:     time.Now,
	}
}

// BindSender wires the channel that can actually put a `command` on the wire.
// Called from the channel factory once per connect; the last binder wins, which
// is correct for the single-installation deployment the platform runs.
func (b *NodeRequestBroker) BindSender(send NodeRequestSender) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.send = send
}

// StartRecommendation records a pending request and sends it. Returns the
// request id, or a public error code when it could not be sent.
func (b *NodeRequestBroker) StartRecommendation(ctx context.Context, host NodeRequestHost, topic, goal string, maxRoles int) (string, string) {
	if host.AIUserID == "" {
		return "", NodeRequestCodeHostNotConfigured
	}

	requestID := "roles_" + uuid.NewString()
	timeout := time.Duration(host.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	content, err := json.Marshal(map[string]interface{}{
		"request_id": requestID,
		"service":    recommendRolesService,
		"action":     recommendRolesAction,
		"params": map[string]interface{}{
			"topic":                 topic,
			"goal":                  goal,
			"max_roles":             maxRoles,
			"recommendation_prompt": host.RecommendationPrompt,
			"config_version":        host.ConfigVersion,
			"protocol_version":      "v1",
			// 融云在客户端每次重连时会把离线历史整段重放；设备端用它丢弃
			// 早已无人等待的旧请求，避免为一次重放白跑一遍 agent。
			"timestamp": b.now().UnixMilli(),
		},
	})
	if err != nil {
		return "", NodeRequestCodeRecommendInvalid
	}

	b.mu.Lock()
	if b.send == nil {
		b.mu.Unlock()
		// The integration is enabled but the channel has not connected yet, so
		// there is no wire to send on — that is an offline host, not a bad
		// request.
		return "", NodeRequestCodeHostOffline
	}
	send := b.send
	record := &pendingNodeRequest{
		requestID:      requestID,
		kind:           recommendRolesAction,
		host:           host,
		maxRoles:       maxRoles,
		targetAIUserID: host.AIUserID,
		expiresAt:      b.now().Add(timeout),
		done:           make(chan struct{}),
		status:         "pending",
	}
	b.pending[requestID] = record
	b.mu.Unlock()

	if err := send(ctx, host.AIUserID, string(content)); err != nil {
		b.settle(requestID, "", NodeRequestCodeRecommendInvalid, nil)
		b.logger.Warn("role recommendation: send failed",
			"request_id", requestID, "ai_user_id", host.AIUserID, "error", err)
		return "", NodeRequestCodeRecommendInvalid
	}
	b.logger.Info("role recommendation: request sent",
		"request_id", requestID, "ai_user_id", host.AIUserID, "max_roles", maxRoles)
	return requestID, ""
}

// AwaitRecommendation blocks until the node answers, the request expires, or the
// caller's context is cancelled. It returns the admin payload plus an empty
// error code, or a nil payload plus a public error code.
func (b *NodeRequestBroker) AwaitRecommendation(ctx context.Context, requestID string) (map[string]interface{}, string) {
	b.mu.Lock()
	record, ok := b.pending[requestID]
	b.mu.Unlock()
	if !ok {
		return nil, NodeRequestCodeRecommendInvalid
	}

	timer := time.NewTimer(time.Until(record.expiresAt))
	defer timer.Stop()

	select {
	case <-record.done:
	case <-timer.C:
		b.settle(requestID, "", NodeRequestCodeRecommendTimeout, nil)
		<-record.done
	case <-ctx.Done():
		b.settle(requestID, "", NodeRequestCodeRecommendTimeout, nil)
		<-record.done
	}

	b.mu.Lock()
	delete(b.pending, requestID)
	status, errorCode, roles := record.status, record.errorCode, record.roles
	b.mu.Unlock()

	if status != "completed" {
		if errorCode == "" {
			errorCode = NodeRequestCodeRecommendTimeout
		}
		return nil, errorCode
	}
	return b.recommendationPayload(record, roles), ""
}

// RecommendRoles runs a full start + await round trip.
func (b *NodeRequestBroker) RecommendRoles(ctx context.Context, host NodeRequestHost, topic, goal string, maxRoles int) (map[string]interface{}, string) {
	requestID, code := b.StartRecommendation(ctx, host, topic, goal, maxRoles)
	if code != "" {
		return nil, code
	}
	return b.AwaitRecommendation(ctx, requestID)
}

// AcceptResponse hands an inbound node `command_result` to the broker. It
// reports whether the message belonged to a request this broker issued, so the
// caller can stop before treating it as a discussion turn. Request ids are
// namespaced (`roles_…` versus the coordinator's `turn_…`), so an unrelated
// command_result simply falls through.
func (b *NodeRequestBroker) AcceptResponse(senderID, content string) bool {
	var result struct {
		RequestID string                 `json:"request_id"`
		MsgType   string                 `json:"msg_type"`
		Payload   map[string]interface{} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return false
	}
	if result.MsgType != "command_result" || result.RequestID == "" {
		return false
	}

	b.mu.Lock()
	record, ok := b.pending[result.RequestID]
	status := ""
	if ok {
		status = record.status
	}
	b.mu.Unlock()
	if !ok {
		return false
	}

	// Owning the id is enough to claim the message, even when we refuse to
	// settle it: from here on it is certainly not a discussion turn.
	if status != "pending" {
		return true
	}
	if record.targetAIUserID != senderID {
		b.logger.Warn("role recommendation: answer from unexpected node",
			"request_id", result.RequestID, "expected", record.targetAIUserID, "got", senderID)
		return true
	}
	if b.now().After(record.expiresAt) {
		b.settle(result.RequestID, "", NodeRequestCodeRecommendTimeout, nil)
		return true
	}

	if raw, ok := result.Payload["roles"]; ok {
		roles, err := parseRecommendedRoles(raw, record.maxRoles)
		if err != nil {
			b.logger.Warn("role recommendation: invalid roles",
				"request_id", result.RequestID, "error", err)
			b.settle(result.RequestID, "", NodeRequestCodeRecommendInvalid, nil)
			return true
		}
		b.settle(result.RequestID, "completed", "", roles)
		return true
	}

	b.settle(result.RequestID, "", mapNodeError(result.Payload["error"]), nil)
	return true
}

// settle records the terminal state and wakes the waiter. First settle wins, so
// a late or duplicated answer cannot overwrite a timeout.
func (b *NodeRequestBroker) settle(requestID, status, errorCode string, roles []map[string]interface{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.pending[requestID]
	if !ok || record.settled {
		return
	}
	record.settled = true
	record.status = status
	record.errorCode = errorCode
	record.roles = roles
	close(record.done)
}

// recommendationPayload renders the admin response: the stored roles get stable
// ids (the same request always yields the same ids) and the host's display
// identity, matching DiscussionHostRecommendation in the admin web.
func (b *NodeRequestBroker) recommendationPayload(record *pendingNodeRequest, roles []map[string]interface{}) map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(roles))
	for index, role := range roles {
		out = append(out, map[string]interface{}{
			"role_id":        uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("%s:%d", record.requestID, index))).String(),
			"role_name":      role["role_name"],
			"role_prompt":    role["role_prompt"],
			"speaking_order": role["speaking_order"],
		})
	}
	return map[string]interface{}{
		"recommendation_id": uuid.NewSHA1(uuid.NameSpaceURL, []byte("recommendation:"+record.requestID)).String(),
		"roles":             out,
		"config_version":    record.host.ConfigVersion,
		"expires_at":        b.now().Add(recommendRolesTTL).UTC().Format(time.RFC3339),
		"host": map[string]interface{}{
			"display_name": record.host.DisplayName,
			"avatar":       record.host.Avatar,
		},
	}
}

// parseRecommendedRoles validates the node's answer. Ported from
// NodeRequestBroker._parse_roles in the legacy discussion/node_requests.py
// (non-strict branch — the admin preview passes max_roles, never a candidate
// model list): names non-empty and unique, prompts non-empty, and speaking
// order an exact permutation of 0..n-1.
func parseRecommendedRoles(raw interface{}, maxRoles int) ([]map[string]interface{}, error) {
	if raw == nil {
		return nil, fmt.Errorf("roles missing")
	}
	list, ok := raw.([]interface{})
	if !ok || len(list) == 0 || len(list) > maxRoles {
		return nil, fmt.Errorf("roles must be a list of 1..%d", maxRoles)
	}

	names := make(map[string]struct{}, len(list))
	orders := make([]int, 0, len(list))
	normalized := make([]map[string]interface{}, 0, len(list))

	for _, item := range list {
		role, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("role must be an object")
		}
		name, err := roleText(role["role_name"], recommendRolesMaxName)
		if err != nil {
			return nil, fmt.Errorf("role_name: %w", err)
		}
		prompt, err := roleText(role["role_prompt"], recommendRolesMaxPrompt)
		if err != nil {
			return nil, fmt.Errorf("role_prompt: %w", err)
		}
		if _, dup := names[name]; dup {
			return nil, fmt.Errorf("duplicate role_name %q", name)
		}
		order, ok := roleOrder(role["speaking_order"])
		if !ok {
			return nil, fmt.Errorf("speaking_order must be an integer")
		}
		names[name] = struct{}{}
		orders = append(orders, order)
		normalized = append(normalized, map[string]interface{}{
			"role_name":      name,
			"role_prompt":    prompt,
			"speaking_order": order,
		})
	}

	sortedOrders := append([]int(nil), orders...)
	sort.Ints(sortedOrders)
	for i, order := range sortedOrders {
		if order != i {
			return nil, fmt.Errorf("speaking_order must be a permutation of 0..%d", len(list)-1)
		}
	}
	sort.SliceStable(normalized, func(i, j int) bool {
		return normalized[i]["speaking_order"].(int) < normalized[j]["speaking_order"].(int)
	})
	return normalized, nil
}

// roleText trims a role field and enforces its length bound, counting runes so a
// CJK role name is not rejected at a third of the intended budget.
func roleText(value interface{}, maximum int) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("must be text")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("must not be empty")
	}
	if len([]rune(text)) > maximum {
		return "", fmt.Errorf("too long")
	}
	return text, nil
}

// roleOrder accepts the integer forms a JSON-producing agent might emit: a
// number (float64 after encoding/json), or a numeric string. Mirrors the legacy
// parser's permissiveness so a correct answer is not thrown away.
func roleOrder(value interface{}) (int, bool) {
	switch v := value.(type) {
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
	case int:
		return v, true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n), true
		}
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return 0, false
		}
		n, err := strconv.Atoi(trimmed)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// mapNodeError collapses everything a node can report into the two codes the
// admin page understands, exactly like NodeRequestBroker._map_node_error.
func mapNodeError(value interface{}) string {
	text, _ := value.(string)
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "timeout", "model_timeout", "role_recommendation_timeout":
		return NodeRequestCodeRecommendTimeout
	}
	return NodeRequestCodeRecommendInvalid
}
