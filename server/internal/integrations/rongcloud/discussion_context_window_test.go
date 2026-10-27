package rongcloud

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// 讨论组上下文窗口（按智能体自身发言截断）的测试（task 2026-10-27/task-005）。
//
// 产品口径（已冻结，见任务书）：
//   - 截断锚点 = 该智能体**自己最后一次发言（含这条）** 到最新；
//   - 新成员（无锚点）取最近 N 条，N = 普通 AI 成员数（不含主持人）；
//   - 主持人豁免，仍看本轮全员发言 + 上一版产物全文；
//   - 窗口至少保留 1 条；
//   - 预算是 4000 字符硬顶（按 rune）。
//
// 这些用例刻意用「集合/序列相等」而不是 Contains 断言：窗口的正确性有一半体现在
// 「什么不该出现」，只查存在性的话，把裁剪整个去掉（windowFor 直接 return h）
// 依然会全绿 —— 那就等于没测。

// windowTestHistory 是 5 条发言、跨 3 轮的固定样本：
//
//	idx 0  轮1 甲
//	idx 1  轮1 乙
//	idx 2  轮2 甲
//	idx 3  轮2 乙
//	idx 4  轮3 乙
func windowTestHistory() interactiveHistory {
	return interactiveHistory{contributions: []interactiveStatement{
		{Round: 1, MemberID: "A", RoleName: "甲", Content: "甲第一轮"},
		{Round: 1, MemberID: "B", RoleName: "乙", Content: "乙第一轮"},
		{Round: 2, MemberID: "A", RoleName: "甲", Content: "甲第二轮"},
		{Round: 2, MemberID: "B", RoleName: "乙", Content: "乙第二轮"},
		{Round: 3, MemberID: "B", RoleName: "乙", Content: "乙第三轮"},
	}}
}

// statementContents 取出窗口里的发言正文（顺序即事件顺序）。
func statementContents(h interactiveHistory) []string {
	out := make([]string, 0, len(h.contributions))
	for _, statement := range h.contributions {
		out = append(out, statement.Content)
	}
	return out
}

// assertStatementContents 断言窗口内容与期望**逐条相等**（长度也相等）：
// 多一条少一条都算错，这正是「边界真的守住了」的断言形态。
func assertStatementContents(t *testing.T, got interactiveHistory, want []string) {
	t.Helper()
	contents := statementContents(got)
	if len(contents) != len(want) {
		t.Fatalf("窗口 = %v（%d 条），期望 %v（%d 条）", contents, len(contents), want, len(want))
	}
	for i := range want {
		if contents[i] != want[i] {
			t.Errorf("窗口[%d] = %q，期望 %q（完整窗口 %v）", i, contents[i], want[i], contents)
		}
	}
}

// TestWindowFor_AnchorStartsAtOwnLastTurn：有锚点时窗口从**自己最后一次发言**起算，
// 更早轮次的任何发言（含自己那一轮的）都必须不在窗口里。
//
// 甲在第 2 轮说过话 → 窗口 = [甲第二轮, 乙第二轮, 乙第三轮]；第 1 轮的甲/乙两条
// 都不在。断言是逐条相等，所以「把裁剪改回全量」会让这个用例直接变红。
func TestWindowFor_AnchorStartsAtOwnLastTurn(t *testing.T) {
	history := windowTestHistory()

	got := history.windowFor("A", 2)

	assertStatementContents(t, got, []string{"甲第二轮", "乙第二轮", "乙第三轮"})
}

// TestWindowFor_NoAnchorTakesTailFallbackCount：新成员（从未发言）没有锚点，
// 取尾部 N 条（N = 普通成员数）。多余的旧发言一条都不能漏进来。
func TestWindowFor_NoAnchorTakesTailFallbackCount(t *testing.T) {
	history := windowTestHistory()

	got := history.windowFor("D", 3) // D 从未发言

	assertStatementContents(t, got, []string{"甲第二轮", "乙第二轮", "乙第三轮"})
}

