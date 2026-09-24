package rongcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type DiscussionService struct {
	queries  *db.Queries
	client   *rongcloudAPIClient
	registry *DiscussionRegistry
	bridge   *DiscussionBridge
	logger   *slog.Logger
}

func NewDiscussionService(queries *db.Queries, client *rongcloudAPIClient, registry *DiscussionRegistry, bridge *DiscussionBridge, logger *slog.Logger) *DiscussionService {
	if logger == nil {
		logger = slog.Default()
	}
	return &DiscussionService{
		queries:  queries,
		client:   client,
		registry: registry,
		bridge:   bridge,
		logger:   logger,
	}
}

func (s *DiscussionService) StartDiscussion(ctx context.Context, chatroomID pgtype.UUID) (DiscussionState, error) {
	if s.registry == nil {
		return DiscussionState{}, errors.New("discussion registry not configured")
	}

	if _, exists := s.registry.Get(chatroomID); exists {
		return DiscussionState{}, errors.New("discussion already in progress for this chatroom")
	}

	chatroom, err := s.queries.GetRongCloudChatroomByID(ctx, chatroomID)
	if err != nil {
		return DiscussionState{}, fmt.Errorf("failed to get chatroom: %w", err)
	}

	members, err := s.queries.ListRongCloudChatroomMembers(ctx, chatroomID)
	if err != nil {
		return DiscussionState{}, fmt.Errorf("failed to list chatroom members: %w", err)
	}

	var speakers []SpeakerInfo
	for _, m := range members {
		if !m.Enabled {
			continue
		}
		node, err := s.queries.GetRongCloudNodeByID(ctx, m.NodeID)
		if err != nil {
			s.logger.Warn("failed to get node for member", "node_id", m.NodeID, "error", err)
			continue
		}
		speakers = append(speakers, SpeakerInfo{
			NodeID:          m.NodeID,
			SpeakingOrder:   int(m.SpeakingOrder.Int32),
			RoleName:        m.RoleName.String,
			Model:           m.Model.String,
			RongcloudUserID: node.RongcloudUserID,
			AiType:          node.AiType.String,
		})
	}

	if len(speakers) == 0 {
		return DiscussionState{}, errors.New("no enabled speakers in chatroom")
	}

	hostNodeID := chatroom.HostNodeID
	if !hostNodeID.Valid {
		hostNodeID = speakers[0].NodeID
	}

	state := DiscussionState{
		ChatroomID:  chatroomID,
		WorkspaceID: chatroom.WorkspaceID,
		HostNodeID:  hostNodeID,
		Status:      StatusStarting,
		Speakers:    speakers,
		StartedAt:   time.Now(),
	}

	eventStore := NewDiscussionEventStore(s.queries, s.logger)
	streamAssembler := NewStreamAssembler()

	_, err = eventStore.Append(ctx, DiscussionEvent{
		ChatroomID:  chatroomID,
		WorkspaceID: chatroom.WorkspaceID,
		EventType:   EventDiscussionStarted,
		NodeID:      hostNodeID,
		Content:     mustJSON(map[string]interface{}{"speakers": len(speakers), "host": chatroomKey(hostNodeID)}),
	})
	if err != nil {
		return DiscussionState{}, fmt.Errorf("failed to record discussion_started event: %w", err)
	}

	coordinator := NewDiscussionCoordinator(
		chatroomID, chatroom.WorkspaceID,
		state, s.client, eventStore, streamAssembler, s.bridge, s.logger,
	)
	s.registry.Put(chatroomID, coordinator)

	maxRounds := int(chatroom.MaxRounds)
	if maxRounds <= 0 {
		maxRounds = 1
	}
	go coordinator.Run(context.Background(), maxRounds)

	return state, nil
}

