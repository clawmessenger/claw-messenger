package rongcloud

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type DiscussionEvent struct {
	ID            pgtype.UUID
	ChatroomID    pgtype.UUID
	WorkspaceID   pgtype.UUID
	EventType     string
	RoundNumber   int32
	SpeakingOrder int32
	NodeID        pgtype.UUID
	Content       []byte
	MsgUID        pgtype.Text
	CreatedAt     pgtype.Timestamptz
}

type DiscussionState struct {
	ChatroomID     pgtype.UUID
	WorkspaceID    pgtype.UUID
	Status         string
	CurrentRound   int
	CurrentSpeaker pgtype.UUID
	Speakers       []SpeakerInfo
	HostNodeID     pgtype.UUID
	// HostPrompt is the operator-written 「人设提示词」 (the admin page's
	// discussion_host_prompt) and applies to the host node only. "" leaves the
	// host on whatever role prompt its chatroom membership carries.
	HostPrompt string
	StartedAt  time.Time
	EndedAt    *time.Time
}

type SpeakerInfo struct {
	NodeID        pgtype.UUID
	SpeakingOrder int
	RoleName      string
	// RolePrompt is this member's 「角色人设」, persisted as
	// rongcloud_chatroom_member.role_instructions. It is what makes two nodes
	// on the same model behave as different participants.
	RolePrompt      string
	Model           string
	RongcloudUserID string
	AiType          string
}

type NodeResponse struct {
	NodeID     string
	MsgType    string
	RequestID  string
	TurnID     string
	Content    string
	StreamType string
	ChunkIndex int
	Error      string
}
