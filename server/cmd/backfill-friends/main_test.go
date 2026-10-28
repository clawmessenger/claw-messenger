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

// ---- 群类会话：按类型回刷（群聊 / 讨论组 / 聊天室） --------------------------

// seedChatroomRowKind 落一行带 conversation_kind 的聊天室。kind 传空串时写 NULL
// （历史行就是这种形态，判定上按聊天室处理）。
func seedChatroomRowKind(t *testing.T, pool *pgxpool.Pool, rcID, kind, portrait string) {
	t.Helper()
	var kindValue any
	if kind != "" {
		kindValue = kind
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO rongcloud_chatroom (workspace_id, rongcloud_chatroom_id, owner_user_id, conversation_kind, portrait_uri)
		VALUES ('00000000-0000-0000-0000-0000000000aa'::uuid, $1, '00000000-0000-0000-0000-0000000000bb'::uuid, $2, $3)`,
		rcID, kindValue, portrait)
	if err != nil {
		t.Fatalf("seed rongcloud_chatroom %s: %v", rcID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM rongcloud_chatroom WHERE rongcloud_chatroom_id = $1`, rcID)
	})
}

// seedGroupRow 落一行 rongcloud_group（群头像的本地镜像）。
func seedGroupRow(t *testing.T, pool *pgxpool.Pool, groupID, portrait string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO rongcloud_group (group_id, group_name, owner_id, portrait_uri)
		VALUES ($1, $2, 'owner-1', $3)`, groupID, "G "+groupID, portrait)
	if err != nil {
		t.Fatalf("seed rongcloud_group %s: %v", groupID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM rongcloud_group WHERE group_id = $1`, groupID)
	})
}

// TestChatroomPortraitsFollowConversationKind 钉住聊天室回刷的两个要点：
//
//  1. 目标图按 conversation_kind 分流（讨论组=圆桌三席、聊天室=两个气泡，空 kind
//     算聊天室），不再是所有人共用一张灰人形；
//  2. 「统一默认图 default_user.png」必须能被预筛选中 —— 换图之前的房间全落在
//     它上面，预筛漏掉这档就等于这条命令永远刷不动存量。
//
// 这一路不推融云（聊天室 API 没有头像字段），所以整个用例不需要假融云服务。
func TestChatroomPortraitsFollowConversationKind(t *testing.T) {
	forcedDryRunEnv(t)
	pool := backfillTestPool(t)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	discussionRC := "bf-disc-" + suffix
	chatroomRC := "bf-room-" + suffix
	noKindRC := "bf-nokind-" + suffix
	customRC := "bf-custom-" + suffix
	const custom = "https://cdn.example.com/uploads/self-chosen.png"

	unified := rongcloud.DefaultUserPortraitURI
	seedChatroomRowKind(t, pool, discussionRC, "discussion", unified)
	seedChatroomRowKind(t, pool, chatroomRC, "chatroom", unified)
	seedChatroomRowKind(t, pool, noKindRC, "", "")
	seedChatroomRowKind(t, pool, customRC, "discussion", custom)
	mine := []string{discussionRC, chatroomRC, noKindRC, customRC}

	// 预筛：三条默认图入选，自定义头像不入选。
	args := append(groupPortraitFixCandidates(), mine)
	rows, err := pool.Query(ctx, fmt.Sprintf(`
		SELECT rongcloud_chatroom_id FROM rongcloud_chatroom
		WHERE rongcloud_chatroom_id = ANY($5::text[]) AND %s
		ORDER BY 1`, groupPortraitNeedsFixWhere), args...)
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
	if len(selected) != 3 {
		t.Fatalf("prefilter selected %v, want the 3 default rows (自定义头像绝不能入选)", selected)
	}
	for _, id := range selected {
		if id == customRC {
			t.Fatalf("prefilter selected the custom row %s", id)
		}
	}

	// dry-run：计划 3 条，库内一个字节都不动。
	summary, err := syncChatroomPortraits(ctx, pool, true, mine...)
	if err != nil {
		t.Fatalf("syncChatroomPortraits(dry run): %v", err)
	}
	if summary.planned < 3 || summary.updated != 0 {
		t.Fatalf("dry run summary = %+v, want at least 3 planned and 0 updated", summary)
	}
	if got := readChatroomPortrait(t, pool, discussionRC); got != unified {
		t.Fatalf("dry run wrote %s: %q", discussionRC, got)
	}

	// commit：按类型落图。
	summary, err = syncChatroomPortraits(ctx, pool, false, mine...)
	if err != nil {
		t.Fatalf("syncChatroomPortraits(commit): %v", err)
	}
	if summary.updated < 3 {
		t.Fatalf("commit updated=%d, want at least 3", summary.updated)
	}
	discussionWant := rongcloud.GroupPortraitURI(rongcloud.GroupPortraitKindDiscussion)
	chatroomWant := rongcloud.GroupPortraitURI(rongcloud.GroupPortraitKindChatroom)
	if got := readChatroomPortrait(t, pool, discussionRC); got != discussionWant {
		t.Fatalf("discussion portrait = %q, want %q", got, discussionWant)
	}
	if got := readChatroomPortrait(t, pool, chatroomRC); got != chatroomWant {
		t.Fatalf("chatroom portrait = %q, want %q", got, chatroomWant)
	}
	// 没有 conversation_kind 的历史行按聊天室处理。
	if got := readChatroomPortrait(t, pool, noKindRC); got != chatroomWant {
		t.Fatalf("kind-less room portrait = %q, want %q", got, chatroomWant)
	}
	// 自定义头像原样保留。
	if got := readChatroomPortrait(t, pool, customRC); got != custom {
		t.Fatalf("custom portrait was overwritten: %q", got)
	}

	// 幂等：再跑一次什么都不该改（回填会被运维反复执行）。
	before := summary.updated
	summary, err = syncChatroomPortraits(ctx, pool, false, mine...)
	if err != nil {
		t.Fatalf("syncChatroomPortraits(second commit): %v", err)
	}
	if summary.updated != 0 {
		t.Fatalf("second run still updated %d rows (was %d)", summary.updated, before)
	}
}

