package rongcloud

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type NodeRegisterParams struct {
	Name         string   `json:"name"`
	MacAddress   string   `json:"mac_address"`
	NodeType     string   `json:"node_type"`
	AIType       string   `json:"ai_type"`
	Capabilities []string `json:"capabilities"`
	WorkspaceID  pgtype.UUID
	OwnerUserID  pgtype.UUID
}

type NodeRegisterResult struct {
	NodeID                 string   `json:"node_id"`
	Token                  string   `json:"token"`
	Capabilities           []string `json:"capabilities"`
	DeviceCredentialTicket string   `json:"device_credential_ticket"`
	BindingVersion         int      `json:"binding_version"`
}

type NodeService struct {
	queries *db.Queries
	client  *rongcloudAPIClient
	box     *secretbox.Box
	logger  *slog.Logger
}

func NewNodeService(queries *db.Queries, client *rongcloudAPIClient, box *secretbox.Box, logger *slog.Logger) *NodeService {
	if logger == nil {
		logger = slog.Default()
	}
	return &NodeService{queries: queries, client: client, box: box, logger: logger}
}

func pgText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

func (s *NodeService) enrollmentToken(bridgeSecret, serverURL, runtimeID string) string {
	message := fmt.Sprintf("quukk/server-enrollment/v1\x00%s\x00%s", serverURL, runtimeID)
	mac := hmac.New(sha256.New, []byte(bridgeSecret))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *NodeService) VerifyEnrollmentToken(token, bridgeSecret, serverURL, runtimeID string) bool {
	expected := s.enrollmentToken(bridgeSecret, serverURL, runtimeID)
	return hmac.Equal([]byte(token), []byte(expected))
}

func (s *NodeService) Register(ctx context.Context, params NodeRegisterParams) (NodeRegisterResult, error) {
	if s.queries == nil {
		return NodeRegisterResult{}, errors.New("rongcloud: database not configured")
	}
	if s.client == nil {
		return NodeRegisterResult{}, errors.New("rongcloud: API client not configured")
	}
	rcUserID := fmt.Sprintf("rc_node_%s", params.MacAddress)
	token, err := s.client.getUserToken(ctx, rcUserID, params.Name, "")
	if err != nil {
		return NodeRegisterResult{}, fmt.Errorf("rongcloud: getUserToken: %w", err)
	}
	encToken := ""
	if s.box != nil {
		sealed, err := s.box.Seal([]byte(token))
		if err != nil {
			return NodeRegisterResult{}, fmt.Errorf("rongcloud: encrypt token: %w", err)
		}
		encToken = base64.StdEncoding.EncodeToString(sealed)
	}
	rcUser, err := s.queries.CreateRongCloudUser(ctx, db.CreateRongCloudUserParams{
		WorkspaceID:     params.WorkspaceID,
		RongcloudUserID: rcUserID,
		Name:            pgText(params.Name),
		TokenEncrypted:  pgText(encToken),
		IsAiNode:        true,
		NodeType:        "ai",
	})
	if err != nil {
		return NodeRegisterResult{}, fmt.Errorf("rongcloud: create user: %w", err)
	}
	nodeID := fmt.Sprintf("node_%s", hex.EncodeToString(rcUser.ID.Bytes[:8]))
	capabilitiesJSON, _ := json.Marshal(params.Capabilities)
	node, err := s.queries.CreateRongCloudNode(ctx, db.CreateRongCloudNodeParams{
		WorkspaceID:     params.WorkspaceID,
		OwnerUserID:     params.OwnerUserID,
		RongcloudUserID: rcUserID,
		NodeID:          nodeID,
		AiType:          pgText(params.AIType),
		Capabilities:     capabilitiesJSON,
		DeployStatus:    "offline",
		BindingVersion:  1,
	})
	if err != nil {
		return NodeRegisterResult{}, fmt.Errorf("rongcloud: create node: %w", err)
	}
	credID, credSecret, err := generateDeviceCredential()
	if err != nil {
		return NodeRegisterResult{}, fmt.Errorf("rongcloud: generate device credential: %w", err)
	}
	encSecret := ""
	if s.box != nil {
		sealed, err := s.box.Seal([]byte(credSecret))
		if err == nil {
			encSecret = base64.StdEncoding.EncodeToString(sealed)
		}
	}
	_, _ = s.queries.CreateRongCloudDevice(ctx, db.CreateRongCloudDeviceParams{
		WorkspaceID:               params.WorkspaceID,
		OwnerUserID:               params.OwnerUserID,
		NodeID:                    node.ID,
		DeviceName:                params.Name,
		DeviceType:                pgText(params.AIType),
		CredentialID:              pgText(credID),
		CredentialSecretEncrypted: pgText(encSecret),
		Status:                    "active",
	})
	return NodeRegisterResult{
		NodeID:                 nodeID,
		Token:                  token,
		Capabilities:           params.Capabilities,
		DeviceCredentialTicket: credID,
		BindingVersion:         int(node.BindingVersion),
	}, nil
}

