package rongcloud

// 讨论组「角色板同步」契约的补充回归用例（task 2026-10-27/task-005，v4 批次）。
//
// 四条都是 b87f55e8d 那批修复的守卫，缺了它们就等于「修完了没人看着」：
//
//	1. interactiveMemberEnabled 的身份判据同时认 member_id 与 node_rc_user_id
//	   —— 放宽之后必须仍然对「禁用 / 非 AI / 陌生账号」返回 false（不能变恒真），
//	   且真实形状（member_id 全为 NULL）下端到端派发不退化、不落 member_skipped；
//	2. role-sync 幂等键长度有界（不再拼明文角色名）+ 是增删集的确定性函数；
//	3. 新成员条目写 model 键（discussion_model 优先，回落节点 model，皆空不写键）；
//	4. 幂等键命中时不重复广播（同一轮反复同步只广播一次）。
//
// 断言形态说明：这里的每条用例都刻意断言「不该出现的东西不出现」（负向断言、
// 计数相等、键不存在），而不是只查存在性 —— 只查存在性的话，把修复整个撤掉
// 依然能绿，那就等于没测。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ---- 成员行改写 helper（模拟两条真实写入链路） ------------------------------

// updateRoomMemberRow 按 node_id 改 fixture 聊天室里的一行 AI 成员。
//
// 两条真实写入链路填的账号列不一样：C 端成员设置走
// UpsertRongCloudChatroomMemberByMemberID（member_id 有值），运维后台的
// ChatroomService.SetMembers 走 CreateRongCloudChatroomMember（列清单里没有
// member_id，重建出来的行是 NULL）。下面这些 helper 就是把两种产物造出来。
func updateRoomMemberRow(t *testing.T, f *discussionFixture, nodeID pgtype.UUID, setClause string, args ...interface{}) {
	t.Helper()
	query := `UPDATE rongcloud_chatroom_member SET ` + setClause + `
		WHERE node_id=$1
		  AND chatroom_id=(SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$2)`
	params := append([]interface{}{nodeID, f.roomRCID}, args...)
	if _, err := f.pool.Exec(context.Background(), query, params...); err != nil {
		t.Fatalf("更新成员行（%s）: %v", setClause, err)
	}
}

// setRoomMemberIDNull 把某成员的 member_id 置 NULL（= SetMembers 重建的产物）。
func setRoomMemberIDNull(t *testing.T, f *discussionFixture, nodeID pgtype.UUID) {
	t.Helper()
	updateRoomMemberRow(t, f, nodeID, "member_id = NULL")
}

// setRoomMemberIDTo 把某成员的 member_id 改成任意账号（造「member_id 与
// node_rc_user_id 不同」的行，用来单独验证 member_id 这一路判据）。
func setRoomMemberIDTo(t *testing.T, f *discussionFixture, nodeID pgtype.UUID, memberID string) {
	t.Helper()
	updateRoomMemberRow(t, f, nodeID, "member_id = $3", memberID)
}

// setRoomMemberEnabled 启用/禁用某成员。
func setRoomMemberEnabled(t *testing.T, f *discussionFixture, nodeID pgtype.UUID, enabled bool) {
	t.Helper()
	updateRoomMemberRow(t, f, nodeID, "enabled = $3", enabled)
}

