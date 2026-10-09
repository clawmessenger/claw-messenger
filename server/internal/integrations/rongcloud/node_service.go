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
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type NodeRegisterParams struct {
	Name          string   `json:"name"`
	MacAddress    string   `json:"mac_address"`
	NodeType      string   `json:"node_type"`
	AIType        string   `json:"ai_type"`
	Capabilities  []string `json:"capabilities"`
	PairingTicket string   `json:"pairing_ticket"` // optional: attribute node to the ticket's workspace
	Agents        []string `json:"agents"`         // optional: agents discovered on the device, reported to the pairing session
	WorkspaceID   pgtype.UUID
	OwnerUserID   pgtype.UUID
}

type NodeRegisterResult struct {
	NodeID                 string   `json:"node_id"`
	Token                  string   `json:"token"`
	Capabilities           []string `json:"capabilities"`
	DeviceCredentialTicket string   `json:"device_credential_ticket"`
	BindingVersion         int      `json:"binding_version"`
}

type NodeService struct {
	queries    *db.Queries
	client     *rongcloudAPIClient
	box        *secretbox.Box
	logger     *slog.Logger
	mu         sync.Mutex
	heartbeats map[pgtype.UUID]time.Time
}

func NewNodeService(queries *db.Queries, client *rongcloudAPIClient, box *secretbox.Box, logger *slog.Logger) *NodeService {
	if logger == nil {
		logger = slog.Default()
	}
	return &NodeService{queries: queries, client: client, box: box, logger: logger, heartbeats: make(map[pgtype.UUID]time.Time)}
}

// heartbeatTTL is how long a device heartbeat stays fresh before the node is
// considered offline.
const heartbeatTTL = 90 * time.Second

// Heartbeat authenticates a device credential and records a liveness tick for
// the node, used to render online/offline status in the legacy device list.
func (s *NodeService) Heartbeat(ctx context.Context, nodeIDText, credentialID, secret string) error {
	if s.queries == nil {
		return errors.New("rongcloud: database not configured")
	}
	node, err := s.queries.GetRongCloudNodeByNodeID(ctx, nodeIDText)
	if err != nil {
		return ErrInvalidDeviceCredential
	}
	device, err := s.queries.GetRongCloudDeviceByNodeAndCredential(ctx, db.GetRongCloudDeviceByNodeAndCredentialParams{
		NodeID:       node.ID,
		CredentialID: pgtype.Text{String: credentialID, Valid: credentialID != ""},
	})
	if err != nil {
		return ErrInvalidDeviceCredential
	}
	if !device.CredentialSecretEncrypted.Valid || s.box == nil {
		return ErrInvalidDeviceCredential
	}
	sealed, err := base64.StdEncoding.DecodeString(device.CredentialSecretEncrypted.String)
	if err != nil {
		return ErrInvalidDeviceCredential
	}
	plain, err := s.box.Open(sealed)
	if err != nil || !hmac.Equal([]byte(plain), []byte(secret)) {
		return ErrInvalidDeviceCredential
	}
	s.mu.Lock()
	s.heartbeats[node.ID] = time.Now()
	s.mu.Unlock()
	return nil
}

// IsOnline reports whether the node has heartbeated within heartbeatTTL.
func (s *NodeService) IsOnline(id pgtype.UUID) bool {
	s.mu.Lock()
	last, ok := s.heartbeats[id]
	s.mu.Unlock()
	return ok && time.Since(last) < heartbeatTTL
}

