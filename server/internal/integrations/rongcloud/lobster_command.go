package rongcloud

// lobster_command.go implements the service=lobster command channel.
//
// uniapp 的「龙虾」模块（subPackages/lobster/pages/lobster/{index,edit,detail}.vue）
// 通过 IM 命令通道管理龙虾档案，客户端定义见
// clawmessenger-uniapp/src/utils/system-service-client.js:720-730：
//
//	getList   → 列表（data 是数组，页面直接 res.data 赋值）
//	getDetail → {lobsterId}
//	create    → {name, variety, weight, status}（+ 客户端顺手带的 userId，忽略）
//	update    → {lobsterId, name, variety, weight, status}
//	delete    → {lobsterId}（软删）
//
// 职责切分与 group/discussion 一致：融云侧动作留本包，表读写走 LobsterStore
// 这组回调（生产实现见本文件末尾的 NewLobsterStore，测试可整组替换）。
//
// 越权口径：所有动作都以请求发送者（msg.FromUserID → claw 用户 id）为属主，
// 查询一律带 user_id 条件；别人的记录与不存在的记录返回同一个 404（不返回 403，
// 避免被用来探测 id 是否存在）。客户端上报的 userId 不作数 —— 与 group/create
// 丢弃 ownerId 的处理同源（group_command.go:161-166）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Lobster is one 龙虾档案 as the C-end reads it back.
type Lobster struct {
	ID        string
	UserID    string
	Name      string
	Variety   string
	Weight    float64
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// LobsterPatch is a partial update; a nil field means "leave as is" so the
// client can send only what it changed (the edit page currently sends all four).
type LobsterPatch struct {
	Name    *string
	Variety *string
	Weight  *float64
	Status  *string
}

// Empty reports whether the patch carries no field at all.
func (p LobsterPatch) Empty() bool {
	return p.Name == nil && p.Variety == nil && p.Weight == nil && p.Status == nil
}

// LobsterStore is the local projection of 龙虾档案. Every callback is provided
// by the database wiring (registration.go); a nil struct disables the service
// with a 503 instead of failing the process.
//
// 每个回调都必须按 userID 过滤 —— 越权隔离是存储层的责任，handler 只负责把
// 发送者换算成 userID 并把「查不到」翻译成 404。
type LobsterStore struct {
	List       func(ctx context.Context, userID string) ([]Lobster, error)
	Get        func(ctx context.Context, userID, lobsterID string) (Lobster, bool, error)
	Create     func(ctx context.Context, lobster Lobster) (Lobster, error)
	Update     func(ctx context.Context, userID, lobsterID string, patch LobsterPatch) (Lobster, bool, error)
	SoftDelete func(ctx context.Context, userID, lobsterID string) (bool, error)
}

// defaultLobsterStatus 与客户端的初始值一致（edit.vue 的 form.status）。
const defaultLobsterStatus = "healthy"

func (h *systemHandler) handleLobsterCommand(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	if h.lobsterStore == nil {
		return h.replyErr(ctx, msg, cmd, 503, "lobster service is unavailable")
	}
	switch cmd.Action {
	case "getList":
		return h.handleLobsterList(ctx, msg, cmd)
	case "getDetail":
		return h.handleLobsterDetail(ctx, msg, cmd)
	case "create":
		return h.handleLobsterCreate(ctx, msg, cmd)
	case "update":
		return h.handleLobsterUpdate(ctx, msg, cmd)
	case "delete":
		return h.handleLobsterDelete(ctx, msg, cmd)
	default:
		return h.replyErr(ctx, msg, cmd, 404, unknownServiceOrActionMessage(cmd))
	}
}

func (h *systemHandler) handleLobsterList(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	userID, err := h.serviceActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
	}
	rows, err := h.lobsterStore.List(ctx, userID)
	if err != nil {
		h.logger.Error("rongcloud: lobster list", "error", err, "user_id", userID)
		return h.replyErr(ctx, msg, cmd, 500, "failed to list lobsters")
	}
	records := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		records = append(records, lobsterRecord(row))
	}
	// 客户端是 `this.lobsters = res.data || []`，data 必须直接是数组。
	return h.replyOK(ctx, msg, cmd, records)
}

func (h *systemHandler) handleLobsterDetail(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	lobsterID := lobsterIDParam(cmd.Params)
	if lobsterID == "" {
		return h.replyErr(ctx, msg, cmd, 400, "lobsterId is required")
	}
	userID, err := h.serviceActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
	}
	row, found, err := h.lobsterStore.Get(ctx, userID, lobsterID)
	if err != nil {
		h.logger.Error("rongcloud: lobster detail", "error", err, "lobster_id", lobsterID)
		return h.replyErr(ctx, msg, cmd, 500, "failed to load the lobster")
	}
	if !found {
		return h.lobsterNotFound(ctx, msg, cmd)
	}
	return h.replyOK(ctx, msg, cmd, lobsterRecord(row))
}

