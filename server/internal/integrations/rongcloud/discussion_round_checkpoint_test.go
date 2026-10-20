package rongcloud

import (
	"strings"
	"testing"
)

// 检查点这三件事必须钉住：解析要能从纪要里把代码块摘干净、认不出的成员不能进
// 事件（立场错归属比没有检查点更糟）、下一轮聚焦要真的传回下一轮。

func TestParseInteractiveCheckpointStripsBlock(t *testing.T) {
	content := "【已达成的共识】\n- 先灰度\n【仍存在的分歧】\n- 灰度粒度\n\n" +
		"```checkpoint\n" +
		`{"memberPositions":[{"memberId":"member-a","position":"支持按机房灰度"}],` +
		`"agreements":["先灰度"],"disagreements":["粒度未定"],"openQuestions":["谁来切流量"],` +
		`"nextFocus":"确定灰度粒度","recommendation":"continue"}` +
		"\n```"

	wire, markdown, ok := parseInteractiveCheckpoint(content)
	if !ok {
		t.Fatal("带 checkpoint 代码块的回包应被识别")
	}
	if strings.Contains(markdown, "checkpoint") || strings.Contains(markdown, "memberPositions") {
		t.Errorf("正文里不该留下机器可读块：\n%s", markdown)
	}
	if !strings.Contains(markdown, "【已达成的共识】") {
		t.Errorf("正文被切坏了：\n%s", markdown)
	}
	if len(wire.MemberPositions) != 1 || wire.MemberPositions[0].MemberID != "member-a" {
		t.Errorf("成员立场没解析出来：%#v", wire.MemberPositions)
	}
	if wire.NextFocus != "确定灰度粒度" || wire.Recommendation != "continue" {
		t.Errorf("聚焦/建议解析错误：%q / %q", wire.NextFocus, wire.Recommendation)
	}
}

func TestParseInteractiveCheckpointToleratesJSONFence(t *testing.T) {
	content := "纪要正文\n```json\n" +
		`{"memberPositions":[{"memberId":"a","position":"反对"}],"recommendation":"finish"}` +
		"\n```"
	wire, markdown, ok := parseInteractiveCheckpoint(content)
	if !ok || len(wire.MemberPositions) != 1 {
		t.Fatalf("json 兜底应能解析：ok=%v wire=%#v", ok, wire)
	}
	if strings.TrimSpace(markdown) != "纪要正文" {
		t.Errorf("正文里残留了代码块：%q", markdown)
	}
}

func TestParseInteractiveCheckpointLeavesPlainAnswerAlone(t *testing.T) {
	cases := map[string]string{
		"没有代码块":    "【已达成的共识】暂无\n建议：继续",
		"无关的 json": "示例数据：\n```json\n{\"foo\":1}\n```",
		"坏的 json":  "正文\n```checkpoint\n{不是 json}\n```",
		"没闭合":      "正文\n```checkpoint\n{\"agreements\":[]}",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, markdown, ok := parseInteractiveCheckpoint(content)
			if ok {
				t.Fatal("不该识别出检查点")
			}
			if markdown != content {
				t.Errorf("解析失败时正文必须原样返回，实际 %q", markdown)
			}
		})
	}
}

