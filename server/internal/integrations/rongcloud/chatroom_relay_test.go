package rongcloud

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 测试聚焦 hub 自身状态机：按序推进、回包唤醒、超时跳过、迟到/伪造回包吞掉。
// 派发动作（sendPrivateJSON → rt.client）在真 runtime 里走融云 HTTP；这里把
// rt.client 置 nil 会让 dispatchTurn 报错 → pump 视同「派发失败跳过」——这正好
// 让我们不需要假融云服务器就能测状态推进。

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout waiting for: " + what)
}

func relaySessionState(hub *chatroomRelayHub, room string) *relaySession {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.sessions[room]
}

func TestRelayMembersFor_OrderAndFilter(t *testing.T) {
	text := pgtype.Text{}
	rows := []db.ListRongCloudChatroomMembersWithNodeRow{
		{
			MemberType:   "user",
			Enabled:      true,
			MemberID:     text,
			NodeRcUserID: pgtype.Text{String: "rc_user", Valid: true},
			RoleName:     pgtype.Text{String: "用户", Valid: true},
		},
		{
			MemberType:      "ai",
			Enabled:         true,
			NodeRcUserID:    pgtype.Text{String: "rc_a", Valid: true},
			RoleName:        pgtype.Text{String: "主持", Valid: true},
			DiscussionModel: pgtype.Text{String: "p/m", Valid: true},
		},
		{
			MemberType:   "ai",
			Enabled:      false, // 禁用成员必须被过滤
			NodeRcUserID: pgtype.Text{String: "rc_b", Valid: true},
			RoleName:     pgtype.Text{String: "B", Valid: true},
		},
		{
			MemberType:   "ai",
			Enabled:      true,
			NodeRcUserID: pgtype.Text{String: "rc_c", Valid: true},
			Nickname:     "成员C", // 无 role_name 时回退 nickname
		},
	}
	members := relayMembersFor(rows)
	if len(members) != 2 {
		t.Fatalf("want 2 members, got %d", len(members))
	}
	if members[0].rcID != "rc_a" || members[1].rcID != "rc_c" {
		t.Fatalf("order wrong: %+v", members)
	}
	if members[0].roleName != "主持" || members[0].model != "p/m" {
		t.Fatalf("member0 fields wrong: %+v", members[0])
	}
	if members[1].roleName != "成员C" {
		t.Fatalf("member1 role fallback wrong: %+v", members[1])
	}
}

func TestRelayHub_SequentialDispatch(t *testing.T) {
	hub := &chatroomRelayHub{sessions: map[string]*relaySession{}, rt: nilRelayRuntime()}
	members := []relayMember{
		{rcID: "rc_a", roleName: "A", order: 0},
		{rcID: "rc_b", roleName: "B", order: 1},
	}
	ctx := context.Background()
	if !hub.Start(ctx, "room1", members, "hello", "", "", 0) {
		t.Fatal("start should succeed")
	}
	if hub.Start(ctx, "room1", members, "again", "", "", 0) {
		t.Fatal("second start must be rejected while running")
	}
	// 首位成员被登记为当前轮。
	waitUntil(t, "first member dispatched", func() bool {
		s := relaySessionState(hub, "room1")
		return s != nil && s.requestID != "" && s.senderRCID == "rc_a"
	})
	s := relaySessionState(hub, "room1")
	reqID, done := s.requestID, s.currentDone
	if !hub.AcceptResponse(ctx, "rc_a", reqID, "answer-a", false, "") {
		t.Fatal("first response should be accepted")
	}
	select {
	case <-done:
	default:
		t.Fatal("done should be closed after response")
	}
	// 第二位成员被登记（注意：rt.client == nil 会让 dispatchTurn 失败，
	// pump 视同跳过——为了真正测「推进」，我们改用错误回包路径:不行。
	// dispatchTurn 失败直接 index++，所以 rc_b 也会被登记后立刻跳过。
	// 本测试主要验证状态机推进顺序，接受这种近似。）
	// 迟到的第一轮回包必须被吞掉。
	if !hub.AcceptResponse(ctx, "rc_a", reqID, "late", false, "") {
		t.Fatal("stale response must be consumed (true), not forwarded")
	}
	// 会话最终被清理（两位成员都因 dispatch 失败被跳过）。
	waitUntil(t, "session cleanup", func() bool {
		return relaySessionState(hub, "room1") == nil
	})
}

