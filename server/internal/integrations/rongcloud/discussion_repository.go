package rongcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 讨论状态机取值（对齐旧后端 discussion/models.py 的 DiscussionStatus）：
//
//	planning → running → summarizing → awaiting_user → synthesizing → completed
//
// 任意阶段都可能转 cancelled / failed。用字符串常量而不是 Go 枚举，是因为这些值
// 直接出现在 REST 快照与事件 payload 里 —— 前后端以字面量为契约，加一层枚举只会
// 多一次转译。
const (
	// DiscussionStatusConfiguring 引导配置中（draft 语义，迁移 595 新增）：
	// 引导式创建先落这个态，确认开始后 CAS 推进到 planning。
	DiscussionStatusConfiguring  = "configuring"
	DiscussionStatusPlanning     = "planning"
	DiscussionStatusRunning      = "running"
	DiscussionStatusSummarizing  = "summarizing"
	DiscussionStatusAwaitingUser = "awaiting_user"
	DiscussionStatusSynthesizing = "synthesizing"
	DiscussionStatusCompleted    = "completed"
	DiscussionStatusCancelled    = "cancelled"
	DiscussionStatusFailed       = "failed"
)

// discussionLiveStatuses 是「一个聊天室同时只能有一个活跃讨论」的部分唯一索引
// uq_rongcloud_discussion_one_active（迁移 589）所覆盖的集合。**改这里必须同步改迁移**，
// 否则索引与代码对「活跃」的定义会漂移。
var discussionLiveStatuses = map[string]struct{}{
	DiscussionStatusConfiguring:  {},
	DiscussionStatusPlanning:     {},
	DiscussionStatusRunning:      {},
	DiscussionStatusSummarizing:  {},
	DiscussionStatusAwaitingUser: {},
	DiscussionStatusSynthesizing: {},
}

// IsLiveDiscussionStatus reports whether a status counts as an active discussion.
func IsLiveDiscussionStatus(status string) bool {
	_, ok := discussionLiveStatuses[status]
	return ok
}

// 讨论模式与轮次策略。round_policy 决定 max_rounds 是否必须有值（迁移 589 的
// ck_rongcloud_discussion_round_policy 约束）：legacy_capped 必须有上限，
// user_confirmed（交互式、每轮等人确认）必须没有。
const (
	DiscussionModeModerated  = "moderated"
	DiscussionModeRoundtable = "roundtable"

	RoundPolicyLegacyCapped  = "legacy_capped"
	RoundPolicyUserConfirmed = "user_confirmed"
)

var (
	// ErrDiscussionNotFound：discussion_id 不存在。
	ErrDiscussionNotFound = errors.New("rongcloud: discussion not found")
	// ErrDiscussionVersionConflict：乐观锁失败。调用方必须回 409 并附最新快照 ——
	// 前端依赖这个响应做重同步，不能吞掉。
	ErrDiscussionVersionConflict = errors.New("rongcloud: discussion version conflict")
	// ErrArtifactNotFound：artifact_id 不存在。
	ErrArtifactNotFound = errors.New("rongcloud: artifact not found")
	// ErrDiscussionEventNotFound：按幂等键查事件时不存在（P7 交互轮次的去重探针）。
	ErrDiscussionEventNotFound = errors.New("rongcloud: discussion event not found")
	// ErrRoundDocumentNotFound：document_id 不存在。
	ErrRoundDocumentNotFound = errors.New("rongcloud: round document not found")
)

