package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// main_test.go 钉住 -sync-portraits 的 dry-run 语义：默认只打印计划，一个字节都
// 不写；只有 -commit 才推送 + 落库。存量补齐一旦写错就是覆盖用户的头像，所以
// 「dry-run 零写」和「推送失败不落库」两条必须在单测里锁住。
//
// DB 不可达时 Skip（与 server 内 DB 用例同一约定）。

// forcedDryRunEnv 清掉会影响默认图决议的 env，保证断言与调用方 shell 无关。
func forcedDryRunEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAW_DEFAULT_PORTRAIT_URI", "")
	t.Setenv("CLAW_AI_NODE_PORTRAIT_URI", "")
	t.Setenv("MULTICA_PUBLIC_URL", "")
}

func backfillTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skipping: cannot build pool: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skipping: db unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedClawIMUser 落一行 claw_im_users；portrait 为空串时写 NULL（线上两种都有）。
func seedClawIMUser(t *testing.T, pool *pgxpool.Pool, userID, portrait string) {
	t.Helper()
	var value any
	if portrait != "" {
		value = portrait
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO claw_im_users (user_id, username, nickname, portrait_uri, password_hash, status)
		VALUES ($1, $2, $3, $4, 'x', 'active')`,
		userID, "u-"+userID, "N "+userID, value)
	if err != nil {
		t.Fatalf("seed claw_im_users %s: %v", userID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM claw_im_users WHERE user_id = $1`, userID)
	})
}

func readClawPortrait(t *testing.T, pool *pgxpool.Pool, userID string) string {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(portrait_uri, '') FROM claw_im_users WHERE user_id = $1`, userID,
	).Scan(&got); err != nil {
		t.Fatalf("read portrait of %s: %v", userID, err)
	}
	return got
}

// backfillPortraitFixture 铺三条典型真人记录：空、旧默认图、用户自定义。
type backfillPortraitFixture struct {
	pool     *pgxpool.Pool
	svc      *rongcloud.NodeService
	ids      []string
	emptyID  string
	legacyID string
	customID string
	constant string
}

func newBackfillPortraitFixture(t *testing.T) *backfillPortraitFixture {
	t.Helper()
	forcedDryRunEnv(t)
	pool := backfillTestPool(t)
	queries := db.New(pool)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	f := &backfillPortraitFixture{
		pool:     pool,
		emptyID:  "bf-empty-" + suffix,
		legacyID: "bf-legacy-" + suffix,
		customID: "bf-custom-" + suffix,
		// 没有融云 client：dry-run 必须完全不碰它；commit 用例则借它验证
		// 「推送失败 → 不写库」。
		svc:      rongcloud.NewNodeService(queries, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))),
		constant: "https://cdn.example.com/uploads/me.png",
	}
	seedClawIMUser(t, pool, f.emptyID, "")
	seedClawIMUser(t, pool, f.legacyID, "https://newsradar.dreamdt.cn/im-test/api/static/claw/avatar-default.png")
	seedClawIMUser(t, pool, f.customID, f.constant)
	f.ids = []string{f.emptyID, f.legacyID, f.customID}
	return f
}

func TestSyncClawUserPortraitsDryRunWritesNothing(t *testing.T) {
	f := newBackfillPortraitFixture(t)

	summary, err := syncClawUserPortraits(context.Background(), f.pool, f.svc, f.ids, true)
	if err != nil {
		t.Fatalf("syncClawUserPortraits(dryRun): %v", err)
	}
	if summary.planned != 2 || summary.skipped != 1 {
		t.Fatalf("dry run summary = %+v, want planned=2 skipped=1", summary)
	}
	if summary.updated != 0 || summary.failed != 0 {
		t.Fatalf("dry run must not report writes: %+v", summary)
	}

	// 一个字节都没写：空仍是空、旧图仍是旧图、自定义原样。
	if got := readClawPortrait(t, f.pool, f.emptyID); got != "" {
		t.Fatalf("dry run wrote the empty row: %q", got)
	}
	if got := readClawPortrait(t, f.pool, f.legacyID); got != "https://newsradar.dreamdt.cn/im-test/api/static/claw/avatar-default.png" {
		t.Fatalf("dry run rewrote the legacy row: %q", got)
	}
	if got := readClawPortrait(t, f.pool, f.customID); got != f.constant {
		t.Fatalf("dry run touched the custom row: %q", got)
	}
}

// commit 分支：这里故意不给融云 client，PushUserPortrait 必然失败 —— 用来证明
// 「先推融云、成功才写库」，失败记录下次仍能命中重试。
func TestSyncClawUserPortraitsWritesOnlyAfterASuccessfulPush(t *testing.T) {
	f := newBackfillPortraitFixture(t)

	summary, err := syncClawUserPortraits(context.Background(), f.pool, f.svc, f.ids, false)
	if err != nil {
		t.Fatalf("syncClawUserPortraits(commit): %v", err)
	}
	if summary.failed != 2 || summary.updated != 0 {
		t.Fatalf("summary = %+v, want failed=2 updated=0", summary)
	}
	if got := readClawPortrait(t, f.pool, f.emptyID); got != "" {
		t.Fatalf("a failed push must leave the row untouched, got %q", got)
	}
	if got := readClawPortrait(t, f.pool, f.legacyID); got != "https://newsradar.dreamdt.cn/im-test/api/static/claw/avatar-default.png" {
		t.Fatalf("a failed push rewrote the legacy row: %q", got)
	}
	// 自定义头像连「计划」都不该出现。
	if got := readClawPortrait(t, f.pool, f.customID); got != f.constant {
		t.Fatalf("custom portrait changed: %q", got)
	}
}

// 已统一的行必须 skipped（幂等）：重复跑 CLI 不能每轮都打一次融云。
func TestSyncClawUserPortraitsSkipsRowsAlreadyOnTheDefault(t *testing.T) {
	forcedDryRunEnv(t)
	pool := backfillTestPool(t)
	userID := fmt.Sprintf("bf-unified-%d", time.Now().UnixNano())
	seedClawIMUser(t, pool, userID, rongcloud.DefaultUserPortraitURI)

	svc := rongcloud.NewNodeService(db.New(pool), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	summary, err := syncClawUserPortraits(context.Background(), pool, svc, []string{userID}, false)
	if err != nil {
		t.Fatalf("syncClawUserPortraits: %v", err)
	}
	if summary.skipped != 1 || summary.updated != 0 || summary.failed != 0 {
		t.Fatalf("summary = %+v, want skipped=1", summary)
	}
	if got := readClawPortrait(t, pool, userID); got != rongcloud.DefaultUserPortraitURI {
		t.Fatalf("row changed: %q", got)
	}
}

// ---- R3：群 / 聊天室 / 成员表回填的 SQL 预筛 --------------------------------

// seedChatroomRow 落一行 rongcloud_chatroom（只为检验预筛，房间内容无关）。
func seedChatroomRow(t *testing.T, pool *pgxpool.Pool, rcID, portrait string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO rongcloud_chatroom (workspace_id, rongcloud_chatroom_id, owner_user_id, portrait_uri)
		VALUES ('00000000-0000-0000-0000-0000000000aa'::uuid, $1, '00000000-0000-0000-0000-0000000000bb'::uuid, $2)`,
		rcID, portrait)
	if err != nil {
		t.Fatalf("seed rongcloud_chatroom %s: %v", rcID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM rongcloud_chatroom WHERE rongcloud_chatroom_id = $1`, rcID)
	})
}

func readChatroomPortrait(t *testing.T, pool *pgxpool.Pool, rcID string) string {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(portrait_uri, '') FROM rongcloud_chatroom WHERE rongcloud_chatroom_id = $1`, rcID,
	).Scan(&got); err != nil {
		t.Fatalf("read chatroom portrait of %s: %v", rcID, err)
	}
	return got
}

