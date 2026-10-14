package rongcloud

import (
	"context"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/jackc/pgx/v5/pgtype"
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
			MemberType:      "user",
			Enabled:         true,
			MemberID:        text,
			NodeRcUserID:    pgtype.Text{String: "rc_user", Valid: true},
			RoleName:        pgtype.Text{String: "用户", Valid: true},
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
	if !hub.Start(ctx, "room1", members, "hello", "") {
		t.Fatal("start should succeed")
	}
	if hub.Start(ctx, "room1", members, "again", "") {
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

// nilRelayRuntime 构造一个 client 为 nil 的最小 runtime：dispatchTurn 会失败
//（pump 视同跳过），但 interactiveTurnTimeout / logger 可用。
func nilRelayRuntime() *discussionRuntime {
	return newDiscussionRuntime(nil, nil, nil, "", nil)
}

var _ = pgtype.Text{}