func (s *NodeService) RefreshToken(ctx context.Context, nodeID string) (string, error) {
	if s.queries == nil || s.client == nil {
		return "", errors.New("rongcloud: service not configured")
	}
	node, err := s.queries.GetRongCloudNodeByNodeID(ctx, nodeID)
	if err != nil {
		return "", fmt.Errorf("rongcloud: get node: %w", err)
	}
	token, err := s.client.getUserToken(ctx, node.RongcloudUserID, "", "")
	if err != nil {
		return "", fmt.Errorf("rongcloud: refresh token: %w", err)
	}
	encToken := ""
	if s.box != nil {
		sealed, err := s.box.Seal([]byte(token))
		if err == nil {
			encToken = base64.StdEncoding.EncodeToString(sealed)
		}
	}
	rcUser, err := s.queries.GetRongCloudUserByRongCloudID(ctx, node.RongcloudUserID)
	if err != nil {
		return "", fmt.Errorf("rongcloud: get user: %w", err)
	}
	_, _ = s.queries.UpdateRongCloudUserToken(ctx, db.UpdateRongCloudUserTokenParams{
		ID:             rcUser.ID,
		TokenEncrypted: pgText(encToken),
	})
	return token, nil
}

func generateDeviceCredential() (string, string, error) {
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", err
	}
	credID := "dc_" + hex.EncodeToString(idBytes)
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", err
	}
	secret := base64.URLEncoding.EncodeToString(secretBytes)
	return credID, secret, nil
}

func (s *NodeService) EnrollDeviceCredential(ctx context.Context, nodeID string) (string, string, error) {
	if s.queries == nil {
		return "", "", errors.New("rongcloud: database not configured")
	}
	node, err := s.queries.GetRongCloudNodeByNodeID(ctx, nodeID)
	if err != nil {
		return "", "", fmt.Errorf("rongcloud: get node for enroll: %w", err)
	}
	credID, secret, err := generateDeviceCredential()
	if err != nil {
		return "", "", err
	}
	encSecret := ""
	if s.box != nil {
		sealed, err := s.box.Seal([]byte(secret))
		if err == nil {
			encSecret = base64.StdEncoding.EncodeToString(sealed)
		}
	}
	_, err = s.queries.CreateRongCloudDevice(ctx, db.CreateRongCloudDeviceParams{
		WorkspaceID:               node.WorkspaceID,
		OwnerUserID:               node.OwnerUserID,
		NodeID:                    node.ID,
		DeviceName:                node.NodeID,
		DeviceType:                node.AiType,
		CredentialID:              pgText(credID),
		CredentialSecretEncrypted: pgText(encSecret),
		Status:                    "active",
	})
	if err != nil {
		return "", "", fmt.Errorf("rongcloud: create device: %w", err)
	}
	return credID, secret, nil
}

func (s *NodeService) ListNodeModels(ctx context.Context, nodeID pgtype.UUID) ([]db.RongcloudNodeModelCatalog, error) {
	if s.queries == nil {
		return nil, errors.New("rongcloud: database not configured")
	}
	return s.queries.ListRongCloudNodeModelCatalogsByNode(ctx, nodeID)
}

func (s *NodeService) ListNodes(ctx context.Context, workspaceID pgtype.UUID) ([]db.RongcloudNode, error) {
	if s.queries == nil {
		return nil, errors.New("rongcloud: database not configured")
	}
	return s.queries.ListRongCloudNodesByWorkspace(ctx, workspaceID)
}

func (s *NodeService) ListDevices(ctx context.Context, workspaceID pgtype.UUID) ([]db.RongcloudDevice, error) {
	if s.queries == nil {
		return nil, errors.New("rongcloud: database not configured")
	}
	return s.queries.ListRongCloudDevicesByWorkspace(ctx, workspaceID)
}

func (s *NodeService) DeleteNode(ctx context.Context, id pgtype.UUID) error {
	if s.queries == nil {
		return errors.New("rongcloud: database not configured")
	}
	return s.queries.DeleteRongCloudNode(ctx, id)
}

func (s *NodeService) DeleteDevice(ctx context.Context, id pgtype.UUID) error {
	if s.queries == nil {
		return errors.New("rongcloud: database not configured")
	}
	return s.queries.DeleteRongCloudDevice(ctx, id)
}