// TestPortraitPrefilterOnlySelectsKnownDefaults 覆盖 R3 新增的 SQL 预筛
// （portraitNeedsFixWhere + portraitFixCandidates）：它与 rongcloud.NeedsDefaultPortrait
// 是同一套判定 —— 空值 / 已知旧默认值入选，**用户自定义头像绝不入选**；
// 且 dry-run 仍然一个字节都不写。
func TestPortraitPrefilterOnlySelectsKnownDefaults(t *testing.T) {
	forcedDryRunEnv(t)
	pool := backfillTestPool(t)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	emptyRC := "bf-room-empty-" + suffix
	legacyRC := "bf-room-legacy-" + suffix
	customRC := "bf-room-custom-" + suffix
	const custom = "https://cdn.example.com/uploads/room-self-chosen.png"

	seedChatroomRow(t, pool, emptyRC, "")
	seedChatroomRow(t, pool, legacyRC, "https://newsradar.dreamdt.cn/im-test/api/static/claw/avatar-default.png")
	seedChatroomRow(t, pool, customRC, custom)
	mine := []string{emptyRC, legacyRC, customRC}

	// 直接跑生产用的预筛 SQL（参数同样取自 portraitFixCandidates）。
	args := append(portraitFixCandidates(), mine)
	rows, err := pool.Query(ctx, fmt.Sprintf(`
		SELECT rongcloud_chatroom_id FROM rongcloud_chatroom
		WHERE rongcloud_chatroom_id = ANY($4::text[]) AND %s
		ORDER BY 1`, portraitNeedsFixWhere), args...)
	if err != nil {
		t.Fatalf("prefilter query: %v", err)
	}
	var selected []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		selected = append(selected, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := []string{emptyRC, legacyRC}
	if len(selected) != len(want) || selected[0] != want[0] || selected[1] != want[1] {
		t.Fatalf("prefilter selected %v, want %v (自定义头像绝不能入选)", selected, want)
	}

	// dry-run：三行全部保持原值（含入选的两行）。
	summary, err := syncLocalPortraitColumn(ctx, pool, "rongcloud_chatroom", "rongcloud_chatroom_id", true)
	if err != nil {
		t.Fatalf("syncLocalPortraitColumn(dry run): %v", err)
	}
	if summary.planned < 2 {
		t.Fatalf("dry run planned=%d, want at least the 2 seeded rows", summary.planned)
	}
	if got := readChatroomPortrait(t, pool, emptyRC); got != "" {
		t.Fatalf("dry run wrote the empty row: %q", got)
	}
	if got := readChatroomPortrait(t, pool, legacyRC); got != "https://newsradar.dreamdt.cn/im-test/api/static/claw/avatar-default.png" {
		t.Fatalf("dry run rewrote the legacy row: %q", got)
	}
	if got := readChatroomPortrait(t, pool, customRC); got != custom {
		t.Fatalf("dry run touched the custom row: %q", got)
	}
}
