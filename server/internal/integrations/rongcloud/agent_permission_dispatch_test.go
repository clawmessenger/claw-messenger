package rongcloud

// 派发侧权限闸门（迁移 607）的回归用例。
//
// 覆盖任务书 A 组五条：群聊过滤 / 全滤 409、聊天室与讨论组用不同开关、
// v3 member_skipped{permission_denied}、以及「查不到权限必须 fail-closed」。
//
// 为什么这些用例必须存在：权限位是整个 R1 唯一真正拦得住「被分享出去的 agent
// 被别人的群拉走」的地方，而它的失败模式全是**静默**的 —— 判错了不会报错，
// 只会表现为「agent 不吭声」或者「关了还在说话」。后者更糟：主人以为关掉了。

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ---- 夹具 ------------------------------------------------------------------

// permEnv 是权限用例的最小环境：一个 workspace + 一个真库连接。
//
// 单独开连接而不是复用别的 fixture：权限闸门的判定只依赖 rongcloud_node 一张表，
// 用不上聊天室/讨论那套重夹具；而群聊中继（StartGroupRelay）根本不查成员表
// （成员由调用方传入），所以这里不需要房间。
type permEnv struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	wsID    pgtype.UUID
}

func newPermEnv(t *testing.T) *permEnv {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("skipping: cannot build pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skipping: db unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return &permEnv{pool: pool, queries: db.New(pool), wsID: seedWorkspaceRow(t, pool)}
}

// permFlags 是五个开关的值。零值即「全关」，与迁移 607 的 DEFAULT 一致。
type permFlags struct {
	Private    bool
	Group      bool
	Chatroom   bool
	Discussion bool
	FS         bool
}

// seedAgent 建一个 AI 节点行并把权限位写成给定值。
//
// claimedBy 为空串表示「未认领」—— 这是一条**必须**单独覆盖的分支：实现里
// 未认领节点一律放行（owner 无法点开关），把它写成已认领会让两类节点混为一谈。
func (e *permEnv) seedAgent(t *testing.T, nodeID, claimedBy string, flags permFlags, behaviorPrompt string) db.RongcloudNode {
	t.Helper()
	seedNode(t, e.queries, e.wsID, nodeID,
		`["discussion_participant","discussion_roundtable","discussion_model_routing","discussion_host"]`)

	claimed := pgtype.Text{}
	if claimedBy != "" {
		claimed = pgtype.Text{String: claimedBy, Valid: true}
	}
	if _, err := e.pool.Exec(context.Background(), `
		UPDATE rongcloud_node
		   SET claimed_by_user_id = $2,
		       allow_private     = $3,
		       allow_group       = $4,
		       allow_chatroom    = $5,
		       allow_discussion  = $6,
		       allow_fs          = $7,
		       behavior_prompt   = $8
		 WHERE node_id = $1`,
		nodeID, claimed, flags.Private, flags.Group, flags.Chatroom, flags.Discussion, flags.FS, behaviorPrompt,
	); err != nil {
		t.Fatalf("set node permission: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM rongcloud_node WHERE node_id = $1`, nodeID)
	})
	reloaded, err := e.queries.GetRongCloudNodeByNodeID(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("reload node %s: %v", nodeID, err)
	}
	return reloaded
}

// setAgentPermission 就地改一个已存在节点的权限位与认领者（供「同一房间换开关」的用例）。
//
// claimedBy 必须给：seedNode 建的节点 claimed_by_user_id 为 NULL，而未认领节点
// 在 allows() 里一律放行 —— 不认领的话「关掉开关」根本测不出来（用例会假绿）。
func (e *permEnv) setAgentPermission(t *testing.T, nodeID, claimedBy string, flags permFlags) {
	t.Helper()
	claimed := pgtype.Text{}
	if claimedBy != "" {
		claimed = pgtype.Text{String: claimedBy, Valid: true}
	}
	if _, err := e.pool.Exec(context.Background(), `
		UPDATE rongcloud_node
		   SET claimed_by_user_id = $2,
		       allow_group = $3, allow_chatroom = $4, allow_discussion = $5
		 WHERE node_id = $1`, nodeID, claimed, flags.Group, flags.Chatroom, flags.Discussion); err != nil {
		t.Fatalf("update node permission: %v", err)
	}
}

// resetAgentPermissionCache 清进程级权限缓存（15s TTL）。
//
// 不清会出两种假结果：前一个用例写进去的「允许」让本用例的「关闭」测不出来，
// 或者反过来的「查不到」负缓存让刚建的节点被判成无权限。缓存是包级变量，
// 用例之间必须自己负责隔离。
func resetAgentPermissionCache() {
	agentPermissionCacheStore.mu.Lock()
	agentPermissionCacheStore.entries = map[string]agentPermissionCacheEntry{}
	agentPermissionCacheStore.mu.Unlock()
}

// permRelayRuntime 造一个「引擎就绪」的 runtime：群聊中继入口要求
// interactiveReady()，即 repo/queries 非空 + interactiveEnabled。
func permRelayRuntime(queries *db.Queries, logger *slog.Logger) *discussionRuntime {
	rt := newDiscussionRuntime(queries, NewDiscussionRepository(queries, logger), nil, "rc_test_sys", logger)
	rt.interactiveEnabled = true
	return rt
}

func newPermHub(rt *discussionRuntime) *chatroomRelayHub {
	return &chatroomRelayHub{sessions: map[string]*relaySession{}, rounds: map[string][]relayRound{}, rt: rt}
}

// ---- A1：群聊 --------------------------------------------------------------

// TestGroupRelayPermissionGate_FiltersSpeakers 钉住「群聊里 allow_group=false 的
// agent 被从 speakers 里摘掉」。
func TestGroupRelayPermissionGate_FiltersSpeakers(t *testing.T) {
	env := newPermEnv(t)
	allowed := env.seedAgent(t, "perm-grp-on-"+uniqueID("t"), "owner-1", permFlags{Group: true}, "")
	denied := env.seedAgent(t, "perm-grp-off-"+uniqueID("t"), "owner-1", permFlags{Group: false}, "")
	resetAgentPermissionCache()

	hub := newPermHub(permRelayRuntime(env.queries, testLogger()))
	aiMembers := []GroupMember{
		{UserID: allowed.RongcloudUserID, Nickname: "开着的"},
		{UserID: denied.RongcloudUserID, Nickname: "关掉的"},
	}
	allMembers := append([]GroupMember{{UserID: "rc_requester"}}, aiMembers...)

	result := hub.StartGroupRelay(context.Background(), "g-perm-"+uniqueID("t"), "rc_requester", "大家好", allMembers, aiMembers)
	if result.code != 200 {
		t.Fatalf("relay should start, got %d (%s)", result.code, result.message)
	}
	data, _ := result.data.(map[string]interface{})
	speakers, _ := data["speakers"].(int)
	if speakers != 1 {
		t.Fatalf("speakers = %v, want 1（只有 allow_group=true 的那个）", data["speakers"])
	}
}

// TestGroupRelayPermissionGate_AllDeniedIs409 钉住「全员被过滤 → 409，且文案是
// 既有的那句」。空列表不能走到「当前无法发起对话」那条兜底 409 —— 两句话对用户
// 含义不同：前者是「没人有权限」，后者是「系统忙」。
func TestGroupRelayPermissionGate_AllDeniedIs409(t *testing.T) {
	env := newPermEnv(t)
	a := env.seedAgent(t, "perm-grp-x-"+uniqueID("t"), "owner-1", permFlags{Group: false}, "")
	b := env.seedAgent(t, "perm-grp-y-"+uniqueID("t"), "owner-1", permFlags{}, "")
	resetAgentPermissionCache()

	hub := newPermHub(permRelayRuntime(env.queries, testLogger()))
	aiMembers := []GroupMember{{UserID: a.RongcloudUserID}, {UserID: b.RongcloudUserID}}
	allMembers := append([]GroupMember{{UserID: "rc_requester"}}, aiMembers...)

	result := hub.StartGroupRelay(context.Background(), "g-perm-all-"+uniqueID("t"), "rc_requester", "大家好", allMembers, aiMembers)
	if result.code != 409 {
		t.Fatalf("code = %d (%s), want 409", result.code, result.message)
	}
	if result.message != "该群没有可发言的 AI 成员" {
		t.Fatalf("message = %q, want 该群没有可发言的 AI 成员", result.message)
	}
}

// TestGroupRelayPermissionGate_UnclaimedStillSpeaks 是**正控**：未认领节点必须
// 照旧能说话。
//
// 没有这条，一个「一律拒绝」的实现也能让上面两条负向用例全绿 —— 那会把所有
// 历史节点永久锁死（它们没有主人，没有任何入口能打开开关）。
func TestGroupRelayPermissionGate_UnclaimedStillSpeaks(t *testing.T) {
	env := newPermEnv(t)
	unclaimed := env.seedAgent(t, "perm-grp-unclaimed-"+uniqueID("t"), "", permFlags{}, "")
	resetAgentPermissionCache()

	hub := newPermHub(permRelayRuntime(env.queries, testLogger()))
	aiMembers := []GroupMember{{UserID: unclaimed.RongcloudUserID}}
	allMembers := append([]GroupMember{{UserID: "rc_requester"}}, aiMembers...)

	result := hub.StartGroupRelay(context.Background(), "g-perm-unclaimed-"+uniqueID("t"), "rc_requester", "hi", allMembers, aiMembers)
	if result.code != 200 {
		t.Fatalf("unclaimed node must still be dispatched, got %d (%s)", result.code, result.message)
	}
}

// ---- A2 / A3：聊天室 vs 讨论组 ---------------------------------------------

// TestRelayGate_ChatroomSceneUsesChatroomSwitch 钉住「房间**没有**讨论行时用
// allow_chatroom」。
//
// 这里刻意让 allow_discussion=true：如果实现错用了讨论组开关，本用例会绿。
// 反过来（allow_chatroom=true + allow_discussion=false）由下一个用例覆盖。
func TestRelayGate_ChatroomSceneUsesChatroomSwitch(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	env := &permEnv{pool: f.pool, queries: f.queries, wsID: f.wsID}
	env.setAgentPermission(t, f.memberNode.NodeID, "owner-1", permFlags{Chatroom: false, Discussion: true})
	resetAgentPermissionCache()

	hub := newPermHub(f.rt)
	result := hub.StartRelay(context.Background(), f.roomRCID, f.ownerRCID, "随便聊聊")
	if result.code != 409 {
		t.Fatalf("chatroom scene must honour allow_chatroom=false, got %d (%s)", result.code, result.message)
	}
	if result.message != "该聊天室没有可发言的 AI 成员" {
		t.Fatalf("message = %q", result.message)
	}
}

// TestRelayGate_DiscussionSceneUsesDiscussionSwitch 是上一条的镜像：
// 房间**有**讨论行时改用 allow_discussion。
//
// 两步同一个房间、同一组开关，只多了「插一条讨论行」—— 差异只能来自场景判定
// （ListByChatroom 是否返回行），这正是最容易写错的那一处。
func TestRelayGate_DiscussionSceneUsesDiscussionSwitch(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	env := &permEnv{pool: f.pool, queries: f.queries, wsID: f.wsID}
	// chatroom 开关开着、discussion 关着：判定一旦错用 chatroom 就会放行。
	env.setAgentPermission(t, f.memberNode.NodeID, "owner-1", permFlags{Chatroom: true, Discussion: false})
	resetAgentPermissionCache()

	_, err := f.rt.repo.Create(context.Background(), CreateDiscussionParams{
		DiscussionID: uniqueID("disc-gate"),
		ChatroomID:   f.roomRCID,
		WorkspaceID:  f.wsID,
		CreatedBy:    f.ownerRCID,
		Topic:        "有讨论行才算讨论组",
		Goal:         "验证场景判定",
		Mode:         DiscussionModeRoundtable,
		Status:       DiscussionStatusPlanning,
		RoundPolicy:  RoundPolicyUserConfirmed,
		RoleSnapshot: mustJSON(map[string]interface{}{}),
	})
	if err != nil {
		t.Fatalf("create discussion row: %v", err)
	}
	resetAgentPermissionCache()

	hub := newPermHub(f.rt)
	result := hub.StartRelay(context.Background(), f.roomRCID, f.ownerRCID, "随便聊聊")
	if result.code != 409 {
		t.Fatalf("discussion scene must honour allow_discussion=false, got %d (%s)", result.code, result.message)
	}
}

// TestRelayGate_DiscussionSceneAllowedByDiscussionSwitch 是正控：插上讨论行之后，
// allow_discussion=true 就真的会派发。
//
// 单独一条的理由：上一条只证明「关着就不派发」，一个「恒 409」的实现同样能过。
func TestRelayGate_DiscussionSceneAllowedByDiscussionSwitch(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	env := &permEnv{pool: f.pool, queries: f.queries, wsID: f.wsID}
	env.setAgentPermission(t, f.memberNode.NodeID, "owner-1", permFlags{Chatroom: false, Discussion: true})
	resetAgentPermissionCache()

	if _, err := f.rt.repo.Create(context.Background(), CreateDiscussionParams{
		DiscussionID: uniqueID("disc-gate-ok"),
		ChatroomID:   f.roomRCID,
		WorkspaceID:  f.wsID,
		CreatedBy:    f.ownerRCID,
		Topic:        "讨论组",
		Goal:         "验证放行",
		Mode:         DiscussionModeRoundtable,
		Status:       DiscussionStatusPlanning,
		RoundPolicy:  RoundPolicyUserConfirmed,
		RoleSnapshot: mustJSON(map[string]interface{}{}),
	}); err != nil {
		t.Fatalf("create discussion row: %v", err)
	}
	resetAgentPermissionCache()

	hub := newPermHub(f.rt)
	result := hub.StartRelay(context.Background(), f.roomRCID, f.ownerRCID, "随便聊聊")
	if result.code != 200 {
		t.Fatalf("allow_discussion=true must dispatch, got %d (%s)", result.code, result.message)
	}
	data, _ := result.data.(map[string]interface{})
	if speakers, _ := data["speakers"].(int); speakers != 1 {
		t.Fatalf("speakers = %v, want 1", data["speakers"])
	}
}

// ---- A4：讨论组 v3 正式轮次 ------------------------------------------------

// TestInteractiveNext_SkipsPermissionDeniedMember 钉住 v3 正式轮次的闸门：
// allow_discussion=false 的成员被跳过，事件是既有的 member_skipped，但 reason
// 换成 permission_denied（前端只校验 reason 长度，不需要同步发版）。
//
// 顺带钉住「全员被跳过 → 本轮判 failed」：被权限跳过不是「大家都在忙」，绝不能
// 去请主持人写一份没有素材的纪要。
func TestInteractiveNext_SkipsPermissionDeniedMember(t *testing.T) {
	f := setupDiscussionFixture(t)
	ctx := context.Background()
	env := &permEnv{pool: f.pool, queries: f.queries, wsID: f.wsID}
	env.setAgentPermission(t, f.memberNode.NodeID, "owner-1", permFlags{Discussion: false})
	resetAgentPermissionCache()

	created := newFixtureDiscussion(t, f)
	memberRC := f.memberNode.RongcloudUserID

	if err := f.rt.interactiveNext(ctx, created); err != nil {
		t.Fatalf("interactiveNext: %v", err)
	}

	events, err := f.rt.repo.ListEventsAfter(ctx, created.DiscussionID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	skipped := false
	for _, e := range events {
		if e.EventType != "member_skipped" {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("decode member_skipped payload: %v", err)
		}
		if payload["memberId"] != memberRC {
			continue
		}
		skipped = true
		if payload["reason"] != "permission_denied" {
			t.Errorf("reason = %v, want permission_denied", payload["reason"])
		}
		if round, _ := payload["round"].(float64); int(round) != 1 {
			t.Errorf("round = %v, want 1", payload["round"])
		}
	}
	if !skipped {
		t.Fatalf("no member_skipped event for %s; events=%v", memberRC, eventTypes(events))
	}

	after, err := f.rt.repo.Get(ctx, created.DiscussionID)
	if err != nil {
		t.Fatalf("get discussion: %v", err)
	}
	if after.Status != DiscussionStatusFailed {
		t.Fatalf("status = %q, want failed（全员被权限跳过不能进汇总）", after.Status)
	}
}

// TestInteractiveNext_PermissionDeniedDoesNotPreemptDisabledReason 钉住两种跳过
// 原因的分工：成员被移除/禁用时仍是旧文案，不能一律盖成 permission_denied
// —— 那会让「节点出故障」看起来像「主人关了开关」，排查方向完全不同。
func TestInteractiveNext_PermissionDeniedDoesNotPreemptDisabledReason(t *testing.T) {
	f := setupDiscussionFixture(t)
	ctx := context.Background()
	env := &permEnv{pool: f.pool, queries: f.queries, wsID: f.wsID}
	env.setAgentPermission(t, f.memberNode.NodeID, "owner-1", permFlags{Discussion: true})
	resetAgentPermissionCache()

	created := newFixtureDiscussion(t, f)
	memberRC := f.memberNode.RongcloudUserID
	// 把成员从聊天室禁用：interactiveMemberEnabled 先判，必须走旧文案。
	if _, err := f.pool.Exec(ctx, `
		UPDATE rongcloud_chatroom_member SET enabled = FALSE
		 WHERE chatroom_id = (SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id = $1)
		   AND member_id = $2`, f.roomRCID, memberRC); err != nil {
		t.Fatalf("disable member: %v", err)
	}

	if err := f.rt.interactiveNext(ctx, created); err != nil {
		t.Fatalf("interactiveNext: %v", err)
	}
	events, err := f.rt.repo.ListEventsAfter(ctx, created.DiscussionID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	found := false
	for _, e := range events {
		if e.EventType != "member_skipped" {
			continue
		}
		var payload map[string]interface{}
		_ = json.Unmarshal(e.Payload, &payload)
		if payload["memberId"] != memberRC {
			continue
		}
		found = true
		if payload["reason"] != "Member is disabled or no longer available" {
			t.Errorf("reason = %v, want the legacy disabled wording", payload["reason"])
		}
	}
	if !found {
		t.Fatalf("no member_skipped event for the disabled member; events=%v", eventTypes(events))
	}
}

// ---- A5：权限查询失败必须 fail-closed --------------------------------------

// syncBuffer 让 slog 在并发下也能被测试读取（pump 跑在 goroutine 里）。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newCaptureLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})), buf
}

// brokenQueries 返回一组指向「已经关掉的连接池」的 queries：任何一次查询都会
// 报错，正好用来模拟「权限表读不出来」。
func brokenQueries(t *testing.T) *db.Queries {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skipping: cannot build pool: %v", err)
	}
	pool.Close() // 立即关闭：后续 Acquire 一律失败
	return db.New(pool)
}

// TestAgentPermissions_FailsClosedOnQueryError 是 R1 最关键的负向断言：读不出
// 权限时必须**当作没有权限**。
//
// fail-open 的后果比「这一轮没人说话」严重得多：一条查库故障就能让所有被主人
// 关掉的 agent 重新开口，而主人以为自己已经关好了。
func TestAgentPermissions_FailsClosedOnQueryError(t *testing.T) {
	env := newPermEnv(t)
	// 节点本身是「全开」的：如果实现把查询失败当成「没限制」，这里就会放行。
	node := env.seedAgent(t, "perm-broken-"+uniqueID("t"), "owner-1", permFlags{Group: true, Chatroom: true, Discussion: true}, "")
	resetAgentPermissionCache()

	logger, buf := newCaptureLogger()
	rt := permRelayRuntime(brokenQueries(t), logger)

	set := rt.agentPermissions(context.Background(), []string{node.RongcloudUserID})
	if len(set) != 0 {
		t.Fatalf("permission set must be empty on query failure, got %+v", set)
	}
	// actor 故意传这个节点的认领者（"owner-1"）：主人豁免也**不能**越过 fail-closed。
	// 读不到权限行 = 不知道有没有主人，此时按「主人来了就放行」猜，等于用一次
	// 查库故障给所有被关掉的 agent 开门。
	for _, scene := range []agentPermissionScene{sceneGroup, sceneChatroom, sceneDiscussion} {
		if set.allows(node.RongcloudUserID, scene, "owner-1") {
			t.Fatalf("scene %d must deny on query failure (fail-open!)", scene)
		}
	}
	if log := buf.String(); !strings.Contains(log, "dispatch denied (no-permission-info)") {
		t.Fatalf("failure path must leave a trace, log=%q", log)
	}
}

// TestGroupRelayPermissionGate_QueryErrorDeniesEveryone 把上一条从「快照层面」
// 抬到「中继入口」：查库失败时群聊一个都不派发（走 409，而不是照常开聊）。
func TestGroupRelayPermissionGate_QueryErrorDeniesEveryone(t *testing.T) {
	env := newPermEnv(t)
	a := env.seedAgent(t, "perm-broken-grp-a-"+uniqueID("t"), "owner-1", permFlags{Group: true}, "")
	b := env.seedAgent(t, "perm-broken-grp-b-"+uniqueID("t"), "owner-1", permFlags{Group: true}, "")
	resetAgentPermissionCache()

	logger, _ := newCaptureLogger()
	hub := newPermHub(permRelayRuntime(brokenQueries(t), logger))
	aiMembers := []GroupMember{{UserID: a.RongcloudUserID}, {UserID: b.RongcloudUserID}}
	allMembers := append([]GroupMember{{UserID: "rc_requester"}}, aiMembers...)

	result := hub.StartGroupRelay(context.Background(), "g-perm-broken-"+uniqueID("t"), "rc_requester", "hi", allMembers, aiMembers)
	if result.code != 409 {
		t.Fatalf("permission lookup failure must deny dispatch, got %d (%s)", result.code, result.message)
	}
}

// eventTypes 是给失败信息用的可读摘要，避免打印整个事件体。
func eventTypes(events []DiscussionEventRecord) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.EventType)
	}
	return out
}

