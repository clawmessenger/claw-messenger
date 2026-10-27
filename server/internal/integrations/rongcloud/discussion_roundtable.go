package rongcloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 圆桌创建编排（P5）。对应旧后端 discussion/roundtable_creation.py。
//
// 一次 createRoundtable 是一个多阶段幂等状态机：
//
//	pending → chatroom_ready → members_ready → discussion_ready → ready
//
// 幂等键 (user_id, client_request_id)：同一个客户端请求无论重放几次，都只
// 推进同一个状态机，最终只会创建一个聊天室、一组成员、一个讨论。跨阶段
// 失败后重试，会从已到达的阶段继续，而不是重头再来。

var roundtableCreateStates = []string{
	"pending", "chatroom_ready", "members_ready", "discussion_ready", "ready",
}

var roundtableRequiredRoleCaps = []string{
	"discussion_participant", "discussion_roundtable", "discussion_model_routing",
}

var roundtableRequiredHostCaps = []string{
	"discussion_host", "discussion_roundtable",
	"discussion_model_routing", "discussion_role_recommendation",
}

// roundtableSafeID 对齐 SAFE_ID = ^[A-Za-z0-9_-]{1,128}$。
var roundtableSafeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// RoundtableError 是创建编排的领域错误。Code 是对外错误码。
type RoundtableError struct {
	Code string
	Data map[string]interface{}
}

func (e *RoundtableError) Error() string { return e.Code }

func roundtableErr(code string) *RoundtableError {
	return &RoundtableError{Code: code}
}

func roundtableRoleErr(code string, roleIndex int) *RoundtableError {
	return &RoundtableError{Code: code, Data: map[string]interface{}{"role_index": roleIndex}}
}

// roundtableRoleInput 是 canonicalize 后的一个角色行。
type roundtableRoleInput struct {
	RoleID        string  `json:"role_id"`
	RoleName      string  `json:"role_name"`
	RolePrompt    string  `json:"role_prompt"`
	NodeID        string  `json:"node_id"`
	Model         *string `json:"model"`
	SpeakingOrder int     `json:"speaking_order"`
}

// roundtableCanonical 是 canonicalize 后的创建请求。
type roundtableCanonical struct {
	ClientRequestID  string                `json:"client_request_id"`
	RecommendationID string                `json:"recommendation_id"`
	ChatroomName     string                `json:"chatroom_name"`
	Topic            string                `json:"topic"`
	Goal             string                `json:"goal"`
	MaxRounds        *int                  `json:"max_rounds"`
	RoundPolicy      string                `json:"round_policy"`
	Roles            []roundtableRoleInput `json:"roles"`
}

