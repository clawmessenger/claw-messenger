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

// ---- 补充：移除成员 / 重复同步幂等 / continue 路径（审查员 Ray 提的 3 个缺口） ----

// roleSyncProgressEvents 取出该讨论的 role-sync 事件（借 discussion_progress 承载）。
func roleSyncProgressEvents(t *testing.T, f *discussionFixture, discussionID string) []DiscussionEventRecord {
	t.Helper()
	events, err := f.rt.repo.ListEventsAfter(context.Background(), discussionID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	out := make([]DiscussionEventRecord, 0, 1)
	for _, event := range events {
		if event.EventType == "discussion_progress" {
			out = append(out, event)
		}
	}
	return out
}

// removeAIMemberFromRoom 把一棵 AI 成员从成员表摘掉（模拟用户在轮中途踢人）。
func removeAIMemberFromRoom(t *testing.T, f *discussionFixture, rcID string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		DELETE FROM rongcloud_chatroom_member
		WHERE member_id=$1
		  AND chatroom_id=(SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$2)`,
		rcID, f.roomRCID); err != nil {
		t.Fatalf("移除成员 %s: %v", rcID, err)
	}
}

// TestRoleSync_RemovedMemberNotDispatchedNextRound 是「中途移除」的引擎级用例：
// 被移除者下一轮不再收到 your_turn，其余成员的 speaking_order 压缩为 0..n-1
// （不跳号）—— 跳号会让设备端的 speaking_order 与角色板对不上。
func TestRoleSync_RemovedMemberNotDispatchedNextRound(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	ctx := context.Background()

	memberA := f.memberNode
	memberB := seedNode(t, f.queries, f.wsID, "ai-node-rm-b-"+uniqueID("t"), `["discussion_participant"]`)
	memberC := seedNode(t, f.queries, f.wsID, "ai-node-rm-c-"+uniqueID("t"), `["discussion_participant"]`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM rongcloud_node WHERE id IN ($1,$2)", memberB.ID, memberC.ID)
	})
	addAIMemberToRoom(t, f, memberB, "质疑者")
	addAIMemberToRoom(t, f, memberC, "补充者")

	d := newWindowFixtureDiscussion(t, f, 1, [][2]string{
		{memberA.RongcloudUserID, "分析者"},
		{memberB.RongcloudUserID, "质疑者"},
		{memberC.RongcloudUserID, "补充者"},
	})
	for _, member := range []db.RongcloudNode{memberA, memberB, memberC} {
		appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", member.RongcloudUserID,
			map[string]interface{}{"round": 1, "content": member.NodeID + " 第一轮"})
	}

	// 轮中途踢掉中间那位（speaking_order = 1）。
	removeAIMemberFromRoom(t, f, memberB.RongcloudUserID)

	current, err := f.rt.repo.Get(ctx, d.DiscussionID)
	if err != nil {
		t.Fatalf("get discussion: %v", err)
	}
	if err := f.rt.interactiveNext(ctx, current); err != nil {
		t.Fatalf("interactiveNext（第 1 轮结束，应触发角色板同步）: %v", err)
	}

	afterSync, err := f.rt.repo.Get(ctx, d.DiscussionID)
	if err != nil {
		t.Fatalf("get after sync: %v", err)
	}
	board := parseRoleSnapshot(t, afterSync.RoleSnapshot)
	if _, exists := board[memberB.RongcloudUserID]; exists {
		t.Fatalf("被移除的成员仍在角色板里: %s", afterSync.RoleSnapshot)
	}
	if got := speakingOrderOf(t, board[memberA.RongcloudUserID]); got != 0 {
		t.Errorf("分析者 speakingOrder = %d，期望 0", got)
	}
	if got := speakingOrderOf(t, board[memberC.RongcloudUserID]); got != 1 {
		t.Errorf("补充者 speakingOrder = %d，期望压缩后的 1（原先 2，不许跳号）", got)
	}

	// 第 2 轮：分析者说完后，下一位必须是补充者（顺序 1），且被移除者拿不到任何派发。
	round2, err := f.rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: afterSync.DiscussionID, ExpectedVersion: afterSync.Version,
		Status: DiscussionStatusRunning, CurrentRound: 2, RoundRevision: 0,
	})
	if err != nil {
		t.Fatalf("进入第 2 轮: %v", err)
	}
	appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", memberA.RongcloudUserID,
		map[string]interface{}{"round": 2, "content": "分析者 第二轮"})
	if round2, err = f.rt.repo.Get(ctx, round2.DiscussionID); err != nil {
		t.Fatalf("get round2: %v", err)
	}
	if err := f.rt.interactiveNext(ctx, round2); err != nil {
		t.Fatalf("interactiveNext（第 2 轮应派发补充者）: %v", err)
	}

	tasks := dispatchTasksByTarget(t, f, d.DiscussionID)
	task, dispatched := tasks[memberC.RongcloudUserID]
	if !dispatched {
		t.Fatalf("补充者没有被派发 your_turn；本次 outbox 目标 = %v", dispatchTargets(tasks))
	}
	if task.SpeakingOrder != 1 {
		t.Errorf("补充者派发 speakingOrder = %d，期望压缩后的 1", task.SpeakingOrder)
	}
	if _, ghost := tasks[memberB.RongcloudUserID]; ghost {
		t.Errorf("被移除的成员仍收到 your_turn：%v", dispatchTargets(tasks))
	}
}

// TestRoleSync_RepeatedSyncIsIdempotent：syncDiscussionRoles 在 begin / continue / 轮末
// 三处都会被调用，成员表没变的第二次同步必须是**无操作** —— 不写快照、不落事件
// （否则前端会看到一串无意义的 discussion_progress，且幂等键同名会撞唯一索引）。
func TestRoleSync_RepeatedSyncIsIdempotent(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	ctx := context.Background()

	member := f.memberNode
	newcomer := seedNode(t, f.queries, f.wsID, "ai-node-idem-"+uniqueID("t"), `["discussion_participant"]`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM rongcloud_node WHERE id=$1", newcomer.ID)
	})
	d := newWindowFixtureDiscussion(t, f, 1, [][2]string{{member.RongcloudUserID, "分析者"}})
	appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", member.RongcloudUserID,
		map[string]interface{}{"round": 1, "content": "甲第一轮"})
	addAIMemberToRoom(t, f, newcomer, "补充者")

	current, err := f.rt.repo.Get(ctx, d.DiscussionID)
	if err != nil {
		t.Fatalf("get discussion: %v", err)
	}
	synced, changed, err := f.rt.syncDiscussionRoles(ctx, current)
	if err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	if !changed {
		t.Fatal("成员表加了人，首次同步必须报告 changed")
	}
	if got := len(roleSyncProgressEvents(t, f, d.DiscussionID)); got != 1 {
		t.Fatalf("首次同步后 role-sync 事件 = %d 条，期望 1", got)
	}

	// 成员表不变，再同步一次。
	again, changedAgain, err := f.rt.syncDiscussionRoles(ctx, synced)
	if err != nil {
		t.Fatalf("二次同步: %v", err)
	}
	if changedAgain {
		t.Error("成员表没有变化，二次同步不该报告 changed")
	}
	if got := len(roleSyncProgressEvents(t, f, d.DiscussionID)); got != 1 {
		t.Errorf("二次同步多落了事件：role-sync = %d 条，期望仍为 1", got)
	}
	if string(again.RoleSnapshot) != string(synced.RoleSnapshot) {
		t.Errorf("二次同步改写了角色板：\n got %s\nwant %s", again.RoleSnapshot, synced.RoleSnapshot)
	}
	if again.Version != synced.Version {
		t.Errorf("二次同步改了 version：%d → %d", synced.Version, again.Version)
	}
}

// TestRoleSync_ContinuePathSyncsBeforeNewRound 覆盖三处调用点里的 continue 通道
// （`interactiveControl` → 进入新轮前同步）——它是「新成员从下一轮生效」的主路径。
//
// 断言：continue 之后新成员已在角色板里，且**本轮首位仍是原 speaking_order=0 的老
// 成员**（不插队）；老成员说完后新成员才被派发。只测 interactiveNext 那处调用点
// 无法证明 continue 通道也同步了。
func TestRoleSync_ContinuePathSyncsBeforeNewRound(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	ctx := context.Background()

	memberA := f.memberNode
	newcomer := seedNode(t, f.queries, f.wsID, "ai-node-cont-"+uniqueID("t"), `["discussion_participant"]`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM rongcloud_node WHERE id=$1", newcomer.ID)
	})
	d := newWindowFixtureDiscussion(t, f, 1, [][2]string{{memberA.RongcloudUserID, "分析者"}})
	appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", memberA.RongcloudUserID,
		map[string]interface{}{"round": 1, "content": "甲第一轮"})
	appendFixtureEvent(t, f, d.DiscussionID, "round_summary_completed", "host-rc",
		map[string]interface{}{"round": 1, "content": "第一轮纪要"})

	// 轮中途加人，然后把讨论推到「等人确认」（主持人已出纪要）。
	addAIMemberToRoom(t, f, newcomer, "补充者")
	awaiting, err := f.rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: d.DiscussionID, ExpectedVersion: d.Version,
		Status: DiscussionStatusAwaitingUser, CurrentRound: 1, RoundRevision: 0,
	})
	if err != nil {
		t.Fatalf("推进到 awaiting_user: %v", err)
	}

	result := f.rt.interactiveControl(ctx, CommandContent{
		Service: "discussion", Action: "continue",
		Params: map[string]interface{}{
			"requestId":    "req-continue-" + uniqueID("t"),
			"stateVersion": awaiting.Version,
		},
	}, awaiting, "", f.ownerRCID, false)
	if result.code != 200 {
		t.Fatalf("continue = %d %s", result.code, result.message)
	}

	afterContinue, err := f.rt.repo.Get(ctx, d.DiscussionID)
	if err != nil {
		t.Fatalf("get after continue: %v", err)
	}
	if afterContinue.CurrentRound != 2 {
		t.Fatalf("continue 后 currentRound = %d，期望 2", afterContinue.CurrentRound)
	}
	board := parseRoleSnapshot(t, afterContinue.RoleSnapshot)
	if got := speakingOrderOf(t, board[newcomer.RongcloudUserID]); got != 1 {
		t.Errorf("continue 后新成员 speakingOrder = %d，期望 1（角色板没同步）", got)
	}

	// 本轮首位仍是老成员（order 0），新成员不插队。
	tasks := dispatchTasksByTarget(t, f, d.DiscussionID)
	first, ok := tasks[memberA.RongcloudUserID]
	if !ok {
		t.Fatalf("continue 后应派发 order=0 的老成员；outbox 目标 = %v", dispatchTargets(tasks))
	}
	if first.Round != 2 || first.SpeakingOrder != 0 {
		t.Errorf("首位派发 round/speakingOrder = %d/%d，期望 2/0", first.Round, first.SpeakingOrder)
	}
	if _, jumped := tasks[newcomer.RongcloudUserID]; jumped {
		t.Error("新成员本轮就插队了（应从下一轮开始生效）")
	}

	// 老成员说完后，新成员在同一轮被派发 —— 证明它确实「从这一轮起」生效。
	appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", memberA.RongcloudUserID,
		map[string]interface{}{"round": 2, "content": "甲第二轮"})
	round2, err := f.rt.repo.Get(ctx, d.DiscussionID)
	if err != nil {
		t.Fatalf("get round2: %v", err)
	}
	if err := f.rt.interactiveNext(ctx, round2); err != nil {
		t.Fatalf("interactiveNext（第 2 轮应派发新成员）: %v", err)
	}
	task, dispatched := dispatchTasksByTarget(t, f, d.DiscussionID)[newcomer.RongcloudUserID]
	if !dispatched {
		t.Fatalf("新成员在老成员之后没有被派发；outbox 目标 = %v",
			dispatchTargets(dispatchTasksByTarget(t, f, d.DiscussionID)))
	}
	if task.SpeakingOrder != 1 || task.Round != 2 {
		t.Errorf("新成员派发 round/speakingOrder = %d/%d，期望 2/1", task.Round, task.SpeakingOrder)
	}
}

// TestRoleSync_DispatchesMemberWithNullMemberID 是 b87f55e8d 那条修复的回归防线，
// 也正好长在「中途改成员要生效」这条路径上。
//
// 运维后台的 ChatroomService.SetMembers 走 CreateRongCloudChatroomMember，那条 SQL 的
// 列清单里没有 member_id —— 管理员在后台改一遍成员，本房间所有 AI 行的 member_id 变成
// NULL。可用性判据若只认 member_id，就会全员判不可用 → 全员 member_skipped → 本轮零产出
// → 讨论被判 failed（dispatch_failed），而 reason 文案完全指不出是 member_id 为 NULL。
//
// 这里把「member_id 为 NULL 的行」造出来，断言它照样被同步进角色板、照样被派发。
// 变异（把判据改回只认 member_id）会让这条用例变红 —— 已在 pristine 生产代码上验过。
func TestRoleSync_DispatchesMemberWithNullMemberID(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	ctx := context.Background()

	memberA := f.memberNode
	newcomer := seedNode(t, f.queries, f.wsID, "ai-node-nullmid-"+uniqueID("t"), `["discussion_participant"]`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM rongcloud_node WHERE id=$1", newcomer.ID)
	})
	addAIMemberToRoom(t, f, newcomer, "补充者")
	// 复制运维后台 SetMembers 的产物：member_id 为 NULL。
	if _, err := f.pool.Exec(ctx, `
		UPDATE rongcloud_chatroom_member SET member_id = NULL
		WHERE node_id=$1
		  AND chatroom_id=(SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$2)`,
		newcomer.ID, f.roomRCID); err != nil {
		t.Fatalf("把 member_id 置 NULL: %v", err)
	}
	// 前置断言：这一行真的变成 NULL 了，否则这条用例会退化成普通的「中途加人」。
	var storedMemberID pgtype.Text
	if err := f.pool.QueryRow(ctx, `
		SELECT member_id FROM rongcloud_chatroom_member
		WHERE node_id=$1
		  AND chatroom_id=(SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$2)`,
		newcomer.ID, f.roomRCID).Scan(&storedMemberID); err != nil {
		t.Fatalf("读回 member_id: %v", err)
	}
	if storedMemberID.Valid {
		t.Fatalf("前置条件不成立：member_id 仍为 %q", storedMemberID.String)
	}

	d := newWindowFixtureDiscussion(t, f, 1, [][2]string{{memberA.RongcloudUserID, "分析者"}})
	appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", memberA.RongcloudUserID,
		map[string]interface{}{"round": 1, "content": "甲第一轮"})

	current, err := f.rt.repo.Get(ctx, d.DiscussionID)
	if err != nil {
		t.Fatalf("get discussion: %v", err)
	}
	if err := f.rt.interactiveNext(ctx, current); err != nil {
		t.Fatalf("interactiveNext（第 1 轮结束，应同步进角色板）: %v", err)
	}
	afterSync, err := f.rt.repo.Get(ctx, d.DiscussionID)
	if err != nil {
		t.Fatalf("get after sync: %v", err)
	}
	if _, exists := parseRoleSnapshot(t, afterSync.RoleSnapshot)[newcomer.RongcloudUserID]; !exists {
		t.Fatalf("member_id 为 NULL 的成员没被同步进角色板: %s", afterSync.RoleSnapshot)
	}

	// 第 2 轮：分析者说完后轮到他 —— 派发前那道可用性判据必须认 node_rc_user_id。
	round2, err := f.rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: afterSync.DiscussionID, ExpectedVersion: afterSync.Version,
		Status: DiscussionStatusRunning, CurrentRound: 2, RoundRevision: 0,
	})
	if err != nil {
		t.Fatalf("进入第 2 轮: %v", err)
	}
	appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", memberA.RongcloudUserID,
		map[string]interface{}{"round": 2, "content": "甲第二轮"})
	if round2, err = f.rt.repo.Get(ctx, round2.DiscussionID); err != nil {
		t.Fatalf("get round2: %v", err)
	}
	if err := f.rt.interactiveNext(ctx, round2); err != nil {
		t.Fatalf("interactiveNext（第 2 轮应派发新成员）: %v", err)
	}

	tasks := dispatchTasksByTarget(t, f, d.DiscussionID)
	task, dispatched := tasks[newcomer.RongcloudUserID]
	if !dispatched {
		t.Fatalf("member_id 为 NULL 的成员被判不可用、没拿到 your_turn；本次 outbox 目标 = %v",
			dispatchTargets(tasks))
	}
	if task.SpeakingOrder != 1 || task.Round != 2 {
		t.Errorf("派发 round/speakingOrder = %d/%d，期望 2/1", task.Round, task.SpeakingOrder)
	}
}
