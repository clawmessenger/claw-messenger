package rongcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 这些测试打真实数据库：讨论仓库的全部价值在于 SQL（乐观锁、部分唯一索引、seq 顺序、
// artifact upsert），用假实现去测等于不测。库不可达时整体跳过，与 internal/handler 的
// 约定一致（DATABASE_URL，默认本机 dev 库）。
func newDiscussionTestPool(t *testing.T) *pgxpool.Pool {
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
		t.Skipf("skipping: database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newDiscussionTestRepo(t *testing.T) (*DiscussionRepository, *pgxpool.Pool) {
	t.Helper()
	pool := newDiscussionTestPool(t)
	return NewDiscussionRepository(db.New(pool), testLogger()), pool
}

// uniqueID 造一个不会与其它测试/历史数据撞车的标识。
func uniqueID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), os.Getpid())
}

func testUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("scan uuid %q: %v", s, err)
	}
	return u
}

// cleanupDiscussion 清掉一个讨论及其事件/产物。事件表没有外键，必须手动删。
func cleanupDiscussion(t *testing.T, pool *pgxpool.Pool, discussionID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, stmt := range []string{
			`DELETE FROM rongcloud_discussion_event WHERE discussion_id = $1`,
			`DELETE FROM rongcloud_artifact WHERE discussion_id = $1`,
			`DELETE FROM rongcloud_discussion WHERE discussion_id = $1`,
		} {
			if _, err := pool.Exec(ctx, stmt, discussionID); err != nil {
				t.Errorf("cleanup %q: %v", stmt, err)
			}
		}
	})
}

func createTestDiscussion(t *testing.T, repo *DiscussionRepository, pool *pgxpool.Pool) DiscussionRecord {
	t.Helper()
	id := uniqueID("disc")
	record, err := repo.Create(context.Background(), CreateDiscussionParams{
		DiscussionID: id,
		ChatroomID:   uniqueID("room"),
		WorkspaceID:  testUUID(t, "550e8400-e29b-41d4-a716-446655440000"),
		CreatedBy:    "user-1",
		Topic:        "Q4 排期",
		Goal:         "定下里程碑",
		MaxRounds:    intPtr(6),
		Mode:         DiscussionModeModerated,
		Status:       DiscussionStatusPlanning,
		RoundPolicy:  RoundPolicyLegacyCapped,
	})
	if err != nil {
		t.Fatalf("create discussion: %v", err)
	}
	cleanupDiscussion(t, pool, id)
	return record
}

func intPtr(v int) *int { return &v }

