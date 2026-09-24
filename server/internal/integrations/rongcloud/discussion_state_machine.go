package rongcloud

import (
	"encoding/json"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5/pgtype"
)

const (
	EventDiscussionStarted  = "discussion_started"
	EventRoundStarted       = "round_started"
	EventTurnStarted        = "turn_started"
	EventTurnCompleted      = "turn_completed"
	EventTurnSkipped        = "turn_skipped"
	EventRoundCompleted     = "round_completed"
	EventDiscussionEnded    = "discussion_ended"
	EventDiscussionPaused   = "discussion_paused"
	EventDiscussionResumed  = "discussion_resumed"
	EventHostChanged        = "host_changed"
	EventError              = "error"
)

const (
	StatusIdle        = "idle"
	StatusStarting    = "starting"
	StatusInProgress  = "in_progress"
	StatusPaused      = "paused"
	StatusEnded       = "ended"
)

func ReplayState(events []DiscussionEvent) (DiscussionState, error) {
	if len(events) == 0 {
		return DiscussionState{Status: StatusIdle}, nil
	}

	sorted := make([]DiscussionEvent, len(events))
	copy(sorted, events)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.Time.Before(sorted[j].CreatedAt.Time)
	})

	state := DiscussionState{
		ChatroomID:  sorted[0].ChatroomID,
		WorkspaceID: sorted[0].WorkspaceID,
		Status:      StatusIdle,
	}

	for _, e := range sorted {
		applyEvent(&state, e)
	}

	return state, nil
}

func applyEvent(state *DiscussionState, e DiscussionEvent) {
	switch e.EventType {
	case EventDiscussionStarted:
		state.Status = StatusStarting
		state.StartedAt = e.CreatedAt.Time
		if len(e.Content) > 0 {
			var speakers []SpeakerInfo
			if err := json.Unmarshal(e.Content, &speakers); err == nil {
				state.Speakers = speakers
			}
		}
		state.HostNodeID = e.NodeID
	case EventRoundStarted:
		state.Status = StatusInProgress
		state.CurrentRound = int(e.RoundNumber)
	case EventTurnStarted:
		state.CurrentSpeaker = e.NodeID
	case EventTurnCompleted, EventTurnSkipped:
		state.CurrentSpeaker = pgtype.UUID{}
	case EventRoundCompleted:
	case EventDiscussionPaused:
		state.Status = StatusPaused
	case EventDiscussionResumed:
		state.Status = StatusInProgress
	case EventDiscussionEnded:
		state.Status = StatusEnded
		t := e.CreatedAt.Time
		state.EndedAt = &t
	case EventHostChanged:
		state.HostNodeID = e.NodeID
	case EventError:
	}
}

func ValidateTransition(currentStatus, eventType string) (string, error) {
	transitions := map[string]map[string]string{
		StatusIdle: {
			EventDiscussionStarted: StatusStarting,
		},
		StatusStarting: {
			EventRoundStarted:     StatusInProgress,
			EventDiscussionEnded:  StatusEnded,
			EventError:            StatusEnded,
		},
		StatusInProgress: {
			EventDiscussionPaused: StatusPaused,
			EventDiscussionEnded:  StatusEnded,
			EventRoundStarted:     StatusInProgress,
			EventTurnStarted:      StatusInProgress,
			EventTurnCompleted:    StatusInProgress,
			EventTurnSkipped:      StatusInProgress,
			EventRoundCompleted:   StatusInProgress,
			EventHostChanged:      StatusInProgress,
			EventError:            StatusInProgress,
		},
		StatusPaused: {
			EventDiscussionResumed: StatusInProgress,
			EventDiscussionEnded:   StatusEnded,
		},
		StatusEnded: {},
	}

	allowed, ok := transitions[currentStatus]
	if !ok {
		return currentStatus, errors.New("invalid current status: " + currentStatus)
	}

	newStatus, ok := allowed[eventType]
	if !ok {
		return currentStatus, errors.New("invalid transition: " + currentStatus + " + " + eventType)
	}

	return newStatus, nil
}

func ElectHost(speakers []SpeakerInfo, failedNodeID pgtype.UUID) (pgtype.UUID, error) {
	sorted := make([]SpeakerInfo, len(speakers))
	copy(sorted, speakers)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].SpeakingOrder < sorted[j].SpeakingOrder
	})

	for _, s := range sorted {
		if s.NodeID.Bytes == failedNodeID.Bytes && s.NodeID.Valid == failedNodeID.Valid {
			continue
		}
		return s.NodeID, nil
	}

	return pgtype.UUID{}, errors.New("no available host candidate")
}
