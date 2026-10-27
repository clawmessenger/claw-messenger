package rongcloud

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 讨论组「中途加人」的测试（task 2026-10-27/task-005）。
//
// 产品口径（已冻结）：角色板只在**轮边界**与聊天室成员表对齐 ——
// interactiveBegin 首轮、interactiveControl 进入新轮、interactiveNext 本轮发言阶段
// 结束处各同步一次；中途插进来的成员从下一轮开始被派发，本轮不插队。
//
// 覆盖三层：纯函数（discussionRolesRebased 的重建规则）→ 真库端到端（成员表插入
// 新成员 → 轮边界同步 → 角色板落库 → 新成员真的被派发 your_turn 且首见上下文是
// 尾部 N 条）→ 事件契约（role-sync 借 discussion_progress 承载，data 必须是空对象）。

// ---- 纯函数：角色板重建 ----------------------------------------------------

// rolesSnapshot 把「成员 ID → 条目字段」编成 role_snapshot 的 JSON。
func rolesSnapshot(t *testing.T, entries map[string]map[string]interface{}) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal role snapshot: %v", err)
	}
	return raw
}

// roleBoard 是纯函数用例的固定角色板：甲(speaking_order 0) + 乙(1) + 主持人。
func roleBoard(t *testing.T) map[string]map[string]interface{} {
	t.Helper()
	return map[string]map[string]interface{}{
		"rc-a": {
			"memberId": "rc-a", "nodeId": "node-rc-a", "nickname": "甲", "roleName": "甲",
			"roleInstructions": "以甲的身份发言", "speakingOrder": float64(0), "isHost": false,
		},
		"rc-b": {
			"memberId": "rc-b", "nodeId": "node-rc-b", "nickname": "乙", "roleName": "乙",
			"roleInstructions": "以乙的身份发言", "speakingOrder": float64(1), "isHost": false,
		},
		"rc-host": {
			"memberId": "rc-host", "nodeId": "node-rc-host", "nickname": "中立主持人",
			"roleName": "中立主持人", "isHost": true,
		},
	}
}

// aiMemberRow 造一行「可用 AI 成员」（成员表侧的唯一真相）。
func aiMemberRow(rcID, roleName, instructions string) db.ListRongCloudChatroomMembersWithNodeRow {
	return db.ListRongCloudChatroomMembersWithNodeRow{
		MemberType:       "ai",
		Enabled:          true,
		NodeRcUserID:     pgtype.Text{String: rcID, Valid: true},
		MemberID:         pgtype.Text{String: rcID, Valid: true},
		NodeID:           pgtype.UUID{},
		BusinessNodeID:   pgtype.Text{String: "node-" + rcID, Valid: true},
		Nickname:         roleName,
		RoleName:         pgtype.Text{String: roleName, Valid: true},
		RoleInstructions: pgtype.Text{String: instructions, Valid: true},
		Capabilities:     []byte(`["discussion_participant"]`),
	}
}

// parseRoleSnapshot 解析重建后的角色板。
func parseRoleSnapshot(t *testing.T, raw json.RawMessage) map[string]map[string]interface{} {
	t.Helper()
	var out map[string]map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("角色板不是合法 JSON: %v (%s)", err, raw)
	}
	return out
}

// speakingOrderOf 读角色板条目的 speaking_order（JSON 数字是 float64）。
func speakingOrderOf(t *testing.T, entry map[string]interface{}) int {
	t.Helper()
	value, ok := entry["speakingOrder"].(float64)
	if !ok {
		t.Fatalf("条目缺少 speakingOrder: %+v", entry)
	}
	return int(value)
}