type DeviceCreateParams struct {
	WorkspaceID pgtype.UUID
	OwnerUserID pgtype.UUID
	NodeID      pgtype.UUID
	DeviceName  string
	DeviceType  string
}

func (s *NodeService) CreateDevice(ctx context.Context, params DeviceCreateParams) (db.RongcloudDevice, error) {
	if s.queries == nil {
		return db.RongcloudDevice{}, errors.New("rongcloud: database not configured")
	}
	credID, credSecret, err := generateDeviceCredential()
	if err != nil {
		return db.RongcloudDevice{}, err
	}
	encSecret := ""
	if s.box != nil {
		sealed, err := s.box.Seal([]byte(credSecret))
		if err == nil {
			encSecret = base64.StdEncoding.EncodeToString(sealed)
		}
	}
	return s.queries.CreateRongCloudDevice(ctx, db.CreateRongCloudDeviceParams{
		WorkspaceID:               params.WorkspaceID,
		OwnerUserID:               params.OwnerUserID,
		NodeID:                    params.NodeID,
		DeviceName:                params.DeviceName,
		DeviceType:                pgText(params.DeviceType),
		CredentialID:              pgText(credID),
		CredentialSecretEncrypted: pgText(encSecret),
		Status:                    "active",
	})
}

func (s *NodeService) AddNodeModel(ctx context.Context, workspaceID, nodeID pgtype.UUID, modelID, provider, modelName string, config json.RawMessage) (db.RongcloudNodeModelCatalog, error) {
	if s.queries == nil {
		return db.RongcloudNodeModelCatalog{}, errors.New("rongcloud: database not configured")
	}
	return s.queries.CreateRongCloudNodeModelCatalog(ctx, db.CreateRongCloudNodeModelCatalogParams{
		WorkspaceID: workspaceID,
		NodeID:      nodeID,
		ModelID:     modelID,
		Provider:    pgText(provider),
		ModelName:   pgText(modelName),
		Config:      config,
	})
}

func (s *NodeService) RemoveNodeModel(ctx context.Context, catalogID pgtype.UUID) error {
	if s.queries == nil {
		return errors.New("rongcloud: database not configured")
	}
	return s.queries.DeleteRongCloudNodeModelCatalog(ctx, catalogID)
}

func (s *NodeService) OpenConnectionSession(ctx context.Context, nodeID string) (string, error) {
	if s.queries == nil {
		return "", errors.New("rongcloud: database not configured")
	}
	node, err := s.queries.GetRongCloudNodeByNodeID(ctx, nodeID)
	if err != nil {
		return "", fmt.Errorf("rongcloud: get node for session: %w", err)
	}
	ts := time.Now().UnixMilli()
	wsHex := hex.EncodeToString(node.WorkspaceID.Bytes[:])
	sessionID := fmt.Sprintf("sess_%s_%x", wsHex, ts)
	configJSON, _ := json.Marshal(map[string]interface{}{
		"node_id":   nodeID,
		"opened_at": ts,
	})
	_, err = s.queries.UpsertRongCloudSystemConfig(ctx, db.UpsertRongCloudSystemConfigParams{
		WorkspaceID:  node.WorkspaceID,
		ConfigKey:    "connection_session:" + sessionID,
		NodeID:       node.ID,
		Config:       configJSON,
		ConfigVersion: 1,
	})
	if err != nil {
		return "", fmt.Errorf("rongcloud: store session: %w", err)
	}
	return sessionID, nil
}

func (s *NodeService) CloseConnectionSession(ctx context.Context, sessionID string) error {
	if s.queries == nil {
		return errors.New("rongcloud: database not configured")
	}
	wsID, ok := parseWorkspaceFromSessionID(sessionID)
	if !ok {
		return fmt.Errorf("rongcloud: invalid session ID format")
	}
	return s.queries.DeleteRongCloudSystemConfig(ctx, db.DeleteRongCloudSystemConfigParams{
		WorkspaceID: wsID,
		ConfigKey:   "connection_session:" + sessionID,
	})
}

func parseWorkspaceFromSessionID(sessionID string) (pgtype.UUID, bool) {
	if len(sessionID) < 38 || sessionID[:5] != "sess_" {
		return pgtype.UUID{}, false
	}
	rest := sessionID[5:]
	if len(rest) < 33 || rest[32] != '_' {
		return pgtype.UUID{}, false
	}
	wsBytes, err := hex.DecodeString(rest[:32])
	if err != nil || len(wsBytes) != 16 {
		return pgtype.UUID{}, false
	}
	var wsID pgtype.UUID
	copy(wsID.Bytes[:], wsBytes)
	wsID.Valid = true
	return wsID, true
}
