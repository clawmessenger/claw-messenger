package rongcloud

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/quota"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"golang.org/x/text/unicode/norm"
)

// 讨论组引导式创建（configuring 链路）的六个 action。
//
// 与旧五阶段 createRoundtable 并行存在、互不改写（uniapp 仍走旧链路）：
//   createRoundtableShell   空组直建：配额 Check → 建房 → 讨论行(configuring, step=topic)
//   submitSetupTopic        S1 提交议题：先 200 {accepted:true}，异步跑角色推荐
//   requestRoleRecommend    S2 换一批/重试：同样先 200，异步重跑推荐
//   confirmRoundtableSetup  S3 确认开始：ensureMembers + CAS configuring→planning + v3 开局
//   resumeSetup             断点续引：touch + 返回快照 + 重播当前步骤 prompt（不落新行）
//   touchSetup              心跳：仅刷新 last_interaction_at（GC 防误删）
//
// 所有状态推进都经 interactiveEmit（幂等键）落 v3 事件 + interactiveBroadcast 广播；
// 刷新/重进靠 REST 事件尾读重放恢复，服务端不持有内存态。

// setup 步骤与事件幂等键前缀（与 web 投影侧共用同一份字面量契约）。
const (
	setupStepTopic   = "topic"
	setupStepRoles   = "roles"
	setupStepConfirm = "confirm"

	setupEventPrompt          = "setup_prompt"
	setupEventStepCompleted   = "setup_step_completed"
	setupEventRecommendFailed = "setup_recommend_failed"

	setupPromptIdemPrefix  = "setup-prompt:"
	setupDoneIdemPrefix    = "setup-done:"
	setupRecFailIdemPrefix = "setup-recfail:"
)

// recommendRoleCountCap 讨论组推荐角色数上限：产品口径 3-5 个角色，
// 下限天然受「一个 agent 只能演一个角色」约束（候选节点数）。
const recommendRoleCountCap = 5

// setupPromptPayload 是 setup_prompt 事件的 data 形状（v3 契约，见 web
// lib/discussion-v3.ts 的 eventData.setup_prompt）。
func setupPromptPayload(step, prompt string) map[string]interface{} {
	return map[string]interface{}{"step": step, "prompt": prompt}
}

// emitSetupPrompt 幂等落一条 setup_prompt 并广播。fresh=false 表示同键事件已存在
// （重放），调用方通常只关心错误。
func (rt *discussionRuntime) emitSetupPrompt(ctx context.Context, d DiscussionRecord, step string, payload map[string]interface{}, idemKey string) error {
	rec, fresh, err := rt.interactiveEmit(ctx, d, interactiveEvent{
		EventType: setupEventPrompt,
		ActorID:   "system",
		Payload:   payload,
		Key:       idemKey,
		StateVer:  d.Version,
	})
	if err != nil {
		return err
	}
	if fresh {
		rt.interactiveBroadcast(ctx, d, rec)
	}
	return nil
}

// findConfiguringSetup 按 chatroom 定位半成品讨论并校验操作者是创建者本人。
// 返回 db 行（带 setup_step）与仓库层记录（事件写入用）。
func (rt *discussionRuntime) findConfiguringSetup(ctx context.Context, chatroomID, actorRcID string) (db.RongcloudDiscussion, DiscussionRecord, *RoundtableError) {
	row, err := rt.queries.GetConfiguringRongCloudDiscussionByChatroom(ctx, chatroomID)
	if err != nil {
		return db.RongcloudDiscussion{}, DiscussionRecord{}, roundtableErr("ROUNDTABLE_CREATE_REQUEST_NOT_FOUND")
	}
	if row.CreatedBy != actorRcID {
		// 不区分不存在与无权（与旧链路 create request 的 403 同语义）。
		return db.RongcloudDiscussion{}, DiscussionRecord{}, roundtableErr("ROUNDTABLE_CREATE_REQUEST_NOT_FOUND")
	}
	d, err := rt.repo.Get(ctx, row.DiscussionID)
	if err != nil {
		return db.RongcloudDiscussion{}, DiscussionRecord{}, roundtableErr("CREATE_ROUNDTABLE_RECOVERABLE")
	}
	return row, d, nil
}

// touchSetup 刷新半成品交互时间（GC 防误删）。best-effort。
func (rt *discussionRuntime) touchSetup(ctx context.Context, discussionID string) {
	_, _ = rt.queries.TouchRongCloudDiscussionSetup(ctx, discussionID)
}

// quotaCheckResult 把配额校验翻译成 commandResult；放行返回 ok=false。
func (rt *discussionRuntime) quotaExceededResult(ctx context.Context, userID string, kind quota.Kind) (commandResult, bool) {
	if rt.quota == nil {
		return commandResult{}, false
	}
	if qErr := rt.quota.Check(ctx, userID, kind); qErr != nil {
		return commandResult{code: 429, message: quota.CodeQuotaExceeded, data: qErr.Data()}, true
	}
	return commandResult{}, false
}

// ---- createRoundtableShell ------------------------------------------------