// GetNodeByNodeID 按业务节点 ID（node_xxx）查节点行，供状态端点使用。
func (s *NodeService) GetNodeByNodeID(ctx context.Context, nodeIDText string) (db.RongcloudNode, error) {
	return s.queries.GetRongCloudNodeByNodeID(ctx, nodeIDText)
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

// rongcloudRCIdentityLimit is the provider's maximum length for a user id.
const rongcloudRCIdentityLimit = 64

// machineIdentityKey derives the stable key used inside rongcloud user ids
// from a raw machine identifier. Rongcloud caps user ids at 64 chars, and CLI
// machine ids look like "clawmessenger-<uuid>" (51 chars); combined ids such
// as rc_node_<machine>_<agent> would overflow. Long raw ids are shortened to
// a stable sha256 prefix while short ones (e.g. real mac addresses) are kept
// verbatim for backwards compatibility with existing rows.
func machineIdentityKey(machineID string) string {
	// "rc_node_" (8) + machine + "_" (1) + agent: reserve room for the longest
	// suffix we ever append. 24 is the longest agent name we reasonably ship.
	reserve := 8 + 1 + 24
	budget := rongcloudRCIdentityLimit - reserve
	if budget < 8 {
		budget = 8
	}
	if len(machineID) <= budget {
		return machineID
	}
	sum := sha256.Sum256([]byte(machineID))
	return "m_" + hex.EncodeToString(sum[:12])
}

func (s *NodeService) Register(ctx context.Context, params NodeRegisterParams) (NodeRegisterResult, error) {
	if s.queries == nil {
		return NodeRegisterResult{}, errors.New("rongcloud: database not configured")
	}
	if s.client == nil {
		return NodeRegisterResult{}, errors.New("rongcloud: API client not configured")
	}
	// Resolve workspace attribution: explicit params take precedence, then a
	// pending pairing ticket. The rongcloud tables require a workspace and an
	// owner, so without either source Register fails fast instead of pushing
	// a NOT NULL violation down into the database.
	if !params.WorkspaceID.Valid {
		resolved, err := s.resolveWorkspaceAttribution(ctx, params.PairingTicket)
		if err != nil {
			return NodeRegisterResult{}, err
		}
		params.WorkspaceID = resolved.workspaceID
		if !params.OwnerUserID.Valid {
			params.OwnerUserID = resolved.ownerUserID
		}
	}
	rcUserID := fmt.Sprintf("rc_node_%s", machineIdentityKey(params.MacAddress))
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
	// Register must be idempotent per machine: a repeat `clawmessenger pair` on a
	// device that already registered would otherwise collide with the unique
	// rongcloud_user_id / node rows and fail with a 500. Reuse the existing
	// identity and just refresh its stored IM token.
	rcUser, err := s.queries.GetRongCloudUserByRongCloudID(ctx, rcUserID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return NodeRegisterResult{}, fmt.Errorf("rongcloud: lookup user: %w", err)
	}
	if err == nil {
		_, _ = s.queries.UpdateRongCloudUserToken(ctx, db.UpdateRongCloudUserTokenParams{
			ID:             rcUser.ID,
			TokenEncrypted: pgText(encToken),
		})
	} else {
		rcUser, err = s.queries.CreateRongCloudUser(ctx, db.CreateRongCloudUserParams{
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
	}
	nodeID := fmt.Sprintf("node_%s", hex.EncodeToString(rcUser.ID.Bytes[:8]))
	capabilitiesJSON, _ := json.Marshal(params.Capabilities)
	node, err := s.queries.GetRongCloudNodeByRongCloudUserID(ctx, rcUserID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return NodeRegisterResult{}, fmt.Errorf("rongcloud: lookup node: %w", err)
	}
	if err != nil {
		node, err = s.queries.CreateRongCloudNode(ctx, db.CreateRongCloudNodeParams{
			WorkspaceID:     params.WorkspaceID,
			OwnerUserID:     params.OwnerUserID,
			RongcloudUserID: rcUserID,
			NodeID:          nodeID,
			AiType:          pgText(params.AIType),
			Capabilities:    capabilitiesJSON,
			DeployStatus:    "offline",
			BindingVersion:  1,
			MachineID:       params.MacAddress,
		})
		if err != nil {
			return NodeRegisterResult{}, fmt.Errorf("rongcloud: create node: %w", err)
		}
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
	// When registration is attributed to a pairing ticket, record the newly
	// created node as the session's candidate so a later claim can mint the
	// device credential. Best-effort: a missing or already-claimed session
	// must not fail the registration itself.
	if params.PairingTicket != "" {
		if candidates, err := json.Marshal([]pgtype.UUID{node.ID}); err == nil {
			_, _ = s.queries.BackfillRongCloudPairingSessionCandidates(ctx, db.BackfillRongCloudPairingSessionCandidatesParams{
				Ticket:           params.PairingTicket,
				CandidateNodeIds: candidates,
			})
		}
		// Report the agents the device discovered so the web dialog can list
		// them for check-box binding. Best-effort, same as the backfill.
		reported := params.Agents
		if reported == nil {
			reported = []string{}
		}
		if agentsJSON, err := json.Marshal(reported); err == nil {
			_, _ = s.queries.UpdateRongCloudPairingSessionReportedAgents(ctx, db.UpdateRongCloudPairingSessionReportedAgentsParams{
				Ticket:         params.PairingTicket,
				ReportedAgents: agentsJSON,
			})
		}
	}
	return NodeRegisterResult{
		NodeID:                 nodeID,
		Token:                  token,
		Capabilities:           params.Capabilities,
		DeviceCredentialTicket: credID,
		BindingVersion:         int(node.BindingVersion),
	}, nil
}

// ErrWorkspaceAttributionRequired is returned by Register when neither an
// explicit workspace nor a usable pairing ticket was supplied. The rongcloud
// tables have no nullable workspace, so an unattributed node cannot be stored.
var ErrWorkspaceAttributionRequired = errors.New("rongcloud: workspace attribution required")

// ErrInvalidPairingTicket is returned by Register when the pairing ticket does
// not exist, is no longer pending, or has expired. Registering with a bad
// ticket must fail loudly, not silently drop the attribution.
var ErrInvalidPairingTicket = errors.New("rongcloud: invalid or expired pairing ticket")

type workspaceAttribution struct {
	workspaceID pgtype.UUID
	ownerUserID pgtype.UUID
}

// resolveWorkspaceAttribution fills the workspace (and a default owner) from a
// pending, unexpired pairing ticket. An empty ticket yields
// ErrWorkspaceAttributionRequired; an unknown, non-pending, or expired one
// yields ErrInvalidPairingTicket. Any other lookup failure is a database
// fault, not a verdict on the ticket: it is logged and returned as a distinct
// error so the handler maps it to 500 instead of a wrong 400. The owner
// defaults to the workspace's first manager (owner role first) because the
// pairing session carries no owner of its own; an explicit params.OwnerUserID
// still wins.
func (s *NodeService) resolveWorkspaceAttribution(ctx context.Context, ticket string) (workspaceAttribution, error) {
	if ticket == "" {
		return workspaceAttribution{}, ErrWorkspaceAttributionRequired
	}
	session, err := s.queries.GetRongCloudPairingSessionByTicket(ctx, ticket)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return workspaceAttribution{}, ErrInvalidPairingTicket
		}
		s.logger.Error("rongcloud: pairing ticket lookup failed", "error", err)
		return workspaceAttribution{}, fmt.Errorf("rongcloud: get pairing session: %w", err)
	}
	if session.Status != "pending" || !session.ExpiresAt.Time.After(time.Now()) {
		return workspaceAttribution{}, ErrInvalidPairingTicket
	}
	managers, err := s.queries.ListWorkspaceManagerUserIDs(ctx, session.WorkspaceID)
	if err != nil {
		s.logger.Error("rongcloud: workspace manager lookup failed", "workspace_id", session.WorkspaceID, "error", err)
		return workspaceAttribution{}, fmt.Errorf("rongcloud: resolve workspace owner: %w", err)
	}
	if len(managers) == 0 {
		// rongcloud_node.owner_user_id is NOT NULL; a zero UUID would only
		// resurface as an opaque insert failure further down.
		return workspaceAttribution{}, fmt.Errorf("rongcloud: workspace %s has no manager to own the node", session.WorkspaceID)
	}
	return workspaceAttribution{workspaceID: session.WorkspaceID, ownerUserID: managers[0]}, nil
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

// LegacyNodeRecords returns all nodes in the installation workspace mapped to
// the legacy NodeRecord shape consumed by the old web client's device list.
func (s *NodeService) LegacyNodeRecords(ctx context.Context) ([]map[string]interface{}, error) {
	if s.queries == nil {
		return nil, errors.New("rongcloud: no queries")
	}
	insts, err := s.queries.ListActiveChannelInstallations(ctx, string(TypeRongCloud))
	if err != nil {
		return nil, err
	}
	if len(insts) == 0 {
		return nil, errors.New("no active rongcloud channel installation found")
	}
	nodes, err := s.queries.ListRongCloudNodesByWorkspace(ctx, insts[0].WorkspaceID)
	if err != nil {
		return nil, err
	}
	records := make([]map[string]interface{}, 0, len(nodes))
	for _, n := range nodes {
		aiType := n.AiType.String
		online := "offline"
		if s.IsOnline(n.ID) {
			online = "online"
		}
		record := map[string]interface{}{
			"node_id":         n.NodeID,
			"node_type":       aiType,
			"name":            aiType,
			"rongcloud_id":    n.RongcloudUserID,
			"deploy_status":   online,
			"realtime_status": online,
			"status":          "active",
			"created_at":      n.CreatedAt.Time,
			"updated_at":      n.UpdatedAt.Time,
		}
		if len(n.Capabilities) > 0 {
			record["capabilities"] = json.RawMessage(n.Capabilities)
		}
		if u, err := s.queries.GetRongCloudUserByRongCloudID(ctx, n.RongcloudUserID); err == nil && u.Name.Valid && u.Name.String != "" {
			record["name"] = u.Name.String
			record["nickname"] = u.Name.String
		}
		records = append(records, record)
	}
	return records, nil
}

// GetUserByID 按 rongcloud_user.id（UUID）直查用户行。
func (s *NodeService) GetUserByID(ctx context.Context, id pgtype.UUID) (db.RongcloudUser, error) {
	return s.queries.GetRongCloudUserByID(ctx, id)
}

// GetUserByRongCloudID 按融云用户 ID（rc_user_id 字符串）查用户行。
func (s *NodeService) GetUserByRongCloudID(ctx context.Context, rcUserID string) (db.RongcloudUser, error) {
	return s.queries.GetRongCloudUserByRongCloudID(ctx, rcUserID)
}

// UpdateUserProfile 更新节点关联融云用户的昵称与头像（空串保旧由调用方处理）。
func (s *NodeService) UpdateUserProfile(ctx context.Context, id pgtype.UUID, name, portrait string) error {
	_, err := s.queries.UpdateRongCloudUserProfile(ctx, db.UpdateRongCloudUserProfileParams{
		ID:          id,
		Name:        pgText(name),
		PortraitUri: pgText(portrait),
	})
	return err
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
		WorkspaceID:   node.WorkspaceID,
		ConfigKey:     "connection_session:" + sessionID,
		NodeID:        node.ID,
		Config:        configJSON,
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

// ErrPairingSessionNotClaimed is returned by BindAgents when the session is
// not (yet) claimed — the device must pair before agents can be bound.
var ErrPairingSessionNotClaimed = errors.New("rongcloud: pairing session not claimed")

// ErrInvalidDeviceCredential is returned by MachineAgentCredentials when the
// caller cannot prove ownership of the machine node's device credential.
var ErrInvalidDeviceCredential = errors.New("rongcloud: invalid device credential")

// BoundAgentResult describes one agent node created (or reused) by BindAgents.
type BoundAgentResult struct {
	Agent  string `json:"agent"`
	NodeID string `json:"nodeId"`
}

// MachineAgentCredential is one bound agent's connection material, handed to
// the device supervisor so it can spawn the per-agent run loop.
type MachineAgentCredential struct {
	Agent          string `json:"agent"`
	NodeID         string `json:"nodeId"`
	RongCloudUser  string `json:"rongCloudUser"`
	Token          string `json:"token"`
	CredentialID   string `json:"credentialId"`
	DeviceSecret   string `json:"deviceSecret"`
}

// BindAgents materialises the user's check-box selection: every selected
// agent becomes its own rongcloud user + node + device credential on the
// device's machine, idempotent on (machine_id, ai_type).
func (s *NodeService) BindAgents(ctx context.Context, session db.RongcloudPairingSession, agents []string) ([]BoundAgentResult, error) {
	if session.Status == "expired" || session.ExpiresAt.Time.Before(time.Now()) {
		return nil, ErrPairingSessionExpired
	}
	if session.Status != "claimed" {
		return nil, ErrPairingSessionNotClaimed
	}
	var candidateNodeIDs []pgtype.UUID
	if err := json.Unmarshal(session.CandidateNodeIds, &candidateNodeIDs); err != nil || len(candidateNodeIDs) == 0 {
		return nil, fmt.Errorf("rongcloud: no candidate nodes in session")
	}
	machineNode, err := s.queries.GetRongCloudNodeByID(ctx, candidateNodeIDs[0])
	if err != nil {
		return nil, fmt.Errorf("rongcloud: get machine node: %w", err)
	}
	results, err := s.bindAgentsToMachine(ctx, machineNode, agents)
	if err != nil {
		return nil, err
	}
	if boundJSON, err := json.Marshal(results); err == nil {
		_, _ = s.queries.UpdateRongCloudPairingSessionBoundAgents(ctx, db.UpdateRongCloudPairingSessionBoundAgentsParams{
			Ticket:      session.Ticket,
			BoundAgents: boundJSON,
		})
	}
	return results, nil
}

// BindAgentsByDeviceCredential lets a paired device bind additional agents on
// its own machine without going through the pairing dialog. Possession of a
// valid machine device credential proves the device already claimed its
// session, so the pairing session is neither looked up nor re-validated (it
// may legitimately be expired by the time the supervisor runs). The
// bound_agents column is a pairing-UI artifact and is intentionally not
// updated here.
func (s *NodeService) BindAgentsByDeviceCredential(ctx context.Context, machineNodeIDText, credentialID, secret string, agents []string) ([]BoundAgentResult, error) {
	if s.queries == nil {
		return nil, errors.New("rongcloud: database not configured")
	}
	machineNode, err := s.queries.GetRongCloudNodeByNodeID(ctx, machineNodeIDText)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: get machine node: %w", err)
	}
	machineDevice, err := s.queries.GetRongCloudDeviceByNodeAndCredential(ctx, db.GetRongCloudDeviceByNodeAndCredentialParams{
		NodeID:       machineNode.ID,
		CredentialID: pgtype.Text{String: credentialID, Valid: credentialID != ""},
	})
	if err != nil {
		return nil, ErrInvalidDeviceCredential
	}
	if !machineDevice.CredentialSecretEncrypted.Valid || s.box == nil {
		return nil, ErrInvalidDeviceCredential
	}
	sealed, err := base64.StdEncoding.DecodeString(machineDevice.CredentialSecretEncrypted.String)
	if err != nil {
		return nil, ErrInvalidDeviceCredential
	}
	plain, err := s.box.Open(sealed)
	if err != nil || !hmac.Equal([]byte(plain), []byte(secret)) {
		return nil, ErrInvalidDeviceCredential
	}
	return s.bindAgentsToMachine(ctx, machineNode, agents)
}

// bindAgentsToMachine resolves the machine identity and materialises one
// node per agent, idempotent on (machine_id, ai_type).
func (s *NodeService) bindAgentsToMachine(ctx context.Context, machineNode db.RongcloudNode, agents []string) ([]BoundAgentResult, error) {
	if s.queries == nil {
		return nil, errors.New("rongcloud: database not configured")
	}
	if s.client == nil {
		return nil, errors.New("rongcloud: API client not configured")
	}
	machineID := machineNode.MachineID
	if machineID == "" {
		// Nodes registered before the machine_id column existed key their
		// identity off the rongcloud user id instead.
		machineID = strings.TrimPrefix(machineNode.RongcloudUserID, "rc_node_")
	}
	if machineID == "" {
		return nil, fmt.Errorf("rongcloud: machine node has no machine identity")
	}

	seen := map[string]bool{}
	results := make([]BoundAgentResult, 0, len(agents))
	for _, agent := range agents {
		agent = strings.TrimSpace(agent)
		if agent == "" || seen[agent] {
			continue
		}
		seen[agent] = true
		nodeIDText, err := s.ensureAgentNode(ctx, machineNode, machineID, agent)
		if err != nil {
			return nil, fmt.Errorf("rongcloud: bind agent %q: %w", agent, err)
		}
		results = append(results, BoundAgentResult{Agent: agent, NodeID: nodeIDText})
	}
	return results, nil
}

// ensureAgentNode creates (or reuses) the rongcloud user + node + device
// credential for one agent on the given machine.
func (s *NodeService) ensureAgentNode(ctx context.Context, machineNode db.RongcloudNode, machineID, agent string) (string, error) {
	rcUserID := fmt.Sprintf("rc_node_%s_%s", machineIdentityKey(machineID), agent)
	name := fmt.Sprintf("%s (%s)", machineNode.NodeID, agent)
	token, err := s.client.getUserToken(ctx, rcUserID, name, "")
	if err != nil {
		return "", fmt.Errorf("getUserToken: %w", err)
	}
	encToken := ""
	if s.box != nil {
		sealed, err := s.box.Seal([]byte(token))
		if err != nil {
			return "", fmt.Errorf("encrypt token: %w", err)
		}
		encToken = base64.StdEncoding.EncodeToString(sealed)
	}
	rcUser, err := s.queries.GetRongCloudUserByRongCloudID(ctx, rcUserID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("lookup user: %w", err)
	}
	if err == nil {
		_, _ = s.queries.UpdateRongCloudUserToken(ctx, db.UpdateRongCloudUserTokenParams{
			ID:             rcUser.ID,
			TokenEncrypted: pgText(encToken),
		})
	} else {
		rcUser, err = s.queries.CreateRongCloudUser(ctx, db.CreateRongCloudUserParams{
			WorkspaceID:     machineNode.WorkspaceID,
			RongcloudUserID: rcUserID,
			Name:            pgText(name),
			TokenEncrypted:  pgText(encToken),
			IsAiNode:        true,
			NodeType:        "ai",
		})
		if err != nil {
			return "", fmt.Errorf("create user: %w", err)
		}
	}
	nodeIDText := fmt.Sprintf("node_%s", hex.EncodeToString(rcUser.ID.Bytes[:8]))
	node, err := s.queries.GetRongCloudNodeByRongCloudUserID(ctx, rcUserID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("lookup node: %w", err)
	}
	if err != nil {
		node, err = s.queries.CreateRongCloudNode(ctx, db.CreateRongCloudNodeParams{
			WorkspaceID:     machineNode.WorkspaceID,
			OwnerUserID:     machineNode.OwnerUserID,
			RongcloudUserID: rcUserID,
			NodeID:          nodeIDText,
			AiType:          pgText(agent),
			Capabilities:    []byte("[]"),
			DeployStatus:    "offline",
			BindingVersion:  1,
			MachineID:       machineID,
		})
		if err != nil {
			return "", fmt.Errorf("create node: %w", err)
		}
	}
	// Device credential: only mint one when none exists yet. The plaintext
	// secret cannot be recovered later, so re-binding must not clobber it.
	if _, err := s.queries.GetRongCloudDeviceByNodeID(ctx, node.ID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("lookup device: %w", err)
		}
		credID, credSecret, err := generateDeviceCredential()
		if err != nil {
			return "", fmt.Errorf("generate device credential: %w", err)
		}
		encSecret := ""
		if s.box != nil {
			sealed, err := s.box.Seal([]byte(credSecret))
			if err == nil {
				encSecret = base64.StdEncoding.EncodeToString(sealed)
			}
		}
		_, err = s.queries.CreateRongCloudDevice(ctx, db.CreateRongCloudDeviceParams{
			WorkspaceID:               machineNode.WorkspaceID,
			OwnerUserID:               machineNode.OwnerUserID,
			NodeID:                    node.ID,
			DeviceName:                name,
			DeviceType:                pgText(agent),
			CredentialID:              pgText(credID),
			CredentialSecretEncrypted: pgText(encSecret),
			Status:                    "active",
		})
		if err != nil {
			return "", fmt.Errorf("create device: %w", err)
		}
	}
	return nodeIDText, nil
}

// MachineAgentCredentials authenticates a device supervisor (machine node
// credential) and returns the connection material of every bound agent node
// on that machine.
func (s *NodeService) MachineAgentCredentials(ctx context.Context, machineNodeIDText, credentialID, secret string) ([]MachineAgentCredential, error) {
	if s.queries == nil {
		return nil, errors.New("rongcloud: database not configured")
	}
	machineNode, err := s.queries.GetRongCloudNodeByNodeID(ctx, machineNodeIDText)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: get machine node: %w", err)
	}
	machineDevice, err := s.queries.GetRongCloudDeviceByNodeAndCredential(ctx, db.GetRongCloudDeviceByNodeAndCredentialParams{
		NodeID:       machineNode.ID,
		CredentialID: pgtype.Text{String: credentialID, Valid: credentialID != ""},
	})
	if err != nil {
		return nil, ErrInvalidDeviceCredential
	}
	if !machineDevice.CredentialSecretEncrypted.Valid || s.box == nil {
		return nil, ErrInvalidDeviceCredential
	}
	sealed, err := base64.StdEncoding.DecodeString(machineDevice.CredentialSecretEncrypted.String)
	if err != nil {
		return nil, ErrInvalidDeviceCredential
	}
	plain, err := s.box.Open(sealed)
	if err != nil || !hmac.Equal([]byte(plain), []byte(secret)) {
		return nil, ErrInvalidDeviceCredential
	}
	machineID := machineNode.MachineID
	if machineID == "" {
		machineID = strings.TrimPrefix(machineNode.RongcloudUserID, "rc_node_")
	}
	nodes, err := s.queries.ListRongCloudNodesByMachineID(ctx, machineID)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: list machine nodes: %w", err)
	}
	creds := make([]MachineAgentCredential, 0, len(nodes))
	for _, n := range nodes {
		if n.ID == machineNode.ID {
			continue // the machine node itself is not an agent target
		}
		if !n.AiType.Valid || n.AiType.String == "" {
			continue
		}
		rcUser, err := s.queries.GetRongCloudUserByRongCloudID(ctx, n.RongcloudUserID)
		if err != nil || !rcUser.TokenEncrypted.Valid || s.box == nil {
			continue
		}
		tokenSealed, err := base64.StdEncoding.DecodeString(rcUser.TokenEncrypted.String)
		if err != nil {
			continue
		}
		token, err := s.box.Open(tokenSealed)
		if err != nil {
			continue
		}
		device, err := s.queries.GetRongCloudDeviceByNodeID(ctx, n.ID)
		if err != nil || !device.CredentialID.Valid || !device.CredentialSecretEncrypted.Valid {
			continue
		}
		secretSealed, err := base64.StdEncoding.DecodeString(device.CredentialSecretEncrypted.String)
		if err != nil {
			continue
		}
		deviceSecret, err := s.box.Open(secretSealed)
		if err != nil {
			continue
		}
		creds = append(creds, MachineAgentCredential{
			Agent:         n.AiType.String,
			NodeID:        n.NodeID,
			RongCloudUser: n.RongcloudUserID,
			Token:         string(token),
			CredentialID:  device.CredentialID.String,
			DeviceSecret:  string(deviceSecret),
		})
	}
	return creds, nil
}
