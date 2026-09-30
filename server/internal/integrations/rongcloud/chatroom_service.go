package rongcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/pkg/db/generated"
)

// ChatroomCreateParams holds parameters for chatroom creation.
type ChatroomCreateParams struct {
	WorkspaceID         pgtype.UUID
	OwnerUserID         pgtype.UUID
	RongcloudChatroomID string
	HostNodeID          pgtype.UUID
	MaxRounds           int32
	ConversationKind    string
	Config              json.RawMessage
}

// ChatroomMemberConfig holds AI member configuration.
type ChatroomMemberConfig struct {
	NodeID           pgtype.UUID
	MemberType       string
	RoleName         string
	RoleInstructions string
	Capabilities     json.RawMessage
	Model            string
	SpeakingOrder    int32
	DiscussionModel  string
}

// ChatroomService manages RongCloud chatrooms and their members.
type ChatroomService struct {
	queries *db.Queries
	client  *rongcloudAPIClient
	logger  *slog.Logger
}

// NewChatroomService creates a new ChatroomService.
func NewChatroomService(queries *db.Queries, client *rongcloudAPIClient, logger *slog.Logger) *ChatroomService {
	if logger == nil {
		logger = slog.Default()
	}
	return &ChatroomService{queries: queries, client: client, logger: logger}
}

// ensureRongCloudChatroom creates a RongCloud chatroom if it does not exist.
func (s *ChatroomService) ensureRongCloudChatroom(ctx context.Context, chatroomID, name string) error {
	if s.client == nil {
		return errors.New("rongcloud: API client not configured")
	}
	return s.client.ensureChatroom(ctx, chatroomID, name)
}

// CreateChatroom creates a chatroom in both RongCloud and the database.
func (s *ChatroomService) CreateChatroom(ctx context.Context, params ChatroomCreateParams) (db.RongcloudChatroom, error) {
	if s.queries == nil {
		return db.RongcloudChatroom{}, errors.New("rongcloud: database not configured")
	}
	if s.client != nil {
		if err := s.ensureRongCloudChatroom(ctx, params.RongcloudChatroomID, ""); err != nil {
			return db.RongcloudChatroom{}, err
		}
	}
	// config is NOT NULL in the schema; a caller that omits it gets the same
	// '{}' default the column declares instead of an insert failure.
	if len(params.Config) == 0 {
		params.Config = json.RawMessage("{}")
	}
	return s.queries.CreateRongCloudChatroom(ctx, db.CreateRongCloudChatroomParams{
		WorkspaceID:         params.WorkspaceID,
		RongcloudChatroomID:  params.RongcloudChatroomID,
		OwnerUserID:          params.OwnerUserID,
		HostNodeID:           params.HostNodeID,
		MaxRounds:            params.MaxRounds,
		ConversationKind:     pgtype.Text{String: params.ConversationKind, Valid: params.ConversationKind != ""},
		Config:               params.Config,
		Status:               "active",
	})
}

// GetChatroom retrieves a chatroom by internal ID.
func (s *ChatroomService) GetChatroom(ctx context.Context, id pgtype.UUID) (db.RongcloudChatroom, error) {
	if s.queries == nil {
		return db.RongcloudChatroom{}, errors.New("rongcloud: database not configured")
	}
	return s.queries.GetRongCloudChatroomByID(ctx, id)
}

// ListChatrooms lists chatrooms in a workspace.
func (s *ChatroomService) ListChatrooms(ctx context.Context, workspaceID pgtype.UUID) ([]db.RongcloudChatroom, error) {
	if s.queries == nil {
		return nil, errors.New("rongcloud: database not configured")
	}
	return s.queries.ListRongCloudChatroomsByWorkspace(ctx, workspaceID)
}

// UpdateChatroom updates a chatroom's configuration.
func (s *ChatroomService) UpdateChatroom(ctx context.Context, id pgtype.UUID, hostNodeID pgtype.UUID, maxRounds int32, conversationKind string, config json.RawMessage) (db.RongcloudChatroom, error) {
	if s.queries == nil {
		return db.RongcloudChatroom{}, errors.New("rongcloud: database not configured")
	}
	return s.queries.UpdateRongCloudChatroom(ctx, db.UpdateRongCloudChatroomParams{
		ID:               id,
		HostNodeID:       hostNodeID,
		MaxRounds:        maxRounds,
		ConversationKind: pgtype.Text{String: conversationKind, Valid: conversationKind != ""},
		Config:           config,
	})
}