// TestGroupPortraitsTargetTheGroupArt 覆盖群这一路的预筛 + 目标图判定：
// 群头像要推融云（真实 HTTP），所以这里只验「哪些行会进候选、目标是什么」，
// 推送本身由 portrait_default_test.go 的 refreshGroupInfo 断言覆盖。
func TestGroupPortraitsTargetTheGroupArt(t *testing.T) {
	forcedDryRunEnv(t)
	pool := backfillTestPool(t)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	unifiedID := "bf-grp-unified-" + suffix
	groupArtID := "bf-grp-art-" + suffix
	discussionArtID := "bf-grp-disc-" + suffix
	customID := "bf-grp-custom-" + suffix
	const custom = "https://cdn.example.com/uploads/our-group.png"

	seedGroupRow(t, pool, unifiedID, rongcloud.DefaultUserPortraitURI)
	seedGroupRow(t, pool, groupArtID, rongcloud.GroupPortraitURI(rongcloud.GroupPortraitKindGroup))
	seedGroupRow(t, pool, discussionArtID, rongcloud.GroupPortraitURI(rongcloud.GroupPortraitKindDiscussion))
	seedGroupRow(t, pool, customID, custom)
	mine := []string{unifiedID, groupArtID, discussionArtID, customID}

	args := append(groupPortraitFixCandidates(), mine)
	rows, err := pool.Query(ctx, fmt.Sprintf(`
		SELECT group_id FROM rongcloud_group
		WHERE group_id = ANY($5::text[]) AND %s
		ORDER BY 1`, groupPortraitNeedsFixWhere), args...)
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
	// 三张我方图都进候选（含已经是目标图的那条：第二层 NeedsGroupPortraitFix 才
	// 决定它 skipped），只有用户自定义头像不进。
	if len(selected) != 3 {
		t.Fatalf("prefilter selected %v, want the 3 default rows", selected)
	}
	for _, id := range selected {
		if id == customID {
			t.Fatalf("prefilter selected the custom group %s", id)
		}
	}

	groupWant := rongcloud.GroupPortraitURI(rongcloud.GroupPortraitKindGroup)
	cases := map[string]bool{
		unifiedID:       true, // 统一默认图 → 要改成群图
		groupArtID:      false, // 已经是群图 → 幂等跳过
		discussionArtID: true, // 类型判错留下的讨论组图 → 要纠正
		customID:        false,
	}
	for id, want := range cases {
		var current string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(portrait_uri,'') FROM rongcloud_group WHERE group_id = $1`, id).Scan(&current); err != nil {
			t.Fatalf("read group %s: %v", id, err)
		}
		if got := rongcloud.NeedsGroupPortraitFix(current, groupWant); got != want {
			t.Fatalf("NeedsGroupPortraitFix(%s = %q) = %v, want %v", id, current, got, want)
		}
	}
}
