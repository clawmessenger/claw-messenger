package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

const turnTimeout = 120 * time.Second

type DiscussionCoordinator struct {
	chatroomID      pgtype.UUID
	workspaceID     pgtype.UUID
	state           DiscussionState
	client          *rongcloudAPIClient
	eventStore      *DiscussionEventStore
	responseCh      chan NodeResponse
	streamAssembler *StreamAssembler
	bridge          *DiscussionBridge
	logger          *slog.Logger
	mu              sync.Mutex
	paused          bool
	done            chan struct{}
}

func NewDiscussionCoordinator(
	chatroomID, workspaceID pgtype.UUID,
	state DiscussionState,
	client *rongcloudAPIClient,
	eventStore *DiscussionEventStore,
	streamAssembler *StreamAssembler,
	bridge *DiscussionBridge,
	logger *slog.Logger,
) *DiscussionCoordinator {
	if logger == nil {
		logger = slog.Default()
	}
	return &DiscussionCoordinator{
		chatroomID:      chatroomID,
		workspaceID:     workspaceID,
		state:           state,
		client:          client,
		eventStore:      eventStore,
		streamAssembler: streamAssembler,
		bridge:          bridge,
		logger:          logger,
		responseCh:      make(chan NodeResponse, 16),
		done:            make(chan struct{}),
	}
}

func (c *DiscussionCoordinator) Run(ctx context.Context, maxRounds int) {
	defer close(c.done)

	c.mu.Lock()
	c.state.Status = StatusInProgress
	c.mu.Unlock()

	for round := 1; round <= maxRounds; round++ {
		if ctx.Err() != nil {
			return
		}
		if c.isPaused() {
			if !c.waitResume(ctx) {
				return
			}
		}

		c.mu.Lock()
		c.state.CurrentRound = round
		c.mu.Unlock()

		c.emitEvent(ctx, EventRoundStarted, int32(round), 0, c.state.HostNodeID, nil)

		for i, speaker := range c.state.Speakers {
			if ctx.Err() != nil {
				return
			}
			if c.isPaused() {
				if !c.waitResume(ctx) {
					return
				}
			}

			c.mu.Lock()
			c.state.CurrentSpeaker = speaker.NodeID
			c.mu.Unlock()

			speakingOrder := int32(i) + 1
			c.emitEvent(ctx, EventTurnStarted, int32(round), speakingOrder, speaker.NodeID, nil)

			if err := c.sendYourTurn(ctx, speaker, round, speakingOrder); err != nil {
				c.logger.Warn("failed to send your_turn", "node_id", speaker.NodeID, "error", err)
				c.emitEvent(ctx, EventError, int32(round), speakingOrder, speaker.NodeID, []byte(fmt.Sprintf(`{"error":"send_failed","detail":%q}`, err.Error())))
				continue
			}

			select {
			case resp := <-c.responseCh:
				c.handleResponse(ctx, resp, round, speakingOrder)
			case <-time.After(turnTimeout):
				c.logger.Warn("turn timed out", "node_id", speaker.NodeID, "round", round)
				c.emitEvent(ctx, EventTurnSkipped, int32(round), speakingOrder, speaker.NodeID, []byte(`{"reason":"timeout"}`))
			case <-ctx.Done():
				return
			}
		}

		c.emitEvent(ctx, EventRoundCompleted, int32(round), 0, c.state.HostNodeID, nil)
	}

	c.mu.Lock()
	c.state.Status = StatusEnded
	now := time.Now()
	c.state.EndedAt = &now
	c.mu.Unlock()

	c.emitEvent(ctx, EventDiscussionEnded, int32(maxRounds), 0, c.state.HostNodeID, nil)
}

func (c *DiscussionCoordinator) handleResponse(ctx context.Context, resp NodeResponse, round int, speakingOrder int32) {
	switch resp.MsgType {
	case "command_result", "text":
		content, _ := json.Marshal(map[string]string{
			"node_id":  resp.NodeID,
			"content":  resp.Content,
			"turn_id":  resp.TurnID,
		})
		c.emitEvent(ctx, EventTurnCompleted, int32(round), speakingOrder, c.state.CurrentSpeaker, content)

	case "stream":
		c.handleStreamResponse(ctx, resp, round, speakingOrder)

	default:
		c.logger.Warn("unknown response msg_type", "msg_type", resp.MsgType)
	}
}

func (c *DiscussionCoordinator) handleStreamResponse(ctx context.Context, resp NodeResponse, round int, speakingOrder int32) {
	switch resp.StreamType {
	case "start":
		c.streamAssembler.StartStream(resp.TurnID, resp.NodeID)

	case "chunk":
		if err := c.streamAssembler.AppendChunk(resp.TurnID, resp.Content); err != nil {
			c.logger.Warn("failed to append stream chunk", "turn_id", resp.TurnID, "error", err)
		}

	case "end":
		fullText, err := c.streamAssembler.CompleteStream(resp.TurnID)
		if err != nil {
			c.logger.Warn("failed to complete stream", "turn_id", resp.TurnID, "error", err)
			return
		}
		content, _ := json.Marshal(map[string]string{
			"node_id":  resp.NodeID,
			"content":  fullText,
			"turn_id":  resp.TurnID,
		})
		c.emitEvent(ctx, EventTurnCompleted, int32(round), speakingOrder, c.state.CurrentSpeaker, content)

	case "error":
		c.streamAssembler.AbortStream(resp.TurnID)
		content, _ := json.Marshal(map[string]string{
			"node_id": resp.NodeID,
			"error":   resp.Error,
			"turn_id": resp.TurnID,
		})
		c.emitEvent(ctx, EventError, int32(round), speakingOrder, c.state.CurrentSpeaker, content)
	}
}