// DeleteChatroom destroys the chatroom on RongCloud and soft-deletes it in the database.
func (s *ChatroomService) DeleteChatroom(ctx context.Context, id pgtype.UUID) error {
	if s.queries == nil {
		return errors.New("rongcloud: database not configured")
	}
	chatroom, err := s.queries.GetRongCloudChatroomByID(ctx, id)
	if err != nil {
		return fmt.Errorf("rongcloud: get chatroom: %w", err)
	}
	if s.client != nil && chatroom.RongcloudChatroomID != "" {
		if err := s.client.destroyChatroom(ctx, chatroom.RongcloudChatroomID); err != nil {
			s.logger.Warn("rongcloud: destroyChatroom failed, continuing with DB delete",
				"rongcloud_chatroom_id", chatroom.RongcloudChatroomID, "error", err)
		}
	}
	return s.queries.DeleteRongCloudChatroom(ctx, id)
}

// SetMembers replaces all AI members in a chatroom and syncs join/quit with RongCloud.
func (s *ChatroomService) SetMembers(ctx context.Context, chatroomID pgtype.UUID, members []ChatroomMemberConfig) error {
	if s.queries == nil {
		return errors.New("rongcloud: database not configured")
	}

	chatroom, err := s.queries.GetRongCloudChatroomByID(ctx, chatroomID)
	if err != nil {
		return fmt.Errorf("rongcloud: get chatroom for member sync: %w", err)
	}
	rcChatroomID := chatroom.RongcloudChatroomID

	oldMembers, err := s.queries.ListRongCloudChatroomMembers(ctx, chatroomID)
	if err != nil {
		return fmt.Errorf("rongcloud: list old members: %w", err)
	}

	oldSet := make(map[pgtype.UUID]bool)
	for _, m := range oldMembers {
		oldSet[m.NodeID] = true
	}

	newSet := make(map[pgtype.UUID]bool)
	nodeToRcUserID := make(map[pgtype.UUID]string)
	for _, m := range members {
		newSet[m.NodeID] = true
		node, err := s.queries.GetRongCloudNodeByID(ctx, m.NodeID)
		if err != nil {
			return fmt.Errorf("rongcloud: get node for member sync: %w", err)
		}
		nodeToRcUserID[m.NodeID] = node.RongcloudUserID
	}

	var addedUserIDs, removedUserIDs []string
	for _, m := range members {
		if !oldSet[m.NodeID] {
			addedUserIDs = append(addedUserIDs, nodeToRcUserID[m.NodeID])
		}
	}
	for _, m := range oldMembers {
		if !newSet[m.NodeID] {
			node, err := s.queries.GetRongCloudNodeByID(ctx, m.NodeID)
			if err != nil {
				s.logger.Warn("rongcloud: get node for removed member failed",
					"node_id", m.NodeID, "error", err)
				continue
			}
			removedUserIDs = append(removedUserIDs, node.RongcloudUserID)
		}
	}

	if err := s.queries.DeleteRongCloudChatroomMembersByChatroom(ctx, chatroomID); err != nil {
		return err
	}
	for _, m := range members {
		// capabilities is NOT NULL; default to '{}' like the column does.
		if len(m.Capabilities) == 0 {
			m.Capabilities = json.RawMessage("{}")
		}
		_, err := s.queries.CreateRongCloudChatroomMember(ctx, db.CreateRongCloudChatroomMemberParams{
			ChatroomID:       chatroomID,
			NodeID:           m.NodeID,
			MemberType:       m.MemberType,
			RoleName:         pgtype.Text{String: m.RoleName, Valid: m.RoleName != ""},
			RoleInstructions: pgtype.Text{String: m.RoleInstructions, Valid: m.RoleInstructions != ""},
			Capabilities:     m.Capabilities,
			Model:            pgtype.Text{String: m.Model, Valid: m.Model != ""},
			SpeakingOrder:    pgtype.Int4{Int32: m.SpeakingOrder, Valid: true},
			Enabled:          true,
			DiscussionModel:  pgtype.Text{String: m.DiscussionModel, Valid: m.DiscussionModel != ""},
		})
		if err != nil {
			return err
		}
	}

	if s.client != nil && rcChatroomID != "" {
		if len(addedUserIDs) > 0 {
			if err := s.client.joinChatroom(ctx, rcChatroomID, addedUserIDs); err != nil {
				s.logger.Warn("rongcloud: joinChatroom failed",
					"chatroom_id", rcChatroomID, "error", err)
			}
		}
		if len(removedUserIDs) > 0 {
			if err := s.client.quitChatroom(ctx, rcChatroomID, removedUserIDs); err != nil {
				s.logger.Warn("rongcloud: quitChatroom failed",
					"chatroom_id", rcChatroomID, "error", err)
			}
		}
	}

	return nil
}

// ListMembers lists members of a chatroom.
func (s *ChatroomService) ListMembers(ctx context.Context, chatroomID pgtype.UUID) ([]db.RongcloudChatroomMember, error) {
	if s.queries == nil {
		return nil, errors.New("rongcloud: database not configured")
	}
	return s.queries.ListRongCloudChatroomMembers(ctx, chatroomID)
}