// TestWindowFor_FallbackCountNeverDropsBelowOne：N 传 0 或负数（历史数据/参数
// 异常）时窗口不得退化成空 —— 空上下文会让 agent 直接凭空开新话题。
func TestWindowFor_FallbackCountNeverDropsBelowOne(t *testing.T) {
	for _, fallback := range []int{0, -2} {
		history := windowTestHistory()

		got := history.windowFor("D", fallback)

		if len(got.contributions) == 0 {
			t.Fatalf("fallbackCount=%d 时窗口退化成空", fallback)
		}
		assertStatementContents(t, got, []string{"乙第三轮"})
	}
}

// TestWindowFor_LastSpeakerStillSeesOneStatement：成员自己就是最后一位发言者时，
// 锚点落在最后一条上 —— 窗口只剩他自己那一条，但绝不能是空的。
func TestWindowFor_LastSpeakerStillSeesOneStatement(t *testing.T) {
	history := windowTestHistory()

	got := history.windowFor("B", 3) // B 的最新发言就是 idx 4

	assertStatementContents(t, got, []string{"乙第三轮"})
}

// TestWindowFor_ReturnsCopyWithoutSharingBackingArray：windowFor 必须返回**新切片**。
//
// 真正会穿透回原切片的通道是元素级共享（windowFor 直接 `h.contributions[start:]`
// 时，窗口[0] 与原切片[start] 是同一个底层数组元素，改一个等于改两个）。所以这里
// 改写窗口元素再回查原切片，同时验证 append 不会让原切片长度漂移。
func TestWindowFor_ReturnsCopyWithoutSharingBackingArray(t *testing.T) {
	history := windowTestHistory()
	original := statementContents(history)
	originalLen := len(history.contributions)

	window := history.windowFor("A", 2) // 起点 idx 2
	if len(window.contributions) != 3 {
		t.Fatalf("前置条件不成立：窗口 = %v，期望 3 条", statementContents(window))
	}

	// 1) 改写窗口元素：原切片必须纹丝不动。
	window.contributions[0].Content = "被篡改"
	if got := history.contributions[2].Content; got != "甲第二轮" {
		t.Fatalf("窗口与原文共享了底层数组：原切片[2] 被改成 %q", got)
	}
	// 2) append 不得让原切片变长（也不得覆盖原切片内容）。
	window.contributions = append(window.contributions, interactiveStatement{Content: "追加"})
	if len(history.contributions) != originalLen {
		t.Fatalf("append 之后原切片长度 = %d，期望 %d", len(history.contributions), originalLen)
	}
	// 3) 原切片内容与调用前逐条一致。
	if got := statementContents(history); strings.Join(got, "|") != strings.Join(original, "|") {
		t.Fatalf("原切片被改动：%v（原 %v）", got, original)
	}
}

// TestWindowFor_KeepsSummariesAndFocus：纪要（summaries）与检查点聚焦（focus）
// 不是累计型上下文，不参与裁剪 —— 裁完必须与裁前一致。
//
// 前置断言「窗口确实变短了」是防恒真的：如果 windowFor 变成 no-op，这条用例
// 会在前置检查上失败，而不是因为 summaries 恰好相等而假绿。
func TestWindowFor_KeepsSummariesAndFocus(t *testing.T) {
	history := windowTestHistory()
	history.summaries = []interactiveStatement{
		{Round: 1, MemberID: "host", RoleName: "主持", Content: "第一轮纪要"},
		{Round: 2, MemberID: "host", RoleName: "主持", Content: "第二轮纪要"},
	}
	history.focus = map[int]string{1: "焦点一", 2: "焦点二"}

	summaryBefore, okBefore := history.lastSummaryBefore(3)
	focusBefore := history.focusForRound(3)

	window := history.windowFor("A", 2) // 裁掉 2 条

	if len(window.contributions) != 3 {
		t.Fatalf("前置条件不成立：窗口没被裁（%v）", statementContents(window))
	}
	summaryAfter, okAfter := window.lastSummaryBefore(3)
	if !okBefore || !okAfter {
		t.Fatalf("纪要必须在裁前裁后都可读：before=%v after=%v", okBefore, okAfter)
	}
	if summaryAfter.Round != summaryBefore.Round || summaryAfter.Content != summaryBefore.Content {
		t.Errorf("纪要被裁剪影响：%+v（原 %+v）", summaryAfter, summaryBefore)
	}
	if len(window.summaries) != len(history.summaries) {
		t.Errorf("summaries 条数 = %d，期望 %d", len(window.summaries), len(history.summaries))
	}
	if got := window.focusForRound(3); got != focusBefore || got != "焦点二" {
		t.Errorf("focusForRound(3) = %q，期望 %q", got, focusBefore)
	}
}