func TestRelayHub_WrongSenderIgnored(t *testing.T) {
	hub := &chatroomRelayHub{sessions: map[string]*relaySession{}, rt: nilRelayRuntime()}
	// 手工放一个已派发状态的会话，不经 pump（pump 的 dispatchTurn 在
	// client==nil 时立刻跳过成员，无法稳定测「错误发送者」路径）。
	s := &relaySession{
		chatroomRCID: "room2",
		members:      []relayMember{{rcID: "rc_a", roleName: "A", order: 0}},
		index:        0,
		requestID:    "relay_1_0",
		senderRCID:   "rc_a",
		currentDone:  make(chan struct{}),
	}
	hub.sessions["room2"] = s
	// 非预期成员回包：吞掉但不推进。
	if !hub.AcceptResponse(context.Background(), "rc_intruder", "relay_1_0", "fake", false, "") {
		t.Fatal("wrong-sender response is consumed (true)")
	}
	select {
	case <-s.currentDone:
		t.Fatal("wrong sender must not advance the turn")
	default:
	}
	// 正主回包推进。
	if !hub.AcceptResponse(context.Background(), "rc_a", "relay_1_0", "ok", false, "") {
		t.Fatal("right sender accepted")
	}
	select {
	case <-s.currentDone:
	default:
		t.Fatal("right sender should close done")
	}
	// requestID 已被作废：重复回包被吞。
	if !hub.AcceptResponse(context.Background(), "rc_a", "relay_1_0", "dup", false, "") {
		t.Fatal("duplicate response consumed (true)")
	}
}

func TestRelayRequestIDPrefix(t *testing.T) {
	hub := &chatroomRelayHub{sessions: map[string]*relaySession{}, rt: nilRelayRuntime()}
	if hub.AcceptResponse(context.Background(), "rc_a", "turn_1_2_x", "x", false, "") {
		t.Fatal("non-relay request id must return false")
	}
	if hub.AcceptResponse(context.Background(), "rc_a", "", "x", false, "") {
		t.Fatal("empty request id must return false")
	}
}

// ---------------------------------------------------------------- 上下文轮数
//
// 「AI 对话轮数」这个设置项的实际语义：每次中继时，每个成员的提示词最多携带
// 这么多轮历史对话。0/负数表示「没设置」→ 默认值；超过上限按上限截断。

