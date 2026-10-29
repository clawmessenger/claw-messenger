package rongcloud

// node_presence_cache.go 是「节点在线探测」的短 TTL 缓存。
//
// 为什么要它：一次讨论组角色推荐要逐台确认候选设备在线（ownedAndEligibleNodes
// → nodeIsOnline → 融云 /user/checkOnline.json），每次往返 1-4s。同一轮推荐里
// 同一条链路可能反复问到同一批节点（候选解析、续推、命令下发前的复查），而且
// 用户在向导里点「换一批 / 重试」时又问一遍。缓存几秒钟的观测值就能把「N 台 ×
// 1-4s」压成「一轮一次」。
//
// 为什么 TTL 必须**短**（4s）：在线判定是「此刻能不能把命令投递到这台设备」的
// 近似，缓存越久越容易把刚上线的设备当成离线（用户看到「没有可用设备」）或者把
// 刚掉线的设备当成在线（命令发出去没人接，用户等到超时）。4s 远小于一次推荐的
// 总耗时（十几秒到 100s+），只是把同一轮里的重复探测合并掉。
//
// 只缓存**查询成功返回的观测值**：查询失败（融云打不通 / 凭据不对）不落缓存，
// 否则一次网络抖动会让那台设备在 TTL 内被稳定判成离线 —— nodeIsOnline 对失败
// 本来就是 fail closed（按离线处理），这个语义不变，但不能让它扩散到后面的重试。
//
// 注意「成功观测」包含**离线**这一侧（负缓存）：online=false 也照样缓存 4s。这是
// 把「N 台 × 1-4s」压成「一轮一次」的必要条件 —— 候选池里离线设备通常占多数，
// 只缓存在线的话省不下什么。代价是刚上线的设备最多 4s 内仍被判离线（用户可能
// 少看到一个候选，点「换一批」即可恢复）；4s 远小于一次推荐 15-100s，且失败
// 观测不落缓存，所以这个代价可接受。
//
// 并发安全：一轮推荐里所有候选设备是并发探测的（discussion_roundtable_stages.go
// 的 ownedAndEligibleNodes），缓存必须能被多个 goroutine 同时读写，用一把互斥锁
// 保护 map（键不多、临界区只有一次 map 读写，锁本身不是瓶颈；比 sync.Map 更
// 容易把「过期删除」写对）。
//
// 过期策略是**惰性删除**：读的时候发现条目已过期就当作未命中并顺手删掉；写的时候
// 如果 map 长到上限，先整体清掉已过期条目，仍然超限就整表丢弃（保守清空，最坏
// 情况是下一轮全部重新探测一次）。不做后台清理 goroutine —— 这个缓存的全部价值
// 就是省掉同一轮里的重复请求，不值得为它挂一个常驻协程。

import (
	"sync"
	"time"
)

// nodePresenceCacheTTL 是单条在线观测值的有效期。
const nodePresenceCacheTTL = 4 * time.Second

// nodePresenceCacheMaxEntries 是 map 的软上限：一台设备一条，正常情况下远小于它
// （线上节点不过两位数）。超限时先清过期、再考虑整表丢弃，避免无界增长。
const nodePresenceCacheMaxEntries = 1024

type nodePresenceEntry struct {
	online    bool
	expiresAt time.Time
}

// nodePresenceCache 是「节点融云 id → 是否在线」的短期缓存。
//
// 零值不可用，必须用 newNodePresenceCache 构造；nil 安全的读写在调用方负责
// （见 nodeIsOnline：client.presence 为 nil 时直接走实时查询）。
type nodePresenceCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]nodePresenceEntry
	// now 可被用例替换，默认 time.Now。
	now func() time.Time
}

// newNodePresenceCache 构造一个 TTL 为 ttl 的空缓存。ttl <= 0 时回落到
// nodePresenceCacheTTL：调用方不该用「关掉缓存」来表达某种意图，需要关就直接
// 不装配缓存（client.presence = nil）。
func newNodePresenceCache(ttl time.Duration) *nodePresenceCache {
	if ttl <= 0 {
		ttl = nodePresenceCacheTTL
	}
	return &nodePresenceCache{
		ttl:     ttl,
		entries: make(map[string]nodePresenceEntry),
		now:     time.Now,
	}
}

// lookup 返回 key 的缓存命中情况。过期条目按未命中处理，并就地删除。
func (c *nodePresenceCache) lookup(key string) (bool, bool) {
	if c == nil || key == "" {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return false, false
	}
	if !c.now().Before(entry.expiresAt) {
		delete(c.entries, key)
		return false, false
	}
	return entry.online, true
}

// store 记下 key 的一次成功观测。
func (c *nodePresenceCache) store(key string, online bool) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries) >= nodePresenceCacheMaxEntries {
		c.purgeExpiredLocked(now)
		if len(c.entries) >= nodePresenceCacheMaxEntries {
			// 仍然超限：整表丢弃。缓存只是加速，丢掉只会多几次探测请求。
			c.entries = make(map[string]nodePresenceEntry, nodePresenceCacheMaxEntries)
		}
	}
	c.entries[key] = nodePresenceEntry{online: online, expiresAt: now.Add(c.ttl)}
}

// purgeExpiredLocked 清掉所有已过期条目；调用方必须持锁。
func (c *nodePresenceCache) purgeExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
}

// size 返回当前条目数（含尚未被惰性删除的过期条目），仅供用例断言。
func (c *nodePresenceCache) size() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
