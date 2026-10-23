package rongcloud

// chatroom_last_message.go turns the newest displayable discussion event of a
// chatroom into the one-line preview the conversation list shows.
//
// Why this exists: chatrooms do not keep history on RongCloud's side, so a
// conversation list built from IM data alone has nothing to render for a
// discussion group — the mini program ended up showing the same placeholder
// ("点击进入") on every row forever. The discussion event log is the only
// reliable timeline, so getList attaches one summary per room.
//
// The row itself is produced by ListRongCloudChatroomLastEvents; this file owns
// the "which events count as a message, and how does it read" policy so both
// clients stay in sync.

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Preview length budget. The list row fits roughly 20 CJK characters; the extra
// headroom keeps a western sentence from being cut mid-word by the client's
// ellipsis too early, while still bounding the payload.
const chatroomLastMessageMaxRunes = 80

// Event kinds exposed to clients. Clients render them differently (a summary is
// labelled 纪要, a contribution is prefixed with the speaker) but never need to
// know the raw event_type strings.
const (
	lastMessageKindSummary      = "summary"
	lastMessageKindContribution = "contribution"
	lastMessageKindInterjection = "interjection"
	lastMessageKindCancelled    = "cancelled"
	lastMessageKindFailed       = "failed"
)

// chatroomLastEventPayload is the subset of the event payload this file reads.
// The payload shapes differ per event_type, so every field is optional.
type chatroomLastEventPayload struct {
	Content string `json:"content"`
	Title   string `json:"title"`
	Reason  string `json:"reason"`
	Round   int32  `json:"round"`
}

// chatroomLastEventSummary is what getList attaches to a room record.
type chatroomLastEventSummary struct {
	Kind    string
	Speaker string
	Text    string
	At      interface{}
	// Round 是 1-based 轮次，0 表示事件里没带。客户端用它把「第 N 轮纪要」拼出来：
	// 同一个组在列表里隔几轮再看，只有正文是分不出这是哪一轮的。
	Round int32
}

