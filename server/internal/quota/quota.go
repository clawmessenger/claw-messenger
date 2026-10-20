// Package quota 实现用户级三类资源（讨论组/群聊/聊天室）的创建配额。
//
// 模型（拍板项）：
//   - 取值语义：-1=不限，0=禁止，N=上限，NULL=跟随套餐/内置默认；
//   - 优先级：claw_im_user_quota 按用户覆盖 > claw_im_plan 套餐 > 内置默认（3/5/1）；
//   - 计数口径：房主名下 status='active' 的对应类型房间（群镜像无 status 列，解散即
//     物理删行）；讨论组不双计 chatroom；configuring 半成品计入，GC 删房自动释放；
//   - 并发防超建：Check 在事务内先取 pg_advisory_xact_lock(hashtext(user_id))，
//     把「查上限 + 数用量」临界区串行化。融云 API 调用在锁外（创建链路无法整包
//     进一个 DB 事务），失败走既有幂等回滚。
//
// 键域：统一用 rc 用户 id（claw_im_users.user_id，即融云 TEXT id）。
package quota

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 资源类别。与 QUOTA_EXCEEDED 错误的 data.kind 取值一致（前后端契约）。
type Kind string

const (
	KindDiscussion Kind = "discussion"
	KindGroup      Kind = "group"
	KindChatroom   Kind = "chatroom"
)

// 内置默认配额（拍板项：讨论组 3 / 群聊 5 / 聊天室 1）。改动需改码重启。
const (
	DefaultDiscussionLimit = 3
	DefaultGroupLimit      = 5
	DefaultChatroomLimit   = 1
)

// CodeQuotaExceeded 是超限错误的对外错误码（命令通道 code=429 / REST 429）。
const CodeQuotaExceeded = "QUOTA_EXCEEDED"

// Limits 是三类资源的最终生效上限（-1/0/N，已按优先级解析完毕）。
type Limits struct {
	DiscussionLimit int `json:"discussion_limit"`
	GroupLimit      int `json:"group_limit"`
	ChatroomLimit   int `json:"chatroom_limit"`
}

// OverrideLimits 是按用户覆盖的写入形态：指针为 nil 表示「清除覆盖，跟随默认」。
type OverrideLimits struct {
	DiscussionLimit *int `json:"discussion_limit"`
	GroupLimit      *int `json:"group_limit"`
	ChatroomLimit   *int `json:"chatroom_limit"`
}

// KindUsage 是一类资源的用量视图。Remaining 为 -1 表示不限。
type KindUsage struct {
	Limit     int `json:"limit"`
	Used      int `json:"used"`
	Remaining int `json:"remaining"`
}

// Usage 是三类资源的用量汇总（GET /api/quota/me 的 data 形状）。
type Usage struct {
	Discussion KindUsage `json:"discussion"`
	Group      KindUsage `json:"group"`
	Chatroom   KindUsage `json:"chatroom"`
}

// Error 是配额超限错误。Code 恒为 QUOTA_EXCEEDED，Kind/Limit/Used 透传给前端
// 渲染「当前用量/上限」提示。
type Error struct {
	Kind  string
	Limit int
	Used  int
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: kind=%s limit=%d used=%d", CodeQuotaExceeded, e.Kind, e.Limit, e.Used)
}

// Code 返回对外错误码。
func (e *Error) Code() string { return CodeQuotaExceeded }

// Data 返回透传给前端的结构化字段（命令通道 commandResult.data）。
func (e *Error) Data() map[string]interface{} {
	return map[string]interface{}{"kind": e.Kind, "limit": e.Limit, "used": e.Used}
}

// Service 是配额服务。持有连接池是因为 Check 需要在事务内取 advisory lock
// （sqlc 的 Queries 不持有事务，临界区必须由这里组）。
type Service struct {
	pool *pgxpool.Pool
}