func TestNormalizeContextRounds(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, defaultRelayContextRounds},
		{-7, defaultRelayContextRounds},
		{1, 1},
		{20, 20},
		{maxRelayContextRounds, maxRelayContextRounds},
		{maxRelayContextRounds + 1, maxRelayContextRounds},
	}
	for _, tc := range cases {
		if got := normalizeContextRounds(tc.in); got != tc.want {
			t.Errorf("normalizeContextRounds(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestRecordRound_TrimsRetainedWindow(t *testing.T) {
	hub := &chatroomRelayHub{rt: nilRelayRuntime()} // rounds 故意留 nil：验证懒初始化
	for i := 0; i < maxRetainedRelayRounds+5; i++ {
		hub.recordRound("room", relayRound{userText: fmt.Sprintf("问题%d", i)})
	}
	hub.mu.Lock()
	got := hub.rounds["room"]
	hub.mu.Unlock()
	if len(got) != maxRetainedRelayRounds {
		t.Fatalf("retained rounds = %d, want %d", len(got), maxRetainedRelayRounds)
	}
	// 丢掉的是最旧的，保留的是最新的。
	if got[0].userText != "问题5" {
		t.Fatalf("oldest retained round = %q, want 问题5", got[0].userText)
	}
	// 空轮不占额度。
	before := len(got)
	hub.recordRound("room", relayRound{userText: "   "})
	hub.mu.Lock()
	after := len(hub.rounds["room"])
	hub.mu.Unlock()
	if after != before {
		t.Fatalf("blank round must not be recorded: %d -> %d", before, after)
	}
}

func TestAttachContext_CapsAtContextRoundsAndDeepCopies(t *testing.T) {
	hub := &chatroomRelayHub{rt: nilRelayRuntime()}
	for i := 1; i <= 5; i++ {
		hub.recordRound("room9", relayRound{
			userText: fmt.Sprintf("问题%d", i),
			replies:  []relayReply{{roleName: "A", content: fmt.Sprintf("回答%d", i)}},
		})
	}
	s := &relaySession{
		chatroomRCID:  "room9",
		userText:      "新问题",
		contextRounds: normalizeContextRounds(2),
	}
	hub.mu.Lock()
	hub.attachContextLocked(s)
	hub.mu.Unlock()
	if len(s.history) != 2 {
		t.Fatalf("history = %d rounds, want 2 (cap)", len(s.history))
	}
	if s.history[0].userText != "问题4" || s.history[1].userText != "问题5" {
		t.Fatalf("history must keep the newest rounds: %+v", s.history)
	}
	// 快照必须与滚动缓存解耦：改缓存不能污染在途提示词。
	hub.mu.Lock()
	hub.rounds["room9"][len(hub.rounds["room9"])-1].replies[0].content = "被改了"
	hub.mu.Unlock()
	if s.history[1].replies[0].content != "回答5" {
		t.Fatalf("history must be a deep copy, got %q", s.history[1].replies[0].content)
	}
}

func TestBuildPrompt_CarriesCappedHistory(t *testing.T) {
	hub := &chatroomRelayHub{rt: nilRelayRuntime()}
	for i := 1; i <= 5; i++ {
		hub.recordRound("room9", relayRound{
			userText: fmt.Sprintf("问题%d", i),
			replies:  []relayReply{{roleName: "A", content: fmt.Sprintf("回答%d", i)}},
		})
	}
	s := &relaySession{
		chatroomRCID:  "room9",
		userText:      "新问题",
		isGroup:       true,
		members:       []relayMember{{rcID: "rc_a", roleName: "A"}},
		contextRounds: normalizeContextRounds(2),
	}
	hub.mu.Lock()
	hub.attachContextLocked(s)
	hub.mu.Unlock()

	prompt := hub.buildPrompt(s, s.members[0])
	if !strings.HasPrefix(prompt, "[group chat] ") {
		t.Fatalf("group prompt prefix must stay intact: %q", prompt)
	}
	if !strings.Contains(prompt, "最近的对话记录") {
		t.Fatalf("prompt must carry the history block: %q", prompt)
	}
	if !strings.Contains(prompt, "问题5") || !strings.Contains(prompt, "回答5") {
		t.Fatalf("prompt must carry the newest round: %q", prompt)
	}
	if strings.Contains(prompt, "问题3") {
		t.Fatalf("prompt must not carry rounds beyond the cap: %q", prompt)
	}
}

func TestBuildPrompt_NoHistoryKeepsLegacyShape(t *testing.T) {
	hub := &chatroomRelayHub{rt: nilRelayRuntime()}
	s := &relaySession{
		chatroomRCID: "room1",
		userText:     "大家好",
		isGroup:      true,
		members:      []relayMember{{rcID: "rc_a", roleName: "A"}},
	}
	prompt := hub.buildPrompt(s, s.members[0])
	if strings.Contains(prompt, "最近的对话记录") {
		t.Fatalf("empty history must not add a block: %q", prompt)
	}
	if !strings.Contains(prompt, "大家好") {
		t.Fatalf("prompt must carry the user message: %q", prompt)
	}
}

func TestRelayHistoryBlock_TruncatesLongReply(t *testing.T) {
	long := strings.Repeat("很", relayHistoryReplyClip+50)
	block := relayHistoryBlock([]relayRound{{
		userText: "问",
		replies:  []relayReply{{roleName: "A", content: long}},
	}}, relayPromptContextLimit)
	if !strings.Contains(block, "（已截断）") {
		t.Fatalf("long reply must be clipped: %q", block)
	}
	if len([]rune(block)) > relayPromptContextLimit {
		t.Fatalf("history block over budget: %d runes", len([]rune(block)))
	}
	// 零/负预算不渲染任何东西 —— 提示词预算必须能被上层收紧。
	if relayHistoryBlock([]relayRound{{userText: "问"}}, 0) != "" {
		t.Fatal("zero budget must render nothing")
	}
}

func TestRelayHistoryBlock_BudgetDropsOldestRounds(t *testing.T) {
	// 每轮 ~ 200 字符，预算只够两轮多：必须留下最新的，丢掉最旧的。
	rounds := make([]relayRound, 0, 6)
	for i := 1; i <= 6; i++ {
		rounds = append(rounds, relayRound{
			userText: fmt.Sprintf("第%d轮的问题", i),
			replies:  []relayReply{{roleName: "A", content: strings.Repeat("答", 180)}},
		})
	}
	block := relayHistoryBlock(rounds, 420)
	if len([]rune(block)) > 420+8 {
		t.Fatalf("block must respect the budget: %d runes", len([]rune(block)))
	}
	if !strings.Contains(block, "第6轮的问题") {
		t.Fatalf("newest round must survive the budget: %q", block)
	}
	if strings.Contains(block, "第1轮的问题") {
		t.Fatalf("oldest round must be dropped first: %q", block)
	}
}

func TestRelayRepliesBlock_SharesBudgetAcrossMembers(t *testing.T) {
	replies := []relayReply{
		{roleName: "A", content: strings.Repeat("甲", 900)},
		{roleName: "B", content: strings.Repeat("乙", 900)},
		{roleName: "C", content: strings.Repeat("丙", 900)},
	}
	block, used := relayRepliesBlock(replies, 900)
	if used > 900 {
		t.Fatalf("replies block over budget: used=%d", used)
	}
	// 三位成员都必须出现在段里 —— 预算平分，不能前几位吃光。
	for _, name := range []string{"- A：", "- B：", "- C："} {
		if !strings.Contains(block, name) {
			t.Fatalf("member %s missing from replies block: %q", name, block)
		}
	}
}

func TestBuildPrompt_StaysUnderArgvCap(t *testing.T) {
	hub := &chatroomRelayHub{rt: nilRelayRuntime()}
	for i := 1; i <= maxRelayContextRounds; i++ {
		hub.recordRound("roomX", relayRound{
			userText: strings.Repeat("问", 500),
			replies: []relayReply{
				{roleName: "A", content: strings.Repeat("答", 1500)},
				{roleName: "B", content: strings.Repeat("答", 1500)},
			},
		})
	}
	members := []relayMember{{rcID: "rc_a", roleName: "A"}, {rcID: "rc_b", roleName: "B"}}
	s := &relaySession{
		chatroomRCID:  "roomX",
		userText:      "新问题",
		isGroup:       true,
		topic:         strings.Repeat("题", 200),
		members:       members,
		contextRounds: normalizeContextRounds(maxRelayContextRounds),
	}
	hub.mu.Lock()
	hub.attachContextLocked(s)
	hub.mu.Unlock()
	s.replies = []relayReply{
		{roleName: "A", content: strings.Repeat("新答", 4000)},
		{roleName: "B", content: strings.Repeat("新答", 4000)},
	}

	prompt := hub.buildPrompt(s, members[0])
	if len([]rune(prompt)) >= 8000 {
		t.Fatalf("prompt must stay under the CLI argv cap (8000), got %d runes", len([]rune(prompt)))
	}
	if !strings.Contains(prompt, "新问题") {
		t.Fatal("user message must never be squeezed out")
	}
}

func TestGroupContextRounds_ReadsGroupSetting(t *testing.T) {
	hub := &chatroomRelayHub{
		rt: nilRelayRuntime(),
		groupMirror: &GroupMirror{
			GroupProfiles: func(_ context.Context, _ []string) ([]GroupProfile, error) {
				return []GroupProfile{{GroupID: "g1", MaxRounds: 7}}, nil
			},
		},
	}
	if got := hub.groupContextRounds(context.Background(), "g1"); got != 7 {
		t.Fatalf("groupContextRounds = %d, want 7 (stored setting)", got)
	}
	// 群被镜像但没设过轮数（COALESCE 出来的 0）→ 默认值。
	hub.groupMirror = &GroupMirror{
		GroupProfiles: func(_ context.Context, _ []string) ([]GroupProfile, error) {
			return []GroupProfile{{GroupID: "g2", MaxRounds: 0}}, nil
		},
	}
	if got := hub.groupContextRounds(context.Background(), "g2"); got != defaultRelayContextRounds {
		t.Fatalf("unset setting must fall back to default, got %d", got)
	}
	// 没配镜像回调 → 默认值，不 panic（部署可以不带群功能）。
	hub.groupMirror = nil
	if got := hub.groupContextRounds(context.Background(), "g3"); got != defaultRelayContextRounds {
		t.Fatalf("nil mirror must fall back to default, got %d", got)
	}
}

func TestPumpRecordsRoundForNextTurn(t *testing.T) {
	hub := &chatroomRelayHub{sessions: map[string]*relaySession{}, rounds: map[string][]relayRound{}, rt: nilRelayRuntime()}
	members := []relayMember{{rcID: "rc_a", roleName: "A", order: 0}}
	if !hub.Start(context.Background(), "room7", members, "第一句", "", "", 5) {
		t.Fatal("start should succeed")
	}
	// dispatchTurn 在 client==nil 时失败 → pump 直接跳过成员并收尾，本轮仍要记账
	// （用户消息本身就是上下文的一部分）。
	waitUntil(t, "round recorded", func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.rounds["room7"]) == 1
	})
	hub.mu.Lock()
	recorded := hub.rounds["room7"][0]
	hub.mu.Unlock()
	if recorded.userText != "第一句" {
		t.Fatalf("recorded user text = %q, want 第一句", recorded.userText)
	}
}

// nilRelayRuntime 构造一个 client 为 nil 的最小 runtime：dispatchTurn 会失败
// （pump 视同跳过），但 interactiveTurnTimeout / logger 可用。
func nilRelayRuntime() *discussionRuntime {
	return newDiscussionRuntime(nil, nil, nil, "", nil)
}

var _ = pgtype.Text{}
