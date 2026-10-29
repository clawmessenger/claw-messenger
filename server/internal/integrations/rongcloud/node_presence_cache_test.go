package rongcloud

// 本文件守两件事：
//  1. presence 探测并发化之后**候选顺序不变**（顺序是「没给 node_ids 时排序截断」
//     和角色↔设备分配的前提，随机化会让同一份配置两次推荐给出不同结果）；
//  2. 短 TTL 缓存真的把同一轮里的重复探测合并掉了，而且过期之后立刻重新问融云
//     —— 缓存过久会让刚上线的设备长时间看不到（这正是这次要修的「转圈很久」的
//     反面代价，必须钉死）。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// countingPresenceServer 造一个回答 /user/checkOnline.json 的融云假服务，并统计
// 真实被调用的次数（缓存是否生效全靠这个计数）。
func countingPresenceServer(t *testing.T, status string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/checkOnline.json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":200,"status":%q}`, status)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// fullRoleCaps 是贡献角色所需的全部能力位（roundtableRequiredRoleCaps）。
const fullRoleCaps = `["discussion_participant","discussion_roundtable","discussion_model_routing"]`

// partialRoleCaps 少一个 discussion_model_routing：能力不全，不该进候选池。
const partialRoleCaps = `["discussion_participant","discussion_roundtable"]`

func presenceFixtureNodes() []db.RongcloudNode {
	return []db.RongcloudNode{
		{NodeID: "node-a", DeployStatus: "online", Capabilities: []byte(fullRoleCaps)},
		{NodeID: "node-b", DeployStatus: "offline", Capabilities: []byte(fullRoleCaps)},
		{NodeID: "node-c", DeployStatus: "online", Capabilities: []byte(partialRoleCaps)},
		{NodeID: "node-host", DeployStatus: "online", Capabilities: []byte(fullRoleCaps)},
		{NodeID: "", DeployStatus: "online", Capabilities: []byte(fullRoleCaps)},
		{NodeID: "node-d", DeployStatus: "online", Capabilities: []byte(fullRoleCaps)},
	}
}

// TestOwnedAndEligibleNodesConcurrentProbeKeepsOrder 并发探测不影响归属/可用
// 集合的内容，并且只对「真的需要探测」的设备发请求。
func TestOwnedAndEligibleNodesConcurrentProbeKeepsOrder(t *testing.T) {
	svc := newRoundtableCreationService(nil, nil)
	svc.nodeOwner = func(context.Context, string) ([]db.RongcloudNode, error) {
		return presenceFixtureNodes(), nil
	}

	owned, eligible, stats := svc.ownedAndEligibleNodesWithStats(
		context.Background(), "user-1", roundtableHostConfig{nodeID: "node-host"})

	// 归属：所有非空 NodeID 都算「我的设备」，与能力位/在线无关。
	wantOwned := []string{"node-a", "node-b", "node-c", "node-host", "node-d"}
	for _, id := range wantOwned {
		if !owned[id] {
			t.Errorf("owned[%q] = false, want true", id)
		}
	}
	if len(owned) != len(wantOwned) {
		t.Errorf("len(owned) = %d, want %d (空 NodeID 必须被跳过)", len(owned), len(wantOwned))
	}

	// 可用：在线 + 能力齐备 + 不是 host。node-b 离线、node-c 能力不全、
	// node-host 是主持自己。
	if len(eligible) != 2 {
		t.Fatalf("len(eligible) = %d, want 2 (node-a/node-d): %v", len(eligible), eligible)
	}
	for _, id := range []string{"node-a", "node-d"} {
		if _, ok := eligible[id]; !ok {
			t.Errorf("eligible 缺 %q", id)
		}
	}
	for _, id := range []string{"node-b", "node-c", "node-host"} {
		if _, ok := eligible[id]; ok {
			t.Errorf("eligible 不该含 %q", id)
		}
	}

	// 统计只数真正探测过的设备（能力不全的与 host 自己不该白问一次）。
	if stats.nodes != 3 {
		t.Errorf("stats.nodes = %d, want 3 (node-a/node-b/node-d)", stats.nodes)
	}
	if stats.online != 2 {
		t.Errorf("stats.online = %d, want 2", stats.online)
	}
}

// TestRoleCandidateNodesPreservesRequestedOrder 前端给了 node_ids 时，候选顺序
// 必须与传入顺序一致 —— 并发探测写结果切片按索引定位就是为了这个。
func TestRoleCandidateNodesPreservesRequestedOrder(t *testing.T) {
	svc := newRoundtableCreationService(nil, nil)
	svc.nodeOwner = func(context.Context, string) ([]db.RongcloudNode, error) {
		return presenceFixtureNodes(), nil
	}

	ids, code, stats := svc.roleCandidateNodesForWithStats(
		context.Background(), "user-1", roundtableHostConfig{nodeID: "node-host"},
		[]string{"node-d", "node-a"}, true)
	if code != "" {
		t.Fatalf("roleCandidateNodesForWithStats code = %q, want success", code)
	}
	want := []string{"node-d", "node-a"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v（顺序必须与请求一致）", ids, want)
		}
	}
	if stats.nodes == 0 {
		t.Error("stats.nodes = 0, want > 0（候选解析必须回传 presence 耗时统计）")
	}
}