// actionCreateRoundtableShell 空组直建：claim（确定性 id）→ 配额 Check →
// 融云建房 + DB 行 → 讨论行(configuring, step=topic) → emit setup_prompt(topic)。
// 幂等：同 (user, client_request_id) 重放返回既有半成品快照，不重复建房。
func (rt *discussionRuntime) actionCreateRoundtableShell(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	if rt.roundtable == nil || !rt.interactiveReady() {
		return errResult(503, "discussion runtime not available")
	}
	clientRequestID := paramTrim(cmd, "client_request_id", "clientRequestId")
	if clientRequestID == "" || !roundtableSafeID.MatchString(clientRequestID) {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	name := paramTrim(cmd, "name", "chatroom_name", "chatroomName")
	if name == "" {
		name = "讨论组"
	}
	if len(name) > 128 {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}

	chatroomID := roundtableNewChatroomID(actor.RcID, clientRequestID)
	discussionID := roundtableNewDiscussionID(chatroomID)

	// 幂等重放：讨论行已存在 → 直接回快照（建房/建讨论/首卡都已做过）。
	if existing, err := rt.queries.GetRongCloudDiscussion(ctx, discussionID); err == nil {
		if existing.CreatedBy != actor.RcID {
			return errResult(409, "CREATE_ROUNDTABLE_CONFLICT")
		}
		return okResult(map[string]interface{}{
			"chatroom_id": chatroomID, "discussion_id": discussionID, "status": existing.Status,
		})
	}

	// 配额硬校验（超限：429 QUOTA_EXCEEDED{kind,limit,used}）。
	if result, blocked := rt.quotaExceededResult(ctx, actor.RcID, quota.KindDiscussion); blocked {
		return result
	}

	// 主持节点必须在场：房间要挂 host 成员，后续推荐也走它。
	host, hostErr := rt.roundtable.resolveHost(ctx)
	if hostErr != "" {
		return errResult(503, hostErr)
	}

	// 复用五阶段编排的 ensure 阶段：canonical 不带角色（确认时才定），
	// validated 只需要 host（ensureChatroom 不读 nodes）。
	canonical := roundtableCanonical{
		ClientRequestID: clientRequestID,
		ChatroomName:    name,
		RoundPolicy:     RoundPolicyUserConfirmed,
	}
	validated := roundtableValidated{host: host, nodes: map[string]db.RongcloudNode{}}
	s := rt.roundtable
	if cerr := s.ensureChatroom(ctx, db.RongcloudRoundtableCreateRequest{}, chatroomID, canonical, validated, actor.RcID); cerr != nil {
		return roundtableErrorResult(cerr)
	}
	if _, cerr := s.ensureDiscussion(ctx, db.RongcloudRoundtableCreateRequest{}, chatroomID, discussionID, canonical, validated, actor.RcID, DiscussionStatusConfiguring); cerr != nil {
		return roundtableErrorResult(cerr)
	}

	d, err := rt.repo.Get(ctx, discussionID)
	if err != nil {
		return errResult(500, "CREATE_ROUNDTABLE_RECOVERABLE")
	}
	// S1 欢迎卡：引导的第一步（议题输入）。幂等键保证重放只落一条。
	if err := rt.emitSetupPrompt(ctx, d, setupStepTopic,
		setupPromptPayload(setupStepTopic, "讨论组已创建。请输入本次讨论的议题："),
		setupPromptIdemPrefix+setupStepTopic+":"+discussionID); err != nil {
		rt.logger.Warn("discussion: setup_prompt(topic) emit failed", "discussionId", discussionID, "error", err)
	}
	return okResult(map[string]interface{}{
		"chatroom_id": chatroomID, "discussion_id": discussionID, "status": DiscussionStatusConfiguring,
	})
}

// ---- submitSetupTopic -----------------------------------------------------

// actionSubmitSetupTopic S1 提交议题：CAS 写 topic 并把断点推到 roles，立即回
// {accepted:true}，角色推荐在后台异步跑完后以 setup_prompt(roles) 事件广播
// （推荐经 broker 往返设备可达 60s，不能卡在命令回调里）。
func (rt *discussionRuntime) actionSubmitSetupTopic(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	if rt.roundtable == nil || !rt.interactiveReady() {
		return errResult(503, "discussion runtime not available")
	}
	chatroomID := paramTrim(cmd, "chatroom_id", "chatroomId")
	topic := paramTrim(cmd, "topic")
	goal := paramTrim(cmd, "goal")
	if chatroomID == "" || topic == "" || len(topic) > 4000 || len(goal) > 8000 {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	row, _, rErr := rt.findConfiguringSetup(ctx, chatroomID, actor.RcID)
	if rErr != nil {
		return roundtableErrorResult(rErr)
	}
	_, err := rt.queries.SubmitRongCloudDiscussionSetupTopic(ctx, db.SubmitRongCloudDiscussionSetupTopicParams{
		DiscussionID: row.DiscussionID,
		Topic:        topic,
		Goal:         goal,
	})
	if err != nil {
		// CAS 未命中：同 topic 重复提交按幂等成功接受（步骤已在 roles），
		// 不同 topic 说明已在更后面的步骤，回 409 让前端重同步。
		if row.SetupStep == setupStepRoles && row.Topic == topic {
			// 幂等重放也补一次名字：上一次可能在改名那步之前就断了。
			rt.nameDiscussionGroupFromTopic(ctx, chatroomID, topic)
			return okResult(map[string]interface{}{"accepted": true})
		}
		return errResult(409, "DISCUSSION_SETUP_STEP_CONFLICT")
	}
	rt.touchSetup(ctx, row.DiscussionID)
	// 有了议题才有名字：建房（createRoundtableShell）时还什么都不知道，只能先落
	// 占位「讨论组」，于是会话列表整排同名、根本分不出哪个是哪个。
	rt.nameDiscussionGroupFromTopic(ctx, chatroomID, topic)
	// 异步推荐：detached context（命令回调结束不取消后台管线）。attempt 取
	// 「已发出的 roles 卡数 + 1」，与换一批同一口径 —— 用户在 S2 回上一步改题
	// 重提时，新推荐卡的幂等键才不会撞上旧卡（撞键会被当重放丢弃，前端永远
	// 等不到新角色）。
	attempt := rt.setupRecommendAttempt(ctx, row.DiscussionID) + 1
	go rt.recommendForSetupAsync(context.Background(), row.DiscussionID, attempt)
	return okResult(map[string]interface{}{"accepted": true})
}

// ---- 讨论组名字：建房时只能占位，拿到议题后补 ----------------------------

// 占位名 = 建房时唯一能落的值（createRoundtableShell 的默认 name）。这些名字出现在
// 会话/聊天室列表里是纯噪音：一排「讨论组」，用户认不出哪个对应哪场讨论。
var discussionGroupPlaceholderNames = map[string]bool{
	"": true, "讨论组": true, "聊天室": true, "未命名讨论": true,
}

// discussionGroupNameFromTopic 由议题派生组名，沿用旧链路的既有约定
// 「议题前缀 + 讨论组」（线上的历史房间全是这个形态，改名不能改成另一种长相）。
//
// 只取首行、折叠空白，再按 **rune**（不是字节）截 24 字：能从分句处断开就断在
// 分句处（「如何让ai自主开发小游戏，并发布到对应平台，不需要…」截成「…对应平台」
// 读得通），断不开才硬截，然后削掉结尾标点 —— 列表一行放不下整段议题。
func discussionGroupNameFromTopic(topic string) string {
	line := strings.TrimSpace(strings.SplitN(topic, "\n", 2)[0])
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return ""
	}
	const maxPrefix = 24
	prefix := line
	if runes := []rune(prefix); len(runes) > maxPrefix {
		pool := string(runes[:maxPrefix])
		// 优先在分句处断：留太短的片段（< 8 字）没有信息量，不如硬截。
		if idx := strings.LastIndexAny(pool, "，。！？；：、,.;!?: "); idx >= 0 {
			if head := pool[:idx]; len([]rune(head)) >= 8 {
				pool = head
			}
		}
		prefix = pool
	}
	name := strings.TrimRight(strings.TrimSpace(prefix), "，。！？、；：,.!?;: ")
	if name == "" {
		return ""
	}
	if strings.HasSuffix(name, "讨论组") {
		return name
	}
	return name + "讨论组"
}

// nameDiscussionGroupFromTopic 把会话名从占位替换成「议题前缀 + 讨论组」。
//
// 只在名字仍是占位时改：用户自己起过名（旧创建链路 / 后台改名）一律不覆盖。
// 改名失败只记日志 —— 名字是锦上添花，不能让它把建组主流程带崩（此刻讨论行已经
// CAS 成功，回错误会让前端以为议题没提交、反复重试）。
func (rt *discussionRuntime) nameDiscussionGroupFromTopic(ctx context.Context, chatroomID, topic string) {
	if rt == nil || rt.queries == nil || chatroomID == "" {
		return
	}
	view, err := rt.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, chatroomID)
	if err != nil {
		return
	}
	current := strings.TrimSpace(view.Name)
	if !discussionGroupPlaceholderNames[current] {
		return
	}
	name := discussionGroupNameFromTopic(topic)
	if name == "" || name == current {
		return
	}
	// 复用现有 profile 更新：需要把头像/轮数原值带回去（该 UPDATE 是整行写）。
	if _, err := rt.queries.UpdateRongCloudChatroomProfileByRongCloudID(ctx, db.UpdateRongCloudChatroomProfileByRongCloudIDParams{
		RongcloudChatroomID: chatroomID,
		Name:                name,
		PortraitUri:         view.PortraitUri,
		MaxRounds:           view.MaxRounds,
	}); err != nil {
		if rt.logger != nil {
			rt.logger.Warn("discussion: rename chatroom from topic failed",
				"chatroomId", chatroomID, "error", err)
		}
	}
}