// DiscussionRecord 是一条讨论实例（rongcloud_discussion）。它对应旧后端的
// DiscussionSnapshot：身份、话题/目标、整个运行时用来推进的乐观锁 version，以及
// 开局时固化的角色板。
type DiscussionRecord struct {
	DiscussionID       string
	ChatroomID         string // 融云聊天室 id（不是内部 UUID）
	WorkspaceID        pgtype.UUID
	CreatedBy          string
	Topic              string
	Goal               string
	MaxRounds          *int // user_confirmed 模式下为 nil
	Mode               string
	Status             string
	CurrentRound       int
	Version            int
	RoleSnapshot       json.RawMessage
	HostConfigSnapshot json.RawMessage
	RecommendationID   string
	ClientRequestID    string
	RoundPolicy        string
	RoundRevision      int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// DiscussionEventRecord 是新链路的一条事件（rongcloud_discussion_event 的新列）。
// seq 每个讨论内从 1 递增，idempotency_key 保证重放不会写进两条。
type DiscussionEventRecord struct {
	ID             pgtype.UUID
	DiscussionID   string
	Seq            int64
	EventType      string
	ActorID        string
	TargetID       string
	Payload        json.RawMessage
	IdempotencyKey string
	V3Envelope     json.RawMessage
	CreatedAt      time.Time
}

// ArtifactRecord 是一条产物（rongcloud_artifact）：讨论的最终纪要/报告。每个讨论的
// 每种 artifact_type 只有一行，重复合成是覆盖 + version 递增。
type ArtifactRecord struct {
	ArtifactID   string
	DiscussionID string
	ArtifactType string
	Title        string
	Content      string
	Version      int
	BaseVersion  int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RoundDocumentRecord 是一条逐轮纪要（rongcloud_discussion_round_document）。
// (discussion_id, round, revision) 唯一，source_request_id 指回促成它的那次
// host 派发 —— 重放时靠它读回同一条文档。
type RoundDocumentRecord struct {
	DocumentID      string
	DiscussionID    string
	Round           int
	Revision        int
	SourceRequestID string
	Title           string
	Content         string
	CreatedAt       time.Time
}

// DiscussionRepository 是圆桌子系统的持久层。它只做「一行进、一行出」的翻译，
// 不持有事务：需要原子性的调用方（讨论运行时）自己用 queries.WithTx 组事务，
// 与仓库其余部分一致。
type DiscussionRepository struct {
	queries *db.Queries
	logger  *slog.Logger
}

func NewDiscussionRepository(queries *db.Queries, logger *slog.Logger) *DiscussionRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &DiscussionRepository{queries: queries, logger: logger}
}

// CreateDiscussionParams 是 Create 的入参。MaxRounds 用指针区分「没有上限」
// （nil，user_confirmed 模式）与「上限为某个数」。
type CreateDiscussionParams struct {
	DiscussionID       string
	ChatroomID         string
	WorkspaceID        pgtype.UUID
	CreatedBy          string
	Topic              string
	Goal               string
	MaxRounds          *int
	Mode               string
	Status             string
	RoleSnapshot       json.RawMessage
	HostConfigSnapshot json.RawMessage
	RecommendationID   string
	ClientRequestID    string
	RoundPolicy        string
	RoundRevision      int
}

func (r *DiscussionRepository) Create(ctx context.Context, p CreateDiscussionParams) (DiscussionRecord, error) {
	row, err := r.queries.CreateRongCloudDiscussion(ctx, db.CreateRongCloudDiscussionParams{
		DiscussionID:       p.DiscussionID,
		ChatroomID:         p.ChatroomID,
		WorkspaceID:        p.WorkspaceID,
		CreatedBy:          p.CreatedBy,
		Topic:              p.Topic,
		Goal:               p.Goal,
		MaxRounds:          int4OrNull(p.MaxRounds),
		Mode:               p.Mode,
		Status:             p.Status,
		RoleSnapshot:       rawOrEmptyObject(p.RoleSnapshot),
		HostConfigSnapshot: rawOrEmptyObject(p.HostConfigSnapshot),
		RecommendationID:   textOrNull(p.RecommendationID),
		ClientRequestID:    textOrNull(p.ClientRequestID),
		RoundPolicy:        p.RoundPolicy,
		RoundRevision:      int32(p.RoundRevision),
	})
	if err != nil {
		return DiscussionRecord{}, fmt.Errorf("rongcloud: create discussion %s: %w", p.DiscussionID, err)
	}
	return discussionRecordFromRow(row), nil
}

func (r *DiscussionRepository) Get(ctx context.Context, discussionID string) (DiscussionRecord, error) {
	row, err := r.queries.GetRongCloudDiscussion(ctx, discussionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DiscussionRecord{}, ErrDiscussionNotFound
	}
	if err != nil {
		return DiscussionRecord{}, fmt.Errorf("rongcloud: get discussion %s: %w", discussionID, err)
	}
	return discussionRecordFromRow(row), nil
}

// GetActiveByChatroom 返回该聊天室当前的活跃讨论，没有则 ErrDiscussionNotFound。
// 由 uq_rongcloud_discussion_one_active 保证最多一条。
func (r *DiscussionRepository) GetActiveByChatroom(ctx context.Context, chatroomID string) (DiscussionRecord, error) {
	row, err := r.queries.GetActiveRongCloudDiscussionByChatroom(ctx, chatroomID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DiscussionRecord{}, ErrDiscussionNotFound
	}
	if err != nil {
		return DiscussionRecord{}, fmt.Errorf("rongcloud: get active discussion for chatroom %s: %w", chatroomID, err)
	}
	return discussionRecordFromRow(row), nil
}

func (r *DiscussionRepository) ListByChatroom(ctx context.Context, chatroomID string) ([]DiscussionRecord, error) {
	rows, err := r.queries.ListRongCloudDiscussionsByChatroom(ctx, chatroomID)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: list discussions for chatroom %s: %w", chatroomID, err)
	}
	out := make([]DiscussionRecord, len(rows))
	for i, row := range rows {
		out[i] = discussionRecordFromRow(row)
	}
	return out, nil
}

