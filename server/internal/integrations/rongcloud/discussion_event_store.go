package rongcloud

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type DiscussionEventStore struct {
	queries *db.Queries
	logger  *slog.Logger
}

func NewDiscussionEventStore(queries *db.Queries, logger *slog.Logger) *DiscussionEventStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &DiscussionEventStore{queries: queries, logger: logger}
}

func (s *DiscussionEventStore) Append(ctx context.Context, event DiscussionEvent) (DiscussionEvent, error) {
	params := db.CreateRongCloudDiscussionEventParams{
		ChatroomID:    event.ChatroomID,
		WorkspaceID:   event.WorkspaceID,
		EventType:     event.EventType,
		RoundNumber:   int32(event.RoundNumber),
		SpeakingOrder: int32(event.SpeakingOrder),
		NodeID:        event.NodeID,
		Content:       event.Content,
		MsgUid:        pgtype.Text{String: event.MsgUID.String, Valid: event.MsgUID.Valid},
	}

	row, err := s.queries.CreateRongCloudDiscussionEvent(ctx, params)
	if err != nil {
		s.logger.Error("failed to append discussion event", "error", err, "event_type", event.EventType)
		return DiscussionEvent{}, err
	}

	return rowToDiscussionEvent(row), nil
}

func (s *DiscussionEventStore) ListByChatroom(ctx context.Context, chatroomID pgtype.UUID) ([]DiscussionEvent, error) {
	rows, err := s.queries.ListRongCloudDiscussionEventsByChatroom(ctx, chatroomID)
	if err != nil {
		s.logger.Error("failed to list discussion events", "error", err, "chatroom_id", chatroomID)
		return nil, err
	}

	events := make([]DiscussionEvent, len(rows))
	for i, row := range rows {
		events[i] = rowToDiscussionEvent(row)
	}
	return events, nil
}

func rowToDiscussionEvent(row db.RongcloudDiscussionEvent) DiscussionEvent {
	return DiscussionEvent{
		ID:            row.ID,
		ChatroomID:    row.ChatroomID,
		WorkspaceID:   row.WorkspaceID,
		EventType:     row.EventType,
		RoundNumber:   row.RoundNumber,
		SpeakingOrder: row.SpeakingOrder,
		NodeID:        row.NodeID,
		Content:       row.Content,
		MsgUID:        row.MsgUid,
		CreatedAt:     row.CreatedAt,
	}
}