// setAllAIMemberIDNull 把所有 AI 行的 member_id 置 NULL —— 管理员在后台改一遍
// 成员之后，本房间的数据就是这个形状。
func setAllAIMemberIDNull(t *testing.T, f *discussionFixture) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE rongcloud_chatroom_member SET member_id = NULL
		WHERE chatroom_id=(SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1)
		  AND member_type='ai'`, f.roomRCID); err != nil {
		t.Fatalf("置空所有 AI 成员的 member_id: %v", err)
	}
}

// roomMemberColumn 读回一行成员的指定列（前置条件断言用，避免用例在「其实没改成」
// 的情况下跑出一个看起来通过的结果）。
func roomMemberColumn(t *testing.T, f *discussionFixture, nodeID pgtype.UUID, column string) interface{} {
	t.Helper()
	var value interface{}
	query := `SELECT ` + column + ` FROM rongcloud_chatroom_member
		WHERE node_id=$1
		  AND chatroom_id=(SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$2)`
	if err := f.pool.QueryRow(context.Background(), query, nodeID, f.roomRCID).Scan(&value); err != nil {
		t.Fatalf("读回成员列 %s: %v", column, err)
	}
	return value
}

// ---- 1. interactiveMemberEnabled 的身份判据 --------------------------------

// TestInteractiveMemberEnabled_GuardsBothAccountColumns 是 b87f55e8d 那条 P1 修复的
// 守卫用例，正反两侧都钉死。
//
// 只测「放宽之后能用」是不够的：判据放宽最典型的翻车方式是变成恒真（任何账号都
// 返回 true），那样「成员被移出/禁用后不再派发」这条语义就丢了。所以正向三条
// （node_rc 单路、member_id 单路、端到端派发）与反向三条（禁用、非 AI、陌生账号）
// 必须写在同一条用例里 —— 它们守的是同一个判据。
//
// 变异（把判据改回只认 member_id，也就是 b87f55e8d 之前的样子）：正向两条与端到端
// 那段变红，反向三条仍绿。已在 -overlay 下验过。
func TestInteractiveMemberEnabled_GuardsBothAccountColumns(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	ctx := context.Background()

	// memberA：两条账号列都正常（对照组）。
	memberA := f.memberNode
	// memberB：稍后把 member_id 置 NULL（= SetMembers 重建的产物）。
	memberB := seedNode(t, f.queries, f.wsID, "ai-node-guard-b-"+uniqueID("t"), `["discussion_participant"]`)
	// memberC：稍后把 member_id 改成一个与 node_rc_user_id 不同的别名
	// （单独验证 member_id 这一路判据）。
	memberC := seedNode(t, f.queries, f.wsID, "ai-node-guard-c-"+uniqueID("t"), `["discussion_participant"]`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			"DELETE FROM rongcloud_node WHERE id IN ($1,$2)", memberB.ID, memberC.ID)
	})
	addAIMemberToRoom(t, f, memberB, "质疑者")
	addAIMemberToRoom(t, f, memberC, "补充者")

	d := newWindowFixtureDiscussion(t, f, 1, [][2]string{
		{memberA.RongcloudUserID, "分析者"},
		{memberB.RongcloudUserID, "质疑者"},
		{memberC.RongcloudUserID, "补充者"},
	})

	// 前置：正常形状下三人都可用。这一步不成立的话，下面的正/反向断言全都失去意义。
	for _, rcID := range []string{memberA.RongcloudUserID, memberB.RongcloudUserID, memberC.RongcloudUserID} {
		if !f.rt.interactiveMemberEnabled(ctx, d, rcID) {
			t.Fatalf("前置条件不成立：member_id 有值时 %s 应可用", rcID)
		}
	}

	// 正向 1：member_id 为 NULL、node_rc_user_id 匹配 → 可用。
	setRoomMemberIDNull(t, f, memberB.ID)
	if value := roomMemberColumn(t, f, memberB.ID, "member_id"); value != nil {
		t.Fatalf("前置条件不成立：member_id 仍为 %v", value)
	}
	if !f.rt.interactiveMemberEnabled(ctx, d, memberB.RongcloudUserID) {
		t.Errorf("member_id 为 NULL 时 %s 被判为不可用 —— 判据没认 node_rc_user_id",
			memberB.RongcloudUserID)
	}

	// 正向 2：member_id 匹配、node_rc_user_id 不匹配 → 可用。
	// 造一行 member_id = 别名、node_rc_user_id = 节点账号 的成员，然后拿**别名**去问。
	aliasC := "rc-legacy-alias-" + uniqueID("t")
	if aliasC == memberC.RongcloudUserID {
		t.Fatalf("用例自身有问题：别名与节点账号相同（%s）", aliasC)
	}
	setRoomMemberIDTo(t, f, memberC.ID, aliasC)
	if value := roomMemberColumn(t, f, memberC.ID, "member_id"); value != aliasC {
		t.Fatalf("前置条件不成立：member_id = %v，期望 %q", value, aliasC)
	}
	if !f.rt.interactiveMemberEnabled(ctx, d, aliasC) {
		t.Errorf("member_id 匹配时 %s 被判为不可用 —— 判据没认 member_id", aliasC)
	}
	// 同一个成员的节点账号这一路也仍然可用（两个分支是「或」，不是互斥）。
	if !f.rt.interactiveMemberEnabled(ctx, d, memberC.RongcloudUserID) {
		t.Errorf("node_rc_user_id 匹配时 %s 被判为不可用", memberC.RongcloudUserID)
	}

	// 反向 1：enabled = false → 不可用。
	setRoomMemberEnabled(t, f, memberA.ID, false)
	if f.rt.interactiveMemberEnabled(ctx, d, memberA.RongcloudUserID) {
		t.Error("成员被禁用后仍被判为可用 —— 判据放宽过头了")
	}
	// 复原并断言复原生效：否则下面的端到端派发会因为「成员被禁用」而假红。
	setRoomMemberEnabled(t, f, memberA.ID, true)
	if !f.rt.interactiveMemberEnabled(ctx, d, memberA.RongcloudUserID) {
		t.Fatal("启用复原失败：成员仍被判为不可用")
	}

	// 反向 2：member_type != 'ai'（房间 owner 是 user 行）→ 不可用。
	if value := roomMemberColumn(t, f, memberA.ID, "member_type"); value != "ai" {
		t.Fatalf("前置条件不成立：memberA 的 member_type = %v", value)
	}
	var ownerType string
	if err := f.pool.QueryRow(ctx, `
		SELECT member_type FROM rongcloud_chatroom_member
		WHERE chatroom_id=(SELECT id FROM rongcloud_chatroom WHERE rongcloud_chatroom_id=$1)
		  AND member_id=$2`, f.roomRCID, f.ownerRCID).Scan(&ownerType); err != nil {
		t.Fatalf("读 owner 成员行: %v", err)
	}
	if ownerType == "ai" {
		t.Fatalf("前置条件不成立：owner 行的 member_type = %q", ownerType)
	}
	if f.rt.interactiveMemberEnabled(ctx, d, f.ownerRCID) {
		t.Error("真人成员（member_type != 'ai'）被判为可用 —— 判据只该认 AI 成员")
	}

	// 反向 3：两个账号列都不匹配的陌生账号 → 不可用。
	if f.rt.interactiveMemberEnabled(ctx, d, "rc-does-not-exist") {
		t.Error("不存在的账号被判为可用 —— 判据放宽过头了")
	}

	// 端到端：模拟管理员在后台改一遍成员（全部 AI 行 member_id 变 NULL），
	// 第 1 轮首位成员仍必须拿到 your_turn，且事件流里不得出现跳过 / 失败。
	setAllAIMemberIDNull(t, f)
	current, err := f.rt.repo.Get(ctx, d.DiscussionID)
	if err != nil {
		t.Fatalf("get discussion: %v", err)
	}
	if err := f.rt.interactiveNext(ctx, current); err != nil {
		t.Fatalf("interactiveNext: %v", err)
	}
	tasks := dispatchTasksByTarget(t, f, d.DiscussionID)
	task, dispatched := tasks[memberA.RongcloudUserID]
	if !dispatched {
		t.Fatalf("member_id 全为 NULL 后首位成员没有拿到 your_turn（疑似全员 member_skipped）；outbox 目标 = %v",
			dispatchTargets(tasks))
	}
	if task.Round != 1 || task.SpeakingOrder != 0 {
		t.Errorf("派发 round/speakingOrder = %d/%d，期望 1/0", task.Round, task.SpeakingOrder)
	}
	events, err := f.rt.repo.ListEventsAfter(ctx, d.DiscussionID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, event := range events {
		switch event.EventType {
		case "member_skipped":
			t.Errorf("出现了 member_skipped（判据仍不认 NULL 的 member_id）：%s", event.Payload)
		case "discussion_failed":
			t.Errorf("讨论被判失败（全员跳过的下游后果）：%s", event.Payload)
		}
	}
}

// ---- 2. role-sync 幂等键：长度有界 + 确定性 ---------------------------------

// TestRoleSyncEvent_IdempotencyKeyStaysBounded 是 P2-2 的回归防线。
//
// 旧写法把角色名明文拼进幂等键（role-sync:轮:added:removed）。角色名是 TEXT 无长度
// 限制、中文明文 3 字节/字，一轮加十来个 80 字的角色名就能顶穿
// uq_rongcloud_discussion_event_idem 的 btree 单条 ~2704 字节上限 —— 于是这条事件
// 压根写不进去，还白跑三次重试（interactiveEmit 的重试循环）。
//
// 这里用 12 位 × 80 字中文成员造出真实规模：先断言明文键确实超限（否则用例复现不了
// 线上那次写失败），再断言真实落库的键长度有硬上界、且是增删集的确定性函数
// （同输入两次算出同一个 key）。
func TestRoleSyncEvent_IdempotencyKeyStaysBounded(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	ctx := context.Background()

	const memberCount = 12
	const nameRunes = 80
	names := make([]string, 0, memberCount)
	nodes := make([]db.RongcloudNode, 0, memberCount)
	for i := 0; i < memberCount; i++ {
		// 每人一个 80 字、字字不同的中文角色名（角色名只进幂等键与日志，进不了 payload）。
		// 刻意用「压不动」的名字：PG 会给 btree 索引键做 pglz 压缩，80 个相同汉字能被
		// 压到几十字节，那样明文键即使 2904 字节也照样写得进去 —— 就复现不了线上那次
		// 写失败了。让每个字都不同且铺在整个 CJK 区，压缩比接近 1。
		name := incompressibleRoleName(i, nameRunes)
		names = append(names, name)
		node := seedNode(t, f.queries, f.wsID,
			fmt.Sprintf("ai-node-long-%02d-%s", i, uniqueID("t")), `["discussion_participant"]`)
		addAIMemberToRoom(t, f, node, name)
		nodes = append(nodes, node)
	}
	t.Cleanup(func() {
		for _, node := range nodes {
			_, _ = f.pool.Exec(context.Background(), "DELETE FROM rongcloud_node WHERE id=$1", node.ID)
		}
	})

	// 前置：旧明文键（role-sync:1:added:removed）确实超过 btree 单条索引键上限
	// ~2704 字节 —— 线上那次写失败就是这么来的。注意：能不能真的写失败还取决于
	// 角色名的可压缩性（重复字会被 PG 压掉，见 incompressibleRoleName 的注释），
	// 所以这条用例的**硬断言**是「键长必须有上界」，而不是「一定写不进去」。
	plaintextKey := fmt.Sprintf("role-sync:%d:%s:%s", 1, strings.Join(names, ","), "")
	if len(plaintextKey) <= 2704 {
		t.Fatalf("前置条件不成立：明文幂等键只有 %d 字节，没到 2704 —— 复现不了那次写失败",
			len(plaintextKey))
	}

	// 角色板只有主持人，成员表里只留上面这 12 人（fixture 自带的那位 AI 成员先摘掉，
	// 免得把「新成员数」算成 13）。
	removeAIMemberFromRoom(t, f, f.memberNode.RongcloudUserID)
	d := newWindowFixtureDiscussion(t, f, 1, nil)
	synced, changed, err := f.rt.syncDiscussionRoles(ctx, d)
	if err != nil {
		t.Fatalf("syncDiscussionRoles: %v", err)
	}
	if !changed {
		t.Fatal("成员表里有 12 个新成员，同步必须报告 changed")
	}

	events := roleSyncProgressEvents(t, f, d.DiscussionID)
	if len(events) != 1 {
		t.Fatalf("role-sync 事件 = %d 条，期望 1（=0 说明幂等键超长被 btree 索引拒了）；"+
			"讨论事件流 = %v", len(events), eventTypesOf(allDiscussionEvents(t, f, d.DiscussionID)))
	}
	key := events[0].IdempotencyKey
	// 硬上界：键长只应该是「前缀 + 前缀 + 16 位 hex」，与角色名长度和个数无关。
	if len(key) >= 200 {
		t.Errorf("幂等键长度 = %d 字节，期望 < 200：%q", len(key), key)
	}
	if want := len("role-sync:1:") + 16; len(key) != want {
		t.Errorf("幂等键长度 = %d 字节，期望恰好 %d（前缀 + 16 位 hex 摘要）：%q",
			len(key), want, key)
	}
	if !strings.HasPrefix(key, "role-sync:1:") {
		t.Errorf("幂等键 = %q，期望以 role-sync:<round>: 开头（轮次是去重维度的一部分）", key)
	}

	// 确定性：键是增删集的**纯函数** —— 这里按角色板顺序重算一次（与 SQL 的排序解耦），
	// 必须与落库的那个键逐字节相同。生产侧算了一次、这里算了第二次，两次同值才说明
	// 重放去重是可靠的；若有人往摘要里掺时间戳/随机盐，这条会立刻红。
	addedFromBoard := addedRoleNamesByOrder(t, synced)
	if len(addedFromBoard) != memberCount {
		t.Fatalf("角色板里的普通成员 = %d 个，期望 %d 个: %s",
			len(addedFromBoard), memberCount, synced.RoleSnapshot)
	}
	if recomputed := fmt.Sprintf("role-sync:%d:%s", 1, roleSyncDigest(addedFromBoard, nil)); recomputed != key {
		t.Errorf("同一组增删两次算出不同的键：\n落库 %q\n重算 %q", key, recomputed)
	}
	// 摘要真的依赖内容（不是个常量）：少一个成员必须换一个键。
	if roleSyncDigest(addedFromBoard, nil) == roleSyncDigest(addedFromBoard[1:], nil) {
		t.Error("增删集不同却算出同一个摘要 —— 重放去重会把两次真实变化当成一次")
	}
}

// incompressibleRoleName 生成一个「pglz 压不动」的中文角色名：每个字都不同，
// 均匀铺在 CJK 统一汉字区（U+4E00..U+9BFF）。确定性 LCG，不用 math/rand，
// 保证同一 seed 每次算出同一个名字（失败信息可复现）。
func incompressibleRoleName(seed, runes int) string {
	var b strings.Builder
	b.Grow(runes * 3)
	state := uint32(seed)*2654435761 + 1
	for i := 0; i < runes; i++ {
		state = state*1664525 + 1013904223
		b.WriteRune(rune(0x4E00 + state%20000))
	}
	return b.String()
}

// addedRoleNamesByOrder 按 speakingOrder 升序取出角色板里的普通成员角色名
// （与讨论内下发到设备端的发言顺序一致，不依赖数据库的排序规则）。
func addedRoleNamesByOrder(t *testing.T, d DiscussionRecord) []string {
	t.Helper()
	board := parseRoleSnapshot(t, d.RoleSnapshot)
	type entry struct {
		order int
		name  string
	}
	entries := make([]entry, 0, len(board))
	for _, item := range board {
		if isHost, _ := item["isHost"].(bool); isHost {
			continue
		}
		name, _ := item["roleName"].(string)
		entries = append(entries, entry{order: speakingOrderOf(t, item), name: name})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].order < entries[j].order })
	names := make([]string, 0, len(entries))
	for _, item := range entries {
		names = append(names, item.name)
	}
	return names
}

// allDiscussionEvents 取该讨论的全部事件（失败信息用）。
func allDiscussionEvents(t *testing.T, f *discussionFixture, discussionID string) []DiscussionEventRecord {
	t.Helper()
	events, err := f.rt.repo.ListEventsAfter(context.Background(), discussionID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return events
}

// ---- 3. 新成员条目写 model 键 ---------------------------------------------

// TestRolesRebased_NewMemberCarriesModelKey 是 P2-3 的回归防线。
//
// 中途加进来的新成员，条目必须带上用户在成员设置里配的模型：缺了这个键，设备端
// 会静默用默认模型 —— 用户配的模型等于没配，而且没有任何报错能指向它。
//
// 三条取值规则一次测完：discussion_model 优先 → 回落到节点 model → 两者皆空
// **不写键**（空串的讨论与改造前逐字节一致，避免给前端多一个空字段）。
// 顺带钉住「已有成员的条目不被同步改写」。
func TestRolesRebased_NewMemberCarriesModelKey(t *testing.T) {
	text := func(value string) pgtype.Text { return pgtype.Text{String: value, Valid: true} }
	unset := pgtype.Text{}

	cases := []struct {
		name            string
		discussionModel pgtype.Text
		nodeModel       pgtype.Text
		wantModel       string // "" 表示「不该写键」
	}{
		{"成员设置里的讨论模型优先", text("discussion-tuned"), text("node-default"), "discussion-tuned"},
		{"只配了节点级模型时回落到它", unset, text("node-default"), "node-default"},
		{"讨论模型是空串时回落到节点模型", text(""), text("node-default"), "node-default"},
		{"两者皆空时不写键", unset, unset, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DiscussionRecord{RoleSnapshot: rolesSnapshot(t, roleBoard(t))}
			// 甲、乙是已有成员（成员表里给它们塞了模型，同步不该把它注入角色板）；
			// 丙是这次中途加进来的新成员。
			rowA := aiMemberRow("rc-a", "甲", "以甲的身份发言")
			rowA.Model = text("should-not-leak-into-existing")
			rowA.DiscussionModel = text("should-not-leak-into-existing")
			rowB := aiMemberRow("rc-b", "乙", "以乙的身份发言")
			rowC := aiMemberRow("rc-c", "丙", "以丙的身份发言")
			rowC.DiscussionModel = tc.discussionModel
			rowC.Model = tc.nodeModel

			snapshot, added, removed, ok := discussionRolesRebased(d,
				[]db.ListRongCloudChatroomMembersWithNodeRow{rowA, rowB, rowC})
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
			model, hasKey := entry["model"]
			if tc.wantModel == "" {
				if hasKey {
					t.Errorf("两者皆空时不该写 model 键，实际写成 %v（空串要与改造前逐字节一致）", model)
				}
			} else {
				if !hasKey {
					t.Fatalf("新成员条目缺少 model 键 → 中途加进来的人会静默使用设备默认模型: %+v", entry)
				}
				if model != tc.wantModel {
					t.Errorf("model = %v，期望 %q", model, tc.wantModel)
				}
			}

			// 已有成员的条目原样保留：成员表里的模型不该被同步搬进角色板
			//（改了成员设置页的模型不等于要给进行中的讨论重新选人设）。
			if _, leaked := board["rc-a"]["model"]; leaked {
				t.Errorf("已有成员的条目被同步改写了（多出 model 键）: %+v", board["rc-a"])
			}
		})
	}
}

// ---- 4. 幂等键命中不重复广播 ----------------------------------------------

// chatroomBroadcast 是假融云收到的一条聊天室广播（讨论组 v3 事件信封）。
type chatroomBroadcast struct {
	EventType string          `json:"eventType"`
	Seq       int64           `json:"seq"`
	Data      json.RawMessage `json:"data"`
}

// decodeChatroomBroadcasts 解出聊天室广播的正文（按发生顺序）。
func decodeChatroomBroadcasts(t *testing.T, entries []map[string]string) []chatroomBroadcast {
	t.Helper()
	out := make([]chatroomBroadcast, 0, len(entries))
	for _, entry := range entries {
		var decoded chatroomBroadcast
		if err := json.Unmarshal([]byte(entry["content"]), &decoded); err != nil {
			t.Fatalf("聊天室广播不是 JSON: %v (%s)", err, entry["content"])
		}
		out = append(out, decoded)
	}
	return out
}

// TestRoleSync_IdempotencyHitDoesNotRebroadcast 是 P2-1 的回归防线，也是「同一轮
// 反复同步」这条路径在**广播层**的断言。
//
// 之前的覆盖只到「事件不新增、version 不变」这一层（见
// TestRoleSync_RepeatedSyncIsIdempotent），那只能证明库里没多一行；而 P2-1 的 bug
// 恰恰是「库里没有新事件、但 C 端又收到一条同 seq 的 discussion_progress」——
// 症状在广播层，不在存储层。所以这条用例把 rt.client 换成记账用假融云，直接数
// 聊天室广播次数。
//
// 四个阶段：新增 → 无变化再同步 → 移除 → 同一轮加回（增删集与首次完全相同，幂等键
// 命中）。最后一阶段是关键：角色板确实变了（changed=true，成员回来了），但事件与
// 广播都不能再涨。
//
// 变异（把 switch 退回 `if emitErr != nil {...} else { broadcast }`，即永远广播）：
// 最后一阶段的广播计数变成 3，这条用例变红。已在 -overlay 下验过。
func TestRoleSync_IdempotencyHitDoesNotRebroadcast(t *testing.T) {
	f := setupDiscussionFixture(t)
	f.rt.interactiveEnabled = true
	rec, client := newRelayRecorder(t)
	f.rt.client = client // 只有装上 client，interactiveBroadcast 才会真的发出去
	ctx := context.Background()

	memberA := f.memberNode
	// 会被「移出 → 加回」的成员：节点先建好，成员表里一开始就有（角色板里没有）。
	flip := seedNode(t, f.queries, f.wsID, "ai-node-flip-"+uniqueID("t"), `["discussion_participant"]`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM rongcloud_node WHERE id=$1", flip.ID)
	})
	addAIMemberToRoom(t, f, flip, "摇摆者")

	d := newWindowFixtureDiscussion(t, f, 1, [][2]string{{memberA.RongcloudUserID, "分析者"}})

	// 阶段 1：首次同步（纯新增）→ 1 条事件 + 1 次广播，且广播的 seq 就是那条事件。
	synced, changed, err := f.rt.syncDiscussionRoles(ctx, d)
	if err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	if !changed {
		t.Fatal("成员表里多了一个人，首次同步必须报告 changed")
	}
	events := roleSyncProgressEvents(t, f, d.DiscussionID)
	if len(events) != 1 {
		t.Fatalf("首次同步后 role-sync 事件 = %d 条，期望 1", len(events))
	}
	broadcasts := decodeChatroomBroadcasts(t, rec.chatroomSnapshot())
	if len(broadcasts) != 1 {
		t.Fatalf("首次同步后聊天室广播 = %d 次，期望 1（广播没发出去的话下面的断言没意义）",
			len(broadcasts))
	}
	if broadcasts[0].Seq != events[0].Seq {
		t.Errorf("广播 seq = %d，期望与落库事件 seq %d 一致", broadcasts[0].Seq, events[0].Seq)
	}
	if broadcasts[0].EventType != "discussion_progress" {
		t.Errorf("广播 eventType = %q，期望 discussion_progress", broadcasts[0].EventType)
	}

	// 阶段 2：成员表没有变化，再同步一次 → 无操作（不落事件、不广播、不动 version）。
	again, changedAgain, err := f.rt.syncDiscussionRoles(ctx, synced)
	if err != nil {
		t.Fatalf("二次同步: %v", err)
	}
	if changedAgain {
		t.Error("成员表没有变化，二次同步不该报告 changed")
	}
	if again.Version != synced.Version {
		t.Errorf("二次同步改了 version：%d → %d", synced.Version, again.Version)
	}
	if got := len(roleSyncProgressEvents(t, f, d.DiscussionID)); got != 1 {
		t.Errorf("二次同步多落了事件：role-sync = %d 条，期望仍为 1", got)
	}
	if got := len(rec.chatroomSnapshot()); got != 1 {
		t.Errorf("二次同步多广播了一次：聊天室广播 = %d 次，期望仍为 1", got)
	}

	// 阶段 3：把成员移出 → 角色板变了（removed 非空）→ 新事件 + 新广播。
	removeAIMemberFromRoom(t, f, flip.RongcloudUserID)
	afterRemove, changedRemove, err := f.rt.syncDiscussionRoles(ctx, again)
	if err != nil {
		t.Fatalf("移除后的同步: %v", err)
	}
	if !changedRemove {
		t.Fatal("成员被移出了，同步必须报告 changed")
	}
	if _, exists := parseRoleSnapshot(t, afterRemove.RoleSnapshot)[flip.RongcloudUserID]; exists {
		t.Fatalf("被移出的成员仍在角色板里: %s", afterRemove.RoleSnapshot)
	}
	if got := len(roleSyncProgressEvents(t, f, d.DiscussionID)); got != 2 {
		t.Fatalf("移除后 role-sync 事件 = %d 条，期望 2", got)
	}
	if got := len(rec.chatroomSnapshot()); got != 2 {
		t.Fatalf("移除后聊天室广播 = %d 次，期望 2", got)
	}

	// 阶段 4（关键）：同一轮把它加回来 —— 增删集与阶段 1 完全相同（added=[摇摆者]、
	// removed=[]），幂等键命中既存事件 → 角色板照常更新，但事件与广播都不能再涨。
	addAIMemberToRoom(t, f, flip, "摇摆者")
	afterReadd, changedReadd, err := f.rt.syncDiscussionRoles(ctx, afterRemove)
	if err != nil {
		t.Fatalf("加回后的同步: %v", err)
	}
	if !changedReadd {
		t.Fatal("成员回来了，同步必须报告 changed（幂等命中的是**事件**，不是角色板更新）")
	}
	if _, back := parseRoleSnapshot(t, afterReadd.RoleSnapshot)[flip.RongcloudUserID]; !back {
		t.Fatalf("幂等命中把角色板更新一起吞掉了（成员没回来）: %s", afterReadd.RoleSnapshot)
	}
	if afterReadd.Version != afterRemove.Version {
		t.Errorf("同步改了 version：%d → %d（role_snapshot 不在乐观锁范围内）",
			afterRemove.Version, afterReadd.Version)
	}
	if got := len(roleSyncProgressEvents(t, f, d.DiscussionID)); got != 2 {
		t.Errorf("幂等命中后事件数 = %d，期望仍为 2", got)
	}
	if got := len(rec.chatroomSnapshot()); got != 2 {
		t.Errorf("幂等命中后聊天室广播 = %d 次，期望仍为 2 —— "+
			"否则 C 端会收到两条同 seq 的 discussion_progress（P2-1 回归）", got)
	}

	// 广播与事件一一对应：两次广播的 seq 必须正好是那两条事件的 seq（顺序一致）。
	finalEvents := roleSyncProgressEvents(t, f, d.DiscussionID)
	finalBroadcasts := decodeChatroomBroadcasts(t, rec.chatroomSnapshot())
	if len(finalBroadcasts) != len(finalEvents) {
		t.Fatalf("广播次数 %d 与事件条数 %d 不一致", len(finalBroadcasts), len(finalEvents))
	}
	for i := range finalEvents {
		if finalBroadcasts[i].Seq != finalEvents[i].Seq {
			t.Errorf("第 %d 次广播 seq = %d，期望 %d", i+1, finalBroadcasts[i].Seq, finalEvents[i].Seq)
		}
	}
}