func TestDiscussionRepositoryCreateAndGet(t *testing.T) {
	repo, pool := newDiscussionTestRepo(t)
	created := createTestDiscussion(t, repo, pool)

	if created.Version != 1 {
		t.Errorf("new discussion should start at version 1, got %d", created.Version)
	}
	if created.MaxRounds == nil || *created.MaxRounds != 6 {
		t.Errorf("MaxRounds = %v, want 6", created.MaxRounds)
	}

	got, err := repo.Get(context.Background(), created.DiscussionID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Topic != "Q4 排期" || got.Goal != "定下里程碑" {
		t.Errorf("round-tripped topic/goal = %q/%q", got.Topic, got.Goal)
	}
	if got.Status != DiscussionStatusPlanning {
		t.Errorf("status = %q", got.Status)
	}

	if _, err := repo.Get(context.Background(), uniqueID("missing")); !errors.Is(err, ErrDiscussionNotFound) {
		t.Errorf("missing discussion should be ErrDiscussionNotFound, got %v", err)
	}
}

// user_confirmed 模式没有轮次上限 —— max_rounds 必须是 NULL，否则迁移 589 的
// ck_rongcloud_discussion_round_policy 会拒绝写入。这条把约束与仓库的指针语义钉在一起。
func TestDiscussionRepositoryUserConfirmedHasNoRoundCap(t *testing.T) {
	repo, pool := newDiscussionTestRepo(t)
	id := uniqueID("disc")
	created, err := repo.Create(context.Background(), CreateDiscussionParams{
		DiscussionID: id,
		ChatroomID:   uniqueID("room"),
		WorkspaceID:  testUUID(t, "550e8400-e29b-41d4-a716-446655440000"),
		CreatedBy:    "user-1",
		Topic:        "开放式议题",
		MaxRounds:    nil,
		Mode:         DiscussionModeRoundtable,
		Status:       DiscussionStatusPlanning,
		RoundPolicy:  RoundPolicyUserConfirmed,
	})
	if err != nil {
		t.Fatalf("create user_confirmed discussion: %v", err)
	}
	cleanupDiscussion(t, pool, id)

	if created.MaxRounds != nil {
		t.Errorf("user_confirmed discussion must have no round cap, got %v", *created.MaxRounds)
	}
}

// 一个聊天室同时只能有一个活跃讨论（uq_rongcloud_discussion_one_active）；讨论结束后
// 才能再开新的。这是「重复点开始」不会开出两个讨论的底层保证。
func TestDiscussionRepositoryOneActivePerChatroom(t *testing.T) {
	repo, pool := newDiscussionTestRepo(t)
	ctx := context.Background()
	chatroomID := uniqueID("room")

	first, err := repo.Create(ctx, CreateDiscussionParams{
		DiscussionID: uniqueID("disc"), ChatroomID: chatroomID,
		WorkspaceID: testUUID(t, "550e8400-e29b-41d4-a716-446655440000"),
		CreatedBy:   "user-1", Topic: "t", Mode: DiscussionModeModerated,
		Status: DiscussionStatusRunning, RoundPolicy: RoundPolicyLegacyCapped, MaxRounds: intPtr(3),
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	cleanupDiscussion(t, pool, first.DiscussionID)

	_, err = repo.Create(ctx, CreateDiscussionParams{
		DiscussionID: uniqueID("disc"), ChatroomID: chatroomID,
		WorkspaceID: testUUID(t, "550e8400-e29b-41d4-a716-446655440000"),
		CreatedBy:   "user-1", Topic: "t2", Mode: DiscussionModeModerated,
		Status: DiscussionStatusRunning, RoundPolicy: RoundPolicyLegacyCapped, MaxRounds: intPtr(3),
	})
	if err == nil {
		t.Fatal("a second live discussion in the same chatroom must be rejected")
	}

	active, err := repo.GetActiveByChatroom(ctx, chatroomID)
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if active.DiscussionID != first.DiscussionID {
		t.Errorf("active = %s, want %s", active.DiscussionID, first.DiscussionID)
	}

	// 结束后就不再占用「活跃」名额。
	if _, err := repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: first.DiscussionID, ExpectedVersion: first.Version,
		Status: DiscussionStatusCompleted, CurrentRound: 3,
	}); err != nil {
		t.Fatalf("complete first: %v", err)
	}
	second, err := repo.Create(ctx, CreateDiscussionParams{
		DiscussionID: uniqueID("disc"), ChatroomID: chatroomID,
		WorkspaceID: testUUID(t, "550e8400-e29b-41d4-a716-446655440000"),
		CreatedBy:   "user-1", Topic: "t3", Mode: DiscussionModeModerated,
		Status: DiscussionStatusRunning, RoundPolicy: RoundPolicyLegacyCapped, MaxRounds: intPtr(3),
	})
	if err != nil {
		t.Fatalf("create after completion: %v", err)
	}
	cleanupDiscussion(t, pool, second.DiscussionID)
}

// 乐观锁：version 不匹配必须报冲突（调用方据此回 409 + 最新快照），而不是静默覆盖。
func TestDiscussionRepositoryAdvanceStateCompareAndSet(t *testing.T) {
	repo, pool := newDiscussionTestRepo(t)
	ctx := context.Background()
	created := createTestDiscussion(t, repo, pool)

	advanced, err := repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: created.DiscussionID, ExpectedVersion: created.Version,
		Status: DiscussionStatusRunning, CurrentRound: 1, RoundRevision: 0,
	})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if advanced.Version != created.Version+1 {
		t.Errorf("version = %d, want %d", advanced.Version, created.Version+1)
	}
	if advanced.Status != DiscussionStatusRunning || advanced.CurrentRound != 1 {
		t.Errorf("advanced record = %+v", advanced)
	}

	// 用已经过期的 version 再推进 → 冲突。
	_, err = repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: created.DiscussionID, ExpectedVersion: created.Version,
		Status: DiscussionStatusCompleted, CurrentRound: 9,
	})
	if !errors.Is(err, ErrDiscussionVersionConflict) {
		t.Fatalf("stale version should conflict, got %v", err)
	}

	// 不存在的讨论 → 未找到（与冲突区分开）。
	_, err = repo.AdvanceState(ctx, AdvanceDiscussionParams{
		DiscussionID: uniqueID("missing"), ExpectedVersion: 1, Status: DiscussionStatusRunning,
	})
	if !errors.Is(err, ErrDiscussionNotFound) {
		t.Fatalf("missing discussion should be ErrDiscussionNotFound, got %v", err)
	}
}

