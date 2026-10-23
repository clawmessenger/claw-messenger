package rongcloud

// friend_device_owner_claim_test.go 钉住 getDeviceOwner 的 O(1) 读路径
// （migrations/600 的 rongcloud_node.claimed_by_user_id）。
//
// 核心契约：归属的真相仍然是融云好友边，但读路径先看本地投影列 ——
//   - 列有值 → 只查一次库，**一次 /friend/get.json 都不能发**（这是本项的存在意义：
//     旧实现是 N 个账号串行各 10s，客户端 180s 先超时）；
//   - 列为空 → 降级扫一遍好友边，并把结果惰性回填（存量数据自愈）；
//   - 回填只写 NULL 行（WHERE ... IS NULL），绝不覆盖写路径写下的新值；
//   - 并发 lookup 只建一次表（10 个 goroutine 不是 10 × N 次调用）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// claimStoreDB 是 claimed_by_user_id 读写的内存实现：按 SQL 形状路由到
// 「声明 / 回填 / 清空」三种语义，其余一切报错，避免测试悄悄绕过真实语句。
type claimStoreDB struct {
	mu     sync.Mutex
	claims map[string]*string // rongcloud_user_id → 认领者（nil = NULL）
	users  []string           // SELECT user_id FROM claw_im_users 的结果
	execs  int
	// backfills 只数「回填」语义的 EXEC。断言它而不是只看列的值，是为了
	// 对准闸门本身：万一以后有人把闸门挪到 backfill 之后、再靠别的机制把
	// 列清回去，结果断言仍绿，这条过程断言会红。
	backfills int
}

func (d *claimStoreDB) claim(nodeRCID string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	owner, ok := d.claims[nodeRCID]
	if !ok || owner == nil {
		return "", ok
	}
	return *owner, ok
}

func (d *claimStoreDB) execCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.execs
}

func (d *claimStoreDB) backfillCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.backfills
}

