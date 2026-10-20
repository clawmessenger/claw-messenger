package rongcloud

import (
	"strings"
	"testing"
)

// 本轮任务协议：首轮立论、中段交叉质询、停滞时收口。这三种形态是讨论质量的
// 主要杠杆，改动这些文案会直接改变线上每个 AI 的发言方式，所以逐条钉住。

func TestInteractiveRoundTaskFirstRoundIsIndependentPosition(t *testing.T) {
	task := interactiveRoundTask(1, false, 0)
	for _, want := range []string{"第 1 轮", "结论", "依据", "最可能被推翻的一点"} {
		if !strings.Contains(task, want) {
			t.Errorf("首轮任务缺少 %q：\n%s", want, task)
		}
	}
	// 首轮不能要求「点名回应」：那时候还没有别人的发言可比。
	if strings.Contains(task, "点名回应") {
		t.Errorf("首轮不该要求交叉质询：\n%s", task)
	}
}

func TestInteractiveRoundTaskLaterRoundsRequireCrossExamination(t *testing.T) {
	task := interactiveRoundTask(3, false, 0)
	for _, want := range []string{"第 3 轮", "点名回应", "角色名", "你改变了什么", "你坚持什么"} {
		if !strings.Contains(task, want) {
			t.Errorf("第 3 轮任务缺少 %q：\n%s", want, task)
		}
	}
	// 轮次必须出现在抬头里，否则成员会把本轮当全新讨论从零重开。
	if !strings.Contains(task, "第 3 轮") {
		t.Errorf("轮次抬头丢失：\n%s", task)
	}
	if strings.Contains(task, "第 4 轮") {
		t.Errorf("轮次写错：\n%s", task)
	}
}

func TestInteractiveRoundTaskStalledBranchDropsRestating(t *testing.T) {
	task := interactiveRoundTask(3, true, 0.91)
	for _, want := range []string{"讨论已停滞", "91%", "新材料", "直接收口"} {
		if !strings.Contains(task, want) {
			t.Errorf("停滞任务缺少 %q：\n%s", want, task)
		}
	}
	// 停滞时不该再鼓励「补充、质疑、收敛都行」这种模糊推进。
	if strings.Contains(task, "你改变了什么") {
		t.Errorf("停滞分支不该沿用常规质询模板：\n%s", task)
	}
}

func TestInteractiveHostTaskKeepsDissentSection(t *testing.T) {
	task := interactiveHostTask(2, false, 0)
	// 第 2 轮起是修订模式：在上一版产物基础上修订，四段结构与「建议」行保持不变。
	for _, want := range []string{
		"第 2 轮", "在它的基础上修订", "完整的新版全文", "【已达成的共识】", "【仍存在的分歧】",
		"【未解决的问题】", "【下一轮聚焦】", "建议：继续", "建议：结束",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("主持人任务缺少 %q：\n%s", want, task)
		}
	}
	if strings.Contains(task, "相似度") {
		t.Errorf("未停滞时不该出现相似度说明：\n%s", task)
	}

	stalledTask := interactiveHostTask(2, true, 0.9)
	if !strings.Contains(stalledTask, "90%") || !strings.Contains(stalledTask, "没有产生新信息") {
		t.Errorf("停滞时纪要任务必须点明无新信息：\n%s", stalledTask)
	}

	// 第 1 轮仍是起草模式（没有上一版可修订），但同样要有四段结构与 dissent 底线。
	firstTask := interactiveHostTask(1, false, 0)
	for _, want := range []string{
		"第 1 轮", "第一版", "【已达成的共识】", "【仍存在的分歧】",
		"不要为了好看把异议磨平", "建议：继续", "建议：结束",
	} {
		if !strings.Contains(firstTask, want) {
			t.Errorf("第 1 轮主持任务缺少 %q：\n%s", want, firstTask)
		}
	}
}

func TestBigramJaccardBounds(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want float64
	}{
		{"identical", "数据库连接池上限", "数据库连接池上限", 1},
		{"disjoint", "数据库连接池上限", "预算审批流程", 0},
		// 两侧各 5 个二元组，只有「连接」重合：交集 1、并集 5+5-1=9 → 1/9。
		{"partial", "数据库连接池", "连接超时重试", 1.0 / 9.0},
		{"empty left", "", "任何内容", 0},
		{"empty right", "任何内容", "", 0},
		{"single rune", "好", "好", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bigramJaccard(tc.a, tc.b); got != tc.want {
				t.Errorf("bigramJaccard(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestInteractiveRoundStalled(t *testing.T) {
	history := interactiveHistory{
		contributions: []interactiveStatement{
			{Round: 1, RoleName: "架构师", Content: "建议先做灰度发布，把风险控制在单个机房。"},
			{Round: 1, RoleName: "产品经理", Content: "灰度发布对用户感知的影响需要提前告知。"},
			// 第 2 轮把第 1 轮的说法换词重述一遍。
			{Round: 2, RoleName: "架构师", Content: "建议先做灰度发布，把风险控制在单个机房。"},
			{Round: 2, RoleName: "产品经理", Content: "灰度发布对用户感知的影响需要提前告知。"},
		},
	}
	stalled, similarity := interactiveRoundStalled(history, 2)
	if !stalled {
		t.Errorf("逐字重述应判为停滞，similarity=%v", similarity)
	}
	if similarity != 1 {
		t.Errorf("逐字重述的相似度应为 1，实际 %v", similarity)
	}

	// 首轮没有可比的上一轮，一律判否。
	if stalled, _ := interactiveRoundStalled(history, 1); stalled {
		t.Error("第 1 轮不能判为停滞")
	}

	// 空轮不能判为停滞：空轮由空轮守卫处理，判成停滞会把「节点全超时」误报成「讨论收敛」。
	empty := interactiveHistory{}
	if stalled, _ := interactiveRoundStalled(empty, 2); stalled {
		t.Error("空轮不能判为停滞")
	}
}

func TestInteractiveRoundStalledAllowsSubstantiveProgress(t *testing.T) {
	history := interactiveHistory{
		contributions: []interactiveStatement{
			{Round: 1, RoleName: "架构师", Content: "建议先做灰度发布，把风险控制在单个机房。"},
			{Round: 2, RoleName: "架构师", Content: "补充一个约束：支付回调必须同机房双写，灰度要按商户维度切。"},
		},
	}
	stalled, similarity := interactiveRoundStalled(history, 2)
	if stalled {
		t.Errorf("新增约束条件不应判为停滞，similarity=%v", similarity)
	}
	if similarity >= interactiveStallThreshold {
		t.Errorf("similarity=%v 应低于阈值 %v", similarity, interactiveStallThreshold)
	}
}