// TestRolesRebased_KeepsExistingRoleFieldsAndOrder：成员表只用来判「谁还在」，
// 已有成员的 role_name / role_instructions / speaking_order 一律以角色板为准 ——
// 有人在成员设置页改了顺序或角色名，不能把进行中的讨论打乱。
func TestRolesRebased_KeepsExistingRoleFieldsAndOrder(t *testing.T) {
	d := DiscussionRecord{RoleSnapshot: rolesSnapshot(t, roleBoard(t))}
	// 成员表里故意写一套「不同的」角色名与顺序（并且把两人顺序反过来）：
	// 重建结果必须仍是角色板那一套。
	rowA := aiMemberRow("rc-a", "被改过的甲", "被改过的指令")
	rowA.SpeakingOrder = pgtype.Int4{Int32: 1, Valid: true}
	rowB := aiMemberRow("rc-b", "被改过的乙", "被改过的指令")
	rowB.SpeakingOrder = pgtype.Int4{Int32: 0, Valid: true}
	rows := []db.ListRongCloudChatroomMembersWithNodeRow{rowB, rowA}

	snapshot, added, removed, ok := discussionRolesRebased(d, rows)
	if !ok {
		t.Fatal("已有成员都还在，重建不该失败")
	}
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("没有增删成员时不该报变更：added=%v removed=%v", added, removed)
	}
	board := parseRoleSnapshot(t, snapshot)
	for rcID, wantName := range map[string]string{"rc-a": "甲", "rc-b": "乙"} {
		entry, exists := board[rcID]
		if !exists {
			t.Fatalf("已有成员 %s 从角色板消失了: %s", rcID, snapshot)
		}
		if entry["roleName"] != wantName {
			t.Errorf("%s roleName = %v，期望沿用角色板的 %q", rcID, entry["roleName"], wantName)
		}
		instructions, _ := entry["roleInstructions"].(string)
		if !strings.Contains(instructions, wantName) || strings.Contains(instructions, "被改过") {
			t.Errorf("%s roleInstructions = %q，期望沿用角色板的人设", rcID, instructions)
		}
	}
	if got := speakingOrderOf(t, board["rc-a"]); got != 0 {
		t.Errorf("rc-a speakingOrder = %d，期望 0", got)
	}
	if got := speakingOrderOf(t, board["rc-b"]); got != 1 {
		t.Errorf("rc-b speakingOrder = %d，期望 1（成员表里的 0 不该覆盖）", got)
	}
}

// TestRolesRebased_AppendsNewMemberWithContiguousOrder：新成员追加到末尾，
// speaking_order 连续（下发到设备端的顺序不能跳号）。
func TestRolesRebased_AppendsNewMemberWithContiguousOrder(t *testing.T) {
	d := DiscussionRecord{RoleSnapshot: rolesSnapshot(t, roleBoard(t))}
	rows := []db.ListRongCloudChatroomMembersWithNodeRow{
		aiMemberRow("rc-a", "甲", "以甲的身份发言"),
		aiMemberRow("rc-b", "乙", "以乙的身份发言"),
		aiMemberRow("rc-c", "丙", "以丙的身份发言"),
	}

	snapshot, added, removed, ok := discussionRolesRebased(d, rows)
	if !ok {
		t.Fatal("重建不该失败")
	}
	if len(removed) != 0 {
		t.Fatalf("没有成员被移除：removed=%v", removed)
	}
	if len(added) != 1 || added[0] != "丙" {
		t.Fatalf("added = %v，期望只有新成员「丙」", added)
	}
	board := parseRoleSnapshot(t, snapshot)
	entry, exists := board["rc-c"]
	if !exists {
		t.Fatalf("新成员 rc-c 没进角色板: %s", snapshot)
	}
	if got := speakingOrderOf(t, entry); got != 2 {
		t.Errorf("新成员 speakingOrder = %d，期望追加到末尾的 2", got)
	}
	orders := make([]int, 0, len(board))
	for _, e := range board {
		if isHost, _ := e["isHost"].(bool); isHost {
			continue
		}
		orders = append(orders, speakingOrderOf(t, e))
	}
	sort.Ints(orders)
	for i, got := range orders {
		if got != i {
			t.Fatalf("普通成员 speaking_order = %v，期望连续 0..n-1", orders)
		}
	}
}