// ---- requestRoleRecommend --------------------------------------------------

// actionRequestRoleRecommend S2 换一批/失败重试/继续推荐：异步重跑推荐，attempt 递增
// （幂等键带 attempt，多次推荐各自落一条 setup_prompt 事件，前端取最新一条）。
//
// 两个可选参数，老前端不传时行为与「换一批」完全一致：
//   - node_ids（nodeIds）本批允许作为候选的设备 —— 前端传「剩余未参与」的那批；
//   - append（append_mode / appendMode）续推：新角色追加进**当前**推荐行，
//     recommendationId 保持不变，用户已添加的角色继续有效。
func (rt *discussionRuntime) actionRequestRoleRecommend(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	if rt.roundtable == nil || !rt.interactiveReady() {
		return errResult(503, "discussion runtime not available")
	}
	chatroomID := paramTrim(cmd, "chatroom_id", "chatroomId")
	if chatroomID == "" {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	// 候选列表沿用既有解析：给了空数组 / 超过上限的形态直接判非法，不做宽松兜底
	// （见 paramStringList 的注释）。
	requested, requestedGiven, listOK := paramStringList(cmd, "node_ids", "nodeIds")
	if !listOK {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	req := setupRecommendRequest{
		nodeIDs:      requested,
		nodeIDsGiven: requestedGiven,
		appendMode:   paramBool(cmd, "append", "append_mode", "appendMode"),
	}
	row, _, rErr := rt.findConfiguringSetup(ctx, chatroomID, actor.RcID)
	if rErr != nil {
		return roundtableErrorResult(rErr)
	}
	if row.SetupStep != setupStepRoles || row.Topic == "" {
		// 议题还没提交就请求推荐是乱序操作。
		return errResult(409, "DISCUSSION_SETUP_STEP_CONFLICT")
	}
	rt.touchSetup(ctx, row.DiscussionID)
	attempt := rt.setupRecommendAttempt(ctx, row.DiscussionID) + 1
	go rt.recommendForSetupBatch(context.Background(), row.DiscussionID, attempt, req)
	return okResult(map[string]interface{}{"accepted": true})
}

// setupRecommendAttempt 数已发出的 roles 推荐卡条数（含失败卡），得出当前 attempt。
func (rt *discussionRuntime) setupRecommendAttempt(ctx context.Context, discussionID string) int {
	events, err := rt.repo.ListEventsAfter(ctx, discussionID, 0)
	if err != nil {
		return 0
	}
	attempt := 0
	for _, e := range events {
		if e.EventType != setupEventPrompt && e.EventType != setupEventRecommendFailed {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if step, _ := payload["step"].(string); step == setupStepRoles {
			attempt++
		}
	}
	return attempt
}

// setupRecommendFailureMessage 把失败码翻成一句人能读懂、能据以行动的话。
//
// 「主持设备在线」和「主持设备能产出推荐」是两回事：进程连着融云只说明
// command 发得出去，agent 卡住 / 模型网关不通 / 应答不合协议都会在往返阶段
// 失败，而它们对用户意味着完全不同的下一步（等、去查设备、点重试）。
func setupRecommendFailureMessage(code string) string {
	switch code {
	case "ROLE_RECOMMENDATION_TIMEOUT":
		return "主持设备在限定时间内没有返回推荐结果，请确认主持设备上的智能体与模型可用后重试"
	case "ROLE_RECOMMENDATION_INVALID":
		return "主持设备返回的推荐结果无法解析，请重试"
	case "SYSTEM_HOST_OFFLINE", "SYSTEM_HOST_NOT_CONFIGURED", "SYSTEM_HOST_DISABLED":
		return "系统主持 AI 当前不可用，请确认主持设备上的 clawmessenger 正在运行"
	case "NO_ELIGIBLE_USER_DEVICES":
		return "没有可用于讨论的在线设备"
	}
	return "角色推荐失败，请重试"
}

// setupRecommendRequest 是一次后台推荐的入参（S1 首推 / S2 换一批 / S2 续推共用）。
//
// 零值等价于历史行为：候选由服务端自动挑、新建一条推荐行。
type setupRecommendRequest struct {
	// nodeIDs / nodeIDsGiven 是本批允许作为候选的设备（前端传「剩余未参与」的那批）。
	// 未给出时沿用「自动选候选」的老路径。
	nodeIDs      []string
	nodeIDsGiven bool
	// appendMode 为 true 表示续推：本批角色追加进当前推荐行，recommendationId 不变。
	appendMode bool
}

// recommendForSetupAsync 是首推（S1）用的入口：没有候选收敛/续推参数，等价于零值
// setupRecommendRequest。保留这个形状，调用方（含既有用例）无需跟着改。
func (rt *discussionRuntime) recommendForSetupAsync(ctx context.Context, discussionID string, attempt int) {
	rt.recommendForSetupBatch(ctx, discussionID, attempt, setupRecommendRequest{})
}

// recommendForSetupBatch 是 S1/S2 共用的后台推荐管线：host 校验 → 候选设备 →
// broker 往返 → 角色绑定设备 → 落 recommendation（TTL 7 天）→ emit
// setup_prompt(roles)。任何一步失败都落 setup_recommend_failed（前端据以展示
// 重试入口），不再回写命令回调（它早已 200）。
func (rt *discussionRuntime) recommendForSetupBatch(ctx context.Context, discussionID string, attempt int, req setupRecommendRequest) {
	logger := rt.logger
	// code 是唯一权威，文案由它派生：主持设备「在线但跑不出推荐」有三种完全
	// 不同的内因（设备端轮次超时 / 应答无法解析 / 通道未装配），过去全部落在
	// 同一句「角色推荐失败，请重试」上，用户只能反复点重试。见
	// setupRecommendFailureMessage。
	fail := func(code string) {
		message := setupRecommendFailureMessage(code)
		d, err := rt.repo.Get(ctx, discussionID)
		if err != nil || d.Status != DiscussionStatusConfiguring {
			return
		}
		rec, fresh, err := rt.interactiveEmit(ctx, d, interactiveEvent{
			EventType: setupEventRecommendFailed,
			ActorID:   "system",
			Payload:   map[string]interface{}{"step": setupStepRoles, "code": code, "message": message},
			Key:       setupRecFailIdemPrefix + discussionID + ":" + intStr(attempt),
			StateVer:  d.Version,
		})
		if err == nil && fresh {
			rt.interactiveBroadcast(ctx, d, rec)
		}
		logger.Warn("discussion: setup 角色推荐失败",
			"discussionId", discussionID, "attempt", attempt, "code", code, "message", message)
	}

	d, err := rt.repo.Get(ctx, discussionID)
	if err != nil {
		return
	}
	// 用户可能已确认开始或半成品已被 GC 回收：推荐不再有意义。
	if d.Status != DiscussionStatusConfiguring {
		return
	}
	host, hostErr := rt.roundtable.resolveHost(ctx)
	if hostErr != "" {
		fail(hostErr)
		return
	}
	candidates, candCode := rt.roundtable.roleCandidateNodesFor(ctx, d.CreatedBy, host, req.nodeIDs, req.nodeIDsGiven)
	if candCode != "" {
		fail(candCode)
		return
	}
	// 续推：把上一批已绑定的设备从候选里剔掉。assignRoleNodes 只在 candidates 里
	// 挑设备，一旦挑中已占用的那台，confirm 就会以 ROLE_DEVICE_DUPLICATED 整单拒绝，
	// 而用户看到的只是一句看不懂的报错。前端本就只传「剩余未参与」，这里是兜底。
	var appendRec db.RongcloudRoleRecommendation
	appendOK := false
	if req.appendMode {
		if rec, ok := rt.currentSetupRecommendation(ctx, discussionID); ok {
			appendRec, appendOK = rec, true
			candidates = excludeNodeIDs(candidates, recommendationRoleNodeIDs(rec.Roles))
			if len(candidates) == 0 {
				// 剩余设备都已被上一批占满：没有可推荐的对象，按「没有可用设备」处理。
				fail("NO_ELIGIBLE_USER_DEVICES")
				return
			}
		} else {
			// 没有当前推荐行（比如首推就失败了）：没有可延续的 recommendationId，
			// 退化成普通推荐，前端从最新一张卡里取新的 recommendationId。
			logger.Warn("discussion: setup 续推找不到当前推荐行，退化为普通推荐",
				"discussionId", discussionID)
		}
	}
	if rt.nodeRequests == nil {
		logger.Warn("discussion: setup 推荐无法下发 —— node request broker 未装配")
		fail("SYSTEM_HOST_OFFLINE")
		return
	}
	timeout := host.timeoutSeconds
	if timeout <= 0 {
		timeout = 60
	}
	bctx, cancel := context.WithTimeout(ctx, time.Duration(timeout+5)*time.Second)
	defer cancel()
	payload, code := rt.nodeRequests.RecommendRoles(bctx, NodeRequestHost{
		AIUserID:             host.aiUserID,
		DisplayName:          host.displayName,
		Avatar:               host.avatar,
		RecommendationPrompt: rt.roundtable.recommendationPrompt(ctx),
		TimeoutSeconds:       timeout,
		ConfigVersion:        configVersionInt(host.configVersion),
	}, d.Topic, d.Goal, min(recommendRoleCountCap, len(candidates)), "")
	if code != "" {
		fail(code)
		return
	}
	// 推荐回来时讨论可能已推进/回收：落事件前再确认一次。
	d, err = rt.repo.Get(ctx, discussionID)
	if err != nil || d.Status != DiscussionStatusConfiguring {
		return
	}
	assignRoleNodes(payloadRoles(payload), candidates)

	rolesJSON, _ := json.Marshal(payload["roles"])
	expiresAt := time.Now().Add(recommendRolesTTL)
	// 落库：一次推荐的收尾只有两种落法 —— 新建一行（首推/换一批），或追加进
	// 当前行（续推）。追加的意义就是 recommendation_id 保持不变：上一批已经下发给
	// 前端的 role_id 全部继续有效，confirm 的校验因此零改动。
	recID := ""
	if appendOK {
		recID = appendRec.RecommendationID
		updated, aErr := rt.queries.AppendRongCloudRoleRecommendationRoles(ctx, db.AppendRongCloudRoleRecommendationRolesParams{
			RecommendationID: recID,
			Column2:          rolesJSON,
			ExpiresAt:        pgtype.Timestamptz{Time: expiresAt, Valid: true},
		})
		if aErr != nil {
			// 推荐行已被清理（或并发删除）：退化成新建一行，并把上一批角色一并带上 ——
			// 否则用户已经添加的角色在新行里查不到，confirm 会被整单拒绝。
			logger.Warn("discussion: setup 续推追加失败，退化为新建推荐行",
				"discussionId", discussionID, "recommendationId", recID, "error", aErr)
			recID, _ = payload["recommendation_id"].(string)
			rolesJSON = mergeRecommendationRolesJSON(appendRec.Roles, rolesJSON)
			recID = rt.insertSetupRecommendation(ctx, d, host, recID, rolesJSON, expiresAt)
		} else {
			recID = updated.RecommendationID
		}
	} else {
		recID, _ = payload["recommendation_id"].(string)
		recID = rt.insertSetupRecommendation(ctx, d, host, recID, rolesJSON, expiresAt)
	}

	// 事件契约里的角色形状（RecommendedRole）：roleName/roleInstructions/nodeId/
	// selected（默认全选，客户端勾选是暂态不落库）+ role_id（确认时回传校验用）。
	// 只发**本批**角色：续推时库里那一行已经是好几批的合集，把全集再发一次
	// 会让前端把上一批角色重复插进清单。
	roles := make([]map[string]interface{}, 0)
	for _, role := range payloadRoles(payload) {
		roles = append(roles, map[string]interface{}{
			"role_id":          stringValue(role["role_id"]),
			"roleName":         stringValue(role["role_name"]),
			"roleInstructions": stringValue(role["role_prompt"]),
			"nodeId":           stringValue(role["node_id"]),
			"selected":         true,
		})
	}
	if err := rt.emitSetupPrompt(ctx, d, setupStepRoles, map[string]interface{}{
		"step":             setupStepRoles,
		"prompt":           "我为这个议题推荐了这些智能体，勾选参与讨论的伙伴：",
		"roles":            roles,
		"recommendationId": recID,
		"expiresAt":        expiresAt.UnixMilli(),
		"attempt":          attempt,
	}, setupPromptIdemPrefix+setupStepRoles+":"+discussionID+":"+intStr(attempt)); err != nil {
		logger.Warn("discussion: setup_prompt(roles) emit failed", "discussionId", discussionID, "error", err)
	}
	rt.touchSetup(ctx, discussionID)
}

// insertSetupRecommendation 落一行新的推荐（首推/换一批）。幂等：同 request_id
// 已落过（重放）时读回既存推荐 id，返回最终生效的 recommendation_id。
func (rt *discussionRuntime) insertSetupRecommendation(ctx context.Context, d DiscussionRecord, host roundtableHostConfig, recID string, rolesJSON []byte, expiresAt time.Time) string {
	if _, err := rt.queries.InsertRongCloudRoleRecommendation(ctx, db.InsertRongCloudRoleRecommendationParams{
		RecommendationID: recID,
		RequestID:        "c2c_" + recID,
		UserID:           d.CreatedBy,
		HostNodeID:       host.nodeID,
		HostAiUserID:     host.aiUserID,
		ConfigVersion:    host.configVersion,
		Topic:            d.Topic,
		Goal:             d.Goal,
		Roles:            rolesJSON,
		ExpiresAt:        pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		if existing, gErr := rt.queries.GetRongCloudRoleRecommendationByRequest(ctx, "c2c_"+recID); gErr == nil {
			return existing.RecommendationID
		}
	}
	return recID
}

// currentSetupRecommendation 取当前正在展示的那行推荐（最近一条 roles 引导卡里的
// recommendationId）。续推要复用同一个 recommendation_id，而 configuring 阶段的
// 讨论行还没写 recommendation_id（confirm 时才落），事件里的这张卡才是唯一的锚点。
func (rt *discussionRuntime) currentSetupRecommendation(ctx context.Context, discussionID string) (db.RongcloudRoleRecommendation, bool) {
	events, err := rt.repo.ListEventsAfter(ctx, discussionID, 0)
	if err != nil {
		return db.RongcloudRoleRecommendation{}, false
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].EventType != setupEventPrompt {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
			continue
		}
		if step, _ := payload["step"].(string); step != setupStepRoles {
			continue
		}
		recID, _ := payload["recommendationId"].(string)
		if recID == "" {
			return db.RongcloudRoleRecommendation{}, false
		}
		rec, err := rt.queries.GetRongCloudRoleRecommendation(ctx, recID)
		if err != nil {
			return db.RongcloudRoleRecommendation{}, false
		}
		return rec, true
	}
	return db.RongcloudRoleRecommendation{}, false
}

// recommendationRoleNodeIDs 取出推荐行里各角色已绑定的设备（续推时要避开）。
func recommendationRoleNodeIDs(raw []byte) map[string]bool {
	var roles []map[string]interface{}
	_ = json.Unmarshal(raw, &roles)
	used := make(map[string]bool, len(roles))
	for _, role := range roles {
		if nodeID, _ := role["node_id"].(string); nodeID != "" {
			used[nodeID] = true
		}
	}
	return used
}

// excludeNodeIDs 从候选里剔掉已占用的设备，保持原有顺序。
func excludeNodeIDs(candidates []string, exclude map[string]bool) []string {
	if len(candidates) == 0 || len(exclude) == 0 {
		return candidates
	}
	out := make([]string, 0, len(candidates))
	for _, nodeID := range candidates {
		if exclude[nodeID] {
			continue
		}
		out = append(out, nodeID)
	}
	return out
}

// mergeRecommendationRolesJSON 把本批角色拼到既有角色数组后面（续推退化路径用）。
func mergeRecommendationRolesJSON(existing, batch []byte) []byte {
	var merged []interface{}
	_ = json.Unmarshal(existing, &merged)
	var incoming []interface{}
	if err := json.Unmarshal(batch, &incoming); err == nil {
		merged = append(merged, incoming...)
	}
	out, _ := json.Marshal(merged)
	return out
}

func stringValue(v interface{}) string {
	s, _ := v.(string)
	return s
}

func intStr(n int) string {
	return strconv.Itoa(n)
}

// ---- confirmRoundtableSetup ------------------------------------------------

// actionConfirmRoundtableSetup S3 确认开始：校验推荐与勾选角色 → ensureMembers
// （复用五阶段实现）→ CAS configuring→planning → emit setup_step_completed(confirm)
// → v3 引擎开局（后续 running/awaiting_user/… 全部走既有链路）。
func (rt *discussionRuntime) actionConfirmRoundtableSetup(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	if rt.roundtable == nil || !rt.interactiveReady() {
		return errResult(503, "discussion runtime not available")
	}
	chatroomID := paramTrim(cmd, "chatroom_id", "chatroomId")
	recommendationID := paramTrim(cmd, "recommendation_id", "recommendationId")
	if chatroomID == "" || recommendationID == "" {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	row, d, rErr := rt.findConfiguringSetup(ctx, chatroomID, actor.RcID)
	if rErr != nil {
		// 幂等重放：半成品已确认（configuring 行不存在）时，若该房间活跃讨论
		// 确属本人，直接回当前状态 —— 网络重试/双击不产生第二轮配置。
		if active, aErr := rt.queries.GetActiveRongCloudDiscussionByChatroom(ctx, chatroomID); aErr == nil && active.CreatedBy == actor.RcID {
			return okResult(map[string]interface{}{
				"chatroom_id": chatroomID, "discussion_id": active.DiscussionID, "status": active.Status,
			})
		}
		return roundtableErrorResult(rErr)
	}
	roles, rErr := parseSetupConfirmRoles(cmd)
	if rErr != nil {
		return roundtableErrorResult(rErr)
	}

	// 推荐必须属于本人、未过期、与当前 host 一致（与旧链路 validateCurrent 同语义，
	// 但允许勾选子集 —— 用户可以在 S2 取消勾选部分角色）。
	rec, err := rt.queries.GetRongCloudRoleRecommendation(ctx, recommendationID)
	if err != nil || rec.UserID != actor.RcID {
		return errResult(410, "RECOMMENDATION_EXPIRED")
	}
	if rec.ExpiresAt.Valid && rec.ExpiresAt.Time.Before(time.Now()) {
		return errResult(410, "RECOMMENDATION_EXPIRED")
	}
	host, hostErr := rt.roundtable.resolveHost(ctx)
	if hostErr != "" {
		return errResult(503, hostErr)
	}
	if rec.HostNodeID != host.nodeID || rec.HostAiUserID != host.aiUserID || rec.ConfigVersion != host.configVersion {
		return errResult(503, "SYSTEM_HOST_OFFLINE")
	}
	recommendedIDs := map[string]bool{}
	var recommended []map[string]interface{}
	_ = json.Unmarshal(rec.Roles, &recommended)
	for _, r := range recommended {
		if id, ok := r["role_id"].(string); ok {
			recommendedIDs[id] = true
		}
	}
	owned, eligible := rt.roundtable.ownedAndEligibleNodes(ctx, actor.RcID, host)
	// 加入侧权限闸门（discussion_join_gate.go）：owned 是按融云好友边算的，里面
	// 既有自己的 agent，也有别人分享过来的 —— 后者可能已经被主人关掉了讨论组
	// 权限。不在这里拦，它会照常被写进成员表，然后派发侧 fail-closed 不吭声。
	allowed, blocked := filterDiscussionJoinNodes(eligible, actor.RcID)
	for i, role := range roles {
		if !recommendedIDs[role.RoleID] {
			return commandResult{code: 400, message: "INVALID_ROUNDTABLE_REQUEST", data: map[string]interface{}{"role_index": i}}
		}
		if role.NodeID == host.nodeID || !owned[role.NodeID] {
			return commandResult{code: 400, message: "ROLE_DEVICE_NOT_OWNED", data: map[string]interface{}{"role_index": i}}
		}
		// 先判权限再判可用：两者的用户动作完全不同（前者去开开关，后者去启动设备），
		// 混成一句「设备当前不可用」会把人引到错误的排查方向。
		if _, ok := blocked[role.NodeID]; ok {
			return commandResult{code: 400, message: "ROLE_DEVICE_PERMISSION_DENIED", data: map[string]interface{}{"role_index": i}}
		}
		if _, ok := allowed[role.NodeID]; !ok {
			return commandResult{code: 400, message: "ROLE_DEVICE_UNAVAILABLE", data: map[string]interface{}{"role_index": i}}
		}
	}

	canonical := roundtableCanonical{
		ClientRequestID:  textValue(row.ClientRequestID),
		RecommendationID: recommendationID,
		Topic:            row.Topic,
		Goal:             row.Goal,
		RoundPolicy:      RoundPolicyUserConfirmed,
		Roles:            roles,
	}
	// nodes 只带**过闸的**那些：ensureMembers 从这里取节点行落成员，多带一个
	// 被权限拒的节点就等于把它写进成员表，而上面的循环已经保证没有这种角色。
	validated := roundtableValidated{host: host, nodes: allowed}
	if cerr := rt.roundtable.ensureMembers(ctx, chatroomID, canonical, validated); cerr != nil {
		return roundtableErrorResult(cerr)
	}

	// 角色板 + 主持快照：与 ensureDiscussion 的写形状逐字一致（v3 引擎按它派发）。
	roleSnapshot, hostSnapshot := setupRoleSnapshots(canonical, validated)
	confirmed, err := rt.queries.ConfirmRongCloudDiscussionSetup(ctx, db.ConfirmRongCloudDiscussionSetupParams{
		DiscussionID:       row.DiscussionID,
		ExpectedVersion:    row.Version,
		RoleSnapshot:       roleSnapshot,
		HostConfigSnapshot: hostSnapshot,
		RecommendationID:   textOrNull(recommendationID),
	})
	if err != nil {
		// CAS 失败：并发确认已推进（幂等接受）或 GC 已回收（诚实报冲突）。
		latest, gErr := rt.queries.GetRongCloudDiscussion(ctx, row.DiscussionID)
		if gErr == nil && latest.Status == DiscussionStatusPlanning && textValue(latest.RecommendationID) == recommendationID {
			d = discussionRecordFromRow(latest)
			return rt.finishSetupConfirm(ctx, d, chatroomID, row.DiscussionID, row.Topic)
		}
		return errResult(409, "DISCUSSION_SETUP_STEP_CONFLICT")
	}
	d = discussionRecordFromRow(confirmed)
	return rt.finishSetupConfirm(ctx, d, chatroomID, row.DiscussionID, row.Topic)
}

// finishSetupConfirm 确认成功后的公共收尾：落完成卡 + 广播 + v3 开局 + 回包。
func (rt *discussionRuntime) finishSetupConfirm(ctx context.Context, d DiscussionRecord, chatroomID, discussionID, topic string) commandResult {
	rec, fresh, err := rt.interactiveEmit(ctx, d, interactiveEvent{
		EventType: setupEventStepCompleted,
		ActorID:   "system",
		Payload:   map[string]interface{}{"step": setupStepConfirm, "topic": topic},
		Key:       setupDoneIdemPrefix + setupStepConfirm + ":" + discussionID,
		StateVer:  d.Version,
	})
	if err != nil {
		rt.logger.Warn("discussion: setup_step_completed emit failed", "discussionId", discussionID, "error", err)
	} else if fresh {
		rt.interactiveBroadcast(ctx, d, rec)
	}
	rt.beginInteractiveSafe(ctx, map[string]interface{}{"discussion_id": discussionID})
	return okResult(map[string]interface{}{
		"chatroom_id": chatroomID, "discussion_id": discussionID, "status": DiscussionStatusPlanning,
	})
}

// setupRoleSnapshots 生成确认时的角色板与主持快照（字段形状对齐 ensureDiscussion）。
func setupRoleSnapshots(canonical roundtableCanonical, validated roundtableValidated) ([]byte, []byte) {
	roles := map[string]interface{}{}
	for _, role := range canonical.Roles {
		node := validated.nodes[role.NodeID]
		roles[node.RongcloudUserID] = map[string]interface{}{
			"memberId":         node.RongcloudUserID,
			"nodeId":           role.NodeID,
			"nickname":         role.RoleName,
			"portraitUri":      "",
			"roleName":         role.RoleName,
			"roleInstructions": role.RolePrompt,
			"capabilities":     roundtableRequiredRoleCaps,
			"isHost":           false,
			"speakingOrder":    role.SpeakingOrder,
		}
		if role.Model != nil && *role.Model != "" {
			roles[node.RongcloudUserID].(map[string]interface{})["model"] = *role.Model
		}
	}
	// host 条目（含 model）与 host_config_snapshot 由同一个函数唯一地拼装，
	// 三个写入点共用 —— 见 roundtableHostSnapshots 的注释。
	return roundtableHostSnapshots(validated.host, roles)
}

// parseSetupConfirmRoles 解析确认时提交的勾选角色（snake_case/camelCase 字段双读）。
// speaking_order 按数组顺序重排（客户端勾选顺序即发言顺序），并做基本限长校验。
//
// 两条口径必须和推荐侧（`parseRecommendedRoles`）一致，否则会出现「推荐能过、
// 确认被拒」的死结，而用户看到的只是一句「请检查角色名称、提示词、设备和模型」：
//   - 限长按**字符数**（rune）计。以前用 len(string) 即 UTF-8 字节数 ——
//     「80」实际只够 26 个汉字、「8000」只够 2666 个汉字，中文角色名/设定
//     一长就整单拒绝；而 broker 解析推荐时用的是 rune，两边差了三倍。
//   - 角色名判重也要按 NFKC + 小写归一化（与 canonicalizeRoundtablePayload 同口径），
//     并给出专门的错误码，让客户端能直说「名字重了」而不是笼统的 400。
func parseSetupConfirmRoles(cmd CommandContent) ([]roundtableRoleInput, *RoundtableError) {
	raw, ok := cmd.Params["roles"].([]interface{})
	if !ok || len(raw) < 1 || len(raw) > 8 {
		return nil, roundtableErr("INVALID_ROUNDTABLE_REQUEST")
	}
	roles := make([]roundtableRoleInput, 0, len(raw))
	nodes := map[string]bool{}
	names := map[string]bool{}
	for i, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, roundtableRoleErr("INVALID_ROUNDTABLE_REQUEST", i)
		}
		roleID := strings.TrimSpace(getStringAny(m, "role_id", "roleId"))
		roleName := strings.TrimSpace(getStringAny(m, "role_name", "roleName"))
		prompt := strings.TrimSpace(getStringAny(m, "role_prompt", "rolePrompt", "role_instructions", "roleInstructions"))
		nodeID := strings.TrimSpace(getStringAny(m, "node_id", "nodeId"))
		if roleID == "" || !roundtableSafeID.MatchString(roleID) || roleName == "" ||
			utf8.RuneCountInString(roleName) > 80 ||
			prompt == "" || utf8.RuneCountInString(prompt) > 8000 ||
			nodeID == "" || utf8.RuneCountInString(nodeID) > 128 {
			return nil, roundtableRoleErr("INVALID_ROUNDTABLE_REQUEST", i)
		}
		nameKey := strings.ToLower(norm.NFKC.String(roleName))
		if names[nameKey] {
			return nil, roundtableRoleErr("ROLE_NAME_DUPLICATED", i)
		}
		names[nameKey] = true
		if nodes[nodeID] {
			return nil, roundtableRoleErr("ROLE_DEVICE_DUPLICATED", i)
		}
		nodes[nodeID] = true
		var model *string
		if ms := strings.TrimSpace(getStringAny(m, "model")); ms != "" {
			if utf8.RuneCountInString(ms) > 256 || !strings.Contains(ms, "/") {
				return nil, roundtableRoleErr("MODEL_NOT_AVAILABLE", i)
			}
			model = &ms
		}
		roles = append(roles, roundtableRoleInput{
			RoleID: roleID, RoleName: roleName, RolePrompt: prompt,
			NodeID: nodeID, Model: model, SpeakingOrder: i,
		})
	}
	return roles, nil
}

// ---- resumeSetup / touchSetup ----------------------------------------------

// actionResumeSetup 断点续引：刷新交互时间、重播当前步骤的引导卡（重播既有事件，
// 不落新行），返回断点快照。US-4：回收截止前回到半成品组，从上次中断的步骤继续。
func (rt *discussionRuntime) actionResumeSetup(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	if rt.roundtable == nil || !rt.interactiveReady() {
		return errResult(503, "discussion runtime not available")
	}
	chatroomID := paramTrim(cmd, "chatroom_id", "chatroomId")
	if chatroomID == "" {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	row, d, rErr := rt.findConfiguringSetup(ctx, chatroomID, actor.RcID)
	if rErr != nil {
		return roundtableErrorResult(rErr)
	}
	rt.touchSetup(ctx, row.DiscussionID)
	// 重播最近一次 setup_prompt（当前步骤的引导卡）：事件已在库里，广播本身幂等。
	if latest, ok := rt.latestSetupPrompt(ctx, row.DiscussionID); ok {
		rt.interactiveBroadcast(ctx, d, latest)
	}
	return okResult(map[string]interface{}{
		"chatroom_id": chatroomID, "discussion_id": row.DiscussionID,
		"status": row.Status, "step": row.SetupStep, "topic": row.Topic,
	})
}

// latestSetupPrompt 取该讨论最近一条 setup_prompt 事件（断点续引重播用）。
func (rt *discussionRuntime) latestSetupPrompt(ctx context.Context, discussionID string) (DiscussionEventRecord, bool) {
	events, err := rt.repo.ListEventsAfter(ctx, discussionID, 0)
	if err != nil {
		return DiscussionEventRecord{}, false
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].EventType == setupEventPrompt {
			return events[i], true
		}
	}
	return DiscussionEventRecord{}, false
}

// actionTouchSetup 心跳：仅刷新 last_interaction_at。非半成品（已确认/已回收）
// 也回 ok —— 心跳不该把「讨论已正常开始」报成错误。
func (rt *discussionRuntime) actionTouchSetup(ctx context.Context, cmd CommandContent, actor discussionActor) commandResult {
	chatroomID := paramTrim(cmd, "chatroom_id", "chatroomId")
	if chatroomID == "" {
		return errResult(400, "INVALID_ROUNDTABLE_REQUEST")
	}
	row, err := rt.queries.GetConfiguringRongCloudDiscussionByChatroom(ctx, chatroomID)
	if err != nil || row.CreatedBy != actor.RcID {
		return okResult(map[string]interface{}{"ok": true, "active": false})
	}
	rt.touchSetup(ctx, row.DiscussionID)
	return okResult(map[string]interface{}{"ok": true, "active": true})
}
