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
// the IM `command` channel. Two clients: the admin 「系统 → 讨论主持 AI → 测试
// 角色推荐」 button (`roles_…`, command_result envelope) and the same page's
// 「供应商 → 模型」选择器 (`catalog_…`, discussion_model_catalog_response
// envelope). The shape is the same for both: start → send → await → validate.
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
	// Model 是主持节点在后台配的模型 route（`provider/slug`），空串 = 不指定、
	// 沿用设备默认模型。
	//
	// 推荐的耗时几乎全在设备端那一次 agent 回合里：3 段 120 字人设 + 一层 JSON
	// 的短任务，旗舰模型要二三十秒，快模型 2-3 秒。不改配置的话这里恒空，
	// 运营在「讨论主持 AI 模型」里选的东西对推荐不生效，只能干等默认模型。
	Model string
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
	// 模型目录往返的失败码（管理端「供应商 → 模型」选择器用）。
	NodeRequestCodeCatalogTimeout = "MODEL_CATALOG_TIMEOUT"
	NodeRequestCodeCatalogInvalid = "MODEL_CATALOG_INVALID"
	// NodeRequestCodeCatalogUnavailable 是**对外统一**的目录失败码：管理端接口
	// 把「节点不存在 / 不是主持节点 / 离线 / 超时 / 应答不合协议」全部折叠成它，
	// 精确原因只写服务端日志 —— 该接口会以系统账号向设备私发 IM 并触发一次本机
	// 探测，不能同时充当「某 node_id 是否存在 / 在线」的探测预言机。
	NodeRequestCodeCatalogUnavailable = "MODEL_CATALOG_UNAVAILABLE"
	// NodeRequestCodeModelNotAvailable 是「目录拿到了、但里面没有这个模型」：
	// 唯一一种应当拒绝保存的模型错误（离线/超时一律放行）。
	NodeRequestCodeModelNotAvailable = "MODEL_NOT_AVAILABLE"
)