// TestRelayGate_RoomTypeLookupFailureIsFailClosed 钉住 G1：**查不到房间类型就不
// 派发**，不许降级成 sceneChatroom。
//
// 两个开关是互不覆盖的维度：只开 allow_chatroom 的 agent 配进一个其实是讨论组的
// 房间，一旦「查不出来就当普通聊天室」，它就能越权进讨论组。所以这里两个开关都
// 开着 —— 只要实现还在降级，就会真的派发出去（200），用例立刻红。
func TestRelayGate_RoomTypeLookupFailureIsFailClosed(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	env := &permEnv{pool: f.pool, queries: f.queries, wsID: f.wsID}
	env.setAgentPermission(t, f.memberNode.NodeID, "owner-1", permFlags{Chatroom: true, Discussion: true})
	resetAgentPermissionCache()

	// 房间与成员照旧从真库读得到，只有「这个房间有没有讨论行」这一次查询失败。
	f.rt.repo = NewDiscussionRepository(brokenQueries(t), testLogger())

	hub := newPermHub(f.rt)
	result := hub.StartRelay(context.Background(), f.roomRCID, f.ownerRCID, "随便聊聊")
	if result.code != 500 {
		t.Fatalf("room type lookup failure must deny dispatch, got %d (%s)", result.code, result.message)
	}
	if !strings.Contains(result.message, "无法确认房间类型") {
		t.Fatalf("message = %q, want 无法确认房间类型", result.message)
	}
}