func (s *DiscussionService) StopDiscussion(ctx context.Context, chatroomID pgtype.UUID) (DiscussionState, error) {
	if s.registry == nil {
		return DiscussionState{}, errors.New("discussion registry not configured")
	}

	coordinator, exists := s.registry.Get(chatroomID)
	if !exists {
		return DiscussionState{}, errors.New("no active discussion for this chatroom")
	}

	coordinator.Pause()

	eventStore := coordinator.eventStore
	wsID := coordinator.workspaceID

	if eventStore != nil {
		_, err := eventStore.Append(ctx, DiscussionEvent{
			ChatroomID:  chatroomID,
			WorkspaceID: wsID,
			EventType:   EventDiscussionEnded,
			Content:     []byte(`{"reason":"manual_stop"}`),
		})
		if err != nil {
			s.logger.Warn("failed to record discussion_ended event", "error", err)
		}
	}

	s.registry.Delete(chatroomID)

	return s.GetDiscussionStatus(ctx, chatroomID)
}

func (s *DiscussionService) PauseDiscussion(ctx context.Context, chatroomID pgtype.UUID) (DiscussionState, error) {
	if s.registry == nil {
		return DiscussionState{}, errors.New("discussion registry not configured")
	}

	coordinator, exists := s.registry.Get(chatroomID)
	if !exists {
		return DiscussionState{}, errors.New("no active discussion for this chatroom")
	}

	coordinator.Pause()

	if coordinator.eventStore != nil {
		_, err := coordinator.eventStore.Append(ctx, DiscussionEvent{
			ChatroomID:  chatroomID,
			WorkspaceID: coordinator.workspaceID,
			EventType:   EventDiscussionPaused,
			Content:     []byte(`{}`),
		})
		if err != nil {
			s.logger.Warn("failed to record discussion_paused event", "error", err)
		}
	}

	return s.GetDiscussionStatus(ctx, chatroomID)
}

func (s *DiscussionService) ResumeDiscussion(ctx context.Context, chatroomID pgtype.UUID) (DiscussionState, error) {
	if s.registry == nil {
		return DiscussionState{}, errors.New("discussion registry not configured")
	}

	coordinator, exists := s.registry.Get(chatroomID)
	if !exists {
		return DiscussionState{}, errors.New("no active discussion for this chatroom")
	}

	coordinator.Resume()

	if coordinator.eventStore != nil {
		_, err := coordinator.eventStore.Append(ctx, DiscussionEvent{
			ChatroomID:  chatroomID,
			WorkspaceID: coordinator.workspaceID,
			EventType:   EventDiscussionResumed,
			Content:     []byte(`{}`),
		})
		if err != nil {
			s.logger.Warn("failed to record discussion_resumed event", "error", err)
		}
	}

	return s.GetDiscussionStatus(ctx, chatroomID)
}

func (s *DiscussionService) GetDiscussionStatus(ctx context.Context, chatroomID pgtype.UUID) (DiscussionState, error) {
	if s.registry != nil {
		if coordinator, exists := s.registry.Get(chatroomID); exists {
			coordinator.mu.Lock()
			state := coordinator.state
			coordinator.mu.Unlock()
			return state, nil
		}
	}

	if s.queries == nil {
		return DiscussionState{}, errors.New("database not configured")
	}

	events, err := s.queries.ListRongCloudDiscussionEventsByChatroom(ctx, chatroomID)
	if err != nil {
		return DiscussionState{}, fmt.Errorf("failed to list discussion events: %w", err)
	}

	discussionEvents := make([]DiscussionEvent, 0, len(events))
	for _, e := range events {
		discussionEvents = append(discussionEvents, rowToDiscussionEvent(rowFromGenerated(e)))
	}

	if len(discussionEvents) == 0 {
		return DiscussionState{ChatroomID: chatroomID, Status: StatusIdle}, nil
	}

	return ReplayState(discussionEvents)
}

func (s *DiscussionService) ListEvents(ctx context.Context, chatroomID pgtype.UUID) ([]DiscussionEvent, error) {
	if s.queries == nil {
		return nil, errors.New("database not configured")
	}

	events, err := s.queries.ListRongCloudDiscussionEventsByChatroom(ctx, chatroomID)
	if err != nil {
		return nil, fmt.Errorf("failed to list discussion events: %w", err)
	}

	discussionEvents := make([]DiscussionEvent, 0, len(events))
	for _, e := range events {
		discussionEvents = append(discussionEvents, rowToDiscussionEvent(rowFromGenerated(e)))
	}

	return discussionEvents, nil
}

func rowFromGenerated(row db.RongcloudDiscussionEvent) db.RongcloudDiscussionEvent {
	return row
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
