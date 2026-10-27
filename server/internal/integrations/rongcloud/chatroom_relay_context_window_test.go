package rongcloud

import (
	"strings"
	"testing"
)

// 群聊中继上下文窗口的测试（task 2026-10-27/task-005）。
//
// 产品口径与讨论组一致（两条链路必须同口径，否则同一个 AI 在群聊与讨论组里看到的
// 「自己」判若两人）：窗口起点是 viewer **自己最后一次发言所在的轮**，该轮只带锚点
// 及其之后的 reply；新入群成员（无锚点）带尾部 N 条（N = 群内 AI 成员数）；
// 结果至少 1 条。因为数据结构是轮粒度，允许半轮截取。
//
// 断言同样用「逐条相等」而不是 Contains：只查存在性的话，把裁剪去掉（整段历史
// 全带上）依然全绿。

// relayTestHistory 是 4 轮固定样本：
//
//	轮 1（问题一）  甲一 / 乙一
//	轮 2（问题二）  乙二 / 甲二 / 丙二
//	轮 3（问题三）  乙三 / 丙三
//	轮 4（问题四）  乙四 / 丙四
func relayTestHistory() []relayRound {
	return []relayRound{
		{userText: "问题一", replies: []relayReply{{roleName: "A", content: "甲一"}, {roleName: "B", content: "乙一"}}},
		{userText: "问题二", replies: []relayReply{{roleName: "B", content: "乙二"}, {roleName: "A", content: "甲二"}, {roleName: "C", content: "丙二"}}},
		{userText: "问题三", replies: []relayReply{{roleName: "B", content: "乙三"}, {roleName: "C", content: "丙三"}}},
		{userText: "问题四", replies: []relayReply{{roleName: "B", content: "乙四"}, {roleName: "C", content: "丙四"}}},
	}
}

// relayWindowDigest 把窗口摊平成「u:用户消息」「角色:发言」序列，便于逐条比对。
func relayWindowDigest(rounds []relayRound) []string {
	out := make([]string, 0, len(rounds)*3)
	for _, round := range rounds {
		out = append(out, "u:"+round.userText)
		for _, reply := range round.replies {
			out = append(out, reply.roleName+":"+reply.content)
		}
	}
	return out
}

// assertRelayWindow 断言窗口摊平后与期望**逐条相等**（长度也相等）。
func assertRelayWindow(t *testing.T, got []relayRound, want []string) {
	t.Helper()
	digest := relayWindowDigest(got)
	if len(digest) != len(want) {
		t.Fatalf("窗口 = %v，期望 %v", digest, want)
	}
	for i := range want {
		if digest[i] != want[i] {
			t.Fatalf("窗口[%d] = %q，期望 %q（完整窗口 %v）", i, digest[i], want[i], digest)
		}
	}
}

// relayTestSession 造一个带着 relayTestHistory 的群聊会话（3 名 AI 成员）。
func relayTestSession(viewer string) (*relaySession, relayMember) {
	members := []relayMember{
		{rcID: "rc_a", roleName: "A", order: 0},
		{rcID: "rc_b", roleName: "B", order: 1},
		{rcID: "rc_c", roleName: "C", order: 2},
	}
	var self relayMember
	for _, member := range members {
		if member.roleName == viewer {
			self = member
		}
	}
	return &relaySession{
		chatroomRCID: "room-window",
		userText:     "新问题",
		isGroup:      true,
		totalRounds:  2,
		roundIndex:   1,
		members:      members,
		history:      relayTestHistory(),
	}, self
}

// TestRelayHistoryWindow_StartsAtOwnLastTurn：viewer 在轮 2 说过话 → 窗口起点是它
// 在轮 2 的那条（含）；轮 1 整轮不在，轮 2 里锚点之前的乙二也不在（允许半轮截取）。
// 锚点所在轮的用户消息（问题二）保留 —— 少了它，那段发言就失去了「在回答什么」。
func TestRelayHistoryWindow_StartsAtOwnLastTurn(t *testing.T) {
	history := relayTestHistory()

	window := relayHistoryWindow(history, "A", 3) // A 的最后一次发言在轮 2 的甲二

	assertRelayWindow(t, window, []string{
		"u:问题二", "A:甲二", "C:丙二",
		"u:问题三", "B:乙三", "C:丙三",
		"u:问题四", "B:乙四", "C:丙四",
	})
}

// TestRelayHistoryWindow_NoAnchorTakesTailN：从未发言的新成员没有锚点，取尾部 N 条
// reply（允许半轮）。多出来的旧发言一条都不能漏进来。
//
// 历史共 9 条 reply，N=3 → 尾部 3 条 = 轮 4 的乙四/丙四 + 轮 3 的丙三，
// 于是窗口从轮 3 的第二条 reply 起算。
func TestRelayHistoryWindow_NoAnchorTakesTailN(t *testing.T) {
	history := relayTestHistory()

	window := relayHistoryWindow(history, "D", 3) // D 从未发言

	assertRelayWindow(t, window, []string{
		"u:问题三", "C:丙三",
		"u:问题四", "B:乙四", "C:丙四",
	})
}