// New 构造配额服务。pool 为 nil 时所有方法退化为「放行 + 零用量」
// （融云集成未装配的部署不引入新失败面）。
func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Resolve 解析用户的最终生效上限：按用户覆盖 > 套餐 > 内置默认。
func (s *Service) Resolve(ctx context.Context, userID string) (Limits, error) {
	limits := Limits{
		DiscussionLimit: DefaultDiscussionLimit,
		GroupLimit:      DefaultGroupLimit,
		ChatroomLimit:   DefaultChatroomLimit,
	}
	if s.pool == nil || userID == "" {
		return limits, nil
	}
	queries := db.New(s.pool)
	// 套餐层：用户 plan_id 指向的套餐，缺省落默认套餐；套餐行缺失/停用回落内置默认。
	plan := db.ClawImPlan{}
	planFound := false
	if planID, err := queries.GetClawIMUserPlanID(ctx, userID); err == nil && planID.Valid {
		if row, pErr := queries.GetClawIMPlan(ctx, planID.Int32); pErr == nil && row.Status == "active" {
			plan, planFound = row, true
		}
	}
	if !planFound {
		if row, pErr := queries.GetDefaultClawIMPlan(ctx); pErr == nil {
			plan, planFound = row, true
		}
	}
	if planFound {
		limits.DiscussionLimit = int(plan.DiscussionLimit)
		limits.GroupLimit = int(plan.GroupLimit)
		limits.ChatroomLimit = int(plan.ChatroomLimit)
	}
	// 覆盖层：非 NULL 的列压过套餐/默认。
	override, err := queries.GetClawIMUserQuota(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return limits, nil
		}
		return Limits{}, fmt.Errorf("quota: resolve override for %s: %w", userID, err)
	}
	if override.DiscussionLimit.Valid {
		limits.DiscussionLimit = int(override.DiscussionLimit.Int32)
	}
	if override.GroupLimit.Valid {
		limits.GroupLimit = int(override.GroupLimit.Int32)
	}
	if override.ChatroomLimit.Valid {
		limits.ChatroomLimit = int(override.ChatroomLimit.Int32)
	}
	return limits, nil
}

// limitFor 取某类资源的生效上限。
func (l Limits) limitFor(kind Kind) int {
	switch kind {
	case KindDiscussion:
		return l.DiscussionLimit
	case KindGroup:
		return l.GroupLimit
	default:
		return l.ChatroomLimit
	}
}

// count 数一类资源的当前用量（计数口径见包注释）。
func (s *Service) count(ctx context.Context, queries *db.Queries, userID string, kind Kind) (int, error) {
	var n int64
	var err error
	switch kind {
	case KindDiscussion:
		n, err = queries.CountActiveDiscussionChatroomsByOwnerRcID(ctx, userID)
	case KindGroup:
		n, err = queries.CountOwnedGroupsByOwnerID(ctx, userID)
	case KindChatroom:
		n, err = queries.CountActivePlainChatroomsByOwnerRcID(ctx, userID)
	default:
		return 0, fmt.Errorf("quota: unknown kind %q", kind)
	}
	if err != nil {
		return 0, fmt.Errorf("quota: count %s for %s: %w", kind, userID, err)
	}
	return int(n), nil
}

// Usage 返回三类资源的 limit/used/remaining（未登录/未装配场景由调用方拦截）。
func (s *Service) Usage(ctx context.Context, userID string) (Usage, error) {
	limits, err := s.Resolve(ctx, userID)
	if err != nil {
		return Usage{}, err
	}
	usage := Usage{}
	if s.pool == nil || userID == "" {
		usage.Discussion = kindUsage(limits.DiscussionLimit, 0)
		usage.Group = kindUsage(limits.GroupLimit, 0)
		usage.Chatroom = kindUsage(limits.ChatroomLimit, 0)
		return usage, nil
	}
	queries := db.New(s.pool)
	for _, kind := range []Kind{KindDiscussion, KindGroup, KindChatroom} {
		used, cErr := s.count(ctx, queries, userID, kind)
		if cErr != nil {
			return Usage{}, cErr
		}
		switch kind {
		case KindDiscussion:
			usage.Discussion = kindUsage(limits.DiscussionLimit, used)
		case KindGroup:
			usage.Group = kindUsage(limits.GroupLimit, used)
		case KindChatroom:
			usage.Chatroom = kindUsage(limits.ChatroomLimit, used)
		}
	}
	return usage, nil
}