// TestRoleCandidateNodesForWrapperStillWorks 兼容入口（三返回值 → 两返回值）
// 不能被这次改造改坏：其它调用方还按两返回值用。
func TestRoleCandidateNodesForWrapperStillWorks(t *testing.T) {
	svc := newRoundtableCreationService(nil, nil)
	svc.nodeOwner = func(context.Context, string) ([]db.RongcloudNode, error) {
		return presenceFixtureNodes(), nil
	}
	ids, code := svc.roleCandidateNodesFor(
		context.Background(), "user-1", roundtableHostConfig{nodeID: "node-host"}, nil, false)
	if code != "" {
		t.Fatalf("code = %q, want success", code)
	}
	// 没给 node_ids → 集合来自 map，必须排序（sort.Strings），否则两次推荐不一致。
	if len(ids) != 2 || ids[0] != "node-a" || ids[1] != "node-d" {
		t.Fatalf("ids = %v, want [node-a node-d]", ids)
	}
}

// TestNodeIsOnlineCachesBrieflyAndRevalidatesAfterTTL 是缓存的核心用例：
// TTL 内重复问同一台设备只打一次融云；TTL 一过必须重新问（刚上线的设备不能
// 被缓存挡在门外）。这里用可控时钟推进，不 sleep。
func TestNodeIsOnlineCachesBrieflyAndRevalidatesAfterTTL(t *testing.T) {
	srv, hits := countingPresenceServer(t, "1")
	client := newRongCloudAPIClient("k", "s", srv.URL, srv.Client(), testLogger())

	clock := time.Unix(1_700_000_000, 0)
	cache := newNodePresenceCache(time.Second)
	cache.now = func() time.Time { return clock }
	client.presence = cache

	node := db.RongcloudNode{NodeID: "node-a", DeployStatus: "offline", RongcloudUserID: "rc_cache_a"}
	ctx := context.Background()

	if !nodeIsOnline(ctx, client, node) {
		t.Fatal("第一次查询应为在线")
	}
	if !nodeIsOnline(ctx, client, node) {
		t.Fatal("TTL 内第二次查询仍应为在线")
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("融云被调用 %d 次, want 1（TTL 内必须命中缓存）", got)
	}

	// 时钟推进过 TTL：缓存过期，必须重新问。
	clock = clock.Add(2 * time.Second)
	if !nodeIsOnline(ctx, client, node) {
		t.Fatal("TTL 过期后重新查询应为在线")
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Fatalf("融云被调用 %d 次, want 2（TTL 过期必须重新校验）", got)
	}
}

// TestNodeIsOnlineDoesNotCacheFailures presence 查询失败（fail closed）不能落
// 缓存：否则一次网络抖动会让那台设备在 TTL 内被稳定判成离线。
func TestNodeIsOnlineDoesNotCacheFailures(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":401,"errorMessage":"signature failed"}`)
	}))
	t.Cleanup(srv.Close)
	client := newRongCloudAPIClient("k", "s", srv.URL, srv.Client(), testLogger())
	node := db.RongcloudNode{NodeID: "node-a", DeployStatus: "online", RongcloudUserID: "rc_fail_a"}
	ctx := context.Background()

	if nodeIsOnline(ctx, client, node) {
		t.Fatal("presence 查询失败必须按离线处理")
	}
	if nodeIsOnline(ctx, client, node) {
		t.Fatal("presence 查询失败必须按离线处理")
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("融云被调用 %d 次, want 2（失败结果不缓存）", got)
	}
}

// TestNodePresenceCacheBoundsAndNilSafety 缓存自身的边界行为：过期条目在写入
// 超限时被清掉，nil 缓存不 panic（用例里 client.presence 可以是 nil）。
func TestNodePresenceCacheBoundsAndNilSafety(t *testing.T) {
	cache := newNodePresenceCache(time.Second)
	clock := time.Unix(1_700_000_000, 0)
	cache.now = func() time.Time { return clock }

	cache.store("a", true)
	if online, hit := cache.lookup("a"); !hit || !online {
		t.Fatalf("lookup(a) = (%v,%v), want (true,true)", online, hit)
	}
	clock = clock.Add(2 * time.Second)
	if _, hit := cache.lookup("a"); hit {
		t.Fatal("过期条目必须按未命中处理")
	}
	if cache.size() != 0 {
		t.Fatalf("size = %d, want 0（过期条目应被惰性删除）", cache.size())
	}

	// 超限时先清过期，再兜底整表丢弃 —— 不允许无界增长。
	clock = clock.Add(time.Hour)
	for i := 0; i < nodePresenceCacheMaxEntries+8; i++ {
		cache.store(fmt.Sprintf("key-%d", i), true)
	}
	if cache.size() > nodePresenceCacheMaxEntries {
		t.Fatalf("size = %d, want <= %d", cache.size(), nodePresenceCacheMaxEntries)
	}

	var nilCache *nodePresenceCache
	if _, hit := nilCache.lookup("a"); hit {
		t.Fatal("nil 缓存必须当作未命中")
	}
	nilCache.store("a", true) // 不能 panic
	if nilCache.size() != 0 {
		t.Fatal("nil 缓存 size 必须为 0")
	}
}
