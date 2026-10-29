package rongcloud

import (
	"context"
	"sync"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestSetupSubmitTopicWhileRecommendingDistinguishesTopicChange 锁住 S1 在途期间的
// 两种回包形状。
//
// 背景：SubmitRongCloudDiscussionSetupTopic 的 WHERE 是
// `status='configuring' AND setup_step IN ('topic','roles')` —— **'roles' 是允许值**，
// 因为「用户在 S2 回上一步改议题」是合法流程。于是「推荐在途（断点正是 roles）时再
// 提交一次议题」会 UPDATE 成功，落到 dispatchSetupRecommend 占门失败那个分支。
// 早期那句注释把它当成了打不到的死分支，回包一刀切 {accepted:true}，结果是：另一
// 个入口提交了不同议题 → 回 200 → DB 里议题已改 → 但真派发给设备的还是旧议题 →
// 前端照常等、最后上屏一批和议题不相干的角色，全程没有任何信号（静默错配）。
//
// 现在按 topic 是否变化分流：
//   - 没变（刷新页面丢 pending、多端重放）→ 纯幂等 200 {accepted:true}；
//   - 变了 → 200 {accepted:true, already_running:true, code:SETUP_TOPIC_CHANGED_WHILE_
//     RECOMMENDING}，且**不派发**，让前端能把「新议题要等下一批」讲给用户。
//
// 用阻塞的 nodeOwner 把第一轮钉在「候选解析」上，等价于设备端正在跑一整轮 LLM。
func TestSetupSubmitTopicWhileRecommendingDistinguishesTopicChange(t *testing.T) {
	f := setupSetupFixture(t)
	ctx := context.Background()
	chatroomID, discussionID := f.createShell(t, "topic-gate-"+uniqueID("t"))

	const (
		oldTopic = "旧议题：如何让 AI 自主开发小游戏"
		newTopic = "新议题：如何把小程序发到海外"
	)

	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	f.rt.roundtable.nodeOwner = func(context.Context, string) ([]db.RongcloudNode, error) {
		entered <- struct{}{}
		<-release
		return []db.RongcloudNode{f.memberNode}, nil
	}

	submit := func(topic string) map[string]interface{} {
		t.Helper()
		r := f.rt.actionSubmitSetupTopic(ctx, CommandContent{
			Service: "discussion", Action: "submitSetupTopic",
			Params: map[string]interface{}{"chatroom_id": chatroomID, "topic": topic},
		}, f.actor)
		if r.code != 200 {
			t.Fatalf("submitSetupTopic(%q): %d %s", topic, r.code, r.message)
		}
		data, _ := r.data.(map[string]interface{})
		if data == nil {
			t.Fatalf("submitSetupTopic(%q) 回包缺 data", topic)
		}
		if data["accepted"] != true {
			t.Fatalf("submitSetupTopic(%q) accepted = %v, want true（首推与在途都是 200 受理）",
				topic, data["accepted"])
		}
		return data
	}

	// 首提：占到门、真的派发一轮。
	first := submit(oldTopic)
	if first["already_running"] == true {
		t.Fatalf("首提不该被去重: %#v", first)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("首提没有进入推荐管线（nodeOwner 未被调用）")
	}
	if !f.rt.recommendGate.busy(discussionID) {
		t.Fatal("首推在跑时闸门必须是占着的，否则后面的分流无从谈起")
	}

	// 在途 + 同 topic：纯幂等，不能多带任何「要提醒用户」的字段。
	same := submit(oldTopic)
	if same["already_running"] != nil {
		t.Fatalf("同 topic 重提不该带 already_running: %#v", same)
	}
	if same["code"] != nil {
		t.Fatalf("同 topic 重提不该带 code: %#v", same)
	}

	// 在途 + 不同 topic：必须显式告诉调用方「这批是旧议题的」。
	changed := submit(newTopic)
	if changed["already_running"] != true {
		t.Fatalf("在途改议题必须回 already_running: %#v", changed)
	}
	if changed["code"] != setupTopicChangedWhileRecommendingCode {
		t.Fatalf("code = %v, want %s（不能复用 %s，语义不同）",
			changed["code"], setupTopicChangedWhileRecommendingCode, setupRecommendInProgressCode)
	}

	// 全程没有第二批被派发：nodeOwner 不该再被调用一次。
	select {
	case <-entered:
		t.Fatal("在途期间派发了第二批（nodeOwner 被调了第二次）—— 设备端单槽串行，叠批只会更慢")
	case <-time.After(200 * time.Millisecond):
	}

	// 新议题确实落库了 —— 前端那句「议题已更新，本批推荐结束后生效」必须是真的。
	row, err := f.queries.GetConfiguringRongCloudDiscussionByChatroom(ctx, chatroomID)
	if err != nil {
		t.Fatalf("读回半成品: %v", err)
	}
	if row.Topic != newTopic {
		t.Fatalf("DB topic = %q, want %q（新议题要落库，否则「下一批生效」无从生效）", row.Topic, newTopic)
	}
	if row.SetupStep != setupStepRoles {
		t.Fatalf("setup_step = %q, want %q", row.SetupStep, setupStepRoles)
	}

	// 放行第一轮：broker 未装配 → fail(SYSTEM_HOST_OFFLINE) → 落失败事件 → defer 清标记。
	releaseOnce.Do(func() { close(release) })
	f.waitEvent(t, discussionID, setupEventRecommendFailed)
	deadline := time.Now().Add(5 * time.Second)
	for f.rt.recommendGate.busy(discussionID) {
		if time.Now().After(deadline) {
			t.Fatal("失败后在途标记没清掉 —— 新议题将永远等不到下一批")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 旧批结束后再提同一议题：这次是真派发（说明上面那次只是压住了派发，没有压住受理）。
	again := submit(newTopic)
	if again["already_running"] == true {
		t.Fatalf("旧批结束后必须能按新议题重新派发: %#v", again)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("旧批结束后没有按新议题派发新一轮")
	}
}