// ---- 派发层：主持人豁免 vs 普通成员裁剪（本需求最重要的一条对立断言） -------

// newWindowFixtureDiscussion 在 fixture 的聊天室里建一条 running 的 user_confirmed
// 讨论。members 是 (融云账号, 角色名) 列表，按给定顺序配 speaking_order；主持人条目
// 由这里统一补齐（角色板缺主持人时 interactiveRoles 会判 ok=false）。
func newWindowFixtureDiscussion(t *testing.T, f *discussionFixture, currentRound int, members [][2]string) DiscussionRecord {
	t.Helper()
	roles := map[string]interface{}{}
	for order, member := range members {
		roles[member[0]] = map[string]interface{}{
			"memberId": member[0], "nodeId": "node-" + member[0],
			"nickname": member[1], "roleName": member[1],
			"roleInstructions": "以" + member[1] + "的身份发言",
			"speakingOrder":    float64(order),
		}
	}
	roles["host-rc"] = map[string]interface{}{
		"memberId": "host-rc", "nodeId": "host-node",
		"nickname": "中立主持人", "roleName": "中立主持人", "isHost": true,
	}
	ctx := context.Background()
	created, err := f.rt.repo.Create(ctx, CreateDiscussionParams{
		DiscussionID: uniqueID("disc"), ChatroomID: f.roomRCID, WorkspaceID: f.wsID,
		CreatedBy: f.ownerRCID, Topic: "上下文窗口", Goal: "形成结论",
		Mode: DiscussionModeRoundtable, Status: DiscussionStatusPlanning,
		RoundPolicy:        RoundPolicyUserConfirmed,
		RoleSnapshot:       mustJSON(roles),
		HostConfigSnapshot: mustJSON(map[string]interface{}{"discussion_host_prompt": "你是中立主持人"}),
	})
	if err != nil {
		t.Fatalf("create discussion: %v", err)
	}
	cleanupDiscussion(t, f.pool, created.DiscussionID)
	running, err := f.rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: created.DiscussionID, ExpectedVersion: created.Version,
		Status: DiscussionStatusRunning, CurrentRound: currentRound, RoundRevision: 0,
	})
	if err != nil {
		t.Fatalf("advance to running round %d: %v", currentRound, err)
	}
	return running
}

// appendFixtureEvent 按事件流顺序落一条讨论事件（seq 由库里当前 max 顺延）。
func appendFixtureEvent(t *testing.T, f *discussionFixture, discussionID, eventType, actor string, payload map[string]interface{}) {
	t.Helper()
	ctx := context.Background()
	seq, err := f.rt.repo.NextEventSeq(ctx, discussionID)
	if err != nil {
		t.Fatalf("next seq: %v", err)
	}
	if _, err := f.rt.repo.AppendEvent(ctx, AppendDiscussionEventParams{
		DiscussionID: discussionID, Seq: seq, EventType: eventType,
		ActorID: actor, TargetID: actor, Payload: mustJSON(payload),
		IdempotencyKey: eventType + ":" + actor + ":" + uniqueID("k"),
	}); err != nil {
		t.Fatalf("append %s: %v", eventType, err)
	}
}