// applyChatroomLastEvents decorates the paginated room list with their newest
// displayable event. A failure here must not fail getList: the list itself is
// what the client needs, the preview is decoration.
func (h *systemHandler) applyChatroomLastEvents(
	ctx context.Context,
	rooms []db.ListRongCloudChatroomViewsForUserRow,
	records []map[string]interface{},
) {
	if len(rooms) == 0 || len(records) != len(rooms) {
		return
	}
	ids := make([]string, 0, len(rooms))
	for _, r := range rooms {
		if id := r.RongcloudChatroomID; id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	rows, err := h.queries.ListRongCloudChatroomLastEvents(ctx, ids)
	if err != nil {
		h.logger.Warn("rongcloud: chatroom last events", "error", err)
		return
	}
	byRoom := make(map[string]db.ListRongCloudChatroomLastEventsRow, len(rows))
	for _, row := range rows {
		byRoom[row.ChatroomID] = row
	}
	for i, r := range rooms {
		row, ok := byRoom[r.RongcloudChatroomID]
		if !ok {
			continue
		}
		summary, ok := summarizeChatroomLastEvent(row)
		if !ok {
			continue
		}
		rec := records[i]
		rec["lastMessage"] = summary.Text
		rec["lastMessageKind"] = summary.Kind
		rec["lastSpeaker"] = summary.Speaker
		rec["lastMessageTime"] = summary.At
		if summary.Round > 0 {
			rec["lastMessageRound"] = summary.Round
		}
	}
}

// summarizeChatroomLastEvent maps one event row to a preview. Reports false for
// events whose payload carries nothing a human would call a message.
func summarizeChatroomLastEvent(row db.ListRongCloudChatroomLastEventsRow) (chatroomLastEventSummary, bool) {
	var payload chatroomLastEventPayload
	if len(row.Payload) > 0 {
		// Tolerate malformed payloads: an unparsable one only costs this row its
		// preview, it must not drop the room from the list.
		_ = json.Unmarshal(row.Payload, &payload)
	}
	roleName := strings.TrimSpace(row.RoleName)

	switch row.EventType {
	case "round_summary_completed":
		// 纪要以【已达成的共识】这类段落标题开头，和客户端加的「纪要：」前缀语义重复，
		// 而且列表上每行都是同样的开头 —— 剥掉，让真正的内容露出来。
		text := stripLeadingSectionLabel(condenseLastMessageText(payload.Content))
		if text == "" {
			text = stripLeadingSectionLabel(condenseLastMessageText(payload.Title))
		}
		if text == "" {
			return chatroomLastEventSummary{}, false
		}
		// The summary is written by the host role; fall back to a neutral label
		// when the role was renamed or the member row is gone.
		speaker := roleName
		if speaker == "" {
			speaker = "主持人"
		}
		return chatroomLastEventSummary{
			Kind:    lastMessageKindSummary,
			Speaker: speaker,
			Text:    text,
			At:      row.CreatedAt,
			Round:   eventRound(payload, row),
		}, true

	case "contribution_completed":
		text := condenseLastMessageText(payload.Content)
		if text == "" {
			return chatroomLastEventSummary{}, false
		}
		speaker := roleName
		if speaker == "" {
			// Node was removed from the room after speaking: keep the message,
			// drop the name rather than showing a raw RongCloud id.
			speaker = ""
		}
		return chatroomLastEventSummary{
			Kind:    lastMessageKindContribution,
			Speaker: speaker,
			Text:    text,
			At:      row.CreatedAt,
			Round:   eventRound(payload, row),
		}, true

	case "user_interjection":
		text := condenseLastMessageText(payload.Content)
		if text == "" {
			return chatroomLastEventSummary{}, false
		}
		// 主持人＝用户本人（项目术语），列表上直接写「你」更好读。
		return chatroomLastEventSummary{
			Kind:    lastMessageKindInterjection,
			Speaker: "你",
			Text:    text,
			At:      row.CreatedAt,
		}, true

	case "discussion_cancelled":
		return chatroomLastEventSummary{
			Kind: lastMessageKindCancelled,
			Text: "讨论已结束",
			At:   row.CreatedAt,
		}, true

	case "discussion_failed":
		return chatroomLastEventSummary{
			Kind: lastMessageKindFailed,
			Text: "讨论中断，可重试",
			At:   row.CreatedAt,
		}, true
	}
	return chatroomLastEventSummary{}, false
}

// condenseLastMessageText flattens model output into one list-friendly line:
// markdown scaffolding off, newlines folded, runs of whitespace collapsed, then
// cut to the preview budget by *rune* (Go's len is bytes — a 24-character CJK
// sentence is 72 bytes and would be chopped mid-character).
func condenseLastMessageText(raw string) string {
	if raw == "" {
		return ""
	}
	replaced := strings.NewReplacer(
		"\r\n", "\n",
		"**", "",
		"__", "",
		"`", "",
		"#", "",
		">", "",
		"|", " ",
		"- ", "",
	).Replace(raw)
	// Keep the paragraph break readable as a separator instead of gluing the
	// tail of one section onto the heading of the next.
	line := strings.Join(strings.Fields(replaced), " ")
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if utf8.RuneCountInString(line) <= chatroomLastMessageMaxRunes {
		return line
	}
	runes := []rune(line)
	return strings.TrimSpace(string(runes[:chatroomLastMessageMaxRunes])) + "…"
}

// stripLeadingSectionLabel 去掉纪要开头的段落标题（【已达成的共识】…）。
//
// 主持人产物的固定结构是「【已达成的共识】/【仍存在的分歧】/…」，列表上每行都从
// 同一个标题开始，一眼看过去全是重复。剥掉之后露出真正的内容；如果剥完什么都不剩
// （只写了一个标题），保留原文 —— 那也是有信息的。
func stripLeadingSectionLabel(text string) string {
	if text == "" || !strings.HasPrefix(text, "【") {
		return text
	}
	end := strings.Index(text, "】")
	if end < 0 {
		return text
	}
	// 阈值按 rune 算：标题一般 2~8 个汉字，超长的说明这不是标题而是正文。
	if utf8.RuneCountInString(text[:end]) > 12 {
		return text
	}
	rest := strings.TrimSpace(text[end+len("】"):])
	if rest == "" {
		return text
	}
	return rest
}

// eventRound 取 1-based 轮次。payload.round 是业务轮次（1 起）；
// 行上的 round_number 是库里的计数（0 起），只在 payload 没带时兜底。
func eventRound(payload chatroomLastEventPayload, row db.ListRongCloudChatroomLastEventsRow) int32 {
	if payload.Round > 0 {
		return payload.Round
	}
	return row.RoundNumber + 1
}