// TestRolesRebased_DropsRemovedMemberAndCompactsOrder：被移出 / 禁用的成员从角色板
// 剔除，剩余成员的 speaking_order 压缩成 0..n-1（否则设备端会看到跳号的顺序）。
func TestRolesRebased_DropsRemovedMemberAndCompactsOrder(t *testing.T) {
	board := roleBoard(t)
	board["rc-c"] = map[string]interface{}{
		"memberId": "rc-c", "nodeId": "node-rc-c", "nickname": "丙", "roleName": "丙",
		"roleInstructions": "以丙的身份发言", "speakingOrder": float64(2),
	}
	d := DiscussionRecord{RoleSnapshot: rolesSnapshot(t, board)}
	// 中间那位（乙）已被移出聊天室。
	rows := []db.ListRongCloudChatroomMembersWithNodeRow{
		aiMemberRow("rc-a", "甲", "以甲的身份发言"),
		aiMemberRow("rc-c", "丙", "以丙的身份发言"),
	}

	snapshot, added, removed, ok := discussionRolesRebased(d, rows)
	if !ok {
		t.Fatal("重建不该失败")
	}
	if len(added) != 0 {
		t.Fatalf("没有新成员：added=%v", added)
	}
	if len(removed) != 1 || removed[0] != "乙" {
		t.Fatalf("removed = %v，期望只有被移出的「乙」", removed)
	}
	rebuilt := parseRoleSnapshot(t, snapshot)
	if _, exists := rebuilt["rc-b"]; exists {
		t.Fatalf("被移出的成员仍在角色板里: %s", snapshot)
	}
	if got := speakingOrderOf(t, rebuilt["rc-a"]); got != 0 {
		t.Errorf("rc-a speakingOrder = %d，期望 0", got)
	}
	if got := speakingOrderOf(t, rebuilt["rc-c"]); got != 1 {
		t.Errorf("rc-c speakingOrder = %d，期望压缩后的 1（原先是 2）", got)
	}
}

// TestRolesRebased_KeepsBoardWhenNoUsableMember：成员表里一个可用普通成员都没有
// （读空 / 全部被禁用 / 只剩真人）时，这次同步必须**不生效**：返回 ok=false 且
// 不给新快照 —— 一次读空把角色板清掉，讨论会直接失去推进依据（interactiveRoles
// 判 ok=false，整场停死）。
func TestRolesRebased_KeepsBoardWhenNoUsableMember(t *testing.T) {
	d := DiscussionRecord{RoleSnapshot: rolesSnapshot(t, roleBoard(t))}
	cases := map[string][]db.ListRongCloudChatroomMembersWithNodeRow{
		"成员表读空": nil,
		"只剩真人": {
			{MemberType: "user", Enabled: true, MemberID: pgtype.Text{String: "rc-owner", Valid: true}},
		},
		"AI 成员全被禁用": func() []db.ListRongCloudChatroomMembersWithNodeRow {
			row := aiMemberRow("rc-a", "甲", "以甲的身份发言")
			row.Enabled = false
			return []db.ListRongCloudChatroomMembersWithNodeRow{row}
		}(),
	}
	for name, rows := range cases {
		snapshot, _, _, ok := discussionRolesRebased(d, rows)
		if ok {
			t.Errorf("%s：不该生效（ok=true）", name)
		}
		if snapshot != nil {
			t.Errorf("%s：不该给新快照（会清空角色板）：%s", name, snapshot)
		}
	}
	// 主持条目缺失的快照同样不该被改写（补主持人不是同步该做的事）。
	noHost := map[string]map[string]interface{}{
		"rc-a": {"memberId": "rc-a", "roleName": "甲", "speakingOrder": float64(0)},
	}
	if _, _, _, ok := discussionRolesRebased(
		DiscussionRecord{RoleSnapshot: rolesSnapshot(t, noHost)},
		[]db.ListRongCloudChatroomMembersWithNodeRow{aiMemberRow("rc-a", "甲", "")},
	); ok {
		t.Error("快照里没有主持条目时不该重建")
	}
}