// roundtablePayloadHash 对齐 create_request_payload_hash：canonical JSON 的
// sha256。结构体字段序固定 → Marshal 输出稳定 → 同一 payload hash 稳定。
func roundtablePayloadHash(c roundtableCanonical) (string, error) {
	encoded, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalizeRoundtablePayload 对齐 _canonicalize。
func canonicalizeRoundtablePayload(params map[string]interface{}) (roundtableCanonical, *RoundtableError) {
	if params == nil {
		return roundtableCanonical{}, roundtableErr("INVALID_ROUNDTABLE_REQUEST")
	}
	policy := "legacy_capped"
	if v, ok := params["round_policy"].(string); ok && v != "" {
		policy = v
	}
	if policy != "legacy_capped" && policy != "user_confirmed" {
		return roundtableCanonical{}, roundtableErr("INVALID_ROUNDTABLE_REQUEST")
	}
	if policy == "user_confirmed" {
		if mr, present := params["max_rounds"]; present && mr != nil {
			return roundtableCanonical{}, roundtableErr("INVALID_ROUNDTABLE_REQUEST")
		}
	}

	rawRoles, ok := params["roles"].([]interface{})
	if !ok || len(rawRoles) < 1 || len(rawRoles) > 8 {
		return roundtableCanonical{}, roundtableErr("INVALID_ROUNDTABLE_REQUEST")
	}

	type parsedRole struct {
		input roundtableRoleInput
		index int
	}
	parsed := make([]parsedRole, 0, len(rawRoles))
	names := map[string]bool{}
	nodes := map[string]bool{}

	for i, raw := range rawRoles {
		role, ok := raw.(map[string]interface{})
		if !ok {
			return roundtableCanonical{}, roundtableRoleErr("INVALID_ROUNDTABLE_REQUEST", i)
		}
		roleID, e := roundtableClean(role["role_id"], 128)
		if e != "" || !roundtableSafeID.MatchString(roleID) {
			return roundtableCanonical{}, roundtableRoleErr("INVALID_ROUNDTABLE_REQUEST", i)
		}
		roleName, e := roundtableClean(role["role_name"], 80)
		if e != "" {
			return roundtableCanonical{}, roundtableRoleErr("INVALID_ROUNDTABLE_REQUEST", i)
		}
		rolePrompt, e := roundtableClean(role["role_prompt"], 8000)
		if e != "" {
			return roundtableCanonical{}, roundtableRoleErr("INVALID_ROUNDTABLE_REQUEST", i)
		}
		nodeID, e := roundtableClean(role["node_id"], 128)
		if e != "" {
			return roundtableCanonical{}, roundtableRoleErr("INVALID_ROUNDTABLE_REQUEST", i)
		}
		var model *string
		if m, present := role["model"]; present && m != nil {
			ms, e := roundtableClean(m, 256)
			if e != "" || !strings.Contains(ms, "/") {
				return roundtableCanonical{}, roundtableRoleErr("MODEL_NOT_AVAILABLE", i)
			}
			model = &ms
		}
		orderRaw, ok := role["speaking_order"].(float64)
		if !ok || orderRaw != float64(int(orderRaw)) {
			return roundtableCanonical{}, roundtableRoleErr("INVALID_ROUNDTABLE_REQUEST", i)
		}
		order := int(orderRaw)
		nameKey := strings.ToLower(norm.NFKC.String(roleName))
		if names[nameKey] {
			return roundtableCanonical{}, roundtableRoleErr("INVALID_ROUNDTABLE_REQUEST", i)
		}
		if nodes[nodeID] {
			return roundtableCanonical{}, roundtableRoleErr("ROLE_DEVICE_DUPLICATED", i)
		}
		names[nameKey] = true
		nodes[nodeID] = true
		parsed = append(parsed, parsedRole{
			input: roundtableRoleInput{
				RoleID: roleID, RoleName: roleName, RolePrompt: rolePrompt,
				NodeID: nodeID, Model: model, SpeakingOrder: order,
			},
			index: i,
		})
	}

	// 按 speaking_order 升序排（对齐 Python 的 sorted(..., key=speaking_order)），
	// 并要求排序后恰好是 0..n-1 的排列。
	sort.SliceStable(parsed, func(a, b int) bool {
		return parsed[a].input.SpeakingOrder < parsed[b].input.SpeakingOrder
	})
	normalized := make([]roundtableRoleInput, 0, len(parsed))
	for idx, p := range parsed {
		if p.input.SpeakingOrder != idx {
			return roundtableCanonical{}, roundtableRoleErr("INVALID_ROUNDTABLE_REQUEST", p.index)
		}
		normalized = append(normalized, p.input)
	}

	maxRounds := 6
	if policy == "legacy_capped" {
		if v, ok := params["max_rounds"]; ok && v != nil {
			if f, ok := v.(float64); ok && f == float64(int(f)) && int(f) >= 1 && int(f) <= 100 {
				maxRounds = int(f)
			} else {
				return roundtableCanonical{}, roundtableErr("INVALID_ROUNDTABLE_REQUEST")
			}
		}
	}

	topic, e := roundtableClean(params["topic"], 4000)
	if e != "" {
		return roundtableCanonical{}, roundtableErr("INVALID_ROUNDTABLE_REQUEST")
	}
	goal, _ := roundtableOptionalClean(params["goal"], 8000)
	chatroomName, e := roundtableClean(params["chatroom_name"], 128)
	if e != "" {
		return roundtableCanonical{}, roundtableErr("INVALID_ROUNDTABLE_REQUEST")
	}
	clientReqID, e := roundtableClean(params["client_request_id"], 128)
	if e != "" || !roundtableSafeID.MatchString(clientReqID) {
		return roundtableCanonical{}, roundtableErr("INVALID_ROUNDTABLE_REQUEST")
	}
	recID, e := roundtableClean(params["recommendation_id"], 128)
	if e != "" {
		return roundtableCanonical{}, roundtableErr("INVALID_ROUNDTABLE_REQUEST")
	}

	out := roundtableCanonical{
		ClientRequestID:  clientReqID,
		RecommendationID: recID,
		ChatroomName:     chatroomName,
		Topic:            topic,
		Goal:             goal,
		RoundPolicy:      policy,
		Roles:            normalized,
	}
	if policy == "legacy_capped" {
		out.MaxRounds = &maxRounds
	}
	return out, nil
}

// roundtableClean 对齐 _clean：字符串、trim、非空、限长。
//
// 限长按**字符数**计。以前用 len(s)（UTF-8 字节数），中文一个字算 3 字节，
// 于是「topic 4000」实际只容 1333 个汉字、「role_name 80」只容 26 个汉字 ——
// 而推荐侧（parseRecommendedRoles → roleText）用的是 rune，两边差三倍，
// 表现就是「推荐正常、确认被一句笼统的 400 拒掉」。
func roundtableClean(v interface{}, limit int) (string, string) {
	s, ok := v.(string)
	if !ok {
		return "", "INVALID_ROUNDTABLE_REQUEST"
	}
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > limit {
		return "", "INVALID_ROUNDTABLE_REQUEST"
	}
	return s, ""
}

func roundtableOptionalClean(v interface{}, limit int) (string, string) {
	if v == nil {
		return "", ""
	}
	return roundtableClean(v, limit)
}

// ---- 创建编排（状态机主体） -----------------------------------------------

// roundtableCreationService 承载 createRoundtable 的编排状态机。
type roundtableCreationService struct {
	queries    *db.Queries
	client     *rongcloudAPIClient
	hostLoader func(ctx context.Context) (DiscussionSystemHostConfig, bool, error)
	// nodeOwner 按用户列「归属」节点（融云好友边语义，node_claim.go）。
	nodeOwner func(ctx context.Context, userID string) ([]db.RongcloudNode, error)
	// workflowAvailable 判定某个 round_policy 此刻是否真的可跑。P5 只有
	// legacy_capped；P7 装配好交互轮次引擎后 user_confirmed 才为真。它与
	// discussionRuntime.roundtablePolicyAvailable 是同一个开关（registration
	// 把后者注入到这里），确保「推荐/创建/能力宣告」三处对 v3 的判定永不漂移。
	workflowAvailable func(policy string) bool
}

func newRoundtableCreationService(queries *db.Queries, client *rongcloudAPIClient) *roundtableCreationService {
	return &roundtableCreationService{queries: queries, client: client}
}

// create 对齐 RoundtableCreationService.create：claim → 阶段推进 → ready。
func (s *roundtableCreationService) create(ctx context.Context, userID string, params map[string]interface{}) (map[string]interface{}, *RoundtableError) {
	canonical, cerr := canonicalizeRoundtablePayload(params)
	if cerr != nil {
		return nil, cerr
	}
	if canonical.RoundPolicy == RoundPolicyUserConfirmed &&
		(s.workflowAvailable == nil || !s.workflowAvailable(canonical.RoundPolicy)) {
		// v3 策略的运行时（interactive rounds）尚未装配时诚实拒绝，不谎报支持 ——
		// 谎报会让前端建出「跑不起来」的讨论。
		return nil, roundtableErr("DISCUSSION_WORKFLOW_UNAVAILABLE")
	}
	payloadJSON, err := json.Marshal(canonical)
	if err != nil {
		return nil, roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
	}
	payloadHash, err := roundtablePayloadHash(canonical)
	if err != nil {
		return nil, roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
	}
	chatroomID := roundtableNewChatroomID(userID, canonical.ClientRequestID)
	discussionID := roundtableNewDiscussionID(chatroomID)

	request, cerr := s.claim(ctx, userID, canonical, chatroomID, discussionID, payloadJSON, payloadHash)
	if cerr != nil {
		return nil, cerr
	}
	if request.Status == "ready" {
		return roundtableReadyResponse(request), nil
	}

	validated, cerr := s.validateCurrent(ctx, userID, canonical)
	if cerr != nil {
		return nil, cerr
	}

	if request.Status == "pending" {
		if cerr := s.ensureChatroom(ctx, request, chatroomID, canonical, validated, userID); cerr != nil {
			return nil, s.recordError(ctx, userID, canonical.ClientRequestID, request, cerr, chatroomID, "")
		}
		request = s.advance(ctx, userID, canonical, request, "chatroom_ready", chatroomID, "")
	}
	if request.Status == "chatroom_ready" {
		if cerr := s.ensureMembers(ctx, chatroomID, canonical, validated); cerr != nil {
			return nil, s.recordError(ctx, userID, canonical.ClientRequestID, request, cerr, chatroomID, "")
		}
		request = s.advance(ctx, userID, canonical, request, "members_ready", chatroomID, "")
	}
	if request.Status == "members_ready" {
		// 旧五阶段链路恒以 planning 落讨论（引导式链路的 configuring 由
		// createRoundtableShell 单独调 ensureDiscussion）。
		discussionID, cerr = s.ensureDiscussion(ctx, request, chatroomID, discussionID, canonical, validated, userID, DiscussionStatusPlanning)
		if cerr != nil {
			return nil, s.recordError(ctx, userID, canonical.ClientRequestID, request, cerr, chatroomID, discussionID)
		}
		request = s.advance(ctx, userID, canonical, request, "discussion_ready", chatroomID, discussionID)
	}
	if request.Status == "discussion_ready" {
		request = s.advance(ctx, userID, canonical, request, "ready", chatroomID, discussionID)
	}
	if request.Status != "ready" {
		return nil, roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
	}
	return roundtableReadyResponse(request), nil
}

// claim 对齐 _claim：幂等插入；既存行 hash 不一致 → CONFLICT。
func (s *roundtableCreationService) claim(ctx context.Context, userID string, canonical roundtableCanonical, chatroomID, discussionID string, payloadJSON []byte, payloadHash string) (db.RongcloudRoundtableCreateRequest, *RoundtableError) {
	recID := pgtype.Text{String: canonical.RecommendationID, Valid: canonical.RecommendationID != ""}
	req, err := s.queries.InsertRoundtableCreateRequest(ctx, db.InsertRoundtableCreateRequestParams{
		UserID:             userID,
		ClientRequestID:    canonical.ClientRequestID,
		RecommendationID:   recID,
		RequestPayload:     payloadJSON,
		RequestPayloadHash: pgtype.Text{String: payloadHash, Valid: true},
		ChatroomID:         pgtype.Text{String: chatroomID, Valid: true},
		DiscussionID:       pgtype.Text{String: discussionID, Valid: true},
	})
	if err != nil {
		return db.RongcloudRoundtableCreateRequest{}, roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
	}
	if req.Status != "pending" {
		// 既存行：同 key 重入。payload hash 不一致 = 另一个请求 → 冲突。
		existingHash := ""
		if req.RequestPayloadHash.Valid {
			existingHash = req.RequestPayloadHash.String
		}
		if existingHash != payloadHash {
			return db.RongcloudRoundtableCreateRequest{}, roundtableErr("CREATE_ROUNDTABLE_CONFLICT")
		}
	}
	return req, nil
}

// advance 对齐 _advance：CAS 推进；失败时取回当前行，只要不落后于目标即接受。
func (s *roundtableCreationService) advance(ctx context.Context, userID string, canonical roundtableCanonical, request db.RongcloudRoundtableCreateRequest, toStatus, chatroomID, discussionID string) db.RongcloudRoundtableCreateRequest {
	expected := request.Status
	updated, err := s.queries.TransitionRoundtableCreateRequest(ctx, db.TransitionRoundtableCreateRequestParams{
		UserID:          userID,
		ClientRequestID: canonical.ClientRequestID,
		Status:          toStatus,
		ChatroomID:      textOrNull(chatroomID),
		DiscussionID:    textOrNull(discussionID),
		Status_2:        expected,
	})
	if err == nil {
		return updated
	}
	// CAS 失败：并发推进者已把状态推得更远 → 取回并接受既成事实。
	current, gErr := s.queries.GetRoundtableCreateRequest(ctx, db.GetRoundtableCreateRequestParams{UserID: userID, ClientRequestID: canonical.ClientRequestID})
	if gErr != nil {
		return request
	}
	return current
}

// validateCurrent 对齐 _validate_current：host 可用 + 推荐有效 + 角色节点合规。
func (s *roundtableCreationService) validateCurrent(ctx context.Context, userID string, canonical roundtableCanonical) (roundtableValidated, *RoundtableError) {
	// 1. 推荐记录必须存在且属于本人、未过期，且 host 一致。
	rec, err := s.queries.GetRongCloudRoleRecommendation(ctx, canonical.RecommendationID)
	if err != nil {
		return roundtableValidated{}, roundtableErr("RECOMMENDATION_EXPIRED")
	}
	if rec.UserID != userID {
		return roundtableValidated{}, roundtableErr("RECOMMENDATION_EXPIRED")
	}
	host, hostErr := s.resolveHost(ctx)
	if hostErr != "" {
		return roundtableValidated{}, roundtableErr(hostErr)
	}
	if rec.HostNodeID != host.nodeID || rec.HostAiUserID != host.aiUserID || rec.ConfigVersion != host.configVersion {
		return roundtableValidated{}, roundtableErr("SYSTEM_HOST_OFFLINE")
	}
	// 2. 提交的 role_id 集合必须与推荐一一对应。
	recommendedIDs := map[string]bool{}
	var rolesList []map[string]interface{}
	_ = json.Unmarshal(rec.Roles, &rolesList)
	for _, r := range rolesList {
		if id, ok := r["role_id"].(string); ok {
			recommendedIDs[id] = true
		}
	}
	submitted := map[string]bool{}
	for i, role := range canonical.Roles {
		if !recommendedIDs[role.RoleID] || submitted[role.RoleID] {
			return roundtableValidated{}, roundtableRoleErr("INVALID_ROUNDTABLE_REQUEST", i)
		}
		submitted[role.RoleID] = true
	}
	if len(submitted) != len(recommendedIDs) {
		return roundtableValidated{}, roundtableRoleErr("INVALID_ROUNDTABLE_REQUEST", 0)
	}
	// 3. 角色节点必须属于本人、能力齐备、不是 host。
	owned, eligible := s.ownedAndEligibleNodes(ctx, userID, host)
	for i, role := range canonical.Roles {
		if role.NodeID == host.nodeID {
			return roundtableValidated{}, roundtableRoleErr("ROLE_DEVICE_UNAVAILABLE", i)
		}
		if !owned[role.NodeID] {
			return roundtableValidated{}, roundtableRoleErr("ROLE_DEVICE_NOT_OWNED", i)
		}
		if _, ok := eligible[role.NodeID]; !ok {
			return roundtableValidated{}, roundtableRoleErr("ROLE_DEVICE_UNAVAILABLE", i)
		}
	}
	return roundtableValidated{host: host, nodes: eligible}, nil
}

// roundtableValidated 是 validateCurrent 的产物。
type roundtableValidated struct {
	host  roundtableHostConfig
	nodes map[string]db.RongcloudNode
}

// roundtableHostConfig 是运行时视角的 system host 配置。
type roundtableHostConfig struct {
	nodeID         string
	aiUserID       string
	displayName    string
	avatar         string
	hostPrompt     string
	configVersion  string
	timeoutSeconds int
	// model 是主持人自己的模型 route（`provider/slug`），空串 = 不指定、沿用
	// 设备默认。三个 host 快照写入点都只在它非空时才落 `model` 键，所以空串
	// 的讨论与改造前逐字节一致。
	model string
}

// roundtableHostRoleEntry 是主持人在角色板里的条目。
//
// 这是**唯一**的 host 条目拼装点：`interactiveRoles`（discussion_interactive.go）
// 只从角色板取 `role.Model`，所以 model 必须写在这里才真正生效。三个写入点
// （引导式确认 / createRoundtable / 阶段机）原先各自内联了一份逐字相同的 map
// 字面量，任何字段调整漏改一处就会出现「有的讨论组生效、有的不生效」——那种
// 失败模式只能靠代码结构防，注释防不住。
//
// 注意这里**没有** speakingOrder：主持人不是排序队列的一员（引擎按 isHost 把他
// 分出来，只在全员发言后汇总）。旧后端同样只在 discussion_order 非空时写这个
// 字段，而 v3 的线上校验器（protocol_v3._role / 前端 discussion-v3.ts）要求它
// 是非负整数 —— 给主持人写 -1 会把整段快照拒掉，写 0 又会让他混进发言顺序。
func roundtableHostRoleEntry(host roundtableHostConfig) map[string]interface{} {
	entry := map[string]interface{}{
		"memberId":         host.aiUserID,
		"nodeId":           host.nodeID,
		"nickname":         host.displayName,
		"portraitUri":      host.avatar,
		"roleName":         roundtableHostRoleName,
		"roleInstructions": "",
		"capabilities":     roundtableRequiredHostCaps,
		"isHost":           true,
	}
	// 非空才写键（不是写空串）：没配置模型时快照与改造前逐字节一致。
	if host.model != "" {
		entry["model"] = host.model
	}
	return entry
}

// roundtableRoleEntryFromMember 把聊天室成员表的一行拼成角色板里的**普通成员**条目；
// ok=false 表示这一行不该进角色板（不是 AI 成员 / 已禁用 / 没有节点的融云账号）。
//
// 抽出这个函数的理由与 roundtableHostRoleEntry 相同：普通成员条目有两个写入点
// （重开讨论、轮边界的中途加人同步），字段形状必须唯一 —— 漏写 capabilities 会让
// 设备端拒掉这条 your_turn，漏写 speakingOrder 会让派发顺序退化成随机。
//
// **主持节点的排除留给调用方**：重开讨论时权威来源是管理端配置（按 node_id 排），
// 中途加人时权威来源是已在快照里的 host 条目（按融云账号排），两者不能合并。
//
// **已知唯一例外**：ensureDiscussion（discussion_roundtable_stages.go，
// 「引导式确认创建」路径）仍自己拼普通成员条目，且**不写 model**。也就是说同一成员
// 经「引导式确认创建」建出来的角色板没有 model 键（设备端回落到默认模型），经
// 老式创建 / 重开讨论 / 中途加人进来的则有。收敛它要改 stages 那条创建路径，不在
// 上下文窗口这次改动范围内，留作后续单独立项 —— 别以为三处写入点已经全部统一。
func roundtableRoleEntryFromMember(row db.ListRongCloudChatroomMembersWithNodeRow, order int) (map[string]interface{}, bool) {
	if row.MemberType != "ai" || !row.Enabled || !row.NodeRcUserID.Valid || row.NodeRcUserID.String == "" {
		return nil, false
	}
	roleName := row.Nickname
	if row.RoleName.Valid && row.RoleName.String != "" {
		roleName = row.RoleName.String
	}
	instructions := ""
	if row.RoleInstructions.Valid {
		instructions = row.RoleInstructions.String
	}
	entry := map[string]interface{}{
		"memberId":         row.NodeRcUserID.String,
		"nodeId":           row.BusinessNodeID.String,
		"nickname":         roleName,
		"portraitUri":      "",
		"roleName":         roleName,
		"roleInstructions": instructions,
		"capabilities":     roundtableRequiredRoleCaps,
		"isHost":           false,
		"speakingOrder":    order,
	}
	// model 只在非空时写键（与 roundtableHostRoleEntry 同一约定，空串的讨论与改造前
	// 逐字节一致）。取值的优先级和 relayModel 一致：成员设置里的「讨论模型」
	// （discussion_model）优先，没有才退回节点级 model。缺了这个键，中途加进来的
	// 成员会静默使用设备默认模型 —— 用户在成员设置里配的模型等于没配。
	model := row.DiscussionModel.String
	if !row.DiscussionModel.Valid || model == "" {
		model = row.Model.String
	}
	if model != "" {
		entry["model"] = model
	}
	return entry, true
}

// roundtableHostSnapshotFields 是 host_config_snapshot 的内容。
//
// 除了 interactiveHostPrompt 读的 discussion_host_prompt，其余字段是留档：
// 排障时能回答「这场讨论当时用的是哪个主持配置、哪个模型」。
func roundtableHostSnapshotFields(host roundtableHostConfig) map[string]interface{} {
	fields := map[string]interface{}{
		"node_id":                host.nodeID,
		"ai_user_id":             host.aiUserID,
		"display_name":           host.displayName,
		"avatar":                 host.avatar,
		"discussion_host_prompt": host.hostPrompt,
		"config_version":         host.configVersion,
	}
	if host.model != "" {
		fields["model"] = host.model
	}
	return fields
}

// roundtableHostSnapshots 把 host 条目塞进角色板并序列化两段快照。
//
// 三个写入点（setup / actions / stages）统一调用它。调用方负责先把普通成员
// 填进 roles；host 的条目由这里唯一地写入，因此不存在「某一条链路忘了带 model」。
func roundtableHostSnapshots(host roundtableHostConfig, roles map[string]interface{}) (roleJSON, hostSnapshot []byte) {
	roles[host.aiUserID] = roundtableHostRoleEntry(host)
	roleJSON, _ = json.Marshal(roles)
	hostSnapshot, _ = json.Marshal(roundtableHostSnapshotFields(host))
	return roleJSON, hostSnapshot
}

// resolveHost 读取管理端配置的 system host，校验在线与能力。
func (s *roundtableCreationService) resolveHost(ctx context.Context) (roundtableHostConfig, string) {
	if s.hostLoader == nil {
		return roundtableHostConfig{}, "SYSTEM_HOST_NOT_CONFIGURED"
	}
	cfg, found, err := s.hostLoader(ctx)
	if err != nil || !found || cfg.NodeID == "" {
		return roundtableHostConfig{}, "SYSTEM_HOST_NOT_CONFIGURED"
	}
	if !cfg.Enabled {
		return roundtableHostConfig{}, "SYSTEM_HOST_DISABLED"
	}
	// rongcloud_system_config.node_id 存的是 rongcloud_node 的 UUID 主键
	//（管理端 eligible-nodes 下发 node.ID.String()），必须按 id 查。
	nodeUUID, err := uuid.Parse(cfg.NodeID)
	if err != nil {
		return roundtableHostConfig{}, "SYSTEM_HOST_NOT_CONFIGURED"
	}
	node, err := s.queries.GetRongCloudNodeByID(ctx, pgtype.UUID{Bytes: nodeUUID, Valid: true})
	if err != nil {
		slog.Warn("rongcloud: 系统主持节点行不存在",
			"configured_node_id", cfg.NodeID, "error", err)
		return roundtableHostConfig{}, "SYSTEM_HOST_OFFLINE"
	}
	if missing := missingCapabilities(node.Capabilities, roundtableRequiredHostCaps); len(missing) > 0 {
		slog.Warn("rongcloud: 系统主持节点缺少能力位",
			"node_id", node.NodeID, "missing", missing,
			"capabilities", string(node.Capabilities))
		return roundtableHostConfig{}, "SYSTEM_HOST_OFFLINE"
	}
	// 在线判定走融云 presence（见 node_presence.go），并把观测结果写回
	// deploy_status 缓存，让列恢复成「最近一次观测」而不是永远 offline。
	online := nodeIsOnline(ctx, s.client, node)
	syncNodePresenceCache(ctx, s.queries, node, online)
	if !online {
		// 对外只有一个 SYSTEM_HOST_OFFLINE，但内因有四种（节点行缺失 / 能力位缺 /
		// 融云侧离线 / 未装配融云客户端），排查时必须能区分，否则只能看到前端
		// 那句「系统主持 AI 当前离线」干瞪眼。
		slog.Warn("rongcloud: 系统主持节点被判为离线",
			"node_id", node.NodeID,
			"rongcloud_user_id", node.RongcloudUserID,
			"deploy_status_column", node.DeployStatus,
			"rongcloud_client_wired", s.client != nil)
		return roundtableHostConfig{}, "SYSTEM_HOST_OFFLINE"
	}
	return roundtableHostConfig{
		nodeID:         node.NodeID,
		aiUserID:       node.RongcloudUserID,
		displayName:    cfg.DisplayName,
		avatar:         cfg.Avatar,
		hostPrompt:     cfg.HostPrompt,
		configVersion:  cfg.ConfigVersion,
		timeoutSeconds: cfg.TimeoutSeconds,
		model:          cfg.Model,
	}, ""
}