// ---- A6：主人豁免（设备主人自己发起时不受自己的开关限制）---------------------
//
// 口径：这五个开关管的是「**别的账号**能不能用我的 agent」。主人自己发起时不该被
// 自己的开关挡住 —— 与设备端私聊闸门 `decidePrivateChat` 的 `reason: "owner"` 同口径。
//
// 为什么这批用例必须存在：豁免是**放宽**，写错的方向全是「本该拒绝的也放行了」，
// 而这类错误没有任何报错，只表现为「别人也能拉走我的 agent」。所以每一条放行
// 断言都配一条同场景的拒绝断言，另外三条负控（未认领 / 认领者为空 / 拿不到
// 发起人）专门保护既有语义不被这次放宽冲掉。

// TestAgentPermissionSet_OwnerExemption 是豁免判定的纯函数用例：不碰库，覆盖
// 三种场景 × 主人/别人，外加四条必须保持原状的负控。
func TestAgentPermissionSet_OwnerExemption(t *testing.T) {
	const (
		owner    = "rc_owner"
		stranger = "rc_stranger"
		node     = "rc_node"
	)
	cases := []struct {
		name  string
		perm  agentPermission
		scene agentPermissionScene
		actor string
		want  bool
		why   string
	}{
		{
			name:  "a. 群聊关闭 + 发起人是主人 → 放行",
			perm:  agentPermission{claimed: true, claimedBy: owner, allowGroup: false},
			scene: sceneGroup, actor: owner, want: true,
			why: "主人关掉「允许群聊发言」后，他自己在群里说话仍然该有回应",
		},
		{
			name:  "b. 群聊关闭 + 发起人是别人 → 拒绝",
			perm:  agentPermission{claimed: true, claimedBy: owner, allowGroup: false},
			scene: sceneGroup, actor: stranger, want: false,
			why: "开关的本职：别的账号不能把主人关掉的 agent 拉进群聊",
		},
		{
			name:  "c. 讨论组关闭 + 组是主人建的（actor=d.CreatedBy）→ 放行",
			perm:  agentPermission{claimed: true, claimedBy: owner, allowDiscussion: false},
			scene: sceneDiscussion, actor: owner, want: true,
			why: "主人自己建的讨论组里，他自己关掉的开关不该让他开不了口",
		},
		{
			name:  "c'. 讨论组关闭 + 组是别人建的 → 拒绝",
			perm:  agentPermission{claimed: true, claimedBy: owner, allowDiscussion: false},
			scene: sceneDiscussion, actor: stranger, want: false,
			why: "别人建的讨论组里没有豁免，这是 allow_discussion 唯一真正拦住的东西",
		},
		{
			name:  "d. 聊天室关闭 + 发起人是主人 → 放行",
			perm:  agentPermission{claimed: true, claimedBy: owner, allowChatroom: false},
			scene: sceneChatroom, actor: owner, want: true,
			why: "同上，聊天室场景按发起人判",
		},
		{
			name:  "d'. 聊天室关闭 + 发起人是别人 → 拒绝",
			perm:  agentPermission{claimed: true, claimedBy: owner, allowChatroom: false},
			scene: sceneChatroom, actor: stranger, want: false,
			why: "同上，场景开关对陌生账号仍然生效",
		},
		{
			name:  "e. 负控：未认领节点即使全关也放行（存量兼容 fail-open）",
			perm:  agentPermission{claimed: false, claimedBy: ""},
			scene: sceneGroup, actor: stranger, want: true,
			why: "没有主人就没有能点开关的人，按「关」处理会把历史节点永久锁死",
		},
		{
			name:  "e'. 负控：未认领节点 + 拿不到发起人 也放行",
			perm:  agentPermission{claimed: false, claimedBy: ""},
			scene: sceneDiscussion, actor: "", want: true,
			why: "未认领的 fail-open 必须在豁免之前判定，缺 actor 不能把它变成拒绝",
		},
		{
			name:  "f. 负控：已认领但认领者为空 → 不豁免，按开关拒绝",
			perm:  agentPermission{claimed: true, claimedBy: "", allowGroup: false},
			scene: sceneGroup, actor: owner, want: false,
			why: "理论上不可达（claimed 由 claimedBy 非空推出），一旦出现只能 fail-closed，" +
				"不能让「空串 == 空串」把所有人的 agent 都放行",
		},
		{
			name:  "g. 负控：actorRCID 为空 → 不豁免，按开关拒绝",
			perm:  agentPermission{claimed: true, claimedBy: owner, allowChatroom: false},
			scene: sceneChatroom, actor: "", want: false,
			why: "拿不到发起人时不猜，缺参数不是放行理由",
		},
		{
			name:  "g'. 负控：actorRCID 为空 + 开关开着 → 照常放行",
			perm:  agentPermission{claimed: true, claimedBy: owner, allowChatroom: true},
			scene: sceneChatroom, actor: "", want: true,
			why: "缺 actor 只是「不豁免」，不是「一律拒绝」——正文口径由开关决定",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := agentPermissionSet{node: tc.perm}
			if got := set.allows(node, tc.scene, tc.actor); got != tc.want {
				t.Fatalf("allows(%q, scene=%d, actor=%q) = %v, want %v（%s）",
					node, tc.scene, tc.actor, got, tc.want, tc.why)
			}
		})
	}

	// 查不到节点行永远是拒绝：豁免只对「查得到、且认领者就是发起人」的行生效。
	t.Run("h. 负控：快照里没有这个 rc id → 即使 actor 是主人也拒绝", func(t *testing.T) {
		empty := agentPermissionSet{}
		for _, scene := range []agentPermissionScene{sceneGroup, sceneChatroom, sceneDiscussion} {
			if empty.allows(node, scene, owner) {
				t.Fatalf("scene %d must stay fail-closed for unknown rc id", scene)
			}
		}
	})
}