func (h *systemHandler) handleLobsterCreate(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	name := strings.TrimSpace(getString(cmd.Params, "name"))
	if name == "" {
		return h.replyErr(ctx, msg, cmd, 400, "name is required")
	}
	userID, err := h.serviceActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
	}
	weight := 0.0
	if parsed, ok := lobsterWeightPtr(cmd.Params["weight"]); ok && parsed != nil {
		weight = *parsed
	}
	status := strings.TrimSpace(getString(cmd.Params, "status"))
	if status == "" {
		status = defaultLobsterStatus
	}
	row, err := h.lobsterStore.Create(ctx, Lobster{
		ID:      newLobsterID(),
		UserID:  userID,
		Name:    name,
		Variety: strings.TrimSpace(getString(cmd.Params, "variety")),
		Weight:  weight,
		Status:  status,
	})
	if err != nil {
		h.logger.Error("rongcloud: lobster create", "error", err, "user_id", userID)
		return h.replyErr(ctx, msg, cmd, 500, "failed to create the lobster")
	}
	h.logger.Info("rongcloud: lobster created", "lobster_id", row.ID, "user_id", userID)
	return h.replyOK(ctx, msg, cmd, lobsterRecord(row))
}

func (h *systemHandler) handleLobsterUpdate(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	lobsterID := lobsterIDParam(cmd.Params)
	if lobsterID == "" {
		return h.replyErr(ctx, msg, cmd, 400, "lobsterId is required")
	}
	userID, err := h.serviceActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
	}
	patch := lobsterPatchFromParams(cmd.Params)
	if patch.Empty() {
		return h.replyErr(ctx, msg, cmd, 400, "no fields to update")
	}
	row, found, err := h.lobsterStore.Update(ctx, userID, lobsterID, patch)
	if err != nil {
		h.logger.Error("rongcloud: lobster update", "error", err, "lobster_id", lobsterID)
		return h.replyErr(ctx, msg, cmd, 500, "failed to update the lobster")
	}
	if !found {
		return h.lobsterNotFound(ctx, msg, cmd)
	}
	return h.replyOK(ctx, msg, cmd, lobsterRecord(row))
}

func (h *systemHandler) handleLobsterDelete(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	lobsterID := lobsterIDParam(cmd.Params)
	if lobsterID == "" {
		return h.replyErr(ctx, msg, cmd, 400, "lobsterId is required")
	}
	userID, err := h.serviceActor(ctx, msg.FromUserID)
	if err != nil {
		return h.replyErr(ctx, msg, cmd, 401, "sender not recognized")
	}
	deleted, err := h.lobsterStore.SoftDelete(ctx, userID, lobsterID)
	if err != nil {
		h.logger.Error("rongcloud: lobster delete", "error", err, "lobster_id", lobsterID)
		return h.replyErr(ctx, msg, cmd, 500, "failed to delete the lobster")
	}
	if !deleted {
		return h.lobsterNotFound(ctx, msg, cmd)
	}
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{
		"lobsterId": lobsterID,
		"deleted":   true,
	})
}

// lobsterNotFound 统一「不存在 / 不属于你」的出口：两者返回同一个 404，
// 否则 403 就等于告诉调用方「这个 id 真实存在」。
func (h *systemHandler) lobsterNotFound(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	return h.replyErr(ctx, msg, cmd, 404, "lobster not found")
}

// lobsterRecord renders one row the way the client reads it (edit.vue / detail.vue
// 都读 name / variety / weight / status / createdAt / updatedAt)。
func lobsterRecord(l Lobster) map[string]interface{} {
	return map[string]interface{}{
		"id":        l.ID,
		"userId":    l.UserID,
		"name":      l.Name,
		"variety":   l.Variety,
		"weight":    l.Weight,
		"status":    l.Status,
		"createdAt": l.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt": l.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// lobsterIDParam 兼容两种写法：客户端发 lobsterId，detail 页的 URL 参数叫 id。
func lobsterIDParam(params map[string]interface{}) string {
	return strings.TrimSpace(getStringAny(params, "lobsterId", "lobster_id", "id"))
}

// lobsterPatchFromParams 只把「确实出现」的键放进 patch —— 缺省键不能写成
// 零值，否则 update 会把未提交的字段清空。
func lobsterPatchFromParams(params map[string]interface{}) LobsterPatch {
	var patch LobsterPatch
	if raw, ok := params["name"]; ok {
		if s, ok := raw.(string); ok {
			trimmed := strings.TrimSpace(s)
			patch.Name = &trimmed
		}
	}
	if raw, ok := params["variety"]; ok {
		if s, ok := raw.(string); ok {
			trimmed := strings.TrimSpace(s)
			patch.Variety = &trimmed
		}
	}
	if raw, ok := params["status"]; ok {
		if s, ok := raw.(string); ok {
			trimmed := strings.TrimSpace(s)
			if trimmed != "" {
				patch.Status = &trimmed
			}
		}
	}
	if weight, ok := lobsterWeightPtr(params["weight"]); ok {
		patch.Weight = weight
	}
	return patch
}

// lobsterWeightPtr 是数值参数的三态写法（同 groupMaxRoundsPtr）：JSON 数字解成
// float64、表单可能给字符串、缺省给 nil。负值夹到 0（重量没有负数）。
func lobsterWeightPtr(value interface{}) (*float64, bool) {
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case int:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil, false
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return nil, false
		}
		number = parsed
	default:
		return nil, false
	}
	if number < 0 {
		number = 0
	}
	return &number, true
}

