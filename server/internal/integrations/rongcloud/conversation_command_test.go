package rongcloud

// conversation_command_test.go covers service=conversation.
//
// 现场（2026-10-26）：小程序「个人资料 → 发消息」每次都在控制台报
// 「保存会话失败: Error: 未知的服务或操作: conversation/save」——Go 端从旧
// Python 后端迁移时漏了这个 service。本文件守住三件事：
//   1. save/get 两个 action 真的能落地，且默认/兼容写法都对；
//   2. 参数错、表写失败、store 未装配各有明确回包，不再落到 404；
//   3. 未知 action 仍走 404 契约（前端 isLegacyChatroomActionError 依赖这段文案）。

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// conversationFixture 是 im_conversations 的内存实现，行为与 SQL 版一致：
// (targetID, conversationType) 唯一，重复 save 覆盖 conversationID。
type conversationFixture struct {
	mu   sync.Mutex
	rows map[string]ConversationRecord
	save error // 非 nil 时模拟写失败
	get  error // 非 nil 时模拟读失败
}

func newConversationFixture() *conversationFixture {
	return &conversationFixture{rows: make(map[string]ConversationRecord)}
}

func (f *conversationFixture) key(targetID string, conversationType int) string {
	return targetID + "|" + itoa(conversationType)
}

// itoa 避免为了一行格式化引入 strconv（测试文件里只用到非负整数）。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 4)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func (f *conversationFixture) store() *ConversationStore {
	return &ConversationStore{
		Save: func(_ context.Context, rec ConversationRecord) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.save != nil {
				return f.save
			}
			f.rows[f.key(rec.TargetID, rec.ConversationType)] = rec
			return nil
		},
		Get: func(_ context.Context, targetID string, conversationType int) (string, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.get != nil {
				return "", false, f.get
			}
			rec, ok := f.rows[f.key(targetID, conversationType)]
			if !ok {
				return "", false, nil
			}
			return rec.ConversationID, true, nil
		},
	}
}

func newConversationHandler(t *testing.T, fixture *conversationFixture) (*imServiceServer, *systemHandler) {
	t.Helper()
	rec, srv := newIMServiceServer(t, imServiceServerOptions{})
	t.Cleanup(srv.Close)
	handler := newIMServiceHandler(t, srv)
	handler.conversations = fixture.store()
	return rec, handler
}

func wantWebSuccess(t *testing.T, reply map[string]interface{}) map[string]interface{} {
	t.Helper()
	if reply["status"] != "success" {
		t.Fatalf("status = %#v, want success (reply %#v)", reply["status"], reply)
	}
	data, _ := reply["data"].(map[string]interface{})
	return data
}

func TestConversationSavePersistsAndIsIdempotent(t *testing.T) {
	fixture := newConversationFixture()
	rec, handler := newConversationHandler(t, fixture)

	sendWebCommand(t, handler, "user-7", "conversation", "save", map[string]interface{}{
		"targetId":         "codex_3300776275",
		"conversationType": 1,
		"conversationId":   "[codex_3300776275][1][]",
	})
	wantWebSuccess(t, rec.last(t))

	if got := fixture.rows[fixture.key("codex_3300776275", 1)]; got.ConversationID != "[codex_3300776275][1][]" {
		t.Fatalf("stored row = %#v, want the conversation id from the request", got)
	}

	// 同一个 (targetId, type) 再存一次是覆盖，不是新增第二行。
	sendWebCommand(t, handler, "user-7", "conversation", "save", map[string]interface{}{
		"targetId":       "codex_3300776275",
		"conversationId": "[codex_3300776275][1][]",
	})
	wantWebSuccess(t, rec.last(t))
	if len(fixture.rows) != 1 {
		t.Fatalf("rows = %d, want 1 (save must upsert on targetId+conversationType)", len(fixture.rows))
	}
}

func TestConversationSaveAcceptsSnakeCaseAndStringType(t *testing.T) {
	fixture := newConversationFixture()
	rec, handler := newConversationHandler(t, fixture)

	// 旧客户端/其它端可能发 snake_case，conversationType 也可能是字符串。
	sendWebCommand(t, handler, "user-7", "conversation", "save", map[string]interface{}{
		"target_id":         "g-1",
		"conversation_type": "3",
		"conversation_id":   "[g-1][3][]",
	})
	wantWebSuccess(t, rec.last(t))

	if _, ok := fixture.rows[fixture.key("g-1", 3)]; !ok {
		t.Fatalf("rows = %#v, want a row for (g-1, 3)", fixture.rows)
	}
}

