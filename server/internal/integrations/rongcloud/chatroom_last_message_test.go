package rongcloud

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestCondenseLastMessageText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空字符串", "", ""},
		{"纯空白", "  \n\t ", ""},
		{"换行折叠成空格", "第一段\n第二段", "第一段 第二段"},
		{"CRLF 也折叠", "第一段\r\n第二段", "第一段 第二段"},
		{"连续空白只留一个", "结论：  可行\t\t但是", "结论： 可行 但是"},
		{"去掉 markdown 星号与标题井号", "## 结论\n**很重要**", "结论 很重要"},
		{"去掉列表前缀与引用符号", "- 第一条\n> 引用", "第一条 引用"},
		{"竖线当空格（表格）", "| 甲 | 乙 |", "甲 乙"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := condenseLastMessageText(c.in); got != c.want {
				t.Fatalf("condenseLastMessageText(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	// 超长按 rune 截断：24 个汉字是 72 字节，按字节切会切坏字符。
	long := strings.Repeat("汉", 120)
	got := condenseLastMessageText(long)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis suffix, got %q", got)
	}
	body := strings.TrimSuffix(got, "…")
	if n := len([]rune(body)); n != chatroomLastMessageMaxRunes {
		t.Fatalf("expected %d runes before ellipsis, got %d", chatroomLastMessageMaxRunes, n)
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatalf("rune cut produced a broken character: %q", got)
		}
	}
}

func TestStripLeadingSectionLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"【已达成的共识】 1. 事实成立", "1. 事实成立"},
		{"【仍存在的分歧】执行长坚持原判", "执行长坚持原判"},
		{"【已达成的共识】", "【已达成的共识】"},        // 剥完为空：保留原文
		{"【已达成的共识】   ", "【已达成的共识】   "},    // 去掉标题后只剩空白：同样保留原文
		{"第3轮纪要 成员立场", "第3轮纪要 成员立场"},     // 没有段标题就不动
		{"【这是一个非常非常长的标题超过了十二个字】正文", "【这是一个非常非常长的标题超过了十二个字】正文"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripLeadingSectionLabel(c.in); got != c.want {
			t.Fatalf("stripLeadingSectionLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSummarizeChatroomLastEvent(t *testing.T) {
	at := pgtype.Timestamptz{Time: time.Date(2026, 10, 23, 5, 27, 22, 0, time.UTC), Valid: true}
	row := func(eventType, payload, role string) db.ListRongCloudChatroomLastEventsRow {
		return db.ListRongCloudChatroomLastEventsRow{
			ChatroomID: "room-1",
			EventType:  eventType,
			ActorID:    pgtype.Text{String: "rc_node_1", Valid: true},
			CreatedAt:  at,
			Payload:    []byte(payload),
			RoleName:   role,
		}
	}

	t.Run("成员发言带角色名", func(t *testing.T) {
		got, ok := summarizeChatroomLastEvent(row("contribution_completed",
			`{"round":1,"content":"结论：先做最小验证。"}`, "执行长"))
		if !ok {
			t.Fatal("expected ok")
		}
		if got.Kind != lastMessageKindContribution || got.Speaker != "执行长" || got.Text != "结论：先做最小验证。" {
			t.Fatalf("unexpected summary: %+v", got)
		}
		if got.At != at {
			t.Fatalf("expected event timestamp kept, got %v", got.At)
		}
	})

	t.Run("主持人总结取正文而不是标题", func(t *testing.T) {
		got, ok := summarizeChatroomLastEvent(row("round_summary_completed",
			`{"round":1,"title":"Round 1 summary","content":"【已达成的共识】\n暂无。"}`, "中立主持人"))
		if !ok {
			t.Fatal("expected ok")
		}
		if got.Kind != lastMessageKindSummary || got.Speaker != "中立主持人" {
			t.Fatalf("unexpected summary: %+v", got)
		}
		// 段标题被剥掉，露出正文；轮次从 payload.round 取
		if got.Text != "暂无。" {
			t.Fatalf("expected section label stripped, got %q", got.Text)
		}
		if got.Round != 1 {
			t.Fatalf("expected round 1, got %d", got.Round)
		}
	})

	t.Run("总结身份丢了退回主持人", func(t *testing.T) {
		got, _ := summarizeChatroomLastEvent(row("round_summary_completed",
			`{"content":"本周结论"}`, ""))
		if got.Speaker != "主持人" {
			t.Fatalf("expected fallback speaker 主持人, got %q", got.Speaker)
		}
	})

	t.Run("用户插话显示为你", func(t *testing.T) {
		got, ok := summarizeChatroomLastEvent(row("user_interjection",
			`{"content":"继续呗","memberId":"850509"}`, ""))
		if !ok || got.Kind != lastMessageKindInterjection || got.Speaker != "你" || got.Text != "继续呗" {
			t.Fatalf("unexpected summary: %+v ok=%v", got, ok)
		}
	})

	t.Run("收尾事件给固定文案", func(t *testing.T) {
		cancelled, ok := summarizeChatroomLastEvent(row("discussion_cancelled", `{"reason":"abandoned"}`, ""))
		if !ok || cancelled.Kind != lastMessageKindCancelled || cancelled.Text != "讨论已结束" {
			t.Fatalf("unexpected cancelled: %+v", cancelled)
		}
		failed, ok := summarizeChatroomLastEvent(row("discussion_failed", `{"reason":"summary_failed"}`, ""))
		if !ok || failed.Kind != lastMessageKindFailed || failed.Text != "讨论中断，可重试" {
			t.Fatalf("unexpected failed: %+v", failed)
		}
	})

	t.Run("空正文的事件不出预览", func(t *testing.T) {
		if _, ok := summarizeChatroomLastEvent(row("contribution_completed", `{"content":"   "}`, "执行长")); ok {
			t.Fatal("expected empty contribution to be skipped")
		}
	})

	t.Run("坏 payload 不 panic，按空处理", func(t *testing.T) {
		if _, ok := summarizeChatroomLastEvent(row("round_summary_completed", `{not json`, "")); ok {
			t.Fatal("expected malformed payload to be skipped")
		}
	})

	t.Run("不认识的类型不展示", func(t *testing.T) {
		if _, ok := summarizeChatroomLastEvent(row("round_checkpoint", `{"round":1}`, "")); ok {
			t.Fatal("expected checkpoint to be skipped")
		}
	})
}

// 链路：getList 要把每间房最近一条「可展示」事件带出来，且跳过比它更新的
// 中间态事件（round_checkpoint 才是时间上最后一条，但它不是消息）。
func TestChatroomGetListIncludesLastMessage(t *testing.T) {
	f := setupChatroomFixtureFull(t)
	if f == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	room, err := f.queries.GetActiveRongCloudChatroomViewByRongCloudID(ctx, f.roomRCID)
	if err != nil {
		t.Fatalf("load room: %v", err)
	}
	discussionID := "disc_lastmsg_" + uniqueID("t")
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO rongcloud_discussion
			(discussion_id, chatroom_id, workspace_id, created_by, topic, status, max_rounds)
		VALUES ($1, $2, $3, $4, $5, 'completed', 10)`,
		discussionID, f.roomRCID, f.wsID, f.owner.RongcloudUserID, "最后一条消息测试",
	); err != nil {
		t.Fatalf("insert discussion: %v", err)
	}

	base := time.Now().Add(-10 * time.Minute)
	insertEvent := func(eventType string, at time.Time, payload map[string]interface{}, actorID string) {
		t.Helper()
		raw, _ := json.Marshal(payload)
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO rongcloud_discussion_event
				(chatroom_id, workspace_id, event_type, round_number, content, discussion_id, actor_id, payload, created_at)
			VALUES ($1, $2, $3, 1, '{}'::jsonb, $4, $5, $6, $7)`,
			room.ID, f.wsID, eventType, discussionID, actorID, raw, at,
		); err != nil {
			t.Fatalf("insert event %s: %v", eventType, err)
		}
	}

	insertEvent("contribution_completed", base, map[string]interface{}{
		"round": 1, "content": "结论：先做最小验证。",
	}, f.memberNode.RongcloudUserID)
	insertEvent("round_summary_completed", base.Add(time.Minute), map[string]interface{}{
		"round": 1, "title": "Round 1 summary", "content": "【已达成的共识】\n暂无。",
	}, f.hostNode.RongcloudUserID)
	// 时间上最新，但不是「消息」：不能被选中，否则列表会显示一段结构化 JSON 的正文。
	insertEvent("round_checkpoint", base.Add(2*time.Minute), map[string]interface{}{
		"round": 1, "recommendation": "continue",
	}, "system")

	r := f.h.actionChatroomGetList(ctx, CommandContent{
		Service: "chatroom", Action: "getList",
		Params: map[string]interface{}{"page": 1, "pageSize": 100},
	}, f.owner)
	if r.code != 200 {
		t.Fatalf("getList failed: %d %s", r.code, r.message)
	}
	data, _ := r.data.(map[string]interface{})
	list, _ := data["list"].([]map[string]interface{})
	var target map[string]interface{}
	for _, roomRecord := range list {
		if roomRecord["chatroomId"] == f.roomRCID {
			target = roomRecord
			break
		}
	}
	if target == nil {
		t.Fatalf("room %q missing from list", f.roomRCID)
	}
	if got := target["lastMessageKind"]; got != lastMessageKindSummary {
		t.Fatalf("expected summary kind, got %v", got)
	}
	// 创建聊天室时服务端会把 host 的 role_name 落成「中立主持人」（默认主持角色），
	// 所以这里断言的是房间成员上的真实角色名，不是建组参数里写的 "host"。
	if got := target["lastSpeaker"]; got != "中立主持人" {
		t.Fatalf("expected speaker from role_name, got %v", got)
	}
	if got, _ := target["lastMessage"].(string); got != "暂无。" {
		t.Fatalf("unexpected lastMessage %q", got)
	}
	if got := target["lastMessageRound"]; got != int32(1) {
		t.Fatalf("expected lastMessageRound 1, got %v", got)
	}
	if _, ok := target["lastMessageTime"]; !ok {
		t.Fatal("expected lastMessageTime in list record")
	}
}