func TestNormalizeInteractiveCheckpointGuardsAttribution(t *testing.T) {
	ordinary := []interactiveRole{
		{MemberID: "member-a", RoleName: "架构师"},
		{MemberID: "member-b", RoleName: "财务"},
		{MemberID: "member-c", RoleName: "财务"}, // 重名：不能按名字认定是谁
		{MemberID: "member-d", RoleName: "产品"},
	}
	wire := interactiveCheckpointWire{
		MemberPositions: []struct {
			MemberID string `json:"memberId"`
			RoleName string `json:"roleName"`
			Name     string `json:"name"`
			Position string `json:"position"`
		}{
			{MemberID: "member-a", Position: " 支持灰度 "},
			{MemberID: "member-a", Position: "重复条目"},              // 同一人重复 → 只留第一条
			{MemberID: "ghost", Position: "编出来的成员"},               // 认不出 → 丢
			{MemberID: "财务", Position: "按名字认不出（重名）"},              // 重名 → 丢
			{MemberID: "", RoleName: "架构师", Position: "按唯一角色名可认"}, // 但 member-a 已占位 → 丢
			{MemberID: "", RoleName: "产品", Position: " 先补埋点再灰度 "}, // 唯一角色名 → 认成 member-d
			{MemberID: "member-b", Position: "  "},                // 空立场 → 丢
		},
		Agreements:     []string{" 先灰度 ", "", "先灰度", "只做核心链路"},
		Disagreements:  nil,
		OpenQuestions:  []string{"谁来切流量"},
		NextFocus:      "  确定灰度粒度  ",
		Recommendation: "",
	}

	payload := normalizeInteractiveCheckpoint(wire, 2, ordinary)
	positions := payload["memberPositions"].([]map[string]interface{})
	if len(positions) != 2 {
		t.Fatalf("应剩 member-a 与按唯一角色名认出的 member-d 两条，实际 %#v", positions)
	}
	if positions[0]["memberId"] != "member-a" || positions[0]["position"] != "支持灰度" {
		t.Errorf("立场内容被改坏或归属错误：%#v", positions[0])
	}
	if positions[1]["memberId"] != "member-d" || positions[1]["position"] != "先补埋点再灰度" {
		t.Errorf("唯一角色名应能认回 member-d：%#v", positions[1])
	}

	agreements := payload["agreements"].([]string)
	if len(agreements) != 2 || agreements[0] != "先灰度" {
		t.Errorf("共识应去空去重，实际 %#v", agreements)
	}
	if payload["round"] != 2 {
		t.Errorf("轮次必须由服务端定，实际 %#v", payload["round"])
	}
	// 前端校验要求这三个字段是数组，nil 会序列化成 null 被整条丢弃。
	for _, key := range []string{"disagreements", "openQuestions"} {
		if payload[key] == nil {
			t.Errorf("%s 不能是 nil", key)
		}
	}
	if payload["recommendation"] != "continue" {
		t.Errorf("没写 recommendation 时默认 continue，实际 %#v", payload["recommendation"])
	}
	if payload["nextFocus"] != "确定灰度粒度" {
		t.Errorf("下一轮聚焦去空白失败：%#v", payload["nextFocus"])
	}
}

func TestNormalizeInteractiveCheckpointCapsItems(t *testing.T) {
	ordinary := []interactiveRole{{MemberID: "member-a", RoleName: "架构师"}}
	agreements := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		agreements = append(agreements, strings.Repeat("共", i+1))
	}
	payload := normalizeInteractiveCheckpoint(interactiveCheckpointWire{
		Agreements: agreements, Recommendation: "FINISH",
	}, 1, ordinary)
	if got := len(payload["agreements"].([]string)); got != interactiveCheckpointMaxItems {
		t.Errorf("条数应截到 %d，实际 %d", interactiveCheckpointMaxItems, got)
	}
	if payload["recommendation"] != "finish" {
		t.Errorf("recommendation 大小写不敏感，实际 %#v", payload["recommendation"])
	}

	long := normalizeInteractiveCheckpoint(interactiveCheckpointWire{
		NextFocus: strings.Repeat("长", interactiveCheckpointMaxRunes+10),
	}, 1, ordinary)
	if got := len([]rune(long["nextFocus"].(string))); got != interactiveCheckpointMaxRunes {
		t.Errorf("过长文本按 rune 截到 %d，实际 %d", interactiveCheckpointMaxRunes, got)
	}
}

func TestInteractiveHostTaskAsksForCheckpointBlock(t *testing.T) {
	task := interactiveHostTask(2, false, 0)
	for _, want := range []string{"```checkpoint", "memberPositions", "nextFocus", "recommendation", "memberId"} {
		if !strings.Contains(task, want) {
			t.Errorf("主持人任务缺少检查点契约片段 %q：\n%s", want, task)
		}
	}
	// 不写 memberId 就无法把立场归到具体成员，这条约束必须显式说出来。
	if !strings.Contains(task, "逐字抄") {
		t.Errorf("没有要求逐字抄 memberId：\n%s", task)
	}
}

func TestInteractiveCheckpointRosterListsMembers(t *testing.T) {
	roster := interactiveCheckpointRoster([]interactiveRole{
		{MemberID: "member-a", RoleName: "架构师"},
		{MemberID: "member-b", Nickname: "财务"},
	})
	for _, want := range []string{"member-a", "架构师", "member-b", "财务"} {
		if !strings.Contains(roster, want) {
			t.Errorf("名册缺少 %q：\n%s", want, roster)
		}
	}
	if interactiveCheckpointRoster(nil) != "" {
		t.Error("没有普通成员时不该产出空名册")
	}
}

