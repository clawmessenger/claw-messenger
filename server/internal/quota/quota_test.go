package quota

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 与 rongcloud 包的 DB 测试同一约定：DATABASE_URL 缺省指向本地开发库，连不上就 Skip。

func setupQuotaFixture(t *testing.T) (*Service, *pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("skipping: cannot build pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skipping: db unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	userID := fmt.Sprintf("quota_test_%p", t)
	_, err = pool.Exec(ctx, `
		INSERT INTO claw_im_users (user_id, username, nickname, password_hash, status)
		VALUES ($1, $2, $3, 'x', 'active') ON CONFLICT DO NOTHING`,
		userID, "owner-"+userID, "Quota Test")
	if err != nil {
		t.Fatalf("seed claw user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM claw_im_user_quota WHERE user_id=$1", userID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM claw_im_users WHERE user_id=$1", userID)
	})
	return New(pool), pool, userID
}

// TestResolveDefaults 验证无覆盖无套餐分配时落到内置默认（3/5/1）。
func TestResolveDefaults(t *testing.T) {
	svc, _, userID := setupQuotaFixture(t)
	limits, err := svc.Resolve(context.Background(), userID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if limits.DiscussionLimit != DefaultDiscussionLimit || limits.GroupLimit != DefaultGroupLimit || limits.ChatroomLimit != DefaultChatroomLimit {
		t.Fatalf("expected builtin defaults 3/5/1, got %+v", limits)
	}
}

// TestResolveOverridePriority 验证覆盖 > 默认，且 NULL 列跟随默认：-1/0/N/NULL 全组合。
func TestResolveOverridePriority(t *testing.T) {
	svc, _, userID := setupQuotaFixture(t)
	ctx := context.Background()
	unlimited, forbidden, seven := -1, 0, 7
	if err := svc.UpsertOverride(ctx, userID, OverrideLimits{
		DiscussionLimit: &unlimited,
		GroupLimit:      &forbidden,
		ChatroomLimit:   &seven,
	}, "adm-test"); err != nil {
		t.Fatalf("upsert override: %v", err)
	}
	limits, err := svc.Resolve(ctx, userID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if limits.DiscussionLimit != -1 || limits.GroupLimit != 0 || limits.ChatroomLimit != 7 {
		t.Fatalf("expected override -1/0/7, got %+v", limits)
	}
	// 清掉 chatroom 覆盖（NULL）→ 该类回到默认。
	if err := svc.UpsertOverride(ctx, userID, OverrideLimits{
		DiscussionLimit: &unlimited,
		GroupLimit:      &forbidden,
		ChatroomLimit:   nil,
	}, "adm-test"); err != nil {
		t.Fatalf("upsert override: %v", err)
	}
	limits, err = svc.Resolve(ctx, userID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if limits.ChatroomLimit != DefaultChatroomLimit {
		t.Fatalf("expected chatroom back to default %d, got %d", DefaultChatroomLimit, limits.ChatroomLimit)
	}
}

// TestCheckZeroLimitForbids 验证 limit=0 立即拦截、limit=-1 直接放行。
func TestCheckZeroAndUnlimited(t *testing.T) {
	svc, _, userID := setupQuotaFixture(t)
	ctx := context.Background()
	// 默认 chatroom_limit=1、用量 0 → 放行。
	if qErr := svc.Check(ctx, userID, KindChatroom); qErr != nil {
		t.Fatalf("expected pass, got %v", qErr)
	}
	forbidden, unlimited := 0, -1
	if err := svc.UpsertOverride(ctx, userID, OverrideLimits{
		GroupLimit:      &forbidden,
		DiscussionLimit: &unlimited,
	}, "adm-test"); err != nil {
		t.Fatalf("upsert override: %v", err)
	}
	qErr := svc.Check(ctx, userID, KindGroup)
	if qErr == nil || qErr.Kind != string(KindGroup) || qErr.Limit != 0 {
		t.Fatalf("expected QUOTA_EXCEEDED{group,0,0}, got %+v", qErr)
	}
	if qErr := svc.Check(ctx, userID, KindDiscussion); qErr != nil {
		t.Fatalf("expected unlimited pass, got %v", qErr)
	}
}

// TestCheckCountsAndBlocksAtLimit 验证「数用量 + 比对」：给房主塞满活跃讨论房后
// Check 必须拦截，且错误里带准确的 limit/used。
func TestCheckCountsAndBlocksAtLimit(t *testing.T) {
	svc, pool, userID := setupQuotaFixture(t)
	ctx := context.Background()
	// 房主镜像行 + N 个 active 讨论房（N = 默认上限 3）。
	var mirrorID string
	err := pool.QueryRow(ctx, `
		INSERT INTO rongcloud_user (workspace_id, rongcloud_user_id, name, node_type)
		VALUES ((SELECT id FROM workspace ORDER BY created_at LIMIT 1), $1, 'm', 'human')
		ON CONFLICT (rongcloud_user_id) DO UPDATE SET name = EXCLUDED.name
		RETURNING id::text`, userID).Scan(&mirrorID)
	if err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	roomIDs := make([]string, 0, DefaultDiscussionLimit)
	for i := 0; i < DefaultDiscussionLimit; i++ {
		roomID := fmt.Sprintf("quota_room_%d_%s", i, userID)
		roomIDs = append(roomIDs, roomID)
		if _, err := pool.Exec(ctx, `
			INSERT INTO rongcloud_chatroom (workspace_id, rongcloud_chatroom_id, owner_user_id, conversation_kind, status, name)
			VALUES ((SELECT id FROM workspace ORDER BY created_at LIMIT 1), $1, $2, 'discussion', 'active', 'r')`,
			roomID, mirrorID); err != nil {
			t.Fatalf("seed chatroom: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM rongcloud_chatroom WHERE rongcloud_chatroom_id LIKE 'quota_room_%'")
		_, _ = pool.Exec(context.Background(), "DELETE FROM rongcloud_user WHERE rongcloud_user_id=$1", userID)
	})
	qErr := svc.Check(ctx, userID, KindDiscussion)
	if qErr == nil {
		t.Fatalf("expected QUOTA_EXCEEDED at limit %d", DefaultDiscussionLimit)
	}
	if qErr.Limit != DefaultDiscussionLimit || qErr.Used != DefaultDiscussionLimit {
		t.Fatalf("expected limit/used %d/%d, got %+v", DefaultDiscussionLimit, DefaultDiscussionLimit, qErr)
	}
	// 讨论组不占 chatroom 额度（不双计）：chatroom 用量应为 0。
	usage, err := svc.Usage(ctx, userID)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage.Chatroom.Used != 0 || usage.Discussion.Used != DefaultDiscussionLimit {
		t.Fatalf("discussion must not double-count as chatroom: %+v", usage)
	}
	// 删房释放配额：一间改 deleted 后 Check 放行。
	if _, err := pool.Exec(ctx, "UPDATE rongcloud_chatroom SET status='deleted' WHERE rongcloud_chatroom_id=$1", roomIDs[0]); err != nil {
		t.Fatalf("soft delete room: %v", err)
	}
	if qErr := svc.Check(ctx, userID, KindDiscussion); qErr != nil {
		t.Fatalf("expected pass after room release, got %v", qErr)
	}
}

// TestCheckConcurrentNoOversell 验证 advisory lock 下并发 Check 不超发：
// 上限 1、已有用量 1 时，8 个并发 Check 必须全部被拦。
func TestCheckConcurrentNoOversell(t *testing.T) {
	svc, pool, userID := setupQuotaFixture(t)
	ctx := context.Background()
	one := 1
	if err := svc.UpsertOverride(ctx, userID, OverrideLimits{GroupLimit: &one}, "adm-test"); err != nil {
		t.Fatalf("upsert override: %v", err)
	}
	groupID := "quota_group_" + userID
	if _, err := pool.Exec(ctx, `
		INSERT INTO rongcloud_group (group_id, group_name, owner_id) VALUES ($1, 'g', $2)
		ON CONFLICT (group_id) DO NOTHING`, groupID, userID); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM rongcloud_group WHERE group_id=$1", groupID)
	})
	var wg sync.WaitGroup
	blocked := make(chan *Error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			blocked <- svc.Check(ctx, userID, KindGroup)
		}()
	}
	wg.Wait()
	close(blocked)
	for qErr := range blocked {
		if qErr == nil {
			t.Fatalf("concurrent check oversold quota")
		}
	}
}