// dispatchTasksByTarget 读回该讨论在 outbox 里的派发任务，按 targetId 索引。
func dispatchTasksByTarget(t *testing.T, f *discussionFixture, discussionID string) map[string]interactiveTask {
	t.Helper()
	commands, err := f.queries.ListRongCloudDiscussionCommandsByDiscussion(context.Background(), discussionID)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	out := map[string]interactiveTask{}
	for _, cmd := range commands {
		var task interactiveTask
		if err := json.Unmarshal(cmd.Payload, &task); err != nil {
			continue
		}
		if task.TargetID != "" {
			out[task.TargetID] = task
		}
	}
	return out
}

// roleFromSnapshot 从角色板取一个角色（含主持人），取不到即失败 —— 避免用例在
// 角色板写错时靠零值角色跑出一个「看起来通过」的结果。
func roleFromSnapshot(t *testing.T, d DiscussionRecord, memberID string) interactiveRole {
	t.Helper()
	ordinary, host, ok := interactiveRoles(d)
	if !ok {
		t.Fatalf("角色板不可用: %s", d.RoleSnapshot)
	}
	if host.MemberID == memberID {
		return host
	}
	for _, role := range ordinary {
		if role.MemberID == memberID {
			return role
		}
	}
	t.Fatalf("角色板里没有成员 %s", memberID)
	return interactiveRole{}
}

// TestDispatch_HostExemptFromWindow_MemberScoped 是本需求的核心对立断言：
//
//   - 主持人派发（出本轮纪要）拿到的上下文里，本轮**全部**成员发言都在，
//     且上一版产物是**全文**（不被 interactiveSummaryClip 截到 1500）；
//   - 普通成员派发走自己的锚点窗口：在自己上次发言之前的那些发言一条都不在，
//     上一版产物仍按 1500 截断。
//
// 断言落在 outbox 的 task.Prompt 上（设备端实际收到的 role_prompt），而不是直接
// 调 interactiveBuildPrompt —— 裁剪决策写在 interactiveDispatch 里，绕过它测
// 「主持人豁免」等于没测。
func TestDispatch_HostExemptFromWindow_MemberScoped(t *testing.T) {
	f := setupDiscussionFixture(t)
	ctx := context.Background()
	members := [][2]string{
		{"rc-analyst", "分析者"},
		{"rc-critic", "质疑者"},
		{"rc-builder", "补充者"},
	}
	d := newWindowFixtureDiscussion(t, f, 3, members)

	// 第 1、2 轮全员发言 + 两轮纪要；第 3 轮还没人开口（当前要派给质疑者）。
	for _, round := range []int{1, 2} {
		for memberIdx, member := range members {
			appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", member[0],
				map[string]interface{}{"round": round,
					"content": []string{"分析者第", "质疑者第", "补充者第"}[memberIdx] + strconv.Itoa(round) + "轮"})
		}
	}
	appendFixtureEvent(t, f, d.DiscussionID, "round_summary_completed", "host-rc",
		map[string]interface{}{"round": 1, "content": "第一轮总结：共识 X"})
	// 第 2 轮纪要做成超长文本：正文 1800 个字（※ 只出现在这段纪要里，便可以用
	// 字符计数把「主持人拿全文 / 普通成员拿 1500」精确区分开）。
	longSummary := strings.Repeat("※", 1800)
	appendFixtureEvent(t, f, d.DiscussionID, "round_summary_completed", "host-rc",
		map[string]interface{}{"round": 2, "content": longSummary})

	// ---- 普通成员：质疑者 ----
	critic := roleFromSnapshot(t, d, "rc-critic")
	if err := f.rt.interactiveDispatch(ctx, d, critic, 1); err != nil {
		t.Fatalf("dispatch critic: %v", err)
	}
	criticPrompt := dispatchTasksByTarget(t, f, d.DiscussionID)["rc-critic"].Prompt
	if criticPrompt == "" {
		t.Fatal("普通成员派发没有落进 outbox")
	}
	// 锚点 = 自己第 2 轮那条：第 2 轮里他之前的「分析者第2轮」与第 1 轮全员都该被裁掉。
	for _, gone := range []string{"分析者第1轮", "质疑者第1轮", "补充者第1轮", "分析者第2轮"} {
		if strings.Contains(criticPrompt, gone) {
			t.Errorf("普通成员窗口漏回了锚点之前的发言 %q：%s", gone, criticPrompt)
		}
	}
	for _, want := range []string{"质疑者第2轮", "补充者第2轮"} {
		if !strings.Contains(criticPrompt, want) {
			t.Errorf("普通成员窗口缺少锚点之后的发言 %q：%s", want, criticPrompt)
		}
	}
	if got := strings.Count(criticPrompt, "※"); got != interactiveSummaryClip {
		t.Errorf("普通成员拿到的上一轮纪要 = %d 字，期望按 interactiveSummaryClip(%d) 截断",
			got, interactiveSummaryClip)
	}

	// ---- 主持人：本轮全员说完后出纪要 ----
	for memberIdx, member := range members {
		appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", member[0],
			map[string]interface{}{"round": 3,
				"content": []string{"分析者第", "质疑者第", "补充者第"}[memberIdx] + "3轮"})
	}
	host := roleFromSnapshot(t, d, "host-rc")
	if err := f.rt.interactiveDispatch(ctx, d, host, 1); err != nil {
		t.Fatalf("dispatch host: %v", err)
	}
	hostPrompt := dispatchTasksByTarget(t, f, d.DiscussionID)["host-rc"].Prompt
	if hostPrompt == "" {
		t.Fatal("主持人派发没有落进 outbox")
	}
	for _, want := range []string{"分析者第3轮", "质疑者第3轮", "补充者第3轮"} {
		if !strings.Contains(hostPrompt, want) {
			t.Errorf("主持人上下文缺少本轮成员发言 %q（豁免被破坏）：%s", want, hostPrompt)
		}
	}
	if got := strings.Count(hostPrompt, "※"); got != 1800 {
		t.Errorf("主持人拿到的上一版产物 = %d 字，期望 1800 字全文（不被 interactiveSummaryClip 截断）", got)
	}
}

