package rongcloud

// discussion_roundtable_recommend_gate.go 是「同一 discussion 同时只跑一批角色推荐」
// 的在途闸门。
//
// 为什么需要它：一次推荐 = 服务端给主持设备下发一条命令 + 设备端跑一次完整的 LLM
// 回合（十几秒到 100s+），而设备端是单槽串行队列 —— 叠加派发不会更快，只会多排
// 一轮，整体更慢、角色还可能翻倍（每批都会 assignRoleNodes 并追加进推荐行）。
//
// 触发叠加的路径在「推荐很慢」这个场景里恰好最常见：
//   - 前端等到 120s+ 看到「比通常慢」的提示，第一反应就是点「重试」；
//   - S1 首推还在跑，用户在 S2 点「换一批」或自动续推又发一条；
//   - 换个入口重试（uniapp / 网页同时开着）。
//
// 去重放在服务端而不是只靠前端：前端只能管住自己这一路，管不住重放、多端与
// 「用户手快连点两下不同入口」。
//
// 实现选型：**进程内存 map**，不动数据库。
//   - 它表达的是「此刻有没有一个 goroutine 在跑」，本质是易失的运行时状态；进程
//     重启后在跑的管线已经随进程一起没了，标记跟着一起消失才是对的（重启丢标记
//     是可接受的：最坏只是允许一次新的派发）。
//   - 落库要新增列/表 + 迁移，还要在每条返回路径上 CAS 清除。管线有 8 条早退
//     分支（讨论已推进/后台已回收/host 不可用/无候选设备……），漏一条就是「永远
//     推荐不了」，比现在更糟。内存版用一个 defer 就能覆盖全部出口。
//   - 不选「复用 attempt / setup_step」：attempt 是**已落事件**的计数（失败重试
//     也要 +1），它不表达「在途」；setup_step 在整个 S2 期间一直是 roles，无法区分
//     「正在跑」与「跑完了等用户挑」。

import (
	"sync"
	"time"
)

// setupRecommendInflightTTL 是「在途」标记的最长存活时间。
//
// 一次推荐的墙钟上限是 host.timeout_seconds（管理端夹逼 ≤ 600s）+ 5s 传输余量，
// 再加候选解析与落库，11 分钟足够宽。它的作用只有一个：万一标记没被清掉（goroutine
// 卡在某个不返回的 IO 上、进程被 suspend 过），不能让这个 discussion 永久推荐不了
// —— 去重是为了省一次 LLM 回合，绝不能变成新的死锁。
const setupRecommendInflightTTL = 11 * time.Minute

// setupRecommendInProgressCode 是「已有在途推荐、本次未派发」的 code。
//
// 刻意不复用超时/失败码（ROLE_RECOMMENDATION_TIMEOUT 等）：那些是「这一批失败了，
// 请重试」，而这个的含义是「这一批还在跑，请等它」—— 前端要据此**保持等待态**而
// 不是切失败卡，混用会把用户从等待直接推到重试。
const setupRecommendInProgressCode = "ROLE_RECOMMEND_IN_PROGRESS"

// setupRecommendGate 记录「哪些 discussion 正在跑推荐」。
//
// 零值可用（map 在首次 tryAcquire 里惰性创建）；并发安全靠一把互斥锁保护 map
// （临界区只有一次 map 读写，键的数量级是「正在配置中的讨论组个数」）。
type setupRecommendGate struct {
	mu       sync.Mutex
	inflight map[string]time.Time
	// now 可被用例替换，默认 time.Now。
	now func() time.Time
}

func (g *setupRecommendGate) clock() time.Time {
	if now := g.now; now != nil {
		return now()
	}
	return time.Now()
}

// tryAcquire 尝试占用 discussionID 的在途位。
//
// 返回 (占位时刻, 是否占到)。占到的一方必须把占位时刻原样交给 release —— 用时刻
// 当令牌，是为了避免「超 TTL 被接管后，上一轮迟到的 release 把新一轮的标记删掉」
// 这种 ABA。
func (g *setupRecommendGate) tryAcquire(discussionID string) (time.Time, bool) {
	if g == nil || discussionID == "" {
		// 未装配 / 无 id：不拦。闸门是省流量的优化，不该成为新的失败面。
		return time.Time{}, true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	if startedAt, busy := g.inflight[discussionID]; busy && now.Sub(startedAt) < setupRecommendInflightTTL {
		return time.Time{}, false
	}
	if g.inflight == nil {
		g.inflight = make(map[string]time.Time, 4)
	}
	g.inflight[discussionID] = now
	return now, true
}

// release 释放 discussionID 的在途位。
//
// 只在标记仍属于本轮（时刻相同）时才删：管线跑满 TTL 被接管的情况下，本轮迟到的
// release 不能把下一轮的标记清掉，否则第三批又能叠进来。
func (g *setupRecommendGate) release(discussionID string, acquiredAt time.Time) {
	if g == nil || discussionID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if startedAt, ok := g.inflight[discussionID]; ok && startedAt.Equal(acquiredAt) {
		delete(g.inflight, discussionID)
	}
}

// busy 返回该 discussion 是否有在途推荐（含尚未被惰性清理的过期标记，仅供用例断言）。
func (g *setupRecommendGate) busy(discussionID string) bool {
	if g == nil || discussionID == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	startedAt, ok := g.inflight[discussionID]
	return ok && g.clock().Sub(startedAt) < setupRecommendInflightTTL
}
