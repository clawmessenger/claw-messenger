package rongcloud

// 行为规范（迁移 607 的 behavior_prompt）注入的回归用例。
//
// 覆盖任务书 C 组：讨论组每轮都注入、群聊注入在最前面且 600 字截断、
// 空规范不注入也不阻断、以及**行为准则不得侵占上下文预算**这一条最容易回归的
// 不变量（它决定极限情况下提示词会不会顶破 CLI 的 argv 上限）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// behaviorSection 是整个注入块的固定开销：标题 + 换行 + 正文 + 两个换行。
const behaviorSectionOverhead = len("【行为准则】\n") + len("\n\n") // = 8

// setAgentBehaviorPrompt 写入自定义行为规范（节点已存在）。
func (e *permEnv) setAgentBehaviorPrompt(t *testing.T, nodeID, prompt string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE rongcloud_node SET behavior_prompt = $2 WHERE node_id = $1`, nodeID, prompt); err != nil {
		t.Fatalf("set behavior prompt: %v", err)
	}
}

// ---- C8：讨论组 v3 每轮都注入 ----------------------------------------------

// TestInteractiveDispatch_InjectsBehaviorPromptEveryRound 用真派发路径（含权限
// 装载 + outbox 落库）连跑两轮，断言两轮的 role_prompt 都带【行为准则】。
//
// 为什么必须跑两轮：注入点写在「拼 prompt」里，一处少写就会表现成「第一轮有、
// 之后没了」；只测一轮看不出来。
func TestInteractiveDispatch_InjectsBehaviorPromptEveryRound(t *testing.T) {
	f := setupDiscussionFixture(t)
	ctx := context.Background()
	env := &permEnv{pool: f.pool, queries: f.queries, wsID: f.wsID}
	const want = "甲准则：只讲事实"
	env.setAgentPermission(t, f.memberNode.NodeID, "owner-1", permFlags{Discussion: true})
	env.setAgentBehaviorPrompt(t, f.memberNode.NodeID, want)
	resetAgentPermissionCache()

	created := newFixtureDiscussion(t, f)
	role := interactiveRole{MemberID: f.memberNode.RongcloudUserID, RoleName: "观看着"}

	if err := f.rt.interactiveDispatch(ctx, created, role, 1); err != nil {
		t.Fatalf("dispatch round 1: %v", err)
	}

	// 推进到第 2 轮（真实链路是主持人汇总后用户确认，这里只要「轮次变了」）。
	cur, err := f.rt.repo.Get(ctx, created.DiscussionID)
	if err != nil {
		t.Fatalf("get discussion: %v", err)
	}
	second, err := f.rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: cur.DiscussionID, ExpectedVersion: cur.Version,
		Status: DiscussionStatusRunning, CurrentRound: 2, RoundRevision: cur.RoundRevision,
	})
	if err != nil {
		t.Fatalf("advance to round 2: %v", err)
	}
	if err := f.rt.interactiveDispatch(ctx, second, role, 1); err != nil {
		t.Fatalf("dispatch round 2: %v", err)
	}

	prompts := outboxPrompts(t, f, created.DiscussionID)
	if len(prompts) != 2 {
		t.Fatalf("outbox rows = %d, want 2", len(prompts))
	}
	for i, prompt := range prompts {
		if !strings.HasPrefix(prompt, "【行为准则】\n"+want) {
			t.Errorf("round %d prompt must start with the behavior section, got %q", i+1, firstRunes(prompt, 60))
		}
	}
}

// TestInteractiveBuildPrompt_BehaviorSectionPrecedesPersona 钉住 v3 里的段落顺序：
// 行为准则在 parts[0]，人设紧随其后。模型对「靠前的系统级约束」权重更高，顺序
// 反过来就变成「人设优先」。
func TestInteractiveBuildPrompt_BehaviorSectionPrecedesPersona(t *testing.T) {
	rt := &discussionRuntime{logger: testLogger()}
	d := DiscussionRecord{
		DiscussionID: "d1", ChatroomID: "r1", Topic: "话题", Goal: "目标",
		Status: DiscussionStatusRunning, CurrentRound: 1,
	}
	prompt := rt.interactiveBuildPrompt(d,
		interactiveRole{MemberID: "m1", RoleName: "分析者", RoleInstructions: "人设：严谨", BehaviorPrompt: "乙准则"},
		"本轮任务", nil, interactiveHistory{})
	if !strings.HasPrefix(prompt, "【行为准则】\n乙准则") {
		t.Fatalf("behavior section must come first: %q", firstRunes(prompt, 80))
	}
	behaviorAt := strings.Index(prompt, "乙准则")
	personaAt := strings.Index(prompt, "人设：严谨")
	if behaviorAt < 0 || personaAt < 0 || behaviorAt > personaAt {
		t.Fatalf("behavior must precede persona: behaviorAt=%d personaAt=%d", behaviorAt, personaAt)
	}
}

// TestInteractiveBuildPrompt_EmptyBehaviorKeepsLegacyShape 是**基线保护**：没有
// 行为准则时（含直接构造的 interactiveRole）不能凭空多出一段 —— 否则既有大量
// prompt 断言会一起变形。
func TestInteractiveBuildPrompt_EmptyBehaviorKeepsLegacyShape(t *testing.T) {
	rt := &discussionRuntime{logger: testLogger()}
	d := DiscussionRecord{
		DiscussionID: "d2", ChatroomID: "r2", Topic: "话题",
		Status: DiscussionStatusRunning, CurrentRound: 1,
	}
	prompt := rt.interactiveBuildPrompt(d,
		interactiveRole{MemberID: "m1", RoleName: "分析者", RoleInstructions: "人设：严谨"},
		"本轮任务", nil, interactiveHistory{})
	if strings.Contains(prompt, "【行为准则】") {
		t.Fatalf("empty behavior must not inject a section: %q", firstRunes(prompt, 80))
	}
	if !strings.HasPrefix(prompt, "人设：严谨") {
		t.Fatalf("prompt must still start with the persona: %q", firstRunes(prompt, 80))
	}
}

// ---- C9：群聊注入位置与 600 字截断 ----------------------------------------

// TestBuildPrompt_BehaviorSectionSitsBeforeSceneAndIsClipped 钉住群聊注入的三件事：
// 位置（紧跟 "[group chat] " 前缀、场景说明之前）、600 字上界、以及截断标记。
func TestBuildPrompt_BehaviorSectionSitsBeforeSceneAndIsClipped(t *testing.T) {
	hub := &chatroomRelayHub{rt: nilRelayRuntime()}
	long := strings.Repeat("规", relayBehaviorPromptLimit+100)
	s := &relaySession{
		chatroomRCID: "roomB", userText: "大家好", isGroup: true,
		members: []relayMember{{rcID: "rc_a", roleName: "A"}},
	}
	member := relayMember{rcID: "rc_a", roleName: "A", behaviorPrompt: long}

	prompt := hub.buildPrompt(s, member)
	const head = "[group chat] 【行为准则】\n"
	if !strings.HasPrefix(prompt, head) {
		t.Fatalf("behavior block must follow the prefix: %q", firstRunes(prompt, 60))
	}
	body := strings.TrimPrefix(prompt, head)
	// 截断后的正文（含标记）必须正好是上限长度。
	section, _, found := strings.Cut(body, "\n\n")
	if !found {
		t.Fatalf("behavior section must be terminated by a blank line: %q", firstRunes(body, 40))
	}
	if got := len([]rune(section)); got != relayBehaviorPromptLimit {
		t.Fatalf("behavior section = %d runes, want %d", got, relayBehaviorPromptLimit)
	}
	if !strings.HasSuffix(section, relayClipSuffix) {
		t.Fatalf("clipped behavior must carry the marker, got tail %q", firstRunesFromEnd(section, 20))
	}
	// 场景说明必须排在准则之后（先全局约束，再「这轮干什么」）。
	if strings.Index(prompt, "这是群聊里的一次接龙聊天") < strings.Index(prompt, "【行为准则】") {
		t.Fatalf("scene description must come after the behavior block")
	}
}

// TestBuildPrompt_EmptyBehaviorDoesNotBlock 是 C10 的正控：规范为空时整条 prompt
// 照样能拼出来（不注入、不中断）。
func TestBuildPrompt_EmptyBehaviorDoesNotBlock(t *testing.T) {
	hub := &chatroomRelayHub{rt: nilRelayRuntime()}
	s := &relaySession{
		chatroomRCID: "roomC", userText: "在吗", isGroup: true,
		members: []relayMember{{rcID: "rc_a", roleName: "A"}},
	}
	prompt := hub.buildPrompt(s, s.members[0])
	if strings.Contains(prompt, "【行为准则】") {
		t.Fatalf("no behavior configured must not inject a section: %q", firstRunes(prompt, 60))
	}
	if !strings.Contains(prompt, "在吗") || len([]rune(prompt)) < 50 {
		t.Fatalf("prompt must still be built without a behavior prompt: %q", prompt)
	}
}

// TestEffectiveBehaviorPromptFallsBackToDefault 钉住「空串/纯空白 = 用模板」，
// 以及查不到节点行时同样回退模板（缺权限行 ≠ 这个 agent 不需要准则）。
func TestEffectiveBehaviorPromptFallsBackToDefault(t *testing.T) {
	for _, stored := range []string{"", "   ", "\n\t"} {
		if got := EffectiveAgentBehaviorPrompt(stored); got != DefaultAgentBehaviorPrompt {
			t.Fatalf("stored %q must fall back to the default template", stored)
		}
	}
	if got := EffectiveAgentBehaviorPrompt("  自定义  "); got != "自定义" {
		t.Fatalf("custom prompt must be trimmed, got %q", got)
	}
	set := agentPermissionSet{}
	if got := set.behaviorPromptFor("missing"); got != DefaultAgentBehaviorPrompt {
		t.Fatalf("missing permission row must still get the default template, got %q", got)
	}
}

// ---- C11：行为准则不得侵占上下文预算 --------------------------------------

// TestBuildPrompt_BehaviorBlockShrinksContextBudget 是本次改动最容易回归的
// 不变量：极限输入下，行为准则的 608 字必须从**整条**上限里扣掉，
// 上下文预算要相应缩小（2875 → 2267），而不是把提示词整体顶长 608 字。
//
// 为什么不能直接断言「整条 < 7000」：各段裁剪有固定开销，实际落点会略微越线，
// 写死数字等于把实现抄一遍。这里改用**可测量的量**：把上下文段落单独量出来
// （整条长度减去「同会话但无上下文」的长度），再看它是否落在扣减后的预算内。
//
// 用例自带「区分力守卫」：先确认不加准则时上下文确实顶满了 2875 档 —— 否则
// 这个场景根本没触到预算边界，后面的断言会变成永真。
func TestBuildPrompt_BehaviorBlockShrinksContextBudget(t *testing.T) {
	hub := &chatroomRelayHub{rt: nilRelayRuntime()}
	behavior := strings.Repeat("规", relayBehaviorPromptLimit+100) // 超长 → 截到 600
	behaviorLen := behaviorSectionOverhead + relayBehaviorPromptLimit

	// 同一会话的两种形态：withContext=false 只保留固定段落（scope/opening/turn/tail）。
	makeSession := func(withContext bool) *relaySession {
		s := &relaySession{
			chatroomRCID: "roomBudget",
			userText:     strings.Repeat("问", relayMaxTextLength), // 用户起头拉到入口上限
			isGroup:      true,
			members:      []relayMember{{rcID: "rc_a", roleName: "A"}, {rcID: "rc_b", roleName: "B"}},
			totalRounds:  3,
		}
		if !withContext {
			return s
		}
		s.topic = strings.Repeat("题", 500)
		s.history = []relayRound{{
			userText: strings.Repeat("旧", 800),
			replies: []relayReply{
				{roleName: "A", content: strings.Repeat("答", 1200)},
				{roleName: "B", content: strings.Repeat("答", 1200)},
			},
		}}
		s.replies = []relayReply{
			{roleName: "A", content: strings.Repeat("新答", 900)},
			{roleName: "B", content: strings.Repeat("新答", 900)},
		}
		return s
	}
	member := relayMember{rcID: "rc_a", roleName: "A"}
	memberWithBehavior := relayMember{rcID: "rc_a", roleName: "A", behaviorPrompt: behavior}

	baseWith := len([]rune(hub.buildPrompt(makeSession(false), memberWithBehavior)))
	fullWith := len([]rune(hub.buildPrompt(makeSession(true), memberWithBehavior)))
	baseWithout := len([]rune(hub.buildPrompt(makeSession(false), member)))
	fullWithout := len([]rune(hub.buildPrompt(makeSession(true), member)))

	// 固定段落 = 无上下文形态减去协议前缀与准则段。
	fixed := baseWith - len("[group chat] ") - behaviorLen
	budget := relayPromptContextLimit
	if room := relayPromptTotalLimit - fixed - behaviorLen; room < budget {
		budget = room
	}
	if budget < 0 {
		budget = 0
	}

	contextWith := fullWith - baseWith
	contextWithout := fullWithout - baseWithout

	// 各段裁剪会在额度之外补一个截断标记（relayClipSuffix），上下文段最多由
	// 议题 / 接力 / 历史三块拼成，每块内部又按行裁剪 —— 留 8 个标记的余量足够，
	// 远小于「没扣预算」会多出来的 608。
	clipSlack := 8 * len([]rune(relayClipSuffix))

	if contextWithout <= budget+clipSlack {
		t.Fatalf("场景没顶到预算边界（context=%d budget=%d），本用例失去区分力", contextWithout, budget)
	}
	if contextWith > budget+clipSlack {
		t.Fatalf("上下文段落 %d 超过扣除准则后的预算 %d（+%d 标记余量）—— 准则侵占了预算",
			contextWith, budget, clipSlack)
	}
	// 准则段落本身确实在（否则上一条会因为「没注入」而假绿）。
	if !strings.Contains(hub.buildPrompt(makeSession(true), memberWithBehavior), "【行为准则】") {
		t.Fatal("behavior section missing from the prompt")
	}
	// 整条仍需留在 argv 上限之内。
	if fullWith >= argvPromptMaxChars {
		t.Fatalf("prompt %d runes must stay under the argv cap (%d)", fullWith, argvPromptMaxChars)
	}
	// 用户起的头是这条链路的锚点，任何预算下都不许被挤掉。
	if !strings.Contains(hub.buildPrompt(makeSession(true), memberWithBehavior), "用户起的头：") {
		t.Fatal("the opening line must never be squeezed out")
	}
}

// outboxPrompts 按创建顺序读出该讨论所有 dispatch 任务里的 role_prompt。
func outboxPrompts(t *testing.T, f *discussionFixture, discussionID string) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT payload FROM rongcloud_discussion_command
		  WHERE discussion_id = $1 AND kind = 'dispatch'
		  ORDER BY created_at, command_id`, discussionID)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan outbox payload: %v", err)
		}
		var task struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(raw, &task); err != nil {
			t.Fatalf("decode outbox payload: %v", err)
		}
		out = append(out, task.Prompt)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox: %v", err)
	}
	return out
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstRunesFromEnd(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}