// ---- 真库端到端：中途加人 → 轮边界同步 → 新成员被派发 ----------------------

// addAIMemberToRoom 把一棵 AI 节点作为可用成员挂进 fixture 的聊天室。
func addAIMemberToRoom(t *testing.T, f *discussionFixture, node db.RongcloudNode, roleName string) {
	t.Helper()
	ctx := context.Background()
	room, err := f.queries.GetRongCloudChatroomViewByRongCloudID(ctx, f.roomRCID)
	if err != nil {
		t.Fatalf("读房间: %v", err)
	}
	if _, err := f.queries.UpsertRongCloudChatroomMemberByMemberID(ctx, db.UpsertRongCloudChatroomMemberByMemberIDParams{
		ChatroomID:       room.ID,
		MemberType:       "ai",
		NodeID:           node.ID,
		MemberID:         pgtype.Text{String: node.RongcloudUserID, Valid: true},
		Nickname:         roleName,
		RoleName:         pgtype.Text{String: roleName, Valid: true},
		RoleInstructions: pgtype.Text{String: "以" + roleName + "的身份发言", Valid: true},
		Capabilities:     []byte(`["discussion_participant"]`),
		Enabled:          true,
		PortraitUri:      DefaultUserPortrait(),
	}); err != nil {
		t.Fatalf("挂 AI 成员 %s: %v", roleName, err)
	}
}