func (c *DiscussionCoordinator) sendYourTurn(ctx context.Context, speaker SpeakerInfo, round int, speakingOrder int32) error {
	if c.bridge != nil && c.bridge.IsServerManagedByType(speaker.AiType) {
		go c.executeBridgeTurn(ctx, speaker, round, int(speakingOrder))
		return nil
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"request_id":     fmt.Sprintf("turn_%d_%d_%s", round, speakingOrder, time.Now().Format("20060102150405")),
		"service":        "discussion",
		"action":         "your_turn",
		"params": map[string]interface{}{
			"round":           round,
			"speaking_order":  speakingOrder,
			"role_name":       speaker.RoleName,
			"model":           speaker.Model,
			"chatroom_id":     chatroomKey(c.chatroomID),
			"discussion_kind": c.state.Status,
		},
	})
	fromUserID := ""
	for _, s := range c.state.Speakers {
		if s.NodeID == c.state.HostNodeID {
			fromUserID = s.RongcloudUserID
			break
		}
	}
	_, err := c.client.sendPrivateMessage(ctx, fromUserID, speaker.RongcloudUserID, objectNameCommand, string(payload))
	if err != nil {
		return err
	}
	return nil
}

func (c *DiscussionCoordinator) executeBridgeTurn(ctx context.Context, speaker SpeakerInfo, round, speakingOrder int) {
	prompt := c.buildPrompt(speaker, round, speakingOrder)
	result, err := c.bridge.ExecuteTurn(ctx, c.chatroomID, speaker.NodeID, prompt, speaker.Model)
	if err != nil {
		c.logger.Error("bridge turn failed, falling through to timeout",
			"node_id", speaker.NodeID.String(),
			"round", round,
			"error", err,
		)
		select {
		case c.responseCh <- NodeResponse{
			NodeID:  speaker.NodeID.String(),
			MsgType: "stream",
			TurnID:  fmt.Sprintf("r%d_s%d", round, speakingOrder),
			Error:   err.Error(),
		}:
		case <-c.done:
		case <-ctx.Done():
		}
		return
	}
	select {
	case c.responseCh <- NodeResponse{
		NodeID:  speaker.NodeID.String(),
		MsgType: "text",
		TurnID:  fmt.Sprintf("r%d_s%d", round, speakingOrder),
		Content: result,
	}:
	case <-c.done:
	case <-ctx.Done():
	}
}

func (c *DiscussionCoordinator) buildPrompt(speaker SpeakerInfo, round, speakingOrder int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("You are participating in a multi-agent discussion (round %d).\n", round))
	sb.WriteString(fmt.Sprintf("Your role: %s\n", speaker.RoleName))
	sb.WriteString(fmt.Sprintf("Speaking order: %d\n", speakingOrder))
	if speaker.Model != "" {
		sb.WriteString(fmt.Sprintf("Model: %s\n", speaker.Model))
	}
	sb.WriteString("\nPlease provide your contribution to the discussion.\n")
	return sb.String()
}

func (c *DiscussionCoordinator) emitEvent(ctx context.Context, eventType string, round, speakingOrder int32, nodeID pgtype.UUID, content []byte) {
	if c.eventStore == nil {
		return
	}
	event := DiscussionEvent{
		ChatroomID:    c.chatroomID,
		WorkspaceID:   c.workspaceID,
		EventType:     eventType,
		RoundNumber:   round,
		SpeakingOrder: speakingOrder,
		NodeID:        nodeID,
		Content:       content,
	}
	_, err := c.eventStore.Append(ctx, event)
	if err != nil {
		c.logger.Warn("failed to append discussion event", "event_type", eventType, "error", err)
	}
}

func (c *DiscussionCoordinator) electNewHost(ctx context.Context, failedNodeID pgtype.UUID) {
	newHost, err := ElectHost(c.state.Speakers, failedNodeID)
	if err != nil {
		c.logger.Error("failed to elect new host", "error", err)
		return
	}
	c.mu.Lock()
	c.state.HostNodeID = newHost
	c.mu.Unlock()
	c.emitEvent(ctx, EventHostChanged, int32(c.state.CurrentRound), 0, newHost, []byte(`{"reason":"host_failed"}`))
}

func (c *DiscussionCoordinator) isPaused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

func (c *DiscussionCoordinator) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = true
}

func (c *DiscussionCoordinator) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = false
}

func (c *DiscussionCoordinator) waitResume(ctx context.Context) bool {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if !c.isPaused() {
				return true
			}
		}
	}
}