func TestDiscussionRepositoryEventsAppendAndListAfterSeq(t *testing.T) {
	repo, pool := newDiscussionTestRepo(t)
	ctx := context.Background()
	created := createTestDiscussion(t, repo, pool)

	for i := 1; i <= 3; i++ {
		seq, err := repo.NextEventSeq(ctx, created.DiscussionID)
		if err != nil {
			t.Fatalf("next seq: %v", err)
		}
		if seq != int64(i) {
			t.Fatalf("seq = %d, want %d", seq, i)
		}
		payload, _ := json.Marshal(map[string]interface{}{"round": i})
		if _, err := repo.AppendEvent(ctx, AppendDiscussionEventParams{
			DiscussionID:   created.DiscussionID,
			Seq:            seq,
			EventType:      "round_started",
			ActorID:        "host",
			Payload:        payload,
			IdempotencyKey: fmt.Sprintf("k-%d", i),
		}); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}

	all, err := repo.ListEventsAfter(ctx, created.DiscussionID, 0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("events = %d, want 3", len(all))
	}
	for i, ev := range all {
		if ev.Seq != int64(i+1) {
			t.Errorf("event[%d].Seq = %d, want %d", i, ev.Seq, i+1)
		}
	}
	if string(all[0].Payload) == "" || all[0].EventType != "round_started" {
		t.Errorf("event[0] = %+v", all[0])
	}

	// 缺口补拉：只要 seq > afterSeq 的。
	tail, err := repo.ListEventsAfter(ctx, created.DiscussionID, 2)
	if err != nil {
		t.Fatalf("list tail: %v", err)
	}
	if len(tail) != 1 || tail[0].Seq != 3 {
		t.Fatalf("tail = %+v, want single seq 3", tail)
	}
}

// 同一个幂等键重放不能写进第二条（唯一索引兜底）——这是重试不产生重复事件的基础。
func TestDiscussionRepositoryEventIdempotencyKeyIsUnique(t *testing.T) {
	repo, pool := newDiscussionTestRepo(t)
	ctx := context.Background()
	created := createTestDiscussion(t, repo, pool)

	appendWith := func(seq int64) error {
		_, err := repo.AppendEvent(ctx, AppendDiscussionEventParams{
			DiscussionID: created.DiscussionID, Seq: seq, EventType: "round_started",
			IdempotencyKey: "same-key",
		})
		return err
	}
	if err := appendWith(1); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := appendWith(2); err == nil {
		t.Fatal("replaying the same idempotency key must be rejected")
	}
}

func TestDiscussionRepositoryArtifactUpsertAndLatest(t *testing.T) {
	repo, pool := newDiscussionTestRepo(t)
	ctx := context.Background()
	created := createTestDiscussion(t, repo, pool)

	first, err := repo.UpsertArtifact(ctx, UpsertArtifactParams{
		ArtifactID: uniqueID("art"), DiscussionID: created.DiscussionID,
		ArtifactType: "summary", Title: "纪要", Content: "v1", Version: 1, BaseVersion: 0,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// 同类型再次合成 → 覆盖同一行，version 递增，artifact_id 不变。
	second, err := repo.UpsertArtifact(ctx, UpsertArtifactParams{
		ArtifactID: uniqueID("art"), DiscussionID: created.DiscussionID,
		ArtifactType: "summary", Title: "纪要", Content: "v2", Version: 2, BaseVersion: 1,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.ArtifactID != first.ArtifactID {
		t.Errorf("upsert must update in place: %s → %s", first.ArtifactID, second.ArtifactID)
	}
	if second.Content != "v2" || second.Version != 2 {
		t.Errorf("artifact = %+v", second)
	}

	latest, err := repo.LatestArtifact(ctx, created.DiscussionID)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.ArtifactID != first.ArtifactID || latest.Content != "v2" {
		t.Errorf("latest = %+v", latest)
	}

	if _, err := repo.LatestArtifact(ctx, uniqueID("nodisc")); !errors.Is(err, ErrArtifactNotFound) {
		t.Errorf("missing artifact should be ErrArtifactNotFound, got %v", err)
	}
}

func TestIsLiveDiscussionStatus(t *testing.T) {
	for _, live := range []string{
		DiscussionStatusPlanning, DiscussionStatusRunning, DiscussionStatusSummarizing,
		DiscussionStatusAwaitingUser, DiscussionStatusSynthesizing,
	} {
		if !IsLiveDiscussionStatus(live) {
			t.Errorf("%q should count as live", live)
		}
	}
	for _, done := range []string{
		DiscussionStatusCompleted, DiscussionStatusCancelled, DiscussionStatusFailed, "", "bogus",
	} {
		if IsLiveDiscussionStatus(done) {
			t.Errorf("%q must not count as live", done)
		}
	}
}