// TestFilterMembersByPermission_PassesActor 钉住「actor 真的透传下去了」。
//
// 上面那条纯函数用例只证明 allows 会豁免；如果 filterMembersByPermission 忘了把
// actorRCID 传给 allows（或调用点传了空串），线上表现是「豁免完全不生效」，
// 而 allows 的单测仍然全绿。这里用一次过滤把接线钉住。
func TestFilterMembersByPermission_PassesActor(t *testing.T) {
	const (
		ownerRC = "rc_owner"
		otherRC = "rc_other"
	)
	members := []relayMember{{rcID: ownerRC}, {rcID: otherRC}}
	set := agentPermissionSet{
		ownerRC: {claimed: true, claimedBy: ownerRC, allowGroup: false},
		otherRC: {claimed: true, claimedBy: otherRC, allowGroup: false},
	}

	kept := filterMembersByPermission(members, set, sceneGroup, ownerRC)
	if len(kept) != 1 || kept[0].rcID != ownerRC {
		t.Fatalf("kept = %+v, want only the owner's own node", kept)
	}

	// 没有 actor（拿不到发起人）：两个都该被开关挡掉。
	if kept := filterMembersByPermission(members, set, sceneGroup, ""); len(kept) != 0 {
		t.Fatalf("kept = %+v, want empty when actor is unknown", kept)
	}
}