// TestRelayHistoryWindow_LastSpeakerKeepsOneReply：viewer 自己就是最后一位发言者时，
// 窗口只剩他那一条，但绝不能退化成一个空窗口（空上下文 = agent 凭空开新话题）。
func TestRelayHistoryWindow_LastSpeakerKeepsOneReply(t *testing.T) {
	history := relayTestHistory()

	window := relayHistoryWindow(history, "C", 3) // C 的最后一次发言是轮 4 的丙四

	assertRelayWindow(t, window, []string{"u:问题四", "C:丙四"})
}

// TestBuildPrompt_KeepsGroupChatPrefix：设备端（xiachat-cli）靠提示词开头的
// "[group chat] " 判定场合；前缀一旦变了，群聊会被当成私聊处理。
func TestBuildPrompt_KeepsGroupChatPrefix(t *testing.T) {
	hub := &chatroomRelayHub{}
	session, self := relayTestSession("A")

	prompt := hub.buildPrompt(session, self)

	if !strings.HasPrefix(prompt, "[group chat] ") {
		t.Fatalf("提示词前缀变了（设备端靠它判 venue）：%.60q", prompt)
	}
}

// TestBuildPrompt_ChainAnchorLandsInsideWindow：「接住「X」的话」点名的那位 X，
// 必须真在上下文窗口里 —— 否则成员被要求接一句自己看不到的话。
//
// X 是上一轮的最后一位（本轮还没人说话，lastSpeaker 回看历史尾部），而历史窗口
// 对 viewer A 已经裁到锚点之后：尾部的发言必须完整保留，这是窗口「尾部恒等于
// history 尾部」这条不变量的直接体现。
func TestBuildPrompt_ChainAnchorLandsInsideWindow(t *testing.T) {
	hub := &chatroomRelayHub{}
	session, self := relayTestSession("A")

	prompt := hub.buildPrompt(session, self)

	for _, want := range []string{"请接住「C」刚才的话往下说", "丙四"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("接龙锚点 %q 没落在窗口里：%s", want, prompt)
		}
	}
}

// TestBuildPrompt_ContextAndTotalStayWithinLimits：上下文段不越 relayPromptContextLimit
// （4000），整条提示词不越 relayPromptTotalLimit（7000）。
//
// 越界的代价不是「上下文少一点」，而是 CLI 的 argv 超限后整轮退化 stdin ——
// openclaw / opencode / hermes 都不支持 stdin，用户看到的是「发了消息没人应」。
func TestBuildPrompt_ContextAndTotalStayWithinLimits(t *testing.T) {
	hub := &chatroomRelayHub{}
	session, self := relayTestSession("A")
	// 把历史放大到远超预算：每轮 3 条 900 字发言 + 500 字用户消息。
	huge := make([]relayRound, 0, 6)
	for round := 1; round <= 6; round++ {
		replies := make([]relayReply, 0, 3)
		for _, name := range []string{"A", "B", "C"} {
			replies = append(replies, relayReply{roleName: name, content: strings.Repeat(string(rune('a'+round)), 900)})
		}
		huge = append(huge, relayRound{userText: strings.Repeat("问", 500), replies: replies})
	}
	session.history = huge
	session.replies = []relayReply{{roleName: "C", content: strings.Repeat("答", 900)}}

	prompt := hub.buildPrompt(session, self)

	if got := len([]rune(prompt)); got > relayPromptTotalLimit {
		t.Fatalf("整条提示词 = %d 字，超过 relayPromptTotalLimit(%d)", got, relayPromptTotalLimit)
	}
	start := strings.Index(prompt, "\n\n更早的对话（")
	end := strings.Index(prompt, "\n\n现在轮到你")
	if start < 0 || end <= start {
		t.Fatalf("提示词里找不到历史段/本轮指令段：%.200q", prompt)
	}
	if got := len([]rune(prompt[start:end])); got > relayPromptContextLimit {
		t.Fatalf("历史段 = %d 字，超过 relayPromptContextLimit(%d)", got, relayPromptContextLimit)
	}
}

// TestBuildPrompt_NoHistoryKeepsLegacyShape：空历史仍返回旧形态 —— 不渲染
// 「更早的对话」段，也不会留下空段落（连着两个以上换行）。
func TestRelayWindow_NoHistoryKeepsLegacyShape(t *testing.T) {
	hub := &chatroomRelayHub{}
	session := &relaySession{
		chatroomRCID: "room-empty",
		userText:     "大家好",
		isGroup:      true,
		totalRounds:  1,
		members:      []relayMember{{rcID: "rc_a", roleName: "A"}},
	}

	prompt := hub.buildPrompt(session, session.members[0])

	if strings.Contains(prompt, "更早的对话") {
		t.Fatalf("空历史不该渲染历史段：%s", prompt)
	}
	if strings.Contains(prompt, "\n\n\n") {
		t.Fatalf("空历史产生了空段落：%q", prompt)
	}
	if !strings.Contains(prompt, "用户起的头：大家好") || !strings.Contains(prompt, "现在轮到你开接") {
		t.Fatalf("空历史必须是旧形态（起头 + 开接指令）：%s", prompt)
	}
}