func TestInteractiveHistoryFocusForRound(t *testing.T) {
	history := interactiveHistory{focus: map[int]string{1: "确定灰度粒度"}}
	if got := history.focusForRound(2); got != "确定灰度粒度" {
		t.Errorf("第 2 轮应取第 1 轮检查点的聚焦，实际 %q", got)
	}
	// 第 1 轮没有上一轮，不能凭空给个聚焦。
	if got := history.focusForRound(1); got != "" {
		t.Errorf("第 1 轮不该有聚焦，实际 %q", got)
	}
	if got := (interactiveHistory{}).focusForRound(3); got != "" {
		t.Errorf("没有检查点时返回空串，实际 %q", got)
	}
}

func TestRoundCheckpointIsAcceptedAsV3Event(t *testing.T) {
	// 白名单缺这一项时事件会被降级成 discussion_progress，前端卡片永远不出现。
	if !isInteractiveV3EventType("round_checkpoint") {
		t.Error("round_checkpoint 必须在 v3 事件白名单里")
	}
}

// 检查点必须用系统账号广播：C 端 ingress 对「节点发送的 discussion_event」只放行
// contribution_completed / round_summary_completed，用主持人节点身份发会被当伪造
// 事件丢掉（而且线上旧前端没有这条白名单，改客户端也救不回来）。
func TestRoundCheckpointIsBroadcastAsSystemEvent(t *testing.T) {
	record := DiscussionRecord{
		RoleSnapshot: mustJSON(map[string]interface{}{
			"member-a": map[string]interface{}{"memberId": "member-a", "roleName": "架构师", "speakingOrder": float64(0)},
			"host":     map[string]interface{}{"memberId": "host", "roleName": "主持人", "isHost": true},
		}),
	}
	checkpoint := DiscussionEventRecord{EventType: "round_checkpoint", ActorID: "system", TargetID: "host"}
	if got := discussionEventSender(record, checkpoint, "claw-system"); got != "claw-system" {
		t.Errorf("检查点应由系统账号广播，实际 %q", got)
	}
	// 反证：把 actor 换成主持人就会走节点账号，那正是会被前端丢掉的那种发件人。
	asNode := DiscussionEventRecord{EventType: "round_checkpoint", ActorID: "host"}
	if got := discussionEventSender(record, asNode, "claw-system"); got != "host" {
		t.Errorf("节点身份应解析为节点账号（说明这条路径确实存在），实际 %q", got)
	}
}

// 提示词里的两处接线：下一轮聚焦要进成员的本轮要求，成员名册只给主持人、
// 且放在上下文块之外（名册和前序发言抢额度会把纪要素材挤掉）。
func TestInteractiveBuildPromptCarriesFocusAndHostRoster(t *testing.T) {
	record := DiscussionRecord{
		DiscussionID: "d1", ChatroomID: "c1",
		Topic: "灰度上线", Goal: "定下灰度粒度", CurrentRound: 2,
		RoleSnapshot: mustJSON(map[string]interface{}{
			"member-a": map[string]interface{}{
				"memberId": "member-a", "roleName": "架构师", "speakingOrder": float64(0),
			},
			"host": map[string]interface{}{
				"memberId": "host", "roleName": "中立主持人", "isHost": true,
			},
		}),
		HostConfigSnapshot: mustJSON(map[string]interface{}{"discussion_host_prompt": "你是中立主持人"}),
	}
	rt := &discussionRuntime{}
	history := interactiveHistory{focus: map[int]string{1: "确定灰度粒度"}}

	memberPrompt := rt.interactiveBuildPrompt(record,
		interactiveRole{MemberID: "member-a", RoleName: "架构师"}, "本轮要求：点名回应。", nil, history)
	if !strings.Contains(memberPrompt, "本轮聚焦") || !strings.Contains(memberPrompt, "确定灰度粒度") {
		t.Errorf("成员提示词没带上下一轮聚焦：\n%s", memberPrompt)
	}
	if strings.Contains(memberPrompt, "成员名册") {
		t.Errorf("成员不该拿到主持人专用的名册：\n%s", memberPrompt)
	}

	hostPrompt := rt.interactiveBuildPrompt(record,
		interactiveRole{MemberID: "host", RoleName: "中立主持人", IsHost: true},
		interactiveHostTask(2, false, 0), nil, history)
	if !strings.Contains(hostPrompt, "memberId=member-a") || !strings.Contains(hostPrompt, "逐字抄") {
		t.Errorf("主持人提示词缺名册或抄写约束：\n%s", hostPrompt)
	}
	if !strings.Contains(hostPrompt, "```checkpoint") {
		t.Errorf("主持人提示词缺检查点代码块要求：\n%s", hostPrompt)
	}
}