func (r *DiscussionRepository) ListActive(ctx context.Context) ([]DiscussionRecord, error) {
	rows, err := r.queries.ListActiveRongCloudDiscussions(ctx)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: list active discussions: %w", err)
	}
	out := make([]DiscussionRecord, len(rows))
	for i, row := range rows {
		out[i] = discussionRecordFromRow(row)
	}
	return out, nil
}

// AdvanceDiscussionParams 是 AdvanceState 的入参。ExpectedVersion 是调用方读到的
// version；不匹配即冲突。
type AdvanceDiscussionParams struct {
	DiscussionID    string
	ExpectedVersion int
	Status          string
	CurrentRound    int
	RoundRevision   int
}

// AdvanceState 是圆桌的 compare-and-set：只有 version 仍是 ExpectedVersion 时才
// 写入新状态并把 version 加一。
//
// 返回 ErrDiscussionVersionConflict 时，调用方必须回 409 并**附上最新快照**——
// 前端靠这个响应把本地状态拉回权威值。返回 ErrDiscussionNotFound 则是这个讨论
// 已经不在了。两者都是「0 行受影响」，所以冲突路径要多查一次来区分。
func (r *DiscussionRepository) AdvanceState(ctx context.Context, p AdvanceDiscussionParams) (DiscussionRecord, error) {
	row, err := r.queries.UpdateRongCloudDiscussionState(ctx, db.UpdateRongCloudDiscussionStateParams{
		DiscussionID:    p.DiscussionID,
		ExpectedVersion: int32(p.ExpectedVersion),
		Status:          p.Status,
		CurrentRound:    int32(p.CurrentRound),
		RoundRevision:   int32(p.RoundRevision),
	})
	if err == nil {
		return discussionRecordFromRow(row), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DiscussionRecord{}, fmt.Errorf("rongcloud: advance discussion %s: %w", p.DiscussionID, err)
	}
	if _, gerr := r.queries.GetRongCloudDiscussion(ctx, p.DiscussionID); gerr != nil {
		return DiscussionRecord{}, ErrDiscussionNotFound
	}
	return DiscussionRecord{}, ErrDiscussionVersionConflict
}