func TestConversationSaveDefaultsConversationTypeToPrivate(t *testing.T) {
	fixture := newConversationFixture()
	rec, handler := newConversationHandler(t, fixture)

	sendWebCommand(t, handler, "user-7", "conversation", "save", map[string]interface{}{
		"targetId":       "u-2",
		"conversationId": "[u-2][1][]",
	})
	wantWebSuccess(t, rec.last(t))

	if _, ok := fixture.rows[fixture.key("u-2", 1)]; !ok {
		t.Fatalf("rows = %#v, want the default conversation type 1", fixture.rows)
	}
}

func TestConversationSaveRejectsMissingFields(t *testing.T) {
	fixture := newConversationFixture()
	rec, handler := newConversationHandler(t, fixture)

	// 与旧后端文案一致：targetId 和 conversationId 不能为空。
	sendWebCommand(t, handler, "user-7", "conversation", "save", map[string]interface{}{
		"targetId": "u-2",
	})
	wantWebError(t, rec.last(t), 400, "targetId 和 conversationId 不能为空")
	if len(fixture.rows) != 0 {
		t.Fatalf("rows = %d, want 0 (an invalid save must not write)", len(fixture.rows))
	}

	sendWebCommand(t, handler, "user-7", "conversation", "save", map[string]interface{}{
		"conversationId": "[u-2][1][]",
	})
	wantWebError(t, rec.last(t), 400, "targetId 和 conversationId 不能为空")
}

func TestConversationGetReturnsNullWhenAbsent(t *testing.T) {
	fixture := newConversationFixture()
	rec, handler := newConversationHandler(t, fixture)

	sendWebCommand(t, handler, "user-7", "conversation", "get", map[string]interface{}{
		"targetId": "u-2",
	})
	data := wantWebSuccess(t, rec.last(t))
	// 旧实现返回 200 + conversationId: null，客户端据此判断「还没落库」。
	if value, ok := data["conversationId"]; !ok || value != nil {
		t.Fatalf("conversationId = %#v, want an explicit null", value)
	}

	sendWebCommand(t, handler, "user-7", "conversation", "save", map[string]interface{}{
		"targetId":       "u-2",
		"conversationId": "[u-2][1][]",
	})
	wantWebSuccess(t, rec.last(t))

	sendWebCommand(t, handler, "user-7", "conversation", "get", map[string]interface{}{
		"targetId": "u-2",
	})
	data = wantWebSuccess(t, rec.last(t))
	if data["conversationId"] != "[u-2][1][]" {
		t.Fatalf("conversationId = %#v, want the stored id", data["conversationId"])
	}
}

func TestConversationStoreFailuresReply500(t *testing.T) {
	fixture := newConversationFixture()
	rec, handler := newConversationHandler(t, fixture)

	fixture.save = errors.New("boom")
	sendWebCommand(t, handler, "user-7", "conversation", "save", map[string]interface{}{
		"targetId":       "u-2",
		"conversationId": "[u-2][1][]",
	})
	wantWebError(t, rec.last(t), 500, "failed to save the conversation")

	fixture.get = errors.New("boom")
	sendWebCommand(t, handler, "user-7", "conversation", "get", map[string]interface{}{
		"targetId": "u-2",
	})
	wantWebError(t, rec.last(t), 500, "failed to load the conversation")
}

func TestConversationWithoutStoreReplies503(t *testing.T) {
	rec, srv := newIMServiceServer(t, imServiceServerOptions{})
	defer srv.Close()
	handler := newIMServiceHandler(t, srv)

	sendWebCommand(t, handler, "user-7", "conversation", "save", map[string]interface{}{
		"targetId":       "u-2",
		"conversationId": "[u-2][1][]",
	})
	wantWebError(t, rec.last(t), 503, "conversation service is unavailable")
}

func TestConversationUnknownActionKeepsTheLegacy404Contract(t *testing.T) {
	fixture := newConversationFixture()
	rec, handler := newConversationHandler(t, fixture)

	sendWebCommand(t, handler, "user-7", "conversation", "feedConversation", map[string]interface{}{})
	wantWebError(t, rec.last(t), 404, "未知的服务或操作", "conversation/feedConversation")
}
