package rongcloud

import (
	"context"
	"sync"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestSetupRequestRoleRecommendDeduplicatesInFlight 是「在途去重」的动作层对拍。
//
// 线上场景：用户等到 120s+ 看到「比通常慢」的软提示，第一反应就是点「重试」——
// 而服务端那一批其实还在跑。叠加派发不会更快（设备端是单槽串行队列），只会再排
// 一整轮 LLM，角色还可能翻倍。这里用一个会阻塞的 nodeOwner 把第一轮管线钉在
// 「候选解析」上，模拟设备端正在跑回合，然后断言：
//
//  1. 在途期间再来一条 requestRoleRecommend（换一批 **和** 继续添加）都不派发，
//     回 200 + already_running + ROLE_RECOMMEND_IN_PROGRESS（不是失败码）；
//  2. 第一轮失败（落 setup_recommend_failed）后，标记必须已经清掉，重试能立刻
//     重新派发 —— 清不掉的标记会让「失败后永远重试不了」，比重复派发更糟。
func TestSetupRequestRoleRecommendDeduplicatesInFlight(t *testing.T) {
	f := setupSetupFixture(t)
	ctx := context.Background()
	chatroomID, discussionID := f.createShell(t, "dedup-"+uniqueID("t"))
	// 直接把断点推到 roles：不走 S1，避免首推先把闸门占掉、干扰断言。
	if _, err := f.queries.SubmitRongCloudDiscussionSetupTopic(ctx, db.SubmitRongCloudDiscussionSetupTopicParams{
		DiscussionID: discussionID, Topic: "去重议题", Goal: "",
	}); err != nil {
		t.Fatalf("推进到 roles: %v", err)
	}

	// 第一轮卡在候选解析：等价于「设备端正在跑一整轮 LLM」。
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	f.rt.roundtable.nodeOwner = func(context.Context, string) ([]db.RongcloudNode, error) {
		entered <- struct{}{}
		<-release
		return []db.RongcloudNode{f.memberNode}, nil
	}

	request := func(appendMode bool) map[string]interface{} {
		t.Helper()
		r := f.rt.actionRequestRoleRecommend(ctx, CommandContent{
			Service: "discussion", Action: "requestRoleRecommend",
			Params: map[string]interface{}{"chatroom_id": chatroomID, "append": appendMode},
		}, f.actor)
		if r.code != 200 {
			t.Fatalf("requestRoleRecommend(append=%v): %d %s", appendMode, r.code, r.message)
		}
		data, _ := r.data.(map[string]interface{})
		if data == nil {
			t.Fatalf("requestRoleRecommend 回包缺 data")
		}
		return data
	}

	first := request(false)
	if first["already_running"] == true {
		t.Fatalf("第一批不该被去重: %#v", first)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("第一轮管线没有进入候选解析")
	}

	// 换一批：在途 → 不派发。
	second := request(false)
	if second["already_running"] != true {
		t.Fatalf("在途时的「换一批」必须回 already_running: %#v", second)
	}
	if second["code"] != setupRecommendInProgressCode {
		t.Fatalf("code = %v, want %s", second["code"], setupRecommendInProgressCode)
	}
	if second["accepted"] != true {
		t.Fatalf("accepted = %v, 形状要与既有约定一致（200 + accepted）", second["accepted"])
	}

	// 继续添加（append）：同样受约束，叠加一样是双批。
	third := request(true)
	if third["already_running"] != true {
		t.Fatalf("在途时的「继续添加」必须同样被去重: %#v", third)
	}

	// 在途期间没有第二批被派发：nodeOwner 不该再被调用一次。
	select {
	case <-entered:
		t.Fatal("在途期间派发了第二批（nodeOwner 被调了第二次）")
	case <-time.After(200 * time.Millisecond):
	}

	// 放行第一轮：broker 未装配 → fail(SYSTEM_HOST_OFFLINE) → 落失败事件。
	releaseOnce.Do(func() { close(release) })
	f.waitEvent(t, discussionID, setupEventRecommendFailed)

	// 标记必须已随 goroutine 的 defer 一起清掉（轮询一下，落事件与 return 有调度间隙）。
	deadline := time.Now().Add(5 * time.Second)
	for f.rt.recommendGate.busy(discussionID) {
		if time.Now().After(deadline) {
			t.Fatal("失败后在途标记没清掉 —— 用户点重试会被永久挡住")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 失败后立刻重试：必须真的重新派发，而不是回 already_running。
	retry := request(false)
	if retry["already_running"] == true {
		t.Fatalf("失败后的重试必须重新派发: %#v", retry)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("重试后第二轮管线没有开始")
	}
}