// TestGroupRelayPermissionGate_OwnerExemptionApplies 是群聊入口的接线验证：
// 同一条消息，发起人是主人 → 200，发起人是别人 → 409。
//
// 两组各自的 hub 是新建的（sessions 空）：同一个 hub 上第二次调用会因为「上一条
// 中继还在跑」走排队分支返回 200，让负向断言假绿。
func TestGroupRelayPermissionGate_OwnerExemptionApplies(t *testing.T) {
	env := newPermEnv(t)
	ownerRC := "rc_owner_" + uniqueID("t")
	// 开关全关，但认领者就是下面那个发起人。
	node := env.seedAgent(t, "perm-grp-owner-"+uniqueID("t"), ownerRC, permFlags{}, "")
	resetAgentPermissionCache()

	const strangerRC = "rc_someone_else"
	aiMembers := []GroupMember{{UserID: node.RongcloudUserID}}
	// 陌生人**也是群成员**：否则会先被「仅群成员可以发起对话」403 拦下，
	// 走到 409 就证明它确实穿过了成员校验、被权限闸门挡住了。
	allMembers := append([]GroupMember{{UserID: ownerRC}, {UserID: strangerRC}}, aiMembers...)

	t.Run("发起人是主人 → 放行", func(t *testing.T) {
		hub := newPermHub(permRelayRuntime(env.queries, testLogger()))
		result := hub.StartGroupRelay(context.Background(), "g-owner-exempt-"+uniqueID("t"),
			ownerRC, "大家好", allMembers, aiMembers)
		if result.code != 200 {
			t.Fatalf("owner-initiated relay must pass the gate, got %d (%s)", result.code, result.message)
		}
		data, _ := result.data.(map[string]interface{})
		if speakers, _ := data["speakers"].(int); speakers != 1 {
			t.Fatalf("speakers = %v, want 1（主人的节点要留在列表里）", data["speakers"])
		}
	})

	t.Run("发起人是别人 → 仍然拒绝", func(t *testing.T) {
		hub := newPermHub(permRelayRuntime(env.queries, testLogger()))
		result := hub.StartGroupRelay(context.Background(), "g-owner-exempt-x-"+uniqueID("t"),
			strangerRC, "大家好", allMembers, aiMembers)
		if result.code != 409 {
			t.Fatalf("stranger-initiated relay must stay denied, got %d (%s)", result.code, result.message)
		}
	})
}