func kindUsage(limit, used int) KindUsage {
	remaining := limit - used
	if limit < 0 {
		remaining = -1
	} else if remaining < 0 {
		remaining = 0
	}
	return KindUsage{Limit: limit, Used: used, Remaining: remaining}
}

// Check 校验用户能否再建一个 kind 类资源：放行返回 nil，超限返回 *Error。
// limit=-1 不限直接放行；limit=0 禁止；否则 used >= limit 即超限。
//
// 临界区用 pg_advisory_xact_lock(hashtext(userID)) 串行化：同一用户的并发创建
// 在「查上限 + 数用量」期间互斥，不超发。锁随事务提交/回滚自动释放。
func (s *Service) Check(ctx context.Context, userID string, kind Kind) *Error {
	if s.pool == nil || userID == "" {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		// 数据库不可用时不引入新失败面：放行并记不到日志是这里拿不到 logger 的
		// 代价；创建链路本身的 DB 写入会紧跟着失败并如实报错。
		return nil
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(s.pool).WithTx(tx)
	if err := queries.AdvisoryLockClawIMUserQuota(ctx, userID); err != nil {
		return nil
	}
	limits, err := s.Resolve(ctx, userID)
	if err != nil {
		return nil
	}
	limit := limits.limitFor(kind)
	if limit < 0 {
		return nil
	}
	used, err := s.count(ctx, queries, userID, kind)
	if err != nil {
		return nil
	}
	if used >= limit {
		return &Error{Kind: string(kind), Limit: limit, Used: used}
	}
	return nil
}

// UpsertOverride 保存管理端的按用户覆盖（三类整体写入，nil=清除该类的覆盖）。
// updated_by 落管理员 id（最小审计）。
func (s *Service) UpsertOverride(ctx context.Context, userID string, limits OverrideLimits, adminID string) error {
	if s.pool == nil {
		return errors.New("quota: database not available")
	}
	row, err := db.New(s.pool).UpsertClawIMUserQuota(ctx, db.UpsertClawIMUserQuotaParams{
		UserID:          userID,
		DiscussionLimit: int4OrNull(limits.DiscussionLimit),
		GroupLimit:      int4OrNull(limits.GroupLimit),
		ChatroomLimit:   int4OrNull(limits.ChatroomLimit),
		UpdatedBy:       adminID,
	})
	_ = row
	if err != nil {
		return fmt.Errorf("quota: upsert override for %s: %w", userID, err)
	}
	return nil
}

// GetOverride 读取按用户覆盖行（管理端 GET 用）；无覆盖时 found=false。
func (s *Service) GetOverride(ctx context.Context, userID string) (OverrideLimits, bool, error) {
	if s.pool == nil {
		return OverrideLimits{}, false, errors.New("quota: database not available")
	}
	row, err := db.New(s.pool).GetClawIMUserQuota(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return OverrideLimits{}, false, nil
	}
	if err != nil {
		return OverrideLimits{}, false, fmt.Errorf("quota: get override for %s: %w", userID, err)
	}
	return OverrideLimits{
		DiscussionLimit: int4Ptr(row.DiscussionLimit),
		GroupLimit:      int4Ptr(row.GroupLimit),
		ChatroomLimit:   int4Ptr(row.ChatroomLimit),
	}, true, nil
}

func int4OrNull(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true}
}

func int4Ptr(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	out := int(v.Int32)
	return &out
}