// TestRoleSync_MidRoundJoinDispatchedNextRound 走真实链路验证「中途加人」：
//
//	讨论跑到第 1 轮中途（两位成员已发言）→ 往成员表插入新 AI 成员 →
//	轮边界同步（interactiveNext 在本轮发言阶段结束处）→ 角色板落库含新成员 →
//	下一轮该成员真的收到 your_turn，且首见上下文是尾部 N 条（N = 普通成员数）。
//
// 轮次推进那一小步直接用 AdvanceState（与 interactiveControl 内部对状态的改动等价），
// 避免把这份用例绑在「主持人回包 → awaiting_user → 用户确认」整条链上。
func TestRoleSync_MidRoundJoinDispatchedNextRound(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	ctx := context.Background()

	// 房间里原有两位 AI 成员（fixture 自带的那个 + 另一个节点）。
	existingA := f.memberNode
	existingB := seedNode(t, f.queries, f.wsID, "ai-node-b-"+uniqueID("t"), `["discussion_participant"]`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM rongcloud_node WHERE id=$1", existingB.ID)
	})
	addAIMemberToRoom(t, f, existingB, "质疑者")
	// 中途要加入的第三位成员：节点先建好，成员表稍后才插（模拟用户在轮中途拉人进组）。
	newcomer := seedNode(t, f.queries, f.wsID, "ai-node-c-"+uniqueID("t"), `["discussion_participant"]`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM rongcloud_node WHERE id=$1", newcomer.ID)
	})

	d := newWindowFixtureDiscussion(t, f, 1, [][2]string{
		{existingA.RongcloudUserID, "分析者"},
		{existingB.RongcloudUserID, "质疑者"},
	})
	appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", existingA.RongcloudUserID,
		map[string]interface{}{"round": 1, "content": "甲第一轮"})
	appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", existingB.RongcloudUserID,
		map[string]interface{}{"round": 1, "content": "乙第一轮"})

	// 轮中途加人：只动成员表，角色板这时还是两位。
	addAIMemberToRoom(t, f, newcomer, "补充者")

	current, err := f.rt.repo.Get(ctx, d.DiscussionID)
	if err != nil {
		t.Fatalf("get discussion: %v", err)
	}
	if err := f.rt.interactiveNext(ctx, current); err != nil {
		t.Fatalf("interactiveNext（第 1 轮结束，应触发角色板同步）: %v", err)
	}

	// 1) 角色板已落库且含新成员，老成员的字段与顺序不变。
	afterSync, err := f.rt.repo.Get(ctx, d.DiscussionID)
	if err != nil {
		t.Fatalf("get after sync: %v", err)
	}
	board := parseRoleSnapshot(t, afterSync.RoleSnapshot)
	entry, exists := board[newcomer.RongcloudUserID]
	if !exists {
		t.Fatalf("轮边界同步后角色板仍没有新成员: %s", afterSync.RoleSnapshot)
	}
	if got := speakingOrderOf(t, entry); got != 2 {
		t.Errorf("新成员 speakingOrder = %d，期望追加到末尾的 2", got)
	}
	if entry["roleName"] != "补充者" {
		t.Errorf("新成员 roleName = %v，期望「补充者」", entry["roleName"])
	}
	if got := speakingOrderOf(t, board[existingA.RongcloudUserID]); got != 0 {
		t.Errorf("原有成员 speakingOrder 被改动：%d", got)
	}

	// 2) 同步后本轮照常进入汇总（新成员本轮不插队）。
	if afterSync.Status != DiscussionStatusSummarizing {
		t.Fatalf("第 1 轮结束后的状态 = %q，期望 %q（新成员不得插进本轮）",
			afterSync.Status, DiscussionStatusSummarizing)
	}
	if _, asked := dispatchTasksByTarget(t, f, d.DiscussionID)["host-rc"]; !asked {
		t.Fatal("第 1 轮结束应派发主持人汇总")
	}

	// 3) 进入第 2 轮：两位老成员发言完毕，轮到时新成员被派发。
	round2, err := f.rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: afterSync.DiscussionID, ExpectedVersion: afterSync.Version,
		Status: DiscussionStatusRunning, CurrentRound: 2, RoundRevision: 0,
	})
	if err != nil {
		t.Fatalf("进入第 2 轮: %v", err)
	}
	appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", existingA.RongcloudUserID,
		map[string]interface{}{"round": 2, "content": "甲第二轮"})
	appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", existingB.RongcloudUserID,
		map[string]interface{}{"round": 2, "content": "乙第二轮"})
	round2, err = f.rt.repo.Get(ctx, round2.DiscussionID)
	if err != nil {
		t.Fatalf("get round2: %v", err)
	}
	if err := f.rt.interactiveNext(ctx, round2); err != nil {
		t.Fatalf("interactiveNext（第 2 轮应派发新成员）: %v", err)
	}

	// 4) 新成员真的拿到了 your_turn（outbox 是派发的唯一持久记录）。
	tasks := dispatchTasksByTarget(t, f, d.DiscussionID)
	task, dispatched := tasks[newcomer.RongcloudUserID]
	if !dispatched {
		t.Fatalf("新成员没有被派发 your_turn；本次 outbox 目标 = %v", dispatchTargets(tasks))
	}
	if task.SpeakingOrder != 2 {
		t.Errorf("派发 speakingOrder = %d，期望 2", task.SpeakingOrder)
	}

	// 5) 新成员首次上下文 = 尾部 N 条（N = 普通成员数 = 3）。
	//    历史共 4 条：[甲一, 乙一, 甲二, 乙二] → 尾部 3 条 = [乙一, 甲二, 乙二]，
	//    所以「甲第一轮」必须不在、其余三条必在。这个组合唯一确定了窗口边界。
	if strings.Contains(task.Prompt, "甲第一轮") {
		t.Errorf("新成员窗口漏回了第 4 条之后的历史「甲第一轮」：%s", task.Prompt)
	}
	for _, want := range []string{"乙第一轮", "甲第二轮", "乙第二轮"} {
		if !strings.Contains(task.Prompt, want) {
			t.Errorf("新成员窗口缺少尾部 3 条中的 %q：%s", want, task.Prompt)
		}
	}
}

// dispatchTargets 列出 outbox 里的派发目标（失败信息用）。
func dispatchTargets(tasks map[string]interactiveTask) []string {
	out := make([]string, 0, len(tasks))
	for target := range tasks {
		out = append(out, target)
	}
	sort.Strings(out)
	return out
}