// TestRelayGate_ChatroomOwnerExemption 是聊天室入口的接线验证：发起人是主人时，
// allow_chatroom=false 不再挡住他自己。
//
// 镜像（主人 ≠ 发起人 → 409）由 TestRelayGate_ChatroomSceneUsesChatroomSwitch 覆盖，
// 那条用的是 "owner-1" 这个陌生认领者，正好是本用例的反面。
func TestRelayGate_ChatroomOwnerExemption(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	env := &permEnv{pool: f.pool, queries: f.queries, wsID: f.wsID}
	env.setAgentPermission(t, f.memberNode.NodeID, f.ownerRCID, permFlags{Chatroom: false})
	resetAgentPermissionCache()

	hub := newPermHub(f.rt)
	result := hub.StartRelay(context.Background(), f.roomRCID, f.ownerRCID, "随便聊聊")
	if result.code != 200 {
		t.Fatalf("owner-initiated chatroom relay must pass the gate, got %d (%s)", result.code, result.message)
	}
	data, _ := result.data.(map[string]interface{})
	if speakers, _ := data["speakers"].(int); speakers != 1 {
		t.Fatalf("speakers = %v, want 1", data["speakers"])
	}
}

// TestInteractiveNext_OwnerCreatedDiscussionNotSkipped 是讨论组 v3 正式轮次的
// 接线验证：allow_discussion=false，但讨论组是主人建的（d.CreatedBy == 认领者），
// 该成员必须照常被派发，不能出 member_skipped{permission_denied}。
//
// 这条替的是「用 d.CreatedBy 而不是本轮触发者」的取舍：v3 的轮次由系统推进，
// 第 2 轮的触发者是 AI，如果按触发者判，豁免会在中途失效 —— 本用例断言的是
// 语义（主人建的局里他的 agent 能说话），而不是某个具体实现细节。
func TestInteractiveNext_OwnerCreatedDiscussionNotSkipped(t *testing.T) {
	f := setupDiscussionFixture(t)
	ctx := context.Background()
	env := &permEnv{pool: f.pool, queries: f.queries, wsID: f.wsID}
	// 认领者 = fixture 的真人 owner = newFixtureDiscussion 里的 CreatedBy。
	env.setAgentPermission(t, f.memberNode.NodeID, f.ownerRCID, permFlags{Discussion: false})
	resetAgentPermissionCache()

	created := newFixtureDiscussion(t, f)
	memberRC := f.memberNode.RongcloudUserID

	if err := f.rt.interactiveNext(ctx, created); err != nil {
		t.Fatalf("interactiveNext: %v", err)
	}

	events, err := f.rt.repo.ListEventsAfter(ctx, created.DiscussionID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	dispatched := false
	for _, e := range events {
		if e.EventType == "member_skipped" {
			var payload map[string]interface{}
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatalf("decode member_skipped payload: %v", err)
			}
			if payload["memberId"] == memberRC && payload["reason"] == "permission_denied" {
				t.Fatalf("owner-created discussion must not skip the owner's node by permission; events=%v",
					eventTypes(events))
			}
		}
		if e.EventType == "assignment_created" {
			var payload map[string]interface{}
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatalf("decode assignment_created payload: %v", err)
			}
			if payload["targetId"] == memberRC {
				dispatched = true
			}
		}
	}
	if !dispatched {
		t.Fatalf("owner's node must be dispatched in an owner-created discussion; events=%v",
			eventTypes(events))
	}
}
