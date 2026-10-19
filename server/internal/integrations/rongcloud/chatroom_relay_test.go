package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// relayRecorder 是一个只记账的假融云：记录私聊派发（your_turn）与群/聊天室
// 广播，用来验证「谁在第几轮被派发、广播了什么」。
type relayRecorder struct {
	mu       sync.Mutex
	privates []map[string]string
	group    []map[string]string
}

func newRelayRecorder(t *testing.T) (*relayRecorder, *rongcloudAPIClient) {
	t.Helper()
	rec := &relayRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		entry := make(map[string]string, len(r.PostForm))
		for k, v := range r.PostForm {
			entry[k] = strings.Join(v, ",")
		}
		rec.mu.Lock()
		switch r.URL.Path {
		case "/message/private/publish.json":
			rec.privates = append(rec.privates, entry)
		case "/message/group/publish.json":
			rec.group = append(rec.group, entry)
		}
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200,"msgUID":"uid-1"}`)
	}))
	t.Cleanup(srv.Close)
	return rec, newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
}

func (r *relayRecorder) snapshot() (privates, group []map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]string(nil), r.privates...),
		append([]map[string]string(nil), r.group...)
}

// broadcastTexts 解出所有群/聊天室广播的正文（按发生顺序）。
func broadcastTexts(t *testing.T, group []map[string]string) []string {
	t.Helper()
	out := make([]string, 0, len(group))
	for _, entry := range group {
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(entry["content"]), &payload); err != nil {
			t.Fatalf("broadcast content is not JSON: %v (%s)", err, entry["content"])
		}
		out = append(out, payload.Content)
	}
	return out
}

// dispatchedParams 解出每次私聊派发的 round 与提示词。
func dispatchedParams(t *testing.T, privates []map[string]string) []struct {
	Round      int
	RolePrompt string
} {
	t.Helper()
	type dispatched struct {
		RequestID string `json:"request_id"`
		Params    struct {
			Round      int    `json:"round"`
			RolePrompt string `json:"role_prompt"`
		} `json:"params"`
	}
	out := make([]struct {
		Round      int
		RolePrompt string
	}, 0, len(privates))
	for _, entry := range privates {
		var decoded dispatched
		if err := json.Unmarshal([]byte(entry["content"]), &decoded); err != nil {
			t.Fatalf("dispatch content is not JSON: %v (%s)", err, entry["content"])
		}
		out = append(out, struct {
			Round      int
			RolePrompt string
		}{Round: decoded.Params.Round, RolePrompt: decoded.Params.RolePrompt})
	}
	return out
}

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
	if !strings.Contains(prompt, "更早的对话") {
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
	if strings.Contains(prompt, "更早的对话") {
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

// 预算远小于内容时，取舍规则是「保住要接的那位」：接龙接不上紧邻的上一位，
// 比看不到更早的人严重得多 —— 所以最后一条必须完整（或至少）留下。
func TestRelayRepliesBlock_PrioritisesLastSpeaker(t *testing.T) {
	replies := []relayReply{
		{roleName: "A", content: strings.Repeat("甲", 900)},
		{roleName: "B", content: strings.Repeat("乙", 900)},
		{roleName: "C", content: strings.Repeat("丙", 900)},
	}
	block, used := relayRepliesBlock(replies, 900)
	if used > 900 || len([]rune(block)) > 900 {
		t.Fatalf("replies block over budget: used=%d runes=%d", used, len([]rune(block)))
	}
	// C 是要接的话头，必须活下来。
	if !strings.Contains(block, "- C：") {
		t.Fatalf("the speaker to follow must survive the budget: %q", block)
	}
}

// 预算够时所有人都要在段里 —— 不能因为「最后一位优先」就把前面的人整段丢掉，
// 而且顺序必须是先来后到、要接的那位在最后（它是接力锚点）。
func TestRelayRepliesBlock_KeepsEveryoneWhenBudgetAllows(t *testing.T) {
	replies := []relayReply{
		{roleName: "A", content: "甲甲甲"},
		{roleName: "B", content: "乙乙乙"},
		{roleName: "C", content: "丙丙丙"},
	}
	block, used := relayRepliesBlock(replies, 900)
	if used > 900 {
		t.Fatalf("replies block over budget: used=%d", used)
	}
	for _, name := range []string{"- A：", "- B：", "- C："} {
		if !strings.Contains(block, name) {
			t.Fatalf("member %s missing from replies block: %q", name, block)
		}
	}
	// 顺序：A → B → C，接力锚点在末尾。
	if !(strings.Index(block, "- A：") < strings.Index(block, "- B：") &&
		strings.Index(block, "- B：") < strings.Index(block, "- C：")) {
		t.Fatalf("replies must stay in speaking order, last one at the tail: %q", block)
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

func TestGroupRelayRounds_ReadsGroupSetting(t *testing.T) {
	hub := &chatroomRelayHub{
		rt: nilRelayRuntime(),
		groupMirror: &GroupMirror{
			GroupProfiles: func(_ context.Context, _ []string) ([]GroupProfile, error) {
				return []GroupProfile{{GroupID: "g1", MaxRounds: 7}}, nil
			},
		},
	}
	if got := hub.groupRelayRounds(context.Background(), "g1"); got != 7 {
		t.Fatalf("groupRelayRounds = %d, want 7 (stored setting)", got)
	}
	// 群被镜像但没设过轮数（COALESCE 出来的 0）→ 默认值。
	hub.groupMirror = &GroupMirror{
		GroupProfiles: func(_ context.Context, _ []string) ([]GroupProfile, error) {
			return []GroupProfile{{GroupID: "g2", MaxRounds: 0}}, nil
		},
	}
	if got := hub.groupRelayRounds(context.Background(), "g2"); got != defaultGroupRelayRounds {
		t.Fatalf("unset setting must fall back to default, got %d", got)
	}
	// 没配镜像回调 → 默认值，不 panic（部署可以不带群功能）。
	hub.groupMirror = nil
	if got := hub.groupRelayRounds(context.Background(), "g3"); got != defaultGroupRelayRounds {
		t.Fatalf("nil mirror must fall back to default, got %d", got)
	}
}

func TestNormalizeGroupRelayRounds(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, defaultGroupRelayRounds},
		{-3, defaultGroupRelayRounds},
		{1, 1},
		{3, 3},
		{maxGroupRelayRounds, maxGroupRelayRounds},
		// 老群存着「上下文窗口」时代的 20/50/100，不能整份拿来当接力轮数。
		{20, maxGroupRelayRounds},
		{100, maxGroupRelayRounds},
	}
	for _, tc := range cases {
		if got := normalizeGroupRelayRounds(tc.in); got != tc.want {
			t.Fatalf("normalizeGroupRelayRounds(%d) = %d, want %d", tc.in, got, tc.want)
		}
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

// acceptFrom 等到 who 被登记为当前轮，然后投递一条回包。假的融云 client 让
// dispatchTurn 真正成功，于是 pump 会按状态机前进（而不是走「派发失败跳过」）。
func acceptFrom(t *testing.T, hub *chatroomRelayHub, room, who, text string) {
	t.Helper()
	var reqID string
	waitUntil(t, "dispatch to "+who, func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		s := hub.sessions[room]
		if s == nil || s.senderRCID != who || s.requestID == "" {
			return false
		}
		reqID = s.requestID
		return true
	})
	if !hub.AcceptResponse(context.Background(), who, reqID, text, false, "") {
		t.Fatalf("response from %s was not accepted by the relay", who)
	}
}

// 群聊「AI 对话轮数」= 2 时，一条用户消息应当让两位成员接力两轮：A1 B1 A2 B2。
func TestGroupRelay_TwoRoundsAcrossMembers(t *testing.T) {
	rec, client := newRelayRecorder(t)
	rt := newDiscussionRuntime(nil, nil, client, "sys-node", testLogger())
	hub := &chatroomRelayHub{sessions: map[string]*relaySession{}, rounds: map[string][]relayRound{}, rt: rt}
	members := []relayMember{
		{rcID: "rc_a", roleName: "A", order: 0},
		{rcID: "rc_b", roleName: "B", order: 1},
	}
	if !hub.StartGroup(context.Background(), "g1", members, "让AI生成个页面", "rc_user", 2) {
		t.Fatal("StartGroup should succeed")
	}

	acceptFrom(t, hub, "g1", "rc_a", "A1")
	acceptFrom(t, hub, "g1", "rc_b", "B1")
	// 第二轮：回到第一位成员，再由第二位收尾。
	acceptFrom(t, hub, "g1", "rc_a", "A2")
	acceptFrom(t, hub, "g1", "rc_b", "B2")

	waitUntil(t, "session cleanup", func() bool {
		return relaySessionState(hub, "g1") == nil
	})

	privates, group := rec.snapshot()
	if got := broadcastTexts(t, group); len(got) != 4 ||
		got[0] != "A1" || got[1] != "B1" || got[2] != "A2" || got[3] != "B2" {
		t.Fatalf("broadcasts = %v, want [A1 B1 A2 B2]", got)
	}
	params := dispatchedParams(t, privates)
	if len(params) != 4 {
		t.Fatalf("want 4 dispatches, got %d", len(params))
	}
	for i, want := range []int{1, 1, 2, 2} {
		if params[i].Round != want {
			t.Fatalf("dispatch %d round = %d, want %d", i, params[i].Round, want)
		}
	}
	if !strings.Contains(params[0].RolePrompt, "第 1/2 轮接龙") {
		t.Fatalf("round-1 prompt should announce the relay count, got %q", params[0].RolePrompt)
	}
	if !strings.Contains(params[2].RolePrompt, "第 2/2 轮") {
		t.Fatalf("round-2 prompt should carry the round hint, got %q", params[2].RolePrompt)
	}
	// 第二轮必须能看到第一轮的发言（否则成员只会把上一轮原样再讲一遍）。
	if !strings.Contains(params[2].RolePrompt, "A1") || !strings.Contains(params[2].RolePrompt, "B1") {
		t.Fatalf("round-2 prompt must carry round-1 speeches, got %q", params[2].RolePrompt)
	}

	hub.mu.Lock()
	rounds := append([]relayRound(nil), hub.rounds["g1"]...)
	hub.mu.Unlock()
	if len(rounds) != 1 {
		t.Fatalf("want one recorded round for the next user message, got %d", len(rounds))
	}
	if rounds[0].userText != "让AI生成个页面" || len(rounds[0].replies) != 4 {
		t.Fatalf("recorded round wrong: user=%q replies=%d", rounds[0].userText, len(rounds[0].replies))
	}
}

// 群聊设成 1 轮时保持旧行为：每人只说一次就收尾。
func TestGroupRelay_SingleRoundWhenConfiguredOne(t *testing.T) {
	rec, client := newRelayRecorder(t)
	rt := newDiscussionRuntime(nil, nil, client, "sys-node", testLogger())
	hub := &chatroomRelayHub{sessions: map[string]*relaySession{}, rounds: map[string][]relayRound{}, rt: rt}
	members := []relayMember{{rcID: "rc_a", roleName: "A", order: 0}}
	if !hub.StartGroup(context.Background(), "g2", members, "hi", "rc_user", 1) {
		t.Fatal("StartGroup should succeed")
	}
	acceptFrom(t, hub, "g2", "rc_a", "only-once")
	waitUntil(t, "session cleanup", func() bool {
		return relaySessionState(hub, "g2") == nil
	})
	privates, group := rec.snapshot()
	if got := broadcastTexts(t, group); len(got) != 1 || got[0] != "only-once" {
		t.Fatalf("broadcasts = %v, want [only-once]", got)
	}
	if params := dispatchedParams(t, privates); len(params) != 1 {
		t.Fatalf("want exactly one dispatch, got %d", len(params))
	}
}

// 回包成功但正文为空：中继以前直接静默跳过，用户看到「发了消息却零回复」。
// 现在必须广播一条可见的占位，且不把空内容记进上下文。
func TestRelay_EmptyReplyStillVisible(t *testing.T) {
	rec, client := newRelayRecorder(t)
	rt := newDiscussionRuntime(nil, nil, client, "sys-node", testLogger())
	hub := &chatroomRelayHub{sessions: map[string]*relaySession{}, rounds: map[string][]relayRound{}, rt: rt}
	members := []relayMember{{rcID: "rc_a", roleName: "A", order: 0}}
	if !hub.StartGroup(context.Background(), "g3", members, "让AI生成个html", "rc_user", 1) {
		t.Fatal("StartGroup should succeed")
	}
	acceptFrom(t, hub, "g3", "rc_a", "")
	waitUntil(t, "session cleanup", func() bool {
		return relaySessionState(hub, "g3") == nil
	})
	_, group := rec.snapshot()
	got := broadcastTexts(t, group)
	if len(got) != 1 || !strings.Contains(got[0], "本轮没有产出内容") {
		t.Fatalf("empty reply should surface a visible placeholder, got %v", got)
	}
	hub.mu.Lock()
	rounds := append([]relayRound(nil), hub.rounds["g3"]...)
	hub.mu.Unlock()
	if len(rounds) != 1 {
		t.Fatalf("want one recorded round, got %d", len(rounds))
	}
	for _, reply := range rounds[0].replies {
		if strings.Contains(reply.content, "本轮没有产出内容") {
			t.Fatal("the placeholder must not be fed back as context")
		}
	}
}

// 接龙语义（用户只起个头，之后是成员之间来回）：第二位及以后的成员，指令必须
// 点名**紧邻的上一位**，而不是笼统的「回应用户」。旧措辞把 agent 钉在「对用户
// 表态」上，于是每位成员各喊一句客套话、谁也不接谁 —— 线上表现为「各说各的」。
func TestBuildPrompt_PointsAtPreviousSpeaker(t *testing.T) {
	hub := &chatroomRelayHub{rt: nilRelayRuntime()}
	s := &relaySession{
		chatroomRCID: "roomD",
		userText:     "大家下午好啊",
		isGroup:      true,
		totalRounds:  1,
		members: []relayMember{
			{rcID: "rc_a", roleName: "hermes"},
			{rcID: "rc_b", roleName: "openclaw"},
		},
	}

	// 第一位：只有用户起的头可接，要明确告诉它「你开接」。
	first := hub.buildPrompt(s, s.members[0])
	if !strings.Contains(first, "现在轮到你开接") {
		t.Fatalf("first speaker must be told to open: %q", first)
	}
	if strings.Contains(first, "请接住") {
		t.Fatalf("first speaker has nobody to follow: %q", first)
	}
	if !strings.HasPrefix(first, "[group chat] ") {
		t.Fatalf("relay prompt prefix must stay intact (CLI 靠它判定场合): %q", first)
	}

	// 第二位：必须点名接住 hermes，并且把 hermes 的原话带上。
	s.replies = []relayReply{{roleName: "hermes", content: "下午好呀～今天过得怎么样？"}}
	second := hub.buildPrompt(s, s.members[1])
	if !strings.Contains(second, "请接住「hermes」刚才的话往下说") {
		t.Fatalf("second speaker must be pointed at the previous one: %q", second)
	}
	if !strings.Contains(second, "下午好呀～今天过得怎么样？") {
		t.Fatalf("previous speech must ride along: %q", second)
	}
	// 用户这一轮只起了个头，接力是成员之间的事 —— 不能再出现「回应用户」。
	if strings.Contains(second, "回应用户") {
		t.Fatalf("relay prompt must not ask the member to answer the user: %q", second)
	}
}

// 多轮接龙：第二轮的第一位接的是**上一轮收尾那位**，不是从头重来；同时用户原话
// 只作为「起头」出现一次，不再写成「用户刚在群里发言」那种与事实相反的话。
func TestBuildPrompt_RoundTwoFollowsPreviousRoundTail(t *testing.T) {
	hub := &chatroomRelayHub{rt: nilRelayRuntime()}
	s := &relaySession{
		chatroomRCID: "roomE",
		userText:     "聊聊天气",
		isGroup:      true,
		totalRounds:  3,
		roundIndex:   1,
		members:      []relayMember{{rcID: "rc_a", roleName: "A"}},
		history: []relayRound{{
			userText: "聊聊天气",
			replies: []relayReply{
				{roleName: "A", content: "今天不错"},
				{roleName: "B", content: "有点热"},
			},
		}},
	}
	prompt := hub.buildPrompt(s, s.members[0])
	if !strings.Contains(prompt, "请接住「B」刚才的话往下说") {
		t.Fatalf("round 2 must follow the previous round's tail: %q", prompt)
	}
	if !strings.Contains(prompt, "第 2/3 轮接龙") {
		t.Fatalf("round hint missing: %q", prompt)
	}
	if !strings.Contains(prompt, "用户起的头：聊聊天气") {
		t.Fatalf("the opening line must stay as the topic anchor: %q", prompt)
	}
	// 第二轮用户并没有发言，不能再说「用户刚在群里发言」。
	if strings.Contains(prompt, "用户刚在群里发言") {
		t.Fatalf("round 2 must not claim the user just spoke: %q", prompt)
	}
	// 上一轮 B 的原话必须在场（接力链条）。
	if !strings.Contains(prompt, "有点热") {
		t.Fatalf("previous round tail must ride along: %q", prompt)
	}
}

// 空产出的成员必须留在**本轮**接力链上：下一位得知道他空手，否则会以为前面
// 没人说过话、只能凭空表态。但他不该进历史存档 —— 用户下次发消息时那是噪音。
func TestRelay_EmptyReplyFeedsNextSpeakerButNotArchive(t *testing.T) {
	rec, client := newRelayRecorder(t)
	rt := newDiscussionRuntime(nil, nil, client, "sys-node", testLogger())
	hub := &chatroomRelayHub{sessions: map[string]*relaySession{}, rounds: map[string][]relayRound{}, rt: rt}
	members := []relayMember{
		{rcID: "rc_a", roleName: "A", order: 0},
		{rcID: "rc_b", roleName: "B", order: 1},
	}
	if !hub.StartGroup(context.Background(), "g4", members, "生成个页面", "rc_user", 1) {
		t.Fatal("StartGroup should succeed")
	}
	acceptFrom(t, hub, "g4", "rc_a", "") // A 空手
	acceptFrom(t, hub, "g4", "rc_b", "B1")
	waitUntil(t, "session cleanup", func() bool {
		return relaySessionState(hub, "g4") == nil
	})

	privates, _ := rec.snapshot()
	params := dispatchedParams(t, privates)
	if len(params) != 2 {
		t.Fatalf("want 2 dispatches, got %d", len(params))
	}
	// B 的提示词里必须写着 A 空手，并点名接 A（而不是让它当第一个接话的人）。
	if !strings.Contains(params[1].RolePrompt, "本轮没有产出内容") {
		t.Fatalf("next speaker must be told the previous one produced nothing: %q", params[1].RolePrompt)
	}
	if strings.Contains(params[1].RolePrompt, "现在轮到你开接") {
		t.Fatalf("next speaker must not be treated as the opener: %q", params[1].RolePrompt)
	}
	// 也不能叫它「接住 A 的话」—— A 根本没交付正文，接不了。
	if strings.Contains(params[1].RolePrompt, "请接住「A」") {
		t.Fatalf("must not ask to follow a member who produced nothing: %q", params[1].RolePrompt)
	}
	if !strings.Contains(params[1].RolePrompt, "这轮没交出发言正文") {
		t.Fatalf("next speaker must be told to carry the thread on its own: %q", params[1].RolePrompt)
	}

	hub.mu.Lock()
	rounds := append([]relayRound(nil), hub.rounds["g4"]...)
	hub.mu.Unlock()
	for _, round := range rounds {
		for _, reply := range round.replies {
			if strings.Contains(reply.content, "本轮没有产出内容") {
				t.Fatalf("placeholder must not reach the archive: %+v", round.replies)
			}
		}
	}
}

// 接龙段与历史段一起装入时，整条提示词仍要不越 CLI 的 argv 上限 —— 越界即
// 退化 stdin，openclaw/opencode/hermes 一律空转。
func TestBuildPrompt_DialogueStaysUnderArgvCap(t *testing.T) {
	hub := &chatroomRelayHub{rt: nilRelayRuntime()}
	for i := 1; i <= maxRelayContextRounds; i++ {
		hub.recordRound("roomZ", relayRound{
			userText: strings.Repeat("问", 300),
			replies: []relayReply{
				{roleName: "A", content: strings.Repeat("答", 900)},
				{roleName: "B", content: strings.Repeat("答", 900)},
			},
		})
	}
	members := []relayMember{{rcID: "rc_a", roleName: "A"}, {rcID: "rc_b", roleName: "B"}}
	s := &relaySession{
		chatroomRCID:  "roomZ",
		userText:      strings.Repeat("档", relayMaxTextLength), // 用户起头拉到入口上限
		isGroup:       true,
		members:       members,
		totalRounds:   3,
		contextRounds: normalizeContextRounds(maxRelayContextRounds),
	}
	hub.mu.Lock()
	hub.attachContextLocked(s)
	hub.mu.Unlock()
	s.replies = []relayReply{
		{roleName: "A", content: strings.Repeat("新答", 4000)},
	}
	prompt := hub.buildPrompt(s, members[1])
	if len([]rune(prompt)) >= argvPromptMaxChars {
		t.Fatalf("dialogue prompt must stay under the argv cap (%d), got %d runes",
			argvPromptMaxChars, len([]rune(prompt)))
	}
	if !strings.Contains(prompt, "用户起的头：") {
		t.Fatal("the opening line must never be squeezed out")
	}
}

func TestRelayHub_StreamPingExtendsTurn(t *testing.T) {
	hub := &chatroomRelayHub{sessions: map[string]*relaySession{}, rt: nilRelayRuntime()}
	s := &relaySession{
		chatroomRCID: "room3",
		members:      []relayMember{{rcID: "rc_a", roleName: "A", order: 0}},
		index:        0,
		requestID:    "relay_9_0",
		senderRCID:   "rc_a",
		currentDone:  make(chan struct{}),
		turnExt:      &turnExtension{deadline: time.Now().Add(10 * time.Millisecond)},
	}
	hub.sessions["room3"] = s
	ctx := context.Background()

	// 心跳（relay_ping_ 前缀）被消化且续期：deadline 被推到未来远处。
	before := s.turnExt.deadline
	if !hub.NotifyStreamActivity("relay_ping_relay_9_0") {
		t.Fatal("ping must be consumed as heartbeat")
	}
	if !s.turnExt.deadline.After(before.Add(time.Second)) {
		t.Fatalf("deadline should be pushed far out, got delta=%v", s.turnExt.deadline.Sub(before))
	}
	// 心跳绝不能当完成回包：done 保持开着。
	select {
	case <-s.currentDone:
		t.Fatal("ping must not close done")
	default:
	}
	// 真正的回包照常推进。
	if !hub.AcceptResponse(ctx, "rc_a", "relay_9_0", "ok", false, "") {
		t.Fatal("real response accepted")
	}
	select {
	case <-s.currentDone:
	default:
		t.Fatal("real response should close done")
	}
}

func TestRelayHub_StreamPingUnknownTurnConsumed(t *testing.T) {
	hub := &chatroomRelayHub{sessions: map[string]*relaySession{}, rt: nilRelayRuntime()}
	// 未知/已作废轮次的心跳：同样按心跳消化（返回 true），绝不漏给旧协调器。
	if !hub.NotifyStreamActivity("relay_ping_relay_gone") {
		t.Fatal("stale ping must still be consumed")
	}
	// 非 relay_ping_ 前缀：不消化，让调用方继续走回包路径。
	if hub.NotifyStreamActivity("relay_1_0") {
		t.Fatal("non-ping request id must not be consumed by the ping path")
	}
}
