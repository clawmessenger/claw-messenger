package rongcloud

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// actorTestPool 建一个指向测试库的连接池；库不可达则 skip（与
// chatroom_commands_test.go 的约定一致）。
func actorTestPool(t *testing.T) *pgxpool.Pool {
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
	return pool
}

// seedClawIMUser 造一条 C 端真人账号行（注册链路只写这张表，不写
// rongcloud_user 镜像）。清理按 user_id 精确删除。
func seedClawIMUser(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO claw_im_users (user_id, username, nickname, password_hash, status)
		 VALUES ($1, $2, $3, 'x', 'active')`,
		userID, "u-"+userID, "小酒-"+userID,
	); err != nil {
		t.Fatalf("seed claw user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM claw_im_users WHERE user_id = $1`, userID)
	})
}

// ensureRongCloudInstallation 保证库里至少有一条 active 的 rongcloud 渠道安装
// （镜像补建要拿它的 workspace 归属）。
func ensureRongCloudInstallation(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM channel_installation
		 WHERE status = 'active' AND channel_type = 'rongcloud'
		 ORDER BY created_at ASC LIMIT 1`,
	).Scan(&id); err == nil {
		return
	}
	wsID := seedWorkspaceRow(t, pool)
	ref := pgtype.UUID{Bytes: [16]byte{0xab, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}, Valid: true}
	if err := pool.QueryRow(ctx,
		`INSERT INTO channel_installation (workspace_id, agent_id, channel_type, config, status, installer_user_id)
		 VALUES ($1, $2, 'rongcloud', '{}'::jsonb, 'active', $2)
		 RETURNING id`,
		wsID, ref,
	).Scan(&id); err != nil {
		t.Fatalf("seed rongcloud installation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM channel_installation WHERE id = $1`, id)
	})
}

// TestChatroomResolveActorPrefersRongCloudUser 钉住快路径：发送者已在
// rongcloud_user 里（节点/安装账号）时直接用该行，不去查 claw_im_users、
// 也不重复补镜像。
func TestChatroomResolveActorPrefersRongCloudUser(t *testing.T) {
	pool := actorTestPool(t)
	queries := db.New(pool)
	wsID := seedWorkspaceRow(t, pool)
	rcID := "actor-rc-" + uniqueID("t")
	owner := seedUser(t, queries, wsID, rcID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM rongcloud_user WHERE rongcloud_user_id = $1`, rcID)
	})

	h := &systemHandler{queries: queries, nodeID: "rc_test_sys", logger: testLogger()}
	got, err := h.resolveActor(context.Background(), rcID)
	if err != nil {
		t.Fatalf("resolveActor: %v", err)
	}
	if got.ID != owner.ID {
		t.Errorf("actor.ID = %v, want the pre-existing row %v", got.ID, owner.ID)
	}
	if got.RongcloudUserID != rcID {
		t.Errorf("actor.RongcloudUserID = %q, want %q", got.RongcloudUserID, rcID)
	}
}

// TestChatroomResolveActorFallsBackToClawUser 是本文件的核心回归：C 端真人的
// 融云 id 只存在于 claw_im_users，chatroom 域的 resolveActor 必须兜底并幂等
// 补建 rongcloud_user 镜像行 —— 否则真人发来的 chatroom 命令会一律 401
// sender not recognized（前端表现为聊天室列表加载失败、房间写动作全不可用）。
func TestChatroomResolveActorFallsBackToClawUser(t *testing.T) {
	pool := actorTestPool(t)
	queries := db.New(pool)
	ensureRongCloudInstallation(t, pool)

	clawID := "clawonly-" + uniqueID("t")
	seedClawIMUser(t, pool, clawID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM rongcloud_user WHERE rongcloud_user_id = $1`, clawID)
	})

	// 前提：镜像表里确实没有这号人，走的必然是兜底分支。
	if _, err := queries.GetRongCloudUserByRongCloudID(context.Background(), clawID); err == nil {
		t.Fatalf("precondition violated: %s already mirrored", clawID)
	}

	h := &systemHandler{queries: queries, nodeID: "rc_test_sys", logger: testLogger()}
	got, err := h.resolveActor(context.Background(), clawID)
	if err != nil {
		t.Fatalf("resolveActor fell through for a claw-only sender: %v", err)
	}
	if got.RongcloudUserID != clawID {
		t.Errorf("actor.RongcloudUserID = %q, want %q", got.RongcloudUserID, clawID)
	}
	if got.NodeType != "human" {
		t.Errorf("actor.NodeType = %q, want human", got.NodeType)
	}
	if !got.ID.Valid {
		t.Fatal("actor.ID is not a valid UUID; chatroom write actions need it as an FK")
	}

	// 幂等：第二次解析（以及任何并发路径）必须复用同一镜像行，不再插入。
	again, err := h.resolveActor(context.Background(), clawID)
	if err != nil {
		t.Fatalf("second resolveActor: %v", err)
	}
	if again.ID != got.ID {
		t.Errorf("mirror is not idempotent: %v then %v", got.ID, again.ID)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rongcloud_user WHERE rongcloud_user_id = $1`, clawID).Scan(&n); err != nil {
		t.Fatalf("count mirrors: %v", err)
	}
	if n != 1 {
		t.Errorf("mirror row count = %d, want 1", n)
	}
}

// TestChatroomResolveActorRejectsUnknownSender 未知发送者仍然报错（上层翻成
// 401 sender not recognized），不能被兜底逻辑吞掉。
func TestChatroomResolveActorRejectsUnknownSender(t *testing.T) {
	pool := actorTestPool(t)
	h := &systemHandler{queries: db.New(pool), nodeID: "rc_test_sys", logger: testLogger()}

	if _, err := h.resolveActor(context.Background(), "nobody-"+uniqueID("t")); err == nil {
		t.Fatal("expected error for an unknown sender")
	}
	if _, err := h.resolveActor(context.Background(), ""); err == nil {
		t.Fatal("expected error for an empty sender")
	}
}
