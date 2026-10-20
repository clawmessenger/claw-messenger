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
			"node_id": resp.NodeID,
			"content": resp.Content,
			"turn_id": resp.TurnID,
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
			"node_id": resp.NodeID,
			"content": fullText,
			"turn_id": resp.TurnID,
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

// personaFor assembles the persona text one speaker must adopt this turn:
// the operator's 「人设提示词」 for the host node, and the member's own
// 「角色人设」 for everybody (the host may carry both — the operator text is the
// moderator role, the membership text an additional assignment).
//
// Both were collected but never delivered before: role_instructions was read
// into SpeakerInfo and dropped, and discussion_host_prompt had no reader at all.
func (c *DiscussionCoordinator) personaFor(speaker SpeakerInfo) string {
	parts := make([]string, 0, 2)
	if c.state.HostPrompt != "" && speaker.NodeID == c.state.HostNodeID {
		parts = append(parts, c.state.HostPrompt)
	}
	if speaker.RolePrompt != "" {
		parts = append(parts, speaker.RolePrompt)
	}
	return strings.Join(parts, "\n\n")
}

func (c *DiscussionCoordinator) sendYourTurn(ctx context.Context, speaker SpeakerInfo, round int, speakingOrder int32) error {
	if c.bridge != nil && c.bridge.IsServerManagedByType(speaker.AiType) {
		go c.executeBridgeTurn(ctx, speaker, round, int(speakingOrder))
		return nil
	}

	params := map[string]interface{}{
		"round":           round,
		"speaking_order":  speakingOrder,
		"role_name":       speaker.RoleName,
		"model":           speaker.Model,
		"chatroom_id":     chatroomKey(c.chatroomID),
		"discussion_kind": c.state.Status,
		// 流式帧路由参数：CLI 据此把 chat_stream 帧直接广播进聊天室会话。
		// requester_id 留空（正式轮次由服务端编排而非某用户发起，web 端
		// validStreamDelivery 对群/聊天室只校验 conversation_id）。
		"conversation_type": 4,
	}
	// Omitted rather than sent empty, so a reader that predates the field sees
	// the same payload shape it always did.
	if persona := c.personaFor(speaker); persona != "" {
		params["role_prompt"] = persona
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"request_id": fmt.Sprintf("turn_%d_%d_%s", round, speakingOrder, time.Now().Format("20060102150405")),
		"service":    "discussion",
		"action":     "your_turn",
		"params":     params,
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
	if persona := c.personaFor(speaker); persona != "" {
		sb.WriteString("\n")
		sb.WriteString(persona)
		sb.WriteString("\n")
	}
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
	// content is NOT NULL JSONB; events without a payload get '{}'.
	if len(content) == 0 {
		content = []byte("{}")
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