// ---- 事件契约：role-sync 借 discussion_progress 承载，data 必须是空对象 ------

// TestRoleSyncEvent_UsesEmptyProgressPayload 是前端契约的回归防线。
//
// web 端对讨论事件做严格 shape 校验：discussion_progress 的 data 是 shape({})，
// 多一个键（round / reason / added 任意一个）都会让**整段**讨论历史被判无效。
// 所以这条事件的 payload 落库后必须是空对象 {}、eventType 仍是 discussion_progress、
// v3 信封里的 round 在顶层。
func TestRoleSyncEvent_UsesEmptyProgressPayload(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	ctx := context.Background()

	member := f.memberNode
	newcomer := seedNode(t, f.queries, f.wsID, "ai-node-sync-"+uniqueID("t"), `["discussion_participant"]`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM rongcloud_node WHERE id=$1", newcomer.ID)
	})
	d := newWindowFixtureDiscussion(t, f, 1, [][2]string{{member.RongcloudUserID, "分析者"}})
	appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", member.RongcloudUserID,
		map[string]interface{}{"round": 1, "content": "甲第一轮"})
	// 轮中途加人，再走轮边界触发同步（同步只在角色板真的变了时才落事件）。
	addAIMemberToRoom(t, f, newcomer, "补充者")

	current, err := f.rt.repo.Get(ctx, d.DiscussionID)
	if err != nil {
		t.Fatalf("get discussion: %v", err)
	}
	if err := f.rt.interactiveNext(ctx, current); err != nil {
		t.Fatalf("interactiveNext: %v", err)
	}

	events, err := f.rt.repo.ListEventsAfter(ctx, d.DiscussionID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var sync []DiscussionEventRecord
	for _, event := range events {
		if event.EventType == "discussion_progress" {
			sync = append(sync, event)
		}
	}
	if len(sync) != 1 {
		t.Fatalf("role-sync 事件条数 = %d，期望 1（当前事件流: %v）", len(sync), eventTypesOf(events))
	}
	rec := sync[0]
	if key := rec.IdempotencyKey; !strings.HasPrefix(key, "role-sync:1:") {
		t.Errorf("幂等键 = %q，期望以 role-sync:<round>: 开头", key)
	}

	// payload 落库后必须是空对象（不是 null、不是带字段的对象）。
	if payload := strings.TrimSpace(string(rec.Payload)); payload != "{}" {
		t.Fatalf("role-sync 事件 payload = %q，必须是空对象 {}", payload)
	}

	var envelope map[string]interface{}
	if err := json.Unmarshal(rec.V3Envelope, &envelope); err != nil {
		t.Fatalf("v3 信封不是 JSON: %v (%s)", err, rec.V3Envelope)
	}
	if envelope["protocolVersion"] != float64(3) {
		t.Errorf("信封 protocolVersion = %v，期望 3", envelope["protocolVersion"])
	}
	if envelope["round"] != float64(1) {
		t.Errorf("信封 round = %v，期望 1（顶层字段，前端靠它定位轮次）", envelope["round"])
	}

	// 前端视角：视图里 eventType 不变、payload 是空对象、round 在顶层。
	view := discussionEventView(rec)
	if view["eventType"] != "discussion_progress" {
		t.Errorf("视图 eventType = %v，期望 discussion_progress", view["eventType"])
	}
	if view["round"] != float64(1) {
		t.Errorf("视图 round = %v，期望 1", view["round"])
	}
	payload, isObject := view["payload"].(map[string]interface{})
	if !isObject {
		t.Fatalf("视图 payload 不是对象: %#v", view["payload"])
	}
	if len(payload) != 0 {
		t.Fatalf("视图 payload = %#v，必须是空对象（多一个键前端就会整段判无效）", payload)
	}
}

// eventTypesOf 列出事件类型（失败信息用）。
func eventTypesOf(events []DiscussionEventRecord) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.EventType)
	}
	return out
}
