package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	"github.com/multica-ai/multica/server/internal/util"
)

// GetRongCloudConfig returns the RongCloud app key for the frontend SDK.
// Returns 403 when the RongCloud integration is not configured.
func (h *Handler) GetRongCloudConfig(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudInstall == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	appKey := h.RongCloudInstall.GetAppKey(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{"appKey": appKey})
}

// RegisterRongCloudAINode registers a new AI node in RongCloud and stores its
// credentials. Public endpoint (no workspace scope) — the node's workspace
// attribution comes from an optional pairing ticket in the request body; the
// owner defaults to a workspace manager resolved by the service.
func (h *Handler) RegisterRongCloudAINode(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	var req struct {
		Name          string   `json:"name"`
		MacAddress    string   `json:"mac_address"`
		NodeType      string   `json:"node_type"`
		AIType        string   `json:"ai_type"`
		Capabilities  []string `json:"capabilities"`
		PairingTicket string   `json:"pairing_ticket"`
		Agents        []string `json:"agents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	result, err := h.RongCloudNode.Register(r.Context(), rongcloud.NodeRegisterParams{
		Name:          req.Name,
		MacAddress:    req.MacAddress,
		NodeType:      req.NodeType,
		AIType:        req.AIType,
		Capabilities:  req.Capabilities,
		PairingTicket: req.PairingTicket,
		Agents:        req.Agents,
	})
	if err != nil {
		if errors.Is(err, rongcloud.ErrWorkspaceAttributionRequired) {
			writeError(w, http.StatusBadRequest, "workspace attribution required: provide a pairing_ticket")
			return
		}
		if errors.Is(err, rongcloud.ErrInvalidPairingTicket) {
			writeError(w, http.StatusBadRequest, "invalid or expired pairing_ticket")
			return
		}
		if strings.Contains(err.Error(), "duplicate") {
			writeError(w, http.StatusConflict, "node already registered")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to register node")
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// RefreshRongCloudToken refreshes a RongCloud AI node's token. Public endpoint
// — the nodeId in the path is the only credential; the token itself proves
// prior registration.
func (h *Handler) RefreshRongCloudToken(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	nodeID := chi.URLParam(r, "nodeId")
	if nodeID == "" {
		writeError(w, http.StatusBadRequest, "missing nodeId")
		return
	}
	token, err := h.RongCloudNode.RefreshToken(r.Context(), nodeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to refresh token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// ListRongCloudChatrooms lists RongCloud chatrooms for the workspace.
// Returns an empty array when the integration is not configured.
func (h *Handler) ListRongCloudChatrooms(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudChatroom == nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	wsID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	chatrooms, err := h.RongCloudChatroom.ListChatrooms(r.Context(), wsID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list chatrooms")
		return
	}
	writeJSON(w, http.StatusOK, chatrooms)
}

// CreateRongCloudChatroom creates a RongCloud chatroom in the workspace.
// Admin-only: requires owner or admin role on the workspace.
func (h *Handler) CreateRongCloudChatroom(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudChatroom == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	wsID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	ownerUUID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	var req struct {
		RongcloudChatroomID string `json:"rongcloud_chatroom_id"`
		MaxRounds           int32  `json:"max_rounds"`
		ConversationKind    string `json:"conversation_kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	chatroom, err := h.RongCloudChatroom.CreateChatroom(r.Context(), rongcloud.ChatroomCreateParams{
		WorkspaceID:         wsID,
		OwnerUserID:         ownerUUID,
		RongcloudChatroomID: req.RongcloudChatroomID,
		MaxRounds:           req.MaxRounds,
		ConversationKind:    req.ConversationKind,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create chatroom")
		return
	}
	writeJSON(w, http.StatusCreated, chatroom)
}

