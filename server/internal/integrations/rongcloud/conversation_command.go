package rongcloud

// conversation_command.go implements the service=conversation command channel.
//
// uniapp 的「个人资料 → 发消息」（subPackages/social/pages/user-profile/index.vue）
// 会先把会话号落一份副本：`conversationService.save({targetId, conversationType,
// conversationId, ...})`，客户端定义见
// clawmessenger-uniapp/src/utils/system-service-client.js:792-795。
//
// 这是从旧 Python 后端移植时漏掉的一个 service（旧实现见
// clawmessenger-server/rongcloud/command_handler.py:5754-5810 的
// _handle_conversation_save / _handle_conversation_get）。漏掉的后果不是功能坏，
// 而是每次点「发消息」都收到 404「未知的服务或操作: conversation/save」，
// 小程序控制台整片报红（2026-10-26 现场）—— 客户端虽然 catch 住了继续跳转，
// 但没人能从这条红字里看出「其实不影响使用」。
//
// 职责边界：这里只存「融云会话号」的映射副本，会话本身归融云 SDK 与
// conversationStore 管；表里丢了也不会让任何会话不可用。所以 save 幂等
// （UPSERT），get 查不到就返回 conversationId: null，不报错。
//
// 越权口径：不做 user 维度隔离 —— 旧实现同样只有 (target_id, conversation_type)
// 唯一键，没有属主列。这张表是「全局会话号索引」，加属主列会让旧客户端的
// ON CONFLICT 失配（见 migrations/605 的注释）。

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ConversationRecord 是 im_conversations 的一行（不含自增 id 与时间戳）。
type ConversationRecord struct {
	TargetID         string
	ConversationType int
	ConversationID   string
}

// ConversationStore 是 im_conversations 的读写回调（生产实现见文件末尾，
// 测试可整组替换 —— 与 LobsterStore 同一套做法）。
type ConversationStore struct {
	Save func(ctx context.Context, rec ConversationRecord) error
	// Get 返回 (conversationId, found, err)。
	Get func(ctx context.Context, targetID string, conversationType int) (string, bool, error)
}

const (
	// 与旧实现一致：conversationType 缺省 1（PRIVATE）。
	defaultConversationType = 1
	maxConversationTargetLen = 128
	maxConversationIDLen    = 256
)

func (h *systemHandler) handleConversationCommand(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	if h.conversations == nil {
		return h.replyErr(ctx, msg, cmd, 503, "conversation service is unavailable")
	}
	switch cmd.Action {
	case "save":
		return h.handleConversationSave(ctx, msg, cmd)
	case "get":
		return h.handleConversationGet(ctx, msg, cmd)
	default:
		return h.replyErr(ctx, msg, cmd, 404, unknownServiceOrActionMessage(cmd))
	}
}

func (h *systemHandler) handleConversationSave(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	rec, errMsg := conversationRecordFromParams(cmd.Params)
	if errMsg != "" {
		return h.replyErr(ctx, msg, cmd, 400, errMsg)
	}
	if err := h.conversations.Save(ctx, rec); err != nil {
		h.logger.Error("rongcloud: conversation save",
			"error", err, "target_id", rec.TargetID, "conversation_type", rec.ConversationType)
		return h.replyErr(ctx, msg, cmd, 500, "failed to save the conversation")
	}
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{"saved": true})
}

func (h *systemHandler) handleConversationGet(ctx context.Context, msg NormalizedMessage, cmd CommandContent) error {
	targetID := strings.TrimSpace(getStringAny(cmd.Params, "targetId", "target_id"))
	if targetID == "" {
		return h.replyErr(ctx, msg, cmd, 400, "targetId is required")
	}
	conversationType := conversationTypeFromParams(cmd.Params)

	conversationID, found, err := h.conversations.Get(ctx, targetID, conversationType)
	if err != nil {
		h.logger.Error("rongcloud: conversation get", "error", err,
			"target_id", targetID, "conversation_type", conversationType)
		return h.replyErr(ctx, msg, cmd, 500, "failed to load the conversation")
	}
	if !found {
		// 旧实现同样返回 200 + conversationId: null，客户端据此判断「还没落库」。
		return h.replyOK(ctx, msg, cmd, map[string]interface{}{"conversationId": nil})
	}
	return h.replyOK(ctx, msg, cmd, map[string]interface{}{"conversationId": conversationID})
}

// conversationRecordFromParams 取参并校验。返回的 errMsg 非空表示参数不合法。
func conversationRecordFromParams(params map[string]interface{}) (ConversationRecord, string) {
	targetID := strings.TrimSpace(getStringAny(params, "targetId", "target_id"))
	conversationID := strings.TrimSpace(getStringAny(params, "conversationId", "conversation_id"))
	if targetID == "" || conversationID == "" {
		// 与旧后端的报错文案一致（command_handler.py:5760）。
		return ConversationRecord{}, "targetId 和 conversationId 不能为空"
	}
	if len(targetID) > maxConversationTargetLen || len(conversationID) > maxConversationIDLen {
		return ConversationRecord{}, "targetId or conversationId is too long"
	}
	return ConversationRecord{
		TargetID:         targetID,
		ConversationType: conversationTypeFromParams(params),
		ConversationID:   conversationID,
	}, ""
}

// conversationTypeFromParams 读 conversationType，兼容数字与字符串两种写法
// （旧实现按 payload.get("conversationType", 1) 直接入库，字符串也照收）。
func conversationTypeFromParams(params map[string]interface{}) int {
	raw, ok := params["conversationType"]
	if !ok {
		raw = params["conversation_type"]
	}
	switch value := raw.(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	case string:
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			if parsed, err := parseIntStrict(trimmed); err == nil {
				return parsed
			}
		}
	}
	return defaultConversationType
}

// parseIntStrict 只接受十进制整数串，不做隐式截断。
func parseIntStrict(value string) (int, error) {
	negative := false
	if strings.HasPrefix(value, "-") {
		negative = true
		value = value[1:]
	}
	if value == "" {
		return 0, errors.New("empty number")
	}
	result := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		result = result*10 + int(r-'0')
	}
	if negative {
		result = -result
	}
	return result, nil
}

// NewConversationStore 构造 im_conversations 的生产读写实现（migrations/605）。
// dbtx 为 nil 时返回 nil —— 调用方据此回 503，与其它 store 一致。
func NewConversationStore(dbtx db.DBTX) *ConversationStore {
	if dbtx == nil {
		return nil
	}
	store := &ConversationStore{}

	store.Save = func(ctx context.Context, rec ConversationRecord) error {
		_, err := dbtx.Exec(ctx, `
			INSERT INTO im_conversations (target_id, conversation_type, conversation_id)
			VALUES ($1, $2, $3)
			ON CONFLICT (target_id, conversation_type) DO UPDATE
			SET conversation_id = EXCLUDED.conversation_id,
			    created_at = now()`,
			rec.TargetID, rec.ConversationType, rec.ConversationID)
		return err
	}

	store.Get = func(ctx context.Context, targetID string, conversationType int) (string, bool, error) {
		var conversationID string
		err := dbtx.QueryRow(ctx, `
			SELECT conversation_id FROM im_conversations
			WHERE target_id = $1 AND conversation_type = $2`,
			targetID, conversationType).Scan(&conversationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return conversationID, true, nil
	}

	return store
}
