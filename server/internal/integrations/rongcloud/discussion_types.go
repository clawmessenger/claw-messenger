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
	StartedAt      time.Time
	EndedAt        *time.Time
}

type SpeakerInfo struct {
	NodeID          pgtype.UUID
	SpeakingOrder   int
	RoleName        string
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