// DeleteRongCloudChatroom deletes a RongCloud chatroom. Admin-only.
func (h *Handler) DeleteRongCloudChatroom(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudChatroom == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
	if !ok {
		return
	}
	if err := h.RongCloudChatroom.DeleteChatroom(r.Context(), chatroomID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete chatroom")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Task 6: Public endpoints ---

// EnrollDeviceCredential enrolls a new device credential for a RongCloud node.
// Public endpoint (no workspace scope) — the nodeId identifies the node.
func (h *Handler) EnrollDeviceCredential(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	var req struct {
		NodeID string `json:"node_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.NodeID == "" {
		writeError(w, http.StatusBadRequest, "missing node_id")
		return
	}
	credID, secret, err := h.RongCloudNode.EnrollDeviceCredential(r.Context(), req.NodeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enroll device credential")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"credential_id": credID,
		"secret":        secret,
	})
}

// CreateConnectionSession opens a connection session for a RongCloud node.
// Public endpoint (no workspace scope).
func (h *Handler) CreateConnectionSession(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	var req struct {
		NodeID string `json:"node_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.NodeID == "" {
		writeError(w, http.StatusBadRequest, "missing node_id")
		return
	}
	sessionID, err := h.RongCloudNode.OpenConnectionSession(r.Context(), req.NodeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create connection session")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"session_id": sessionID})
}

// CloseConnectionSession closes a connection session.
// Public endpoint (no workspace scope).
func (h *Handler) CloseConnectionSession(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	sessionID := chi.URLParam(r, "sessionId")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "missing sessionId")
		return
	}
	if err := h.RongCloudNode.CloseConnectionSession(r.Context(), sessionID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to close connection session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ClaimRongCloudPairing lets a device claim a pairing session with a ticket.
// Public endpoint (no workspace scope): the ticket itself is the credential;
// an optional client claim key, bound at session creation, hardens it.
func (h *Handler) ClaimRongCloudPairing(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudPairing == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	ticket := chi.URLParam(r, "ticket")
	if ticket == "" {
		writeError(w, http.StatusBadRequest, "missing ticket")
		return
	}
	var req struct {
		ClientClaimKey string `json:"client_claim_key"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	result, err := h.RongCloudPairing.ClaimSession(r.Context(), ticket, req.ClientClaimKey, req.IdempotencyKey)
	if err != nil {
		switch {
		case errors.Is(err, rongcloud.ErrUnknownPairingTicket):
			writeError(w, http.StatusNotFound, "pairing session not found")
		case errors.Is(err, rongcloud.ErrPairingSessionNotPending):
			writeError(w, http.StatusConflict, "pairing session is not pending")
		case errors.Is(err, rongcloud.ErrPairingSessionExpired):
			writeError(w, http.StatusGone, "pairing session expired")
		case errors.Is(err, rongcloud.ErrInvalidClaimKey):
			writeError(w, http.StatusBadRequest, "invalid client claim key")
		default:
			writeError(w, http.StatusInternalServerError, "failed to claim pairing session")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_credential_id": result.DeviceCredentialID,
		"device_secret":        result.DeviceSecret,
		"node_id":              result.NodeID,
		"session":              result.Session,
	})
}

// --- Task 7: Workspace member GET endpoints ---

// GetRongCloudChatroom retrieves a single chatroom by ID.
func (h *Handler) GetRongCloudChatroom(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudChatroom == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
	if !ok {
		return
	}
	chatroom, err := h.RongCloudChatroom.GetChatroom(r.Context(), chatroomID)
	if err != nil {
		writeError(w, http.StatusNotFound, "chatroom not found")
		return
	}
	writeJSON(w, http.StatusOK, chatroom)
}

// ListRongCloudNodes lists RongCloud AI nodes for the workspace.
func (h *Handler) ListRongCloudNodes(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	wsID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	nodes, err := h.RongCloudNode.ListNodes(r.Context(), wsID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list nodes")
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}

// ListRongCloudNodeModels lists model catalogs for a node.
func (h *Handler) ListRongCloudNodeModels(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	nodeID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "nodeId"), "node id")
	if !ok {
		return
	}
	models, err := h.RongCloudNode.ListNodeModels(r.Context(), nodeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list node models")
		return
	}
	writeJSON(w, http.StatusOK, models)
}

// ListRongCloudDevices lists RongCloud devices for the workspace.
func (h *Handler) ListRongCloudDevices(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	wsID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	devices, err := h.RongCloudNode.ListDevices(r.Context(), wsID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list devices")
		return
	}
	writeJSON(w, http.StatusOK, devices)
}

// GetRongCloudSystemHost retrieves the system host configuration for the workspace.
func (h *Handler) GetRongCloudSystemHost(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudInstall == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	wsID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	cfg, err := h.RongCloudInstall.GetSystemConfig(r.Context(), wsID, "system_host")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{})
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// GetRongCloudPairing retrieves a pairing session by ticket.
func (h *Handler) GetRongCloudPairing(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudPairing == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	ticket := chi.URLParam(r, "ticket")
	if ticket == "" {
		writeError(w, http.StatusBadRequest, "missing ticket")
		return
	}
	session, err := h.RongCloudPairing.GetSession(r.Context(), ticket)
	if err != nil {
		writeError(w, http.StatusNotFound, "pairing session not found")
		return
	}
	writeJSON(w, http.StatusOK, session)
}

// --- Task 8: Workspace admin POST/PUT/DELETE endpoints ---

// UpdateRongCloudChatroom updates a chatroom's configuration.
func (h *Handler) UpdateRongCloudChatroom(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudChatroom == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
	if !ok {
		return
	}
	var req struct {
		HostNodeID       string          `json:"host_node_id"`
		MaxRounds        int32           `json:"max_rounds"`
		ConversationKind string          `json:"conversation_kind"`
		Config           json.RawMessage `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	hostNodeID, err := util.ParseUUID(req.HostNodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid host_node_id")
		return
	}
	chatroom, err := h.RongCloudChatroom.UpdateChatroom(r.Context(), chatroomID, hostNodeID, req.MaxRounds, req.ConversationKind, req.Config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update chatroom")
		return
	}
	writeJSON(w, http.StatusOK, chatroom)
}

// SetRongCloudChatroomMembers replaces all AI members in a chatroom.
func (h *Handler) SetRongCloudChatroomMembers(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudChatroom == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
	if !ok {
		return
	}
	var req struct {
		Members []struct {
			NodeID           string          `json:"node_id"`
			MemberType       string          `json:"member_type"`
			RoleName         string          `json:"role_name"`
			RoleInstructions string          `json:"role_instructions"`
			Capabilities     json.RawMessage `json:"capabilities"`
			Model            string          `json:"model"`
			SpeakingOrder    int32           `json:"speaking_order"`
			DiscussionModel  string          `json:"discussion_model"`
		} `json:"members"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	members := make([]rongcloud.ChatroomMemberConfig, 0, len(req.Members))
	for _, m := range req.Members {
		nodeUUID, err := util.ParseUUID(m.NodeID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid member node_id")
			return
		}
		members = append(members, rongcloud.ChatroomMemberConfig{
			NodeID:           nodeUUID,
			MemberType:       m.MemberType,
			RoleName:         m.RoleName,
			RoleInstructions: m.RoleInstructions,
			Capabilities:     m.Capabilities,
			Model:            m.Model,
			SpeakingOrder:    m.SpeakingOrder,
			DiscussionModel:  m.DiscussionModel,
		})
	}
	if err := h.RongCloudChatroom.SetMembers(r.Context(), chatroomID, members); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set chatroom members")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// AddRongCloudNodeModel adds a model to a node's catalog.
func (h *Handler) AddRongCloudNodeModel(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	nodeID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "nodeId"), "node id")
	if !ok {
		return
	}
	wsID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	var req struct {
		ModelID   string          `json:"model_id"`
		Provider  string          `json:"provider"`
		ModelName string          `json:"model_name"`
		Config    json.RawMessage `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	catalog, err := h.RongCloudNode.AddNodeModel(r.Context(), wsID, nodeID, req.ModelID, req.Provider, req.ModelName, req.Config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to add node model")
		return
	}
	writeJSON(w, http.StatusCreated, catalog)
}

// DeleteRongCloudNode deletes a RongCloud AI node.
func (h *Handler) DeleteRongCloudNode(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	nodeID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "nodeId"), "node id")
	if !ok {
		return
	}
	if err := h.RongCloudNode.DeleteNode(r.Context(), nodeID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete node")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// CreateRongCloudDevice creates a new device for a node.
func (h *Handler) CreateRongCloudDevice(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	wsID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	ownerUUID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	var req struct {
		NodeID     string `json:"node_id"`
		DeviceName string `json:"device_name"`
		DeviceType string `json:"device_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	nodeUUID, err := util.ParseUUID(req.NodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node_id")
		return
	}
	device, err := h.RongCloudNode.CreateDevice(r.Context(), rongcloud.DeviceCreateParams{
		WorkspaceID: wsID,
		OwnerUserID: ownerUUID,
		NodeID:      nodeUUID,
		DeviceName:  req.DeviceName,
		DeviceType:  req.DeviceType,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create device")
		return
	}
	writeJSON(w, http.StatusCreated, device)
}

// DeleteRongCloudDevice deletes a RongCloud device.
func (h *Handler) DeleteRongCloudDevice(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudNode == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	deviceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "deviceId"), "device id")
	if !ok {
		return
	}
	if err := h.RongCloudNode.DeleteDevice(r.Context(), deviceID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete device")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// UpdateRongCloudSystemHost sets the system host node for the workspace.
func (h *Handler) UpdateRongCloudSystemHost(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudInstall == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	wsID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	var req struct {
		NodeID string          `json:"node_id"`
		Config json.RawMessage `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	nodeUUID, err := util.ParseUUID(req.NodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node_id")
		return
	}
	cfg, err := h.RongCloudInstall.UpsertSystemConfig(r.Context(), wsID, "system_host", nodeUUID, req.Config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update system host")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// CreateRongCloudPairing creates a pairing session.
func (h *Handler) CreateRongCloudPairing(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudPairing == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	wsID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	var req struct {
		CandidateNodeIDs []string `json:"candidate_node_ids"`
		ClientClaimKey   string   `json:"client_claim_key"`
		ExpiresInSeconds int      `json:"expires_in_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	candidates := make([]pgtype.UUID, 0, len(req.CandidateNodeIDs))
	for _, idStr := range req.CandidateNodeIDs {
		nodeUUID, err := util.ParseUUID(idStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid candidate node id")
			return
		}
		candidates = append(candidates, nodeUUID)
	}
	params := rongcloud.PairingCreateParams{
		WorkspaceID:      wsID,
		CandidateNodeIDs: candidates,
		ClientClaimKey:   req.ClientClaimKey,
	}
	if req.ExpiresInSeconds > 0 {
		params.ExpiresIn = time.Duration(req.ExpiresInSeconds) * time.Second
	}
	session, err := h.RongCloudPairing.CreateSession(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create pairing session")
		return
	}
	writeJSON(w, http.StatusCreated, session)
}

// --- Phase 3: Discussion endpoints ---

// StartRongCloudDiscussion starts a multi-AI-node discussion in a chatroom.
// Admin-only: requires owner or admin role on the workspace.
func (h *Handler) StartRongCloudDiscussion(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudDiscussion == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
	if !ok {
		return
	}
	state, err := h.RongCloudDiscussion.StartDiscussion(r.Context(), chatroomID)
	if err != nil {
		if strings.Contains(err.Error(), "already active") {
			writeError(w, http.StatusConflict, "discussion already active")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to start discussion")
		return
	}
	writeJSON(w, http.StatusCreated, state)
}

// StopRongCloudDiscussion stops an active discussion.
// Admin-only: requires owner or admin role on the workspace.
func (h *Handler) StopRongCloudDiscussion(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudDiscussion == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
	if !ok {
		return
	}
	state, err := h.RongCloudDiscussion.StopDiscussion(r.Context(), chatroomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to stop discussion")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// PauseRongCloudDiscussion pauses an active discussion.
// Admin-only: requires owner or admin role on the workspace.
func (h *Handler) PauseRongCloudDiscussion(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudDiscussion == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
	if !ok {
		return
	}
	state, err := h.RongCloudDiscussion.PauseDiscussion(r.Context(), chatroomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to pause discussion")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// ResumeRongCloudDiscussion resumes a paused discussion.
// Admin-only: requires owner or admin role on the workspace.
func (h *Handler) ResumeRongCloudDiscussion(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudDiscussion == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
	if !ok {
		return
	}
	state, err := h.RongCloudDiscussion.ResumeDiscussion(r.Context(), chatroomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resume discussion")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// GetRongCloudDiscussion retrieves the current discussion status for a chatroom.
func (h *Handler) GetRongCloudDiscussion(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudDiscussion == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
	if !ok {
		return
	}
	state, err := h.RongCloudDiscussion.GetDiscussionStatus(r.Context(), chatroomID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "idle"})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// ListRongCloudDiscussionEvents lists discussion events for a chatroom.
func (h *Handler) ListRongCloudDiscussionEvents(w http.ResponseWriter, r *http.Request) {
	if h.RongCloudDiscussion == nil {
		writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud integration is not configured")
		return
	}
	chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
	if !ok {
		return
	}
	events, err := h.RongCloudDiscussion.ListEvents(r.Context(), chatroomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list discussion events")
		return
	}
	writeJSON(w, http.StatusOK, events)
}