// UpdateRoleSnapshot 落一份新的角色板（中途加人/移人的轮边界同步）。
//
// 刻意**不用 CAS**：role_snapshot 不参与乐观锁（AdvanceState 的 version 只守
// status 与轮次指针）。把它算进 CAS 会让同一次用户动作里的「先同步角色板、
// 再推进轮次」互相打架 —— 客户端带来的 stateVersion 在同步之后就已经过期，
// 用户点「继续」会莫名拿到 409。并发写者算的都是成员表的同一份投影，重复写
// 是无害的。
func (r *DiscussionRepository) UpdateRoleSnapshot(ctx context.Context, discussionID string, snapshot json.RawMessage) (DiscussionRecord, error) {
	row, err := r.queries.UpdateRongCloudDiscussionRoleSnapshot(ctx, db.UpdateRongCloudDiscussionRoleSnapshotParams{
		DiscussionID: discussionID,
		RoleSnapshot: rawOrEmptyObject(snapshot),
	})
	if err != nil {
		return DiscussionRecord{}, fmt.Errorf("rongcloud: update role snapshot for %s: %w", discussionID, err)
	}
	return discussionRecordFromRow(row), nil
}

func (r *DiscussionRepository) Delete(ctx context.Context, discussionID string) error {
	if err := r.queries.DeleteRongCloudDiscussion(ctx, discussionID); err != nil {
		return fmt.Errorf("rongcloud: delete discussion %s: %w", discussionID, err)
	}
	return nil
}

// NextEventSeq 返回该讨论下一个可用的事件序号（从 1 开始）。
//
// 刻意不让 AppendEvent 自己算 seq：序号必须在调用方持有的行锁/事务里分配，
// 否则两个并发写者会算出同一个值（唯一索引会拒掉，但那是把并发问题变成随机失败）。
// 这与旧后端的分层一致 —— 仓库不做并发控制，运行时来做。
func (r *DiscussionRepository) NextEventSeq(ctx context.Context, discussionID string) (int64, error) {
	maxSeq, err := r.queries.GetRongCloudDiscussionEventMaxSeq(ctx, textOrNull(discussionID))
	if err != nil {
		return 0, fmt.Errorf("rongcloud: next event seq for %s: %w", discussionID, err)
	}
	return maxSeq + 1, nil
}

// AppendDiscussionEventParams 是 AppendEvent 的入参。Seq 由调用方经 NextEventSeq
// 在事务内取得。
type AppendDiscussionEventParams struct {
	DiscussionID   string
	Seq            int64
	EventType      string
	ActorID        string
	TargetID       string
	Payload        json.RawMessage
	IdempotencyKey string
	V3Envelope     json.RawMessage
}

func (r *DiscussionRepository) AppendEvent(ctx context.Context, p AppendDiscussionEventParams) (DiscussionEventRecord, error) {
	row, err := r.queries.AppendRongCloudDiscussionEvent(ctx, db.AppendRongCloudDiscussionEventParams{
		DiscussionID:   textOrNull(p.DiscussionID),
		Seq:            pgtype.Int8{Int64: p.Seq, Valid: true},
		EventType:      p.EventType,
		ActorID:        textOrNull(p.ActorID),
		TargetID:       textOrNull(p.TargetID),
		Payload:        rawOrEmptyObject(p.Payload),
		IdempotencyKey: textOrNull(p.IdempotencyKey),
		V3Envelope:     p.V3Envelope,
	})
	if err != nil {
		return DiscussionEventRecord{}, fmt.Errorf("rongcloud: append event to %s: %w", p.DiscussionID, err)
	}
	return discussionEventRecordFromRow(row), nil
}