func (d *claimStoreDB) Exec(_ context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.execs++
	switch {
	case strings.Contains(sql, "SET claimed_by_user_id = NULL"):
		node, _ := args[0].(string)
		owner, _ := args[1].(string)
		if cur, ok := d.claims[node]; ok && cur != nil && (owner == "" || *cur == owner) {
			d.claims[node] = nil
		}
	case strings.Contains(sql, "AND claimed_by_user_id IS NULL"):
		d.backfills++
		owner, _ := args[0].(string)
		node, _ := args[1].(string)
		if cur, ok := d.claims[node]; ok && cur == nil {
			value := owner
			d.claims[node] = &value
		}
	case strings.Contains(sql, "SET claimed_by_user_id = $1 WHERE rongcloud_user_id"):
		owner, _ := args[0].(string)
		node, _ := args[1].(string)
		if _, ok := d.claims[node]; ok {
			value := owner
			d.claims[node] = &value
		}
	default:
		return pgconn.NewCommandTag(""), fmt.Errorf("claimStoreDB: unexpected exec: %s", sql)
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (d *claimStoreDB) Query(_ context.Context, sql string, _ ...interface{}) (pgx.Rows, error) {
	if !strings.Contains(sql, "FROM claw_im_users") {
		return nil, fmt.Errorf("claimStoreDB: unexpected query: %s", sql)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return &claimRows{users: append([]string(nil), d.users...)}, nil
}

func (d *claimStoreDB) QueryRow(_ context.Context, sql string, args ...interface{}) pgx.Row {
	if !strings.Contains(sql, "SELECT claimed_by_user_id") {
		return claimRow{err: fmt.Errorf("claimStoreDB: unexpected query row: %s", sql)}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	node, _ := args[0].(string)
	owner, ok := d.claims[node]
	if !ok {
		return claimRow{err: pgx.ErrNoRows}
	}
	return claimRow{owner: owner}
}

type claimRow struct {
	owner *string
	err   error
}

func (r claimRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	target, ok := dest[0].(*pgtype.Text)
	if !ok {
		return fmt.Errorf("claimRow: unsupported dest %T", dest[0])
	}
	if r.owner == nil {
		*target = pgtype.Text{}
		return nil
	}
	*target = pgtype.Text{String: *r.owner, Valid: true}
	return nil
}

type claimRows struct {
	users []string
	index int
}

func (r *claimRows) Close()                                       {}
func (r *claimRows) Err() error                                   { return nil }
func (r *claimRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT") }
func (r *claimRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *claimRows) RawValues() [][]byte                          { return nil }
func (r *claimRows) Conn() *pgx.Conn                              { return nil }
func (r *claimRows) TypeMap() *pgtype.Map                         { return nil }
func (r *claimRows) Next() bool {
	if r.index >= len(r.users) {
		return false
	}
	r.index++
	return true
}
func (r *claimRows) Values() ([]interface{}, error) { return nil, errors.New("not implemented") }
func (r *claimRows) Scan(dest ...interface{}) error {
	target, ok := dest[0].(*string)
	if !ok {
		return fmt.Errorf("claimRows: unsupported dest %T", dest[0])
	}
	*target = r.users[r.index-1]
	return nil
}

// 列有值 → 一次 SELECT 命中，FriendIDs 调用次数必须为 0。这是本项最核心的断言：
// 任何一次多余的融云调用都意味着热路径又回到了 O(N)。
func TestFriendDeviceOwnerReadsClaimColumnWithoutTouchingRongCloud(t *testing.T) {
	owner := "user-a"
	stub := &claimFriendServer{friends: map[string][]string{"user-a": {"rc_node_1"}}}
	srv := newClaimFriendServer(t, stub)
	defer srv.Close()
	d := &claimStoreDB{claims: map[string]*string{"rc_node_1": &owner}, users: []string{"user-a", "user-b"}}
	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	store := NewFriendStore(d, client, testLogger())

	got, found, err := store.NodeOwner(context.Background(), "rc_node_1")
	if err != nil {
		t.Fatalf("NodeOwner: %v", err)
	}
	if !found || got != owner {
		t.Fatalf("NodeOwner = (%q, %v), want (%q, true)", got, found, owner)
	}
	if calls := d.execCount(); calls != 0 {
		t.Errorf("column hit must not write anything, got %d execs", calls)
	}
	// 列命中时连 /friend/get.json 都不该发。
	if calls := stub.callCount(); calls != 0 {
		t.Fatalf("RongCloud friend calls = %d, want 0 when the claim column is populated", calls)
	}
}

// 列为空 → 降级扫好友边，并把解析结果写回列（存量自愈）。
func TestFriendDeviceOwnerFallsBackAndBackfillsTheColumn(t *testing.T) {
	stub := &claimFriendServer{friends: map[string][]string{
		"user-a": {"rc_node_1", "rc_node_a2"},
		"user-b": {"rc_node_b1"},
	}}
	srv := newClaimFriendServer(t, stub)
	defer srv.Close()
	d := &claimStoreDB{claims: map[string]*string{"rc_node_1": nil}, users: []string{"user-a", "user-b"}}
	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	store := NewFriendStore(d, client, testLogger())

	got, found, err := store.NodeOwner(context.Background(), "rc_node_1")
	if err != nil {
		t.Fatalf("NodeOwner: %v", err)
	}
	if !found || got != "user-a" {
		t.Fatalf("NodeOwner = (%q, %v), want (user-a, true)", got, found)
	}
	if calls := stub.callCount(); calls != 2 {
		t.Errorf("friend calls = %d, want one per active account (2)", calls)
	}
	if owner, ok := d.claim("rc_node_1"); !ok || owner != "user-a" {
		t.Fatalf("column after fallback = (%q, exists %v), want user-a backfilled", owner, ok)
	}
	// 反证：backfillCount 会数到东西（否则 §2.7 那条「回填次数为 0」的
	// 过程断言可能是空断言）。
	if n := d.backfillCount(); n != 1 {
		t.Fatalf("backfill EXEC count = %d, want exactly 1 — the counter must not be vacuous", n)
	}

	// 回填之后第二次 lookup 必须走列（不再发融云请求）。
	before := stub.callCount()
	if _, _, err := store.NodeOwner(context.Background(), "rc_node_1"); err != nil {
		t.Fatalf("second NodeOwner: %v", err)
	}
	if calls := stub.callCount(); calls != before {
		t.Errorf("second lookup made %d more friend calls, want 0", calls-before)
	}
}

// 惰性回填不能覆盖写路径写下的新值（WHERE ... IS NULL 的意义）。
func TestNodeClaimBackfillDoesNotOverwriteAnExistingClaim(t *testing.T) {
	existing := "user-b"
	d := &claimStoreDB{claims: map[string]*string{"rc_node_1": &existing}, users: []string{"user-a"}}
	o := &friendNodeOwners{dbtx: d, logger: testLogger()}

	o.backfill(context.Background(), "rc_node_1", "user-a")

	if owner, _ := d.claim("rc_node_1"); owner != "user-b" {
		t.Fatalf("column = %q, want the existing claim user-b to survive the backfill", owner)
	}
}

// P1：回填不得把「build 之前」解析出的 owner 写回列。
//
// 时序（确定性构造，不靠 sleep）：
//
//	列 NULL → lookup → build() 发出 /friend/get.json
//	         → 假融云在应答前回调 → 写路径 ClearNodeClaim（清列 + epoch+1）
//	         → build 带着解绑前的结果返回 → 回填
//
// 旧实现里 `WHERE claimed_by_user_id IS NULL` 此时仍然成立（列刚好被清成
// NULL），于是旧 owner 被写回列；而回填只补 NULL、不覆盖非 NULL，列从此
// 永不自愈 —— 「解绑后答旧 owner」从 ≤15s 变成**永久**。
func TestBackfillSkipsAStaleOwnerWhenAClaimWriteLandsMidBuild(t *testing.T) {
	ctx := context.Background()
	d := &claimStoreDB{claims: map[string]*string{"rc_node_1": nil}, users: []string{"user-a"}}
	svc := NewNodeService(nil, nil, nil, testLogger())
	svc.SetClaimDB(d)

	stub := &claimFriendServer{friends: map[string][]string{"user-a": {"rc_node_1"}}}
	var once sync.Once
	// build 期间发生解绑：撤好友边 + 清列（内部会 bumpClaimEpoch）。
	stub.onCall = func() {
		once.Do(func() {
			stub.mu.Lock()
			stub.friends["user-a"] = nil
			stub.mu.Unlock()
			if err := svc.ClearNodeClaim(ctx, "rc_node_1", "user-a"); err != nil {
				t.Errorf("ClearNodeClaim during build: %v", err)
			}
		})
	}
	srv := newClaimFriendServer(t, stub)
	defer srv.Close()
	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	store := NewFriendStore(d, client, testLogger())

	// 这一次读可能拿到 build 时的旧值（窗口 = build 耗时，已登记），此处不断言。
	if _, _, err := store.NodeOwner(ctx, "rc_node_1"); err != nil {
		t.Fatalf("NodeOwner during the race: %v", err)
	}

	// 核心断言（结果）：列必须还是 NULL，不能被解绑前的结果写回去。
	if owner, ok := d.claim("rc_node_1"); !ok || owner != "" {
		t.Fatalf("column after a mid-build unbind = (%q, exists %v), want NULL — a backfill must not persist a pre-unbind owner", owner, ok)
	}
	// 过程断言：这一轮 lookup 根本不该发出回填的 EXEC。只看列的值不够——
	// 若有人把闸门挪到 backfill 之后、再靠别的机制把列清回去，结果断言仍绿。
	if n := d.backfillCount(); n != 0 {
		t.Fatalf("backfill EXEC count = %d, want 0 — the gate must stop the write, not undo it", n)
	}

	// 后续读：build 期间有写 → 这份 map 的 epoch 已落后 → 不复用，重建。
	// 好友边已空，所以归属必须消失（不是「永久答旧 owner」）。
	before := stub.callCount()
	got, found, err := store.NodeOwner(ctx, "rc_node_1")
	if err != nil {
		t.Fatalf("NodeOwner after the race: %v", err)
	}
	if found {
		t.Fatalf("NodeOwner after the race = (%q, true), want not found — the stale map must not be served again", got)
	}
	if calls := stub.callCount(); calls == before {
		t.Fatalf("second lookup reused the stale map: friend calls stayed at %d, want a rebuild", before)
	}
	// 重建也解析不到 owner（边已撤），全程不应有任何回填。
	if n := d.backfillCount(); n != 0 {
		t.Fatalf("backfill EXEC count after rebuild = %d, want 0", n)
	}
}

// 10 个 goroutine 并发 lookup，FriendIDs 总次数必须 ≤ 账号数（不是 N×10）。
func TestFriendDeviceOwnerBuildsOnceUnderConcurrency(t *testing.T) {
	stub := &claimFriendServer{friends: map[string][]string{
		"user-a": {"rc_node_1"},
		"user-b": {"rc_node_b1"},
		"user-c": {"rc_node_c1"},
	}}
	srv := newClaimFriendServer(t, stub)
	defer srv.Close()
	d := &claimStoreDB{
		claims: map[string]*string{"rc_node_1": nil},
		users:  []string{"user-a", "user-b", "user-c"},
	}
	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	store := NewFriendStore(d, client, testLogger())

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner, found, err := store.NodeOwner(context.Background(), "rc_node_1")
			if err != nil || !found || owner != "user-a" {
				t.Errorf("concurrent NodeOwner = (%q, %v, %v)", owner, found, err)
			}
		}()
	}
	wg.Wait()

	if calls := stub.callCount(); calls > 3 {
		t.Fatalf("friend calls = %d for 3 accounts × 10 concurrent lookups, want ≤ 3 (one build)", calls)
	}
}

// 手删好友（写路径 ⑤）只能清「自己的那条边」，删别人的边不能抹掉真实归属。
func TestReleaseNodeClaimOnlyClearsTheCallersOwnEdge(t *testing.T) {
	ctx := context.Background()
	owner := "user-a"
	d := &claimStoreDB{claims: map[string]*string{"rc_node_1": &owner}}
	o := &friendNodeOwners{dbtx: d, logger: testLogger()}

	if err := o.release(ctx, "rc_node_1", "user-b"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got, _ := d.claim("rc_node_1"); got != "user-a" {
		t.Fatalf("claim = %q, want user-a untouched by another account's delete", got)
	}

	if err := o.release(ctx, "rc_node_1", "user-a"); err != nil {
		t.Fatalf("release(owner): %v", err)
	}
	if got, ok := d.claim("rc_node_1"); ok && got != "" {
		t.Fatalf("claim = %q, want cleared by its owner", got)
	}
}

// 解绑（写路径 ③/④）之后**立刻**查归属，不能再答旧 owner。
//
// 这条同时是 claimEpoch 的变异护栏：把 ClearNodeClaim 里那行
// bumpClaimEpoch() 注释掉，列被清空 → 读路径落回 15s map 缓存 → 依然答
// user-a，测试重红。也就是说它证明的不是「加了失效调用」，而是「窗口真的关了」。
func TestUnbindDoesNotServeTheCachedOwner(t *testing.T) {
	ctx := context.Background()
	stub := &claimFriendServer{friends: map[string][]string{"user-a": {"rc_node_1"}}}
	srv := newClaimFriendServer(t, stub)
	defer srv.Close()
	d := &claimStoreDB{claims: map[string]*string{"rc_node_1": nil}, users: []string{"user-a"}}
	client := newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger())
	store := NewFriendStore(d, client, testLogger())
	svc := NewNodeService(nil, nil, nil, testLogger())
	svc.SetClaimDB(d)

	// 1. 先让读路径建一次表（缓存里有 rc_node_1 → user-a），并把结果回填进列。
	if got, found, err := store.NodeOwner(ctx, "rc_node_1"); err != nil || !found || got != "user-a" {
		t.Fatalf("initial NodeOwner = (%q, %v, %v), want (user-a, true, nil)", got, found, err)
	}

	// 2. 解绑：好友边消失 + 写路径清列（内部会 bumpClaimEpoch）。
	stub.mu.Lock()
	stub.friends["user-a"] = nil
	stub.mu.Unlock()
	if err := svc.ClearNodeClaim(ctx, "rc_node_1", "user-a"); err != nil {
		t.Fatalf("ClearNodeClaim: %v", err)
	}

	// 3. 立刻再查：缓存必须已作废，答案来自重新反查（边没了 → 404）。
	got, found, err := store.NodeOwner(ctx, "rc_node_1")
	if err != nil {
		t.Fatalf("NodeOwner after unbind: %v", err)
	}
	if found {
		t.Fatalf("NodeOwner after unbind = (%q, true), want not found — the 15s map cache must be invalidated by the claim write", got)
	}
}

// NodeService 侧的写原语：没有注入 DBTX 时明确报「未配置」，注入后按 owner 匹配清空。
func TestNodeServiceClaimPrimitives(t *testing.T) {
	ctx := context.Background()
	svc := NewNodeService(nil, nil, nil, testLogger())

	if err := svc.SetNodeClaim(ctx, "rc_node_1", "user-a"); !errors.Is(err, ErrNodeClaimStoreUnavailable) {
		t.Fatalf("SetNodeClaim without a DBTX = %v, want ErrNodeClaimStoreUnavailable", err)
	}

	owner := "user-a"
	d := &claimStoreDB{claims: map[string]*string{"rc_node_1": &owner}}
	svc.SetClaimDB(d)

	if err := svc.SetNodeClaim(ctx, "rc_node_1", "user-b"); err != nil {
		t.Fatalf("SetNodeClaim: %v", err)
	}
	if got, _ := d.claim("rc_node_1"); got != "user-b" {
		t.Fatalf("claim = %q, want user-b", got)
	}

	if err := svc.ClearNodeClaim(ctx, "rc_node_1", "user-c"); err != nil {
		t.Fatalf("ClearNodeClaim(other): %v", err)
	}
	if got, _ := d.claim("rc_node_1"); got != "user-b" {
		t.Fatalf("claim = %q, want user-b kept when another account asks to clear", got)
	}

	if err := svc.ClearNodeClaim(ctx, "rc_node_1", ""); err != nil {
		t.Fatalf("ClearNodeClaim(unconditional): %v", err)
	}
	if got, _ := d.claim("rc_node_1"); got != "" {
		t.Fatalf("claim = %q, want cleared", got)
	}
}

// UnbindNode 会在删行之前清投影列。行删掉之后列自然不存在，这里断言的是
// 「解绑之后没有任何残留的认领」（真库；没有 DATABASE_URL 时跳过）。
func TestUnbindNodeLeavesNoClaimBehind(t *testing.T) {
	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("skipping: could not connect to database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skipping: database not reachable: %v", err)
	}
	t.Cleanup(pool.Close)

	var workspaceID, userID string
	if err := pool.QueryRow(ctx, `SELECT id FROM workspace LIMIT 1`).Scan(&workspaceID); err != nil {
		t.Skipf("skipping: no workspace row: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM "user" LIMIT 1`).Scan(&userID); err != nil {
		t.Skipf("skipping: no user row: %v", err)
	}

	nodeRC := fmt.Sprintf("rc_node_claimtest_%d", os.Getpid())
	nodeID := fmt.Sprintf("node_claimtest_%d", os.Getpid())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM rongcloud_node WHERE rongcloud_user_id = $1`, nodeRC)
	})

	if _, err := pool.Exec(ctx, `
		INSERT INTO rongcloud_node
			(workspace_id, owner_user_id, rongcloud_user_id, node_id, capabilities,
			 deploy_status, binding_version, machine_id, claimed_by_user_id)
		VALUES ($1, $2, $3, $4, '[]'::jsonb, 'offline', 1, 'claimtest', $5)`,
		workspaceID, userID, nodeRC, nodeID, "user-a"); err != nil {
		t.Fatalf("insert node: %v", err)
	}

	svc := NewNodeService(db.New(pool), nil, nil, testLogger())
	svc.SetClaimDB(pool)
	node, err := svc.GetNodeByNodeID(ctx, nodeID)
	if err != nil {
		t.Fatalf("GetNodeByNodeID: %v", err)
	}
	if err := svc.UnbindNode(ctx, node, "user-a"); err != nil {
		t.Fatalf("UnbindNode: %v", err)
	}

	var left int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM rongcloud_node WHERE rongcloud_user_id = $1`, nodeRC).Scan(&left); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if left != 0 {
		t.Fatalf("node rows left = %d, want the row deleted (and its claim with it)", left)
	}
}
