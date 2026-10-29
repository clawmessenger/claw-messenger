package rongcloud

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSetupRecommendGateSingleFlight 钉住闸门的基本语义：一批在跑时第二批进不来，
// 释放后立刻能再来一批（失败后「重试」必须立刻可用）。
func TestSetupRecommendGateSingleFlight(t *testing.T) {
	var gate setupRecommendGate
	first, ok := gate.tryAcquire("discussion-1")
	if !ok {
		t.Fatal("首次占位必须成功")
	}
	if _, ok := gate.tryAcquire("discussion-1"); ok {
		t.Fatal("在途期间的第二次占位必须失败")
	}
	// 不同 discussion 互不影响（多用户同时配置是常态）。
	if _, ok := gate.tryAcquire("discussion-2"); !ok {
		t.Fatal("另一个 discussion 不该被挡住")
	}
	gate.release("discussion-1", first)
	if gate.busy("discussion-1") {
		t.Fatal("release 后必须不再 busy")
	}
	if _, ok := gate.tryAcquire("discussion-1"); !ok {
		t.Fatal("释放后必须能重新占位（失败重试路径）")
	}
}

// TestSetupRecommendGateConcurrentAcquireWinsOnce 是「两个并发 request 只派发一次」的
// 闸门层对拍：N 个 goroutine 同时抢，只能有一个占到。
func TestSetupRecommendGateConcurrentAcquireWinsOnce(t *testing.T) {
	var gate setupRecommendGate
	const racers = 32
	var winners atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := gate.tryAcquire("discussion-race"); ok {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("并发占位成功数 = %d, want 1", got)
	}
	if !gate.busy("discussion-race") {
		t.Fatal("抢到的一方必须留下在途标记")
	}
}

// TestSetupRecommendGateTTLReclaimsAndStaleReleaseIsIgnored 钉住两条防死锁/防 ABA 的
// 语义：
//  1. 标记超过 TTL 未清（goroutine 卡死/进程被 suspend）时按过期处理，不能让这个
//     discussion 永久推荐不了 —— 去重是为了省一次 LLM 回合，不能变成新的死锁；
//  2. 被接管之后，上一轮迟到的 release 不能把新一轮的标记删掉（否则第三批又能叠）。
func TestSetupRecommendGateTTLReclaimsAndStaleReleaseIsIgnored(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	gate := setupRecommendGate{now: func() time.Time { return now }}
	staleAt, ok := gate.tryAcquire("discussion-1")
	if !ok {
		t.Fatal("首次占位必须成功")
	}

	// TTL 内：仍算在途。
	now = now.Add(setupRecommendInflightTTL - time.Second)
	if _, ok := gate.tryAcquire("discussion-1"); ok {
		t.Fatal("TTL 内不该被接管")
	}

	// 超过 TTL：接管。
	now = now.Add(2 * time.Second)
	freshAt, ok := gate.tryAcquire("discussion-1")
	if !ok {
		t.Fatal("超过 TTL 必须允许接管（防死锁）")
	}

	// 上一轮迟到的 release（带上旧时刻）不能清掉新一轮的标记。
	gate.release("discussion-1", staleAt)
	if !gate.busy("discussion-1") {
		t.Fatal("陈旧 release 不该清掉新一轮标记")
	}

	gate.release("discussion-1", freshAt)
	if gate.busy("discussion-1") {
		t.Fatal("本轮 release 后必须不再 busy")
	}
}

// TestSetupRecommendGateNilAndEmptyKeyAreTransparent 钉住「闸门永不制造新的失败面」：
// 未装配（nil）/ 讨论 id 为空时一律放行。
func TestSetupRecommendGateNilAndEmptyKeyAreTransparent(t *testing.T) {
	var nilGate *setupRecommendGate
	if _, ok := nilGate.tryAcquire("discussion-1"); !ok {
		t.Fatal("nil 闸门必须放行")
	}
	nilGate.release("discussion-1", time.Time{})
	if nilGate.busy("discussion-1") {
		t.Fatal("nil 闸门不该 busy")
	}

	var gate setupRecommendGate
	if _, ok := gate.tryAcquire(""); !ok {
		t.Fatal("空 discussionId 必须放行")
	}
	gate.release("", time.Time{})
}