// TestDispatch_StallCheckRunsOnFullHistory 钉住裁剪与停滞判定的**先后顺序**。
//
// 停滞检测是引擎级判断（第 N 轮是否在重述第 N-1 轮），必须吃全量历史；如果先按
// 成员锚点裁窗口再比，上一轮发言可能整段不在窗口里，相似度恒为 0，停滞协议永远
// 不触发（成员会一直「交叉质询」到用户放弃）。
//
// 这里让第 1、2 轮的发言逐字相同（相似度 1.0），而质疑者的锚点正好落在第 2 轮
// 自己那条上 —— 裁剪后的窗口里上一轮空无一条。提示词出现「讨论已停滞」就证明
// 判定发生在裁剪之前。
func TestDispatch_StallCheckRunsOnFullHistory(t *testing.T) {
	f := setupDiscussionFixture(t)
	ctx := context.Background()
	members := [][2]string{{"rc-analyst", "分析者"}, {"rc-critic", "质疑者"}}
	d := newWindowFixtureDiscussion(t, f, 2, members)

	same := map[string]string{
		"rc-analyst": "我认为应该先做 A，理由是成本最低而且可回滚，这是我最没把握的一点。",
		"rc-critic":  "我不同意先做 A，成本被低估了，而且回滚会丢掉已经验证过的数据，我坚持先补数据。",
	}
	for _, round := range []int{1, 2} {
		for _, member := range members {
			appendFixtureEvent(t, f, d.DiscussionID, "contribution_completed", member[0],
				map[string]interface{}{"round": round, "content": same[member[0]]})
		}
	}

	critic := roleFromSnapshot(t, d, "rc-critic")
	if err := f.rt.interactiveDispatch(ctx, d, critic, 1); err != nil {
		t.Fatalf("dispatch critic: %v", err)
	}
	prompt := dispatchTasksByTarget(t, f, d.DiscussionID)["rc-critic"].Prompt

	if !strings.Contains(prompt, "讨论已停滞") {
		t.Fatalf("第 2 轮与第 1 轮逐字相同却未判停滞（疑似先裁窗口再判停滞）：%s", prompt)
	}
}
