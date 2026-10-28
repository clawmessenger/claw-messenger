package rongcloud

// 插话（R6）版本放宽的回归用例。
//
// 背景：流式回答期间 stateVersion 一直在变，如果插话还按乐观锁硬冲突，
// 「随时插话」恰恰在最需要它的时刻（agent 正说着话）永远失败。现在版本落后
// 一律按库中最新接受，只有**终态**仍然拒绝 —— 终态之后没有轮次能承载这条意见，
// 接受它等于让用户看到「已接受」却永远没人读。

import (
	"context"
	"testing"
)

// interjectCmd 组一条 v3 interject 命令。
func interjectCmd(discussionID string, stateVersion int, messageUID, content string) CommandContent {
	return CommandContent{
		Service: "discussion", Action: "interject", WebEnvelope: true,
		Params: discussionParams(
			"discussionId", discussionID,
			"stateVersion", stateVersion,
			"messageUid", messageUID,
			"content", content,
		),
	}
}

// interjectAs 以聊天室 owner 身份执行一次 v3 插话。
func interjectAs(t *testing.T, f *discussionFixture, cmd CommandContent) commandResult {
	t.Helper()
	f.rt.interactiveEnabled = true
	result, handled := f.rt.interactiveLifecycleAction(context.Background(), "interject", cmd,
		discussionActor{RcID: f.ownerRCID})
	if !handled {
		t.Fatal("interject must be handled by the v3 engine, not fall back to legacy")
	}
	return result
}

// bumpVersion 推进一次讨论版本（模拟流式期间状态机自己在动），让客户端手里的
// stateVersion 变旧。
func bumpVersion(t *testing.T, f *discussionFixture, d DiscussionRecord) DiscussionRecord {
	t.Helper()
	next, err := f.rt.repo.AdvanceState(context.Background(), AdvanceDiscussionParams{
		DiscussionID: d.DiscussionID, ExpectedVersion: d.Version,
		Status: DiscussionStatusRunning, CurrentRound: d.CurrentRound,
		RoundRevision: d.RoundRevision,
	})
	if err != nil {
		t.Fatalf("bump version: %v", err)
	}
	return next
}

// TestInteractiveInterject_AcceptsStaleStateVersion 是 R6 的核心断言：
// 带上过期的 stateVersion 也要 200（不再 409），并回 appliedRound 告诉前端
// 「会在第几轮读到」。
func TestInteractiveInterject_AcceptsStaleStateVersion(t *testing.T) {
	f := setupDiscussionFixture(t)
	created := newFixtureDiscussion(t, f)
	staleVersion := created.Version
	bumpVersion(t, f, created) // 客户端手里的版本从此过期

	uid := "uid-stale-" + uniqueID("t")
	result := interjectAs(t, f, interjectCmd(created.DiscussionID, staleVersion, uid, "补充一个观点"))
	if result.code != 200 {
		t.Fatalf("stale stateVersion must be accepted, got %d (%s)", result.code, result.message)
	}
	data, _ := result.data.(map[string]interface{})
	applied, ok := data["appliedRound"].(int)
	if !ok || applied < 1 {
		t.Fatalf("response must carry appliedRound >= 1, got %v", data["appliedRound"])
	}
	// running 且本轮尚未全员完成 → 归当前轮（顺延只发生在轮末/汇总/未开局）。
	if applied != created.CurrentRound {
		t.Fatalf("appliedRound = %d, want %d", applied, created.CurrentRound)
	}
}