// EventByIdempotencyKey 按 (discussion_id, idempotency_key) 查一条事件；不存在时
// 返回 ErrDiscussionEventNotFound。P7 的交互轮次把每一次推进都挂在一个幂等键上：
// 融云回调是 at-least-once、设备重连会整段重放离线历史，所以「先查键、命中即
// 无操作」是引擎唯一的重放保护。
func (r *DiscussionRepository) EventByIdempotencyKey(ctx context.Context, discussionID, key string) (DiscussionEventRecord, error) {
	row, err := r.queries.GetRongCloudDiscussionEventByIdempotency(ctx, db.GetRongCloudDiscussionEventByIdempotencyParams{
		DiscussionID:   textOrNull(discussionID),
		IdempotencyKey: textOrNull(key),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DiscussionEventRecord{}, ErrDiscussionEventNotFound
	}
	if err != nil {
		return DiscussionEventRecord{}, fmt.Errorf("rongcloud: event by idempotency key %s/%s: %w", discussionID, key, err)
	}
	return discussionEventRecordFromRow(row), nil
}

// ListEventsAfter 返回 seq 严格大于 afterSeq 的事件，按 seq 升序 —— 这正是前端
// 「权威快照 + 缺口补拉」所需要的（afterSeq=0 即全量）。
func (r *DiscussionRepository) ListEventsAfter(ctx context.Context, discussionID string, afterSeq int64) ([]DiscussionEventRecord, error) {
	rows, err := r.queries.ListRongCloudDiscussionEventsAfterSeq(ctx, db.ListRongCloudDiscussionEventsAfterSeqParams{
		DiscussionID: textOrNull(discussionID),
		Seq:          pgtype.Int8{Int64: afterSeq, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("rongcloud: list events after %d for %s: %w", afterSeq, discussionID, err)
	}
	out := make([]DiscussionEventRecord, len(rows))
	for i, row := range rows {
		out[i] = discussionEventRecordFromRow(row)
	}
	return out, nil
}

// UpsertArtifactParams 是 UpsertArtifact 的入参。ArtifactID 只在首次插入时使用
// （同一 discussion_id + artifact_type 已存在则走 UPDATE）。
type UpsertArtifactParams struct {
	ArtifactID   string
	DiscussionID string
	ArtifactType string
	Title        string
	Content      string
	Version      int
	BaseVersion  int
}

func (r *DiscussionRepository) UpsertArtifact(ctx context.Context, p UpsertArtifactParams) (ArtifactRecord, error) {
	row, err := r.queries.UpsertRongCloudArtifact(ctx, db.UpsertRongCloudArtifactParams{
		ArtifactID:   p.ArtifactID,
		DiscussionID: p.DiscussionID,
		ArtifactType: p.ArtifactType,
		Title:        p.Title,
		Content:      p.Content,
		Version:      int32(p.Version),
		BaseVersion:  int32(p.BaseVersion),
	})
	if err != nil {
		return ArtifactRecord{}, fmt.Errorf("rongcloud: upsert artifact for %s: %w", p.DiscussionID, err)
	}
	return artifactRecordFromRow(row), nil
}

func (r *DiscussionRepository) GetArtifact(ctx context.Context, artifactID string) (ArtifactRecord, error) {
	row, err := r.queries.GetRongCloudArtifact(ctx, artifactID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArtifactRecord{}, ErrArtifactNotFound
	}
	if err != nil {
		return ArtifactRecord{}, fmt.Errorf("rongcloud: get artifact %s: %w", artifactID, err)
	}
	return artifactRecordFromRow(row), nil
}

// LatestArtifact 返回该讨论最近更新的产物；没有则 ErrArtifactNotFound。
func (r *DiscussionRepository) LatestArtifact(ctx context.Context, discussionID string) (ArtifactRecord, error) {
	row, err := r.queries.GetLatestRongCloudArtifactByDiscussion(ctx, discussionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArtifactRecord{}, ErrArtifactNotFound
	}
	if err != nil {
		return ArtifactRecord{}, fmt.Errorf("rongcloud: get latest artifact for %s: %w", discussionID, err)
	}
	return artifactRecordFromRow(row), nil
}

// ListRoundDocuments 返回该讨论的逐轮纪要（按 round/revision 升序，与旧后端
// round_documents.list_for_discussion 的读序一致）。
func (r *DiscussionRepository) ListRoundDocuments(ctx context.Context, discussionID string) ([]RoundDocumentRecord, error) {
	rows, err := r.queries.ListRongCloudRoundDocumentsForDiscussion(ctx, discussionID)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: list round documents for %s: %w", discussionID, err)
	}
	out := make([]RoundDocumentRecord, len(rows))
	for i, row := range rows {
		out[i] = roundDocumentRecordFromRow(row)
	}
	return out, nil
}

// GetRoundDocument 按 document_id 取一条逐轮纪要；不存在时 ErrRoundDocumentNotFound。
func (r *DiscussionRepository) GetRoundDocument(ctx context.Context, documentID string) (RoundDocumentRecord, error) {
	row, err := r.queries.GetRongCloudRoundDocument(ctx, documentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RoundDocumentRecord{}, ErrRoundDocumentNotFound
	}
	if err != nil {
		return RoundDocumentRecord{}, fmt.Errorf("rongcloud: get round document %s: %w", documentID, err)
	}
	return roundDocumentRecordFromRow(row), nil
}

// ---- 行 → 领域对象 --------------------------------------------------------

func discussionRecordFromRow(row db.RongcloudDiscussion) DiscussionRecord {
	return DiscussionRecord{
		DiscussionID:       row.DiscussionID,
		ChatroomID:         row.ChatroomID,
		WorkspaceID:        row.WorkspaceID,
		CreatedBy:          row.CreatedBy,
		Topic:              row.Topic,
		Goal:               row.Goal,
		MaxRounds:          int4Ptr(row.MaxRounds),
		Mode:               row.Mode,
		Status:             row.Status,
		CurrentRound:       int(row.CurrentRound),
		Version:            int(row.Version),
		RoleSnapshot:       json.RawMessage(row.RoleSnapshot),
		HostConfigSnapshot: json.RawMessage(row.HostConfigSnapshot),
		RecommendationID:   textValue(row.RecommendationID),
		ClientRequestID:    textValue(row.ClientRequestID),
		RoundPolicy:        row.RoundPolicy,
		RoundRevision:      int(row.RoundRevision),
		CreatedAt:          row.CreatedAt.Time,
		UpdatedAt:          row.UpdatedAt.Time,
	}
}

func discussionEventRecordFromRow(row db.RongcloudDiscussionEvent) DiscussionEventRecord {
	return DiscussionEventRecord{
		ID:             row.ID,
		DiscussionID:   textValue(row.DiscussionID),
		Seq:            row.Seq.Int64,
		EventType:      row.EventType,
		ActorID:        textValue(row.ActorID),
		TargetID:       textValue(row.TargetID),
		Payload:        json.RawMessage(row.Payload),
		IdempotencyKey: textValue(row.IdempotencyKey),
		V3Envelope:     json.RawMessage(row.V3Envelope),
		CreatedAt:      row.CreatedAt.Time,
	}
}

func artifactRecordFromRow(row db.RongcloudArtifact) ArtifactRecord {
	return ArtifactRecord{
		ArtifactID:   row.ArtifactID,
		DiscussionID: row.DiscussionID,
		ArtifactType: row.ArtifactType,
		Title:        row.Title,
		Content:      row.Content,
		Version:      int(row.Version),
		BaseVersion:  int(row.BaseVersion),
		CreatedAt:    row.CreatedAt.Time,
		UpdatedAt:    row.UpdatedAt.Time,
	}
}

func roundDocumentRecordFromRow(row db.RongcloudDiscussionRoundDocument) RoundDocumentRecord {
	return RoundDocumentRecord{
		DocumentID:      row.DocumentID,
		DiscussionID:    row.DiscussionID,
		Round:           int(row.Round),
		Revision:        int(row.Revision),
		SourceRequestID: row.SourceRequestID,
		Title:           row.Title,
		Content:         row.Content,
		CreatedAt:       row.CreatedAt.Time,
	}
}

// ---- pgtype 小工具 --------------------------------------------------------

func textOrNull(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func textValue(value pgtype.Text) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func int4OrNull(value *int) pgtype.Int4 {
	if value == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*value), Valid: true}
}

func int4Ptr(value pgtype.Int4) *int {
	if !value.Valid {
		return nil
	}
	out := int(value.Int32)
	return &out
}

// rawOrEmptyObject 让空的 JSONB 列落成 {}（列都是 NOT NULL DEFAULT '{}'），
// 而不是写入 NULL 被约束拒绝。
func rawOrEmptyObject(value json.RawMessage) []byte {
	if len(value) == 0 {
		return []byte("{}")
	}
	return value
}