// The discussion runtime's role-recommendation request, as it rides the shared
// `{request_id, service, action, params}` command envelope. Keep in sync with
// the `recommend_roles` branch in packages/xiachat-cli/src/im.ts.
const (
	recommendRolesService   = "discussion"
	recommendRolesAction    = "recommend_roles"
	recommendRolesMaxName   = 80
	recommendRolesMaxPrompt = 8000
	// recommendRolesMaxNodeID 与设备端（role-recommend.ts 的 segment 校验）以及
	// 命令参数校验共用同一个 node_id 长度上限。
	recommendRolesMaxNodeID = 128
	// recommendRolesMaxStyle 是讨论风格 id 的长度上限，与设备端
	// role-recommend.ts 的解析保持一致：更长的一律当没选。
	recommendRolesMaxStyle = 64
	recommendRolesTTL      = 7 * 24 * time.Hour

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
	// catalog 是 MODEL_CATALOG 族请求的终态（roles 族恒为零值）。两者共用
	// status/errorCode，靠 kind 区分，避免再造一套等待机制。
	catalog DiscussionModelCatalog
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
//
// style 是 C 端选的讨论风格 id（free / brainstorm / debate / review /
// consensus）。它为空时**不下发**这个键，旧设备端与旧行为都不受影响；设备端
// 认不出来时也会退回 goal 里的风格短语自行识别。
func (b *NodeRequestBroker) StartRecommendation(ctx context.Context, host NodeRequestHost, topic, goal string, maxRoles int, style string) (string, string) {
	if host.AIUserID == "" {
		return "", NodeRequestCodeHostNotConfigured
	}

	requestID := "roles_" + uuid.NewString()
	timeout := time.Duration(host.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	params := map[string]interface{}{
		"topic":                 topic,
		"goal":                  goal,
		"max_roles":             maxRoles,
		"recommendation_prompt": host.RecommendationPrompt,
		"config_version":        host.ConfigVersion,
		"protocol_version":      "v1",
		// 融云在客户端每次重连时会把离线历史整段重放；设备端用它丢弃
		// 早已无人等待的旧请求，避免为一次重放白跑一遍 agent。
		"timestamp": b.now().UnixMilli(),
	}
	if style != "" {
		params["style"] = style
	}
	// 只在配了模型时才下发这个键：老 CLI 不认识未知键（忽略即可），但把一个空串
	// 发过去会让它把「没指定」当成「指定了空模型」—— 设备端据非空判定，空串会被
	// 它自己过滤掉，这里不发更明确。
	if model := strings.TrimSpace(host.Model); model != "" {
		params["model"] = model
	}

	content, err := json.Marshal(map[string]interface{}{
		"request_id": requestID,
		"service":    recommendRolesService,
		"action":     recommendRolesAction,
		"params":     params,
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
		b.logger.Warn("role recommendation: no sender bound, reporting host offline",
			"ai_user_id", host.AIUserID)
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
		// 发送失败后调用方直接返回错误码，不会走 AwaitRecommendation 里的
		// delete —— 不在这里释放，这条记录会永久驻留 pending（无界泄漏）。
		b.forgetRequest(requestID)
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
func (b *NodeRequestBroker) RecommendRoles(ctx context.Context, host NodeRequestHost, topic, goal string, maxRoles int, style string) (map[string]interface{}, string) {
	requestID, code := b.StartRecommendation(ctx, host, topic, goal, maxRoles, style)
	if code != "" {
		return nil, code
	}
	return b.AwaitRecommendation(ctx, requestID)
}

// ---- 模型目录请求族（protocolVersion 2）---------------------------------
//
// 管理端「讨论主持 AI」的「供应商 → 模型」两级选择器需要主持节点自报的模型
// 目录。C 端是浏览器直连设备（clawmessenger-web/src/services/node-model-catalog.ts），
// 但管理端没有 IM SDK，所以由服务端代理这一次往返：内容和线上的 C 端请求
// **逐字节同构**（`msg_type` / `protocolVersion:2` / `requestId` / `timestamp`），
// 设备端 CLI 的 `parseModelCatalogRequest`（packages/xiachat-cli/src/model-catalog.ts）
// 因此无需任何改动就能应答。
//
// 请求 id 前缀 `catalog_`，与 `roles_` / `turn_` 区分：三条族共用一个 pending
// 表，靠前缀就能保证一个族永远看不到另一个族的回包。
const (
	catalogRequestMsgType  = "discussion_model_catalog_request"
	catalogResponseMsgType = "discussion_model_catalog_response"
	catalogProtocolVersion = 2
	catalogRequestPrefix   = "catalog_"

	// 与 web 的 normalizeCatalog 同一套上限（node-model-catalog.ts）：
	// 越界的响应整份作废，而不是截断——半份目录会让操作员选到一个不存在的模型。
	catalogMaxProviders     = 64
	catalogMaxModelsPerProv = 500
	catalogMaxSegmentLen    = 128
	catalogMaxLabelLen      = 500
)

// DiscussionModelCatalog 与 C 端的 NodeModelCatalog 同形
// （clawmessenger-web/src/types/discussion.ts），只是字段名改成 Go 的大写形式。
// 管理端页面据此渲染与「设备详情」页一致的级联选择器。
type DiscussionModelCatalog struct {
	// DefaultModel 是设备自己的默认模型 route（`provider/slug`），可能为空：
	// 设备没配默认、或默认不在目录里时前端拿不到它。
	DefaultModel string
	Providers    []DiscussionModelProvider
}

// DiscussionModelProvider 是一个已配置的供应商及其可枚举的模型。
type DiscussionModelProvider struct {
	ID     string
	Name   string
	Models []DiscussionModelEntry
}

// DiscussionModelEntry 是供应商下的一个模型（id 是 provider 内唯一的 slug）。
type DiscussionModelEntry struct {
	ID   string
	Name string
}

// StartModelCatalog records a pending catalog request and sends it to the host.
//
// force=true 表示「别拿缓存糊弄我」：管理端点「重新检测」时用。设备端据此跳过
// 「缓存还新鲜就直接答」的判定（packages/xiachat-cli 的 ModelCatalogLoader.load），
// 当轮重新枚举本机模型。老设备不认这个键、行为与从前一致，所以它可以随时下发。
func (b *NodeRequestBroker) StartModelCatalog(
	ctx context.Context, host NodeRequestHost, force bool,
) (string, string) {
	if host.AIUserID == "" {
		return "", NodeRequestCodeHostNotConfigured
	}

	requestID := catalogRequestPrefix + uuid.NewString()
	timeout := time.Duration(host.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	// 设备的 `parseModelCatalogRequest` 只认这几个键，多一个键它也不拒（它不
	// 做键集白名单），但保持与 web 逐字节同构能省掉一次联调排查 —— 所以
	// refresh 只在真的要强制时才出现，普通请求的报文与历史版本逐字节一致。
	payload := map[string]interface{}{
		"msg_type":        catalogRequestMsgType,
		"protocolVersion": catalogProtocolVersion,
		"requestId":       requestID,
		"timestamp":       b.now().UnixMilli(),
	}
	if force {
		payload["refresh"] = true
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return "", NodeRequestCodeCatalogInvalid
	}

	b.mu.Lock()
	if b.send == nil {
		b.mu.Unlock()
		b.logger.Warn("model catalog: no sender bound, reporting host offline",
			"ai_user_id", host.AIUserID)
		return "", NodeRequestCodeHostOffline
	}
	send := b.send
	b.pending[requestID] = &pendingNodeRequest{
		requestID:      requestID,
		kind:           catalogRequestMsgType,
		host:           host,
		targetAIUserID: host.AIUserID,
		expiresAt:      b.now().Add(timeout),
		done:           make(chan struct{}),
		status:         "pending",
	}
	b.mu.Unlock()

	if err := send(ctx, host.AIUserID, string(content)); err != nil {
		b.settleCatalog(requestID, "", NodeRequestCodeCatalogInvalid, DiscussionModelCatalog{})
		// 同 StartRecommendation：这一族也在热路径上（页面每次切节点都会拉一次
		// 目录），发送失败必须立刻释放记录，否则 pending 无界增长。
		b.forgetRequest(requestID)
		b.logger.Warn("model catalog: send failed",
			"request_id", requestID, "ai_user_id", host.AIUserID, "error", err)
		return "", NodeRequestCodeCatalogInvalid
	}
	b.logger.Info("model catalog: request sent",
		"request_id", requestID, "ai_user_id", host.AIUserID)
	return requestID, ""
}

// AwaitModelCatalog blocks until the node answers, the request expires, or the
// caller's context is cancelled.
func (b *NodeRequestBroker) AwaitModelCatalog(ctx context.Context, requestID string) (DiscussionModelCatalog, string) {
	b.mu.Lock()
	record, ok := b.pending[requestID]
	b.mu.Unlock()
	if !ok {
		return DiscussionModelCatalog{}, NodeRequestCodeCatalogInvalid
	}

	timer := time.NewTimer(time.Until(record.expiresAt))
	defer timer.Stop()

	select {
	case <-record.done:
	case <-timer.C:
		b.settleCatalog(requestID, "", NodeRequestCodeCatalogTimeout, DiscussionModelCatalog{})
		<-record.done
	case <-ctx.Done():
		b.settleCatalog(requestID, "", NodeRequestCodeCatalogTimeout, DiscussionModelCatalog{})
		<-record.done
	}

	b.mu.Lock()
	delete(b.pending, requestID)
	status, errorCode, catalog := record.status, record.errorCode, record.catalog
	b.mu.Unlock()

	if status != "completed" {
		if errorCode == "" {
			errorCode = NodeRequestCodeCatalogTimeout
		}
		return DiscussionModelCatalog{}, errorCode
	}
	return catalog, ""
}

// FetchModelCatalog runs a full start + await round trip.
//
// 它**只是**一次尽力而为的往返：调用方（管理端保存/目录查询）必须自己决定
// 「拿不到目录」时是否降级，而不是把这个错误当致命错误——主持节点常常离线。
func (b *NodeRequestBroker) FetchModelCatalog(
	ctx context.Context, host NodeRequestHost, force bool,
) (DiscussionModelCatalog, string) {
	requestID, code := b.StartModelCatalog(ctx, host, force)
	if code != "" {
		return DiscussionModelCatalog{}, code
	}
	return b.AwaitModelCatalog(ctx, requestID)
}

// AcceptCatalogResponse hands an inbound `discussion_model_catalog_response` to
// the broker. Reports whether the message belonged to a catalog request this
// broker issued — an unrelated command simply falls through to the discussion
// command dispatcher.
//
// 与 AcceptResponse 的关键差别是**入站信封不同**：catalog 响应不是
// `command_result`，它自己带 `msg_type`（web 的 handleNodeModelCatalogResponse
// 也是按 msg_type 直配的），所以必须在 channel 路由层单独识别。
func (b *NodeRequestBroker) AcceptCatalogResponse(senderID, content string) bool {
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return false
	}
	if result["msg_type"] != catalogResponseMsgType {
		return false
	}
	requestID, _ := result["requestId"].(string)
	if requestID == "" {
		return false
	}
	// protocolVersion 不符（旧设备/未来协议）时整份丢弃：设备端的
	// parseModelCatalogRequest 对请求侧也是同样处理，两端口径必须一致。
	if version, ok := result["protocolVersion"].(float64); !ok || int(version) != catalogProtocolVersion {
		return false
	}

	b.mu.Lock()
	record, ok := b.pending[requestID]
	status := ""
	if ok {
		status = record.status
	}
	b.mu.Unlock()
	if !ok {
		return false
	}

	// 认领这个 id 就到此为止：它肯定不是讨论命令，别再让分发器当命令处理。
	if status != "pending" {
		return true
	}
	if record.targetAIUserID != senderID {
		b.logger.Warn("model catalog: answer from unexpected node",
			"request_id", requestID, "expected", record.targetAIUserID, "got", senderID)
		return true
	}
	if b.now().After(record.expiresAt) {
		b.settleCatalog(requestID, "", NodeRequestCodeCatalogTimeout, DiscussionModelCatalog{})
		return true
	}

	catalog, err := normalizeModelCatalog(result)
	if err != nil {
		b.logger.Warn("model catalog: invalid payload",
			"request_id", requestID, "error", err)
		b.settleCatalog(requestID, "", NodeRequestCodeCatalogInvalid, DiscussionModelCatalog{})
		return true
	}
	b.settleCatalog(requestID, "completed", "", catalog)
	return true
}

// settleCatalog 是 model_catalog 族的 settle：与 settle 同一把锁、同一套
// 「首次结算取胜」语义，只是终态字段不同。
func (b *NodeRequestBroker) settleCatalog(requestID, status, errorCode string, catalog DiscussionModelCatalog) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.pending[requestID]
	if !ok || record.settled {
		return
	}
	record.settled = true
	record.status = status
	record.errorCode = errorCode
	record.catalog = catalog
	close(record.done)
}

// catalogSegment 是 route 的一段（provider id 或 model id）：非空、≤128 字符、
// 无空白、不含 `/`。与 web 的 segment() 一字不差（node-model-catalog.ts:18-25）：
// 两端口径一偏，管理端就会把一个 web 侧永远选不到的模型写进配置。
func catalogSegment(value interface{}) (string, bool) {
	text, ok := value.(string)
	if !ok || text == "" || len(text) > catalogMaxSegmentLen {
		return "", false
	}
	if strings.TrimSpace(text) != text || strings.Contains(text, "/") {
		return "", false
	}
	if strings.ContainsAny(text, " \t\r\n") {
		return "", false
	}
	return text, true
}

// catalogLabel 是展示名：非空、≤500 字符（web 的 label()）。
func catalogLabel(value interface{}) (string, bool) {
	text, ok := value.(string)
	if !ok || text == "" || len(text) > catalogMaxLabelLen {
		return "", false
	}
	return text, true
}

// normalizeModelCatalog 严格校验设备回的目录，键集/唯一性/长度任何一项不符
// 就整份作废。移植自 web 的 normalizeCatalog：宁可报「目录不可用」，也不要
// 把半份目录渲染成下拉框让操作员保存一个设备根本跑不了的模型。
func normalizeModelCatalog(payload map[string]interface{}) (DiscussionModelCatalog, error) {
	rawProviders, ok := payload["providers"]
	if !ok || rawProviders == nil {
		return DiscussionModelCatalog{}, fmt.Errorf("providers missing")
	}
	list, ok := rawProviders.([]interface{})
	if !ok || len(list) > catalogMaxProviders {
		return DiscussionModelCatalog{}, fmt.Errorf("providers must be a list of at most %d", catalogMaxProviders)
	}

	defaultModel := ""
	if raw, ok := payload["defaultModel"]; ok && raw != nil {
		text, ok := raw.(string)
		if !ok {
			return DiscussionModelCatalog{}, fmt.Errorf("defaultModel must be text or null")
		}
		defaultModel = text
	}

	providers := make([]DiscussionModelProvider, 0, len(list))
	routes := make(map[string]struct{})
	seenProviders := make(map[string]struct{}, len(list))
	for _, item := range list {
		entry, ok := item.(map[string]interface{})
		if !ok {
			return DiscussionModelCatalog{}, fmt.Errorf("provider must be an object")
		}
		id, ok := catalogSegment(entry["id"])
		if !ok {
			return DiscussionModelCatalog{}, fmt.Errorf("provider id invalid")
		}
		name, ok := catalogLabel(entry["name"])
		if !ok {
			return DiscussionModelCatalog{}, fmt.Errorf("provider name invalid")
		}
		if _, dup := seenProviders[id]; dup {
			return DiscussionModelCatalog{}, fmt.Errorf("duplicate provider %q", id)
		}
		seenProviders[id] = struct{}{}
		rawModels, ok := entry["models"].([]interface{})
		if !ok || len(rawModels) > catalogMaxModelsPerProv {
			return DiscussionModelCatalog{}, fmt.Errorf("models must be a list of at most %d", catalogMaxModelsPerProv)
		}
		models := make([]DiscussionModelEntry, 0, len(rawModels))
		seenModels := make(map[string]struct{}, len(rawModels))
		for _, rawModel := range rawModels {
			model, ok := rawModel.(map[string]interface{})
			if !ok {
				return DiscussionModelCatalog{}, fmt.Errorf("model must be an object")
			}
			modelID, ok := catalogSegment(model["id"])
			if !ok {
				return DiscussionModelCatalog{}, fmt.Errorf("model id invalid")
			}
			modelName, ok := catalogLabel(model["name"])
			if !ok {
				return DiscussionModelCatalog{}, fmt.Errorf("model name invalid")
			}
			if _, dup := seenModels[modelID]; dup {
				return DiscussionModelCatalog{}, fmt.Errorf("duplicate model %q in %q", modelID, id)
			}
			seenModels[modelID] = struct{}{}
			routes[id+"/"+modelID] = struct{}{}
			models = append(models, DiscussionModelEntry{ID: modelID, Name: modelName})
		}
		providers = append(providers, DiscussionModelProvider{ID: id, Name: name, Models: models})
	}

	// 设备的默认模型必须真的在目录里，否则丢弃（web 同款）。留在目录外的
	// defaultModel 会让前端把它当成一个可选值展示出来，实际设备跑不了。
	if defaultModel != "" {
		if _, ok := routes[defaultModel]; !ok {
			defaultModel = ""
		}
	}
	return DiscussionModelCatalog{DefaultModel: defaultModel, Providers: providers}, nil
}

// CatalogHasRoute reports whether `provider/model` appears in the catalog.
// 管理端保存时用它做存在性校验（目录取到才校验；取不到就放行）。
func (c DiscussionModelCatalog) CatalogHasRoute(provider, model string) bool {
	if provider == "" || model == "" {
		return false
	}
	models, found := c.ProviderModels(provider)
	if !found {
		return false
	}
	for _, entry := range models {
		if entry.ID == model {
			return true
		}
	}
	return false
}

// ProviderModels 返回目录里该 provider 声明的模型清单，以及这个 provider 是否
// 出现在目录中。
//
// 两者必须分开返回，调用方要靠它区分两种完全不同的「查不到」：
//   - provider 不在目录里 → 明确不存在，可以拒绝；
//   - provider 在、模型清单却是空的 → 节点没上报（自建 OpenAI 兼容端点的模型
//     常由服务端动态给出，节点侧只报得出 provider 名），这是「没有信息」而不是
//     「信息说没有」，据此拒绝会让这类节点的模型永远配不上。
func (c DiscussionModelCatalog) ProviderModels(provider string) ([]DiscussionModelEntry, bool) {
	if provider == "" {
		return nil, false
	}
	for _, candidate := range c.Providers {
		if candidate.ID == provider {
			return candidate.Models, true
		}
	}
	return nil, false
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

	// 设备回的原文必须留痕：对外只暴露两个码（TIMEOUT / INVALID），没有这条
	// 日志时「设备卡住」「模型报错」「应答格式坏」在服务端看起来完全一样。
	b.logger.Warn("role recommendation: node reported an error",
		"request_id", result.RequestID,
		"node", senderID,
		"error", result.Payload["error"])

	b.settle(result.RequestID, "", mapNodeError(result.Payload["error"]), nil)
	return true
}

// forgetRequest 释放一条「已结算且确定无人等待」的 pending 记录。
//
// 只有发送失败那条路径需要显式调用：那里调用方拿到错误码就直接返回，不会走
// Await* 里的 delete(b.pending, …)。其余路径（正常回包 / 超时 / ctx 取消）都由
// Await* 收尾，不需要也不应该提前删——否则等待方会拿到 unknown request。
func (b *NodeRequestBroker) forgetRequest(requestID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, requestID)
}

// pendingCount 是测试用的观测口（pending 表里残留多少条请求）。
func (b *NodeRequestBroker) pendingCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
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
//
// node_id / model 只在角色真的带上它们时才出现。C 端（RoundtableSetupDialog /
// uniapp flow.js）要求每个角色都落到一台所选设备上，缺了 node_id 它会把整份推荐
// 判成 ROLE_RECOMMENDATION_INVALID；而管理端预览走的是非严格解析，根本没有设备
// 分配，多出两个恒为空的字段反而会污染它已有的响应形状。
func (b *NodeRequestBroker) recommendationPayload(record *pendingNodeRequest, roles []map[string]interface{}) map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(roles))
	for index, role := range roles {
		entry := map[string]interface{}{
			"role_id":        uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("%s:%d", record.requestID, index))).String(),
			"role_name":      role["role_name"],
			"role_prompt":    role["role_prompt"],
			"speaking_order": role["speaking_order"],
		}
		for _, key := range []string{"node_id", "model"} {
			if value, ok := role[key]; ok {
				entry[key] = value
			}
		}
		out = append(out, entry)
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
//
// node_id / model 是 Python 严格模式（candidate_models 非空）才要求的字段。这里
// 只做类型层透传：语料里出现的合法值原样保留，让下游（持有候选设备清单的
// roleCandidateNodes 调用方）决定认不认。旧设备端不产这两个字段，解析结果与
// 以前逐字节一致；管理端预览路径因此完全不受影响。
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
		// 严格模式的设备端会把「这个角色交给哪台设备、用哪个模型」一起带回来。
		// 只在值本身可用时透传：坏值留给 assignRoleNodes 丢弃并重新分配，而不是
		// 把一份本可修复的推荐整份判死。
		if nodeID, err := roleText(role["node_id"], recommendRolesMaxNodeID); err == nil {
			normalized[len(normalized)-1]["node_id"] = nodeID
		}
		if model, ok := role["model"]; ok && model != nil {
			if text, ok := model.(string); ok {
				if trimmed := strings.TrimSpace(text); trimmed != "" {
					normalized[len(normalized)-1]["model"] = trimmed
				}
			}
		}
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
//
// 除了三个历史精确串，还必须认设备端自己生成的两类超时原文：
//   - `agent turn timeout after 300000ms`（packages/xiachat-cli/src/agents.ts，
//     轮次跑到设备端上限就抛这句）；
//   - 各家 agent 转上来的 `timed out` / `ETIMEDOUT`。
//
// 它们过去全部落到 INVALID，前端于是把「主持设备 300 秒没产出」显示成
// 「角色推荐失败，请重试」—— 与真正的原因（设备端智能体卡住 / 模型不可用）
// 差着一整个排查方向，用户只能反复点重试。
func mapNodeError(value interface{}) string {
	text, _ := value.(string)
	lower := strings.ToLower(strings.TrimSpace(text))
	switch lower {
	case "timeout", "model_timeout", "role_recommendation_timeout":
		return NodeRequestCodeRecommendTimeout
	}
	if strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out") {
		return NodeRequestCodeRecommendTimeout
	}
	return NodeRequestCodeRecommendInvalid
}