// TestInteractiveInterject_DuplicateMessageUIDKeepsAppliedRound 钉住幂等分支：
// 设备重投同一条插话（流式期间很常见）必须仍然 200，且 appliedRound 与首次一致
// —— 否则前端会把同一个气泡的「将在第 N 轮读到」改成另一个轮次。
func TestInteractiveInterject_DuplicateMessageUIDKeepsAppliedRound(t *testing.T) {
	f := setupDiscussionFixture(t)
	created := newFixtureDiscussion(t, f)
	staleVersion := created.Version
	bumpVersion(t, f, created)

	uid := "uid-dup-" + uniqueID("t")
	first := interjectAs(t, f, interjectCmd(created.DiscussionID, staleVersion, uid, "同一个观点"))
	if first.code != 200 {
		t.Fatalf("first interjection failed: %d (%s)", first.code, first.message)
	}
	firstData, _ := first.data.(map[string]interface{})
	firstRound, _ := firstData["appliedRound"].(int)

	second := interjectAs(t, f, interjectCmd(created.DiscussionID, staleVersion, uid, "同一个观点"))
	if second.code != 200 {
		t.Fatalf("duplicate messageUid must be accepted idempotently, got %d (%s)", second.code, second.message)
	}
	secondData, _ := second.data.(map[string]interface{})
	if got, _ := secondData["appliedRound"].(int); got != firstRound {
		t.Fatalf("duplicate appliedRound = %v, want %d", secondData["appliedRound"], firstRound)
	}

	// 同 uid 不同内容 = 客户端串号，必须拒（幂等键冲突不能静默吞掉）。
	conflict := interjectAs(t, f, interjectCmd(created.DiscussionID, staleVersion, uid, "换了内容"))
	if conflict.code != 400 {
		t.Fatalf("conflicting content for the same messageUid must be rejected, got %d (%s)",
			conflict.code, conflict.message)
	}
}

// TestInteractiveInterject_RejectsTerminalDiscussion 钉住「终态仍然拒绝」。
//
// 三种终态分别测：客户端带**最新**版本时走「版本一致但仍要看状态」这条路，
// 漏判会让用户在已结束的讨论上看到「已接受」。
func TestInteractiveInterject_RejectsTerminalDiscussion(t *testing.T) {
	for _, terminal := range []string{
		DiscussionStatusCompleted,
		DiscussionStatusCancelled,
		DiscussionStatusFailed,
	} {
		t.Run(terminal, func(t *testing.T) {
			f := setupDiscussionFixture(t)
			ctx := context.Background()
			created := newFixtureDiscussion(t, f)
			final, err := f.rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
				DiscussionID: created.DiscussionID, ExpectedVersion: created.Version,
				Status: terminal, CurrentRound: created.CurrentRound,
				RoundRevision: created.RoundRevision,
			})
			if err != nil {
				t.Fatalf("advance to %s: %v", terminal, err)
			}
			if IsLiveDiscussionStatus(final.Status) {
				t.Fatalf("%s must not count as live", terminal)
			}

			result := interjectAs(t, f, interjectCmd(created.DiscussionID, final.Version,
				"uid-terminal-"+uniqueID("t"), "还想补一句"))
			if result.code != 409 {
				t.Fatalf("terminal %s must reject interjection, got %d (%s)",
					terminal, result.code, result.message)
			}
		})
	}
}

// TestInteractiveInterject_RejectsTerminalEvenWithStaleVersion 补上另一条路径：
// 版本过期 → 实现会先刷新到库中最新 → 刷新后可能已经是终态，必须再判一次。
// 只测「版本一致」那一路会漏掉刷新后的二次判定。
func TestInteractiveInterject_RejectsTerminalEvenWithStaleVersion(t *testing.T) {
	f := setupDiscussionFixture(t)
	ctx := context.Background()
	created := newFixtureDiscussion(t, f)
	staleVersion := created.Version
	if _, err := f.rt.repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: created.DiscussionID, ExpectedVersion: created.Version,
		Status: DiscussionStatusCompleted, CurrentRound: created.CurrentRound,
		RoundRevision: created.RoundRevision,
	}); err != nil {
		t.Fatalf("complete discussion: %v", err)
	}

	result := interjectAs(t, f, interjectCmd(created.DiscussionID, staleVersion,
		"uid-terminal-stale-"+uniqueID("t"), "还想补一句"))
	if result.code != 409 {
		t.Fatalf("terminal discussion must reject even with a stale version, got %d (%s)",
			result.code, result.message)
	}
}