// newLobsterID mints an opaque TEXT id. 客户端把它当字符串透传（详情页拼在 URL
// query 里），所以只需要全局唯一、无需可读。
func newLobsterID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// rand.Read 失败意味着系统熵源异常；用时间戳兜底总比返回空 id 好，
		// 主键冲突会由数据库唯一约束拦下并回 500。
		return "lob_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "lob_" + hex.EncodeToString(buf)
}

// NewLobsterStore 是 LobsterStore 的生产实现：表在 migrations/599。
//
// 每个查询都带 user_id，越权与不存在走同一个「零行」路径；删除只写
// deleted_at。dbtx 为空时返回 nil（对应 service 回 503，不 panic）。
func NewLobsterStore(dbtx db.DBTX) *LobsterStore {
	if dbtx == nil {
		return nil
	}
	store := &LobsterStore{}

	store.List = func(ctx context.Context, userID string) ([]Lobster, error) {
		rows, err := dbtx.Query(ctx, `
			SELECT id, user_id, name, variety, weight, status, created_at, updated_at
			FROM claw_lobster
			WHERE user_id = $1 AND deleted_at IS NULL
			ORDER BY created_at DESC`, userID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := make([]Lobster, 0, 8)
		for rows.Next() {
			var row Lobster
			if err := rows.Scan(&row.ID, &row.UserID, &row.Name, &row.Variety,
				&row.Weight, &row.Status, &row.CreatedAt, &row.UpdatedAt); err != nil {
				return nil, err
			}
			out = append(out, row)
		}
		return out, rows.Err()
	}

	store.Get = func(ctx context.Context, userID, lobsterID string) (Lobster, bool, error) {
		var row Lobster
		err := dbtx.QueryRow(ctx, `
			SELECT id, user_id, name, variety, weight, status, created_at, updated_at
			FROM claw_lobster
			WHERE user_id = $1 AND id = $2 AND deleted_at IS NULL`, userID, lobsterID).
			Scan(&row.ID, &row.UserID, &row.Name, &row.Variety,
				&row.Weight, &row.Status, &row.CreatedAt, &row.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return Lobster{}, false, nil
		}
		if err != nil {
			return Lobster{}, false, err
		}
		return row, true, nil
	}

	store.Create = func(ctx context.Context, lobster Lobster) (Lobster, error) {
		var row Lobster
		err := dbtx.QueryRow(ctx, `
			INSERT INTO claw_lobster (id, user_id, name, variety, weight, status)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, user_id, name, variety, weight, status, created_at, updated_at`,
			lobster.ID, lobster.UserID, lobster.Name, lobster.Variety,
			lobster.Weight, lobster.Status).
			Scan(&row.ID, &row.UserID, &row.Name, &row.Variety,
				&row.Weight, &row.Status, &row.CreatedAt, &row.UpdatedAt)
		if err != nil {
			return Lobster{}, err
		}
		return row, nil
	}

	store.Update = func(ctx context.Context, userID, lobsterID string, patch LobsterPatch) (Lobster, bool, error) {
		// COALESCE 让「没提交的字段保持原值」落在一条语句里（补丁语义），
		// 不需要先读后写，也就没有读写之间的并发覆盖。
		var row Lobster
		err := dbtx.QueryRow(ctx, `
			UPDATE claw_lobster SET
				name       = COALESCE($3::text, name),
				variety    = COALESCE($4::text, variety),
				weight     = COALESCE($5::double precision, weight),
				status     = COALESCE($6::text, status),
				updated_at = now()
			WHERE user_id = $1 AND id = $2 AND deleted_at IS NULL
			RETURNING id, user_id, name, variety, weight, status, created_at, updated_at`,
			userID, lobsterID, patch.Name, patch.Variety, patch.Weight, patch.Status).
			Scan(&row.ID, &row.UserID, &row.Name, &row.Variety,
				&row.Weight, &row.Status, &row.CreatedAt, &row.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return Lobster{}, false, nil
		}
		if err != nil {
			return Lobster{}, false, err
		}
		return row, true, nil
	}

	store.SoftDelete = func(ctx context.Context, userID, lobsterID string) (bool, error) {
		tag, err := dbtx.Exec(ctx, `
			UPDATE claw_lobster SET deleted_at = now(), updated_at = now()
			WHERE user_id = $1 AND id = $2 AND deleted_at IS NULL`, userID, lobsterID)
		if err != nil {
			return false, err
		}
		return tag.RowsAffected() > 0, nil
	}

	return store
}
