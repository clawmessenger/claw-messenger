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
	"os"
	"regexp"
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
	// claimMu guards the memoised friend-edge inversion behind NodeClaimants.
	claimMu      sync.Mutex
	claimCache   map[string]string
	claimCacheAt time.Time
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

// OmStatusForNode 返回给定节点所在机器的运维助手（ops 节点）的融云 id 与在线状态。
//
// 背景：ops 节点在设备列表里是隐藏的，只以 om_rongcloud_id 的形式挂在同机
// agent 节点上；而 ops agent 进程和其他 agent 一样按自己的 nodeId 心跳，所以
// 它的在线状态可以直接从心跳表读。没有 MachineID（未绑定机器）或机器上没有
// ops 节点时 found=false，调用方据此省略字段，让前端回退到节点自身状态。
//
// online 的语义是「运维端能不能干活」，不是「ops 进程最近 90s 有没有打卡」：
// ops 子进程由宿主 supervisor 托管，宿主有心跳就一定会被拉起来，所以 ops
// 心跳空窗时回退宿主心跳（见下方 hostAlive）。
func (s *NodeService) OmStatusForNode(ctx context.Context, node db.RongcloudNode) (omRongcloudID string, online bool, found bool, err error) {
	if node.MachineID == "" {
		return "", false, false, nil
	}
	nodes, err := s.queries.ListRongCloudNodesByWorkspace(ctx, node.WorkspaceID)
	if err != nil {
		return "", false, false, fmt.Errorf("list nodes: %w", err)
	}
	// 优先该 agent 的专属运维节点（新 CLI 起的 per-agent ops 子进程按自己的
	// nodeId 心跳）；机器上只有机器级 ops（旧 CLI）时回退到它。机器级 ops
	// 的旧命名（"rc_node_<key>_ops"）同样以 "_ops" 结尾，必须用同机 agent
	// 集合判别，不能只看后缀。
	agentRCs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if n.MachineID == node.MachineID && !isHiddenNode(n) {
			agentRCs[n.RongcloudUserID] = true
		}
	}
	perAgentRC := perAgentOpsRCUserID(node.RongcloudUserID)
	// 宿主节点有心跳 = 这台机器的 supervisor 还活着。ops 子进程是 supervisor
	// 拉起的、和它服务的 agent 同生共死，所以「宿主在线」已经足够说明运维端
	// 在跑 —— ops 自己的心跳只是 30s 一拍，服务端重启后心跳表是空的，头 90s
	// 读到的一定是离线。运维是随设备常驻的能力，不能因为一拍空窗就报离线，
	// 否则用户每次重启服务都会看到「运维助手离线」。
	hostAlive := s.IsOnline(node.ID)
	var machineLevel *db.RongcloudNode
	for _, n := range nodes {
		if n.MachineID != node.MachineID || n.AiType.String != OpsAgentType {
			continue
		}
		if n.RongcloudUserID == perAgentRC {
			return n.RongcloudUserID, s.IsOnline(n.ID) || hostAlive, true, nil
		}
		if _, perAgent := classifyOpsRCUser(n.RongcloudUserID, agentRCs); !perAgent {
			copied := n
			machineLevel = &copied
		}
	}
	if machineLevel != nil {
		return machineLevel.RongcloudUserID, s.IsOnline(machineLevel.ID) || hostAlive, true, nil
	}
	return "", false, false, nil
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

// machineIdentityKey derives the stable key embedded in every rongcloud user id
// a machine owns: "<agent>_<key>" for its agent nodes and "rc_node_<key>" for
// the machine node itself.
//
// Rongcloud caps user ids at 64 chars, and CLI machine ids are numeric (10
// digits), so the new shape never overflows. Older rows carry
// "clawmessenger-<uuid>" (51 chars) and legacy MAC addresses, and anything too
// long is shortened to a stable sha256 prefix.
func machineIdentityKey(machineID string) string {
	// Frozen at the value the original "rc_node_<machine>_<agent>" scheme used:
	// already-registered machines were hashed against it, so widening the budget
	// would derive a second key for the same machine and duplicate every node
	// under it. The new "<agent>_<key>" shape only needs
	// 64 - len("antigravity") - 1 = 52.
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

// OpsAgentType is the ai_type of the built-in maintenance agent that every
// clawmessenger device binds (see packages/xiachat-cli src/ops.ts). Its node
// exists only to power the ops (运维) feature, so it is hidden from the
// user-facing device list and friend list.
const OpsAgentType = "ops"

// isMachineNode reports whether n is the per-machine infrastructure node that
// Register creates for a device, as opposed to a bound agent node that
// ensureAgentNode creates as "<agent>_<machine key>". The machine node's
// rongcloud user id is the raw machine key behind the "rc_node_" prefix and
// carries no agent suffix, which is what distinguishes it from its children.
func isMachineNode(n db.RongcloudNode) bool {
	if n.MachineID == "" {
		return false
	}
	return n.RongcloudUserID == "rc_node_"+machineIdentityKey(n.MachineID)
}

// isHiddenNode reports whether n must be excluded from the user-facing device
// list and friend list. Two kinds of nodes are internal plumbing: the
// per-machine infrastructure node and the built-in ops maintenance agent.
func isHiddenNode(n db.RongcloudNode) bool {
	return n.AiType.String == OpsAgentType || isMachineNode(n)
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
	// The device reports few (usually zero) capabilities; the discussion set the
	// runtime will actually ask this node for is derived from its ai_type. See
	// node_capabilities.go.
	capabilitiesJSON := nodeCapabilitiesJSON(params.Capabilities, params.AIType)
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
	} else if refreshed := nodeCapabilitiesJSONFromStored(node.Capabilities, params.AIType); string(refreshed) != string(node.Capabilities) {
		// Register is idempotent per machine, so a device that registered before
		// the discussion capability set existed keeps its row. Repair it in place
		// on the next pair instead of leaving it without any capabilities.
		if updated, err := s.queries.UpdateRongCloudNodeCapabilities(ctx, db.UpdateRongCloudNodeCapabilitiesParams{
			ID:           node.ID,
			Capabilities: refreshed,
		}); err == nil {
			node = updated
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
		Capabilities:           MergeNodeCapabilities(params.Capabilities, params.AIType),
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

// LegacyNodeRecords returns the nodes the given claw user owns, mapped to the
// legacy NodeRecord shape consumed by the old web client's device list.
// Ownership truth is the RongCloud friend edge (same as NodeClaimants inverts):
// the claw user_id doubles as its rc account id (the login flow issues the rc
// token with user_id as the rc id), so FriendIDs(user_id) lists exactly the
// bound agent nodes. Every returned record carries binding_role=owner — the
// legacy web/mini-program device lists gate discussion participation on
// binding_role === 'owner', and a missing field used to blank the
// eligible-device set ("当前没有可参与讨论的设备"). An empty userID falls back
// to the old workspace-wide listing without filtering for internal callers.
func (s *NodeService) LegacyNodeRecords(ctx context.Context, userID string) ([]map[string]interface{}, error) {
	if s.queries == nil {
		return nil, errors.New("rongcloud: no queries")
	}
	var ownedRC map[string]bool
	if userID != "" {
		if s.client == nil {
			return nil, errors.New("rongcloud: API client not configured")
		}
		friends, err := s.client.FriendIDs(ctx, userID)
		if err != nil {
			return nil, err
		}
		ownedRC = make(map[string]bool, len(friends))
		for _, friend := range friends {
			ownedRC[friend] = true
		}
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
	// The ops (运维) node for each machine powers the maintenance feature but
	// must not surface as its own device row. Index it by machine so the
	// visible agent nodes can advertise the capability via om_rongcloud_id.
	// per-agent ops 节点单独建索引：om_rongcloud_id 始终指向机器级 ops
	// （旧前端/旧 CLI 的兼容入口），ops_rongcloud_id 才是该 agent 专属的
	// 运维会话对端。判定依赖 agent 融云 id 集合（见 classifyOpsRCUser：
	// 旧方案机器级 ops 的 rc 同样以 "_ops" 结尾，单看后缀必误判）。
	agentRCs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if !isHiddenNode(n) {
			agentRCs[n.RongcloudUserID] = true
		}
	}
	opsByMachine := make(map[string]string)
	opsByAgentRC := make(map[string]string)
	// The machine's own infrastructure node is hidden too, but its nickname is
	// the only human-readable label for the box ("小久", "SHGuoZiWei-10.1.1.225").
	// Agent nodes are named after their agent and the machine key the web
	// derives from the RongCloud id is an opaque hash, so without this a machine
	// running three agents shows three indistinguishable rows — the exact
	// "which one is which" the device list is supposed to answer.
	machineNameByMachine := make(map[string]string)
	for _, n := range nodes {
		if n.AiType.String == OpsAgentType && n.MachineID != "" {
			if owner, perAgent := classifyOpsRCUser(n.RongcloudUserID, agentRCs); perAgent {
				opsByAgentRC[owner] = n.RongcloudUserID
			} else {
				opsByMachine[n.MachineID] = n.RongcloudUserID
			}
		}
		if !isMachineNode(n) {
			continue
		}
		u, err := s.queries.GetRongCloudUserByRongCloudID(ctx, n.RongcloudUserID)
		if err != nil || !u.Name.Valid {
			continue
		}
		if label := machineDisplayName(u.Name.String, n.AiType.String); label != "" {
			machineNameByMachine[n.MachineID] = label
		}
	}
	records := make([]map[string]interface{}, 0, len(nodes))
	for _, n := range nodes {
		// Only bound agent nodes are user-facing; the per-machine
		// infrastructure node and the ops maintenance agent stay internal.
		if isHiddenNode(n) {
			continue
		}
		// When the caller is a claw user, only their friend-edge-owned nodes
		// surface — the HTTP list is per-user, not workspace-wide. Internal
		// callers (userID == "") keep the workspace-wide view.
		if ownedRC != nil && !ownedRC[n.RongcloudUserID] {
			continue
		}
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
			"binding_role":    "owner",
			"deploy_status":   online,
			"realtime_status": online,
			"status":          "active",
			"created_at":      n.CreatedAt.Time,
			"updated_at":      n.UpdatedAt.Time,
		}
		// Expose the machine's ops node to the client so the device detail
		// can still offer the 运维 entry point without listing the ops node.
		if opsRC, ok := opsByMachine[n.MachineID]; ok && opsRC != "" {
			record["om_rongcloud_id"] = opsRC
			record["has_om_capability"] = true
		}
		// per-agent 运维节点存在时下发专属对端：新前端优先用它开运维会话，
		// 每个智能体的运维聊天记录在融云侧就是两条独立私聊。
		if opsRC, ok := opsByAgentRC[n.RongcloudUserID]; ok && opsRC != "" {
			record["ops_rongcloud_id"] = opsRC
			record["has_om_capability"] = true
		}
		// The box this agent runs on. Sibling agent nodes of one machine all
		// carry the same value, which is what lets the list tell "three agents
		// on 小久" apart from "three different machines".
		if label, ok := machineNameByMachine[n.MachineID]; ok && label != "" {
			record["machine_name"] = label
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

// UpdateUserProfile 更新节点关联融云用户的昵称与头像（空串保旧由调用方处理），
// 并同步推送融云侧用户信息（/user/refresh.json）。融云好友列表渲染的是
// 融云侧的昵称，而 getToken 不会更新已存在用户的信息——若只写本地库，
// 好友列表会一直显示旧昵称（或裸的用户 id）。
func (s *NodeService) UpdateUserProfile(ctx context.Context, id pgtype.UUID, name, portrait string) error {
	user, err := s.queries.UpdateRongCloudUserProfile(ctx, db.UpdateRongCloudUserProfileParams{
		ID:          id,
		Name:        pgText(name),
		PortraitUri: pgText(portrait),
	})
	if err != nil {
		return err
	}
	if s.client != nil && user.RongcloudUserID != "" {
		if _, err := s.client.refreshUser(ctx, user.RongcloudUserID, name, portrait); err != nil {
			return fmt.Errorf("rongcloud: refresh user info: %w", err)
		}
	}
	return nil
}

// ListUserFriends returns a claw/IM user's RongCloud friend list mapped to the
// legacy UserInfo shape the web client's contact list expects. This backs the
// HTTP /api/user/friends fallback the frontend calls alongside the IM SDK.
func (s *NodeService) ListUserFriends(ctx context.Context, rongCloudUserID string) ([]map[string]interface{}, error) {
	if s.client == nil {
		return nil, errors.New("rongcloud: API client not configured")
	}
	friends, err := s.client.getFriends(ctx, rongCloudUserID)
	if err != nil {
		return nil, err
	}
	list := make([]map[string]interface{}, 0, len(friends))
	for _, f := range friends {
		nickname := f.Name
		if nickname == "" {
			nickname = f.UserID
		}
		list = append(list, map[string]interface{}{
			"userId":      f.UserID,
			"username":    f.UserID,
			"nickname":    nickname,
			"portraitUri": f.PortraitURI,
		})
	}
	return list, nil
}

// BackfillFriendsOptions tunes BackfillAgentFriends.
type BackfillFriendsOptions struct {
	// IncludeOps also friends the internal ops (运维) node. Off by default:
	// the ops node is hidden from the user-facing friend list by design and
	// stays reachable through the device-detail maintenance entry instead.
	IncludeOps bool
	// Bidirectional also writes the reverse edge (agent -> owner). RongCloud
	// friend relations are one-way, so this mirrors the legacy device-claim
	// flow, which added both directions.
	Bidirectional bool
	// DryRun reports what would happen without calling RongCloud.
	DryRun bool
}

// BackfillFriendOutcome records one owner->agent friend attempt.
type BackfillFriendOutcome struct {
	Owner  string `json:"owner"`
	Agent  string `json:"agent"`
	NodeRC string `json:"node_rc"`
	// Status is one of: planned (dry run), added, already, failed.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// BackfillAgentFriends ensures every visible agent node of workspaceID is a
// friend of each owner rongcloud user id. It repairs bindings created before
// BindAgents wrote the friend edge: the web client's contact list reads the
// RongCloud friend list, so those devices never appeared there. Writes are
// idempotent — RongCloud answers 25460 for an existing edge, reported as
// "already". Machine infrastructure nodes and (unless IncludeOps) the ops node
// are skipped, matching the device-list visibility rules.
func (s *NodeService) BackfillAgentFriends(ctx context.Context, workspaceID pgtype.UUID, owners []string, opts BackfillFriendsOptions) ([]BackfillFriendOutcome, error) {
	if s.queries == nil || s.client == nil {
		return nil, errors.New("rongcloud: service not configured")
	}
	nodes, err := s.queries.ListRongCloudNodesByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: list nodes: %w", err)
	}
	agents := make([]db.RongcloudNode, 0, len(nodes))
	for _, n := range nodes {
		if isMachineNode(n) {
			continue
		}
		if n.AiType.String == OpsAgentType && !opts.IncludeOps {
			continue
		}
		agents = append(agents, n)
	}
	outcomes := make([]BackfillFriendOutcome, 0, len(owners)*len(agents))
	for _, owner := range owners {
		owner = strings.TrimSpace(owner)
		if owner == "" {
			continue
		}
		for _, agent := range agents {
			outcome := BackfillFriendOutcome{Owner: owner, Agent: agent.AiType.String, NodeRC: agent.RongcloudUserID}
			if opts.DryRun {
				outcome.Status = "planned"
				outcomes = append(outcomes, outcome)
				continue
			}
			already, err := s.client.addFriend(ctx, owner, agent.RongcloudUserID, "")
			if err != nil {
				outcome.Status = "failed"
				outcome.Error = err.Error()
				outcomes = append(outcomes, outcome)
				continue
			}
			if already {
				outcome.Status = "already"
			} else {
				outcome.Status = "added"
			}
			if opts.Bidirectional {
				if _, rerr := s.client.addFriend(ctx, agent.RongcloudUserID, owner, ""); rerr != nil {
					s.logger.Warn("rongcloud: backfill reverse friend failed",
						"owner", owner, "agent", agent.RongcloudUserID, "error", rerr)
				}
			}
			outcomes = append(outcomes, outcome)
		}
	}
	return outcomes, nil
}

// NicknameSyncOutcome records one /user/refresh.json nickname-sync attempt.
type NicknameSyncOutcome struct {
	NodeRC string `json:"node_rc"`
	Name   string `json:"name"`
	// Status is one of: planned (dry run), planned-rename (dry run, legacy
	// default name to rewrite), renamed, refreshed, skipped, failed.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// SyncNodeNicknames pushes every node's authoritative database nickname to
// RongCloud (/user/refresh.json). RongCloud renders the friend list and IM
// conversations with the RongCloud-side user info, and getToken only sets it
// at user creation — renames written to the database alone (the behaviour
// before UpdateUserProfile started pushing to RongCloud) left the friend list
// showing the initial name or the raw node id. Every node of the workspace is
// synced, including the hidden machine/ops nodes: they never appear in the
// friend list, but their IM conversations still render the RongCloud name.
// Idempotent; nothing is written when opts.DryRun is set.
func (s *NodeService) SyncNodeNicknames(ctx context.Context, workspaceID pgtype.UUID, opts BackfillFriendsOptions) ([]NicknameSyncOutcome, error) {
	if s.queries == nil || s.client == nil {
		return nil, errors.New("rongcloud: service not configured")
	}
	nodes, err := s.queries.ListRongCloudNodesByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: list nodes: %w", err)
	}
	outcomes := make([]NicknameSyncOutcome, 0, len(nodes))
	for _, n := range nodes {
		u, err := s.queries.GetRongCloudUserByRongCloudID(ctx, n.RongcloudUserID)
		if err != nil {
			outcomes = append(outcomes, NicknameSyncOutcome{
				NodeRC: n.RongcloudUserID, Status: "failed",
				Error: fmt.Sprintf("lookup user: %v", err),
			})
			continue
		}
		name := u.Name.String
		if name == "" {
			outcomes = append(outcomes, NicknameSyncOutcome{
				NodeRC: n.RongcloudUserID, Status: "skipped",
				Error: "no nickname stored in database",
			})
			continue
		}
		// Names minted by the old default embedded the internal machine node id
		// ("node_587ae8bb0ef54b26 (hermes)"), which is what the contact list
		// showed for every node the owner had never renamed. Rewriting the row
		// before pushing keeps the database and RongCloud in step — the device
		// list reads the stored name, not the RongCloud one.
		if repaired := repairLegacyAgentNodeName(name); repaired != name {
			if opts.DryRun {
				outcomes = append(outcomes, NicknameSyncOutcome{
					NodeRC: n.RongcloudUserID, Name: repaired, Status: "planned-rename",
				})
				continue
			}
			if err := s.UpdateUserProfile(ctx, u.ID, repaired, u.PortraitUri.String); err != nil {
				outcomes = append(outcomes, NicknameSyncOutcome{
					NodeRC: n.RongcloudUserID, Name: repaired, Status: "failed", Error: err.Error(),
				})
				continue
			}
			outcomes = append(outcomes, NicknameSyncOutcome{
				NodeRC: n.RongcloudUserID, Name: repaired, Status: "renamed",
			})
			continue
		}
		if opts.DryRun {
			outcomes = append(outcomes, NicknameSyncOutcome{
				NodeRC: n.RongcloudUserID, Name: name, Status: "planned",
			})
			continue
		}
		if _, err := s.client.refreshUser(ctx, n.RongcloudUserID, name, u.PortraitUri.String); err != nil {
			outcomes = append(outcomes, NicknameSyncOutcome{
				NodeRC: n.RongcloudUserID, Name: name, Status: "failed", Error: err.Error(),
			})
			continue
		}
		outcomes = append(outcomes, NicknameSyncOutcome{
			NodeRC: n.RongcloudUserID, Name: name, Status: "refreshed",
		})
	}
	return outcomes, nil
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

// UnbindNode removes one bound node the way the Python server's claw.unbind did:
// drop the node row (and its model catalogue), and take the node out of the
// owner's RongCloud contact list so it also disappears from their friend list.
//
// The friend removal is best-effort — a missing edge, or a RongCloud hiccup,
// must not fail the unbind the user actually asked for. The RongCloud account
// itself is kept on purpose: "the RongCloud id is the account's primary key",
// so a later re-bind reuses it instead of minting a duplicate.
func (s *NodeService) UnbindNode(ctx context.Context, node db.RongcloudNode, ownerRongCloudID string) error {
	if s.queries == nil {
		return errors.New("rongcloud: database not configured")
	}
	if s.client != nil && ownerRongCloudID != "" && node.RongcloudUserID != "" {
		if err := s.client.RemoveFriends(ctx, ownerRongCloudID, node.RongcloudUserID); err != nil {
			s.logger.Warn("rongcloud: remove friend on unbind failed",
				"owner", ownerRongCloudID, "node_rc", node.RongcloudUserID, "error", err)
		}
	}
	catalogs, err := s.queries.ListRongCloudNodeModelCatalogsByNode(ctx, node.ID)
	if err != nil {
		return fmt.Errorf("list node model catalogue: %w", err)
	}
	for _, catalog := range catalogs {
		if err := s.queries.DeleteRongCloudNodeModelCatalog(ctx, catalog.ID); err != nil {
			return fmt.Errorf("delete node model catalogue: %w", err)
		}
	}
	return s.queries.DeleteRongCloudNode(ctx, node.ID)
}

// UnregisterNode 注销并解绑：在 UnbindNode 的本地清理之上，把 agent 的融云
// 账号（以及它的 per-agent 运维账号）在融云侧永久注销 —— 资料、消息历史、
// 会话与推送绑定全部清空，user id 只能以空身份重新激活，不可恢复，所以必须
// 由调用方显式 opt-in（HTTP 层 unregister=true）。
//
// 顺序是先注销融云账号、后删本地行：注销失败时本地什么都没动，用户可以
// 直接重试；反过来先删本地行再失败，重试入口（节点行）就没了。机器级共享
// 的 ops 账号不动 —— 同机其他智能体还在用它。
func (s *NodeService) UnregisterNode(ctx context.Context, node db.RongcloudNode, ownerRongCloudID string) error {
	if s.queries == nil {
		return errors.New("rongcloud: database not configured")
	}
	// 先找到该 agent 的 per-agent 运维节点（存在与否取决于设备端 CLI 版本），
	// 它的本地行与融云账号随 agent 一起注销。
	var opsNode *db.RongcloudNode
	if node.MachineID != "" {
		nodes, err := s.queries.ListRongCloudNodesByMachineID(ctx, node.MachineID)
		if err != nil {
			return fmt.Errorf("list machine nodes: %w", err)
		}
		want := perAgentOpsRCUserID(node.RongcloudUserID)
		for _, n := range nodes {
			if n.AiType.String == OpsAgentType && n.RongcloudUserID == want {
				copied := n
				opsNode = &copied
				break
			}
		}
	}
	if s.client != nil {
		ids := []string{node.RongcloudUserID}
		if opsNode != nil {
			ids = append(ids, opsNode.RongcloudUserID)
		}
		if err := s.client.DeactivateUser(ctx, ids...); err != nil {
			return fmt.Errorf("rongcloud: deactivate user: %w", err)
		}
	}
	if opsNode != nil {
		if err := s.UnbindNode(ctx, *opsNode, ownerRongCloudID); err != nil {
			return fmt.Errorf("unbind per-agent ops node: %w", err)
		}
	}
	return s.UnbindNode(ctx, node, ownerRongCloudID)
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
	Agent         string `json:"agent"`
	NodeID        string `json:"nodeId"`
	RongCloudUser string `json:"rongCloudUser"`
	Token         string `json:"token"`
	CredentialID  string `json:"credentialId"`
	DeviceSecret  string `json:"deviceSecret"`
	// OpsFor 仅出现在 per-agent 运维节点行上：该 ops 节点专属服务于哪个
	// agent 的融云 id（"<agentRC>"，本行 rc 为 "<agentRC>_ops"）。supervisor
	// 据此为每个 agent 各起一个 ops 子进程；空串 = 机器级 ops（全机共享）。
	OpsFor string `json:"opsFor,omitempty"`
}

// BindAgents materialises the user's check-box selection: every selected
// agent becomes its own rongcloud user + node + device credential on the
// device's machine, idempotent on (machine_id, ai_type). ownerRongCloudID is
// the bound human user's rongcloud id; when set, each newly bound agent (except
// the internal ops agent) is added to that user's IM friend list.
func (s *NodeService) BindAgents(ctx context.Context, session db.RongcloudPairingSession, agents []string, ownerRongCloudID string) ([]BoundAgentResult, error) {
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
	results, err := s.bindAgentsToMachine(ctx, machineNode, agents, ownerRongCloudID)
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
	// Device-credential binds have no owning human user in scope, so no friend
	// relation is created here (the ops agent is bound this way anyway).
	return s.bindAgentsToMachine(ctx, machineNode, agents, "")
}

// bindAgentsToMachine resolves the machine identity and materialises one
// node per agent, idempotent on (machine_id, ai_type). When ownerRongCloudID
// is set, each bound agent (except the internal ops agent) is also added to
// that user's IM friend list so it shows up in the contact list.
func (s *NodeService) bindAgentsToMachine(ctx context.Context, machineNode db.RongcloudNode, agents []string, ownerRongCloudID string) ([]BoundAgentResult, error) {
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
		binding, err := s.ensureAgentNode(ctx, machineNode, machineID, agent)
		if err != nil {
			return nil, fmt.Errorf("rongcloud: bind agent %q: %w", agent, err)
		}
		// Surface the bound agent in the owner's friend list. The built-in ops
		// agent is internal plumbing, so it is never added. Best-effort: a
		// failed friend write must not fail the bind itself.
		if ownerRongCloudID != "" && agent != OpsAgentType && binding.RongcloudUserID != "" {
			if _, err := s.client.addFriend(ctx, ownerRongCloudID, binding.RongcloudUserID, ""); err != nil {
				s.logger.Warn("rongcloud: add friend for bound agent failed",
					"owner", ownerRongCloudID, "agent", agent, "error", err)
			}
		}
		results = append(results, BoundAgentResult{Agent: agent, NodeID: binding.NodeID})
	}
	return results, nil
}

// agentNodeRCUserID is the RongCloud user id for one agent bound to a machine.
//
// Format: "<agent>_<machine key>", e.g. hermes_1234567890. It follows the node
// id convention the Python server has always used ("<node_type>_<numeric id>",
// see clawmessenger-server/id_generator.py), which is also what the web parses
// (clawmessenger-web/src/lib/ai-node-id.ts) and what users read in the device
// list. The machine node keeps the older "rc_node_<machine key>" form.
func agentNodeRCUserID(agent, machineID string) string {
	return agent + "_" + machineIdentityKey(machineID)
}

// DefaultAgentNodeName is the display name an agent node carries until its
// owner renames it: the agent type itself ("hermes"). Everything user-facing —
// the contact list, IM session titles, the web device list — renders this
// RongCloud-side name, so it must never embed an internal identifier.
func DefaultAgentNodeName(agent string) string {
	return strings.TrimSpace(agent)
}

// DefaultAgentNodePortrait 返回 AI 节点的默认头像 URL。两端前端（web
// ChatAvatar.tsx / uniapp conversation-list.vue）早就在用这张 openclaw 图作
// AI 兜底；把它同时写进融云侧资料，好友列表/会话头部就能直接渲染，不再依赖
// 各端各自的本地兜底。可用 CLAW_AI_NODE_PORTRAIT_URI 覆盖。
func DefaultAgentNodePortrait(agent string) string {
	if v := strings.TrimSpace(os.Getenv("CLAW_AI_NODE_PORTRAIT_URI")); v != "" {
		return v
	}
	return "https://newsradar.dreamdt.cn/newschat/static/imgs/rongyun/openclaw.png"
}

// machineDisplayName reduces the name a device registered under to the host
// part alone.
//
// The CLI registers a machine as "<os.hostname()> <aiType>" (see
// packages/xiachat-cli/src/main.ts), so the infrastructure node's stored
// nickname carries whichever agent happened to run `pair`. In the device list
// that suffix is noise: the row already says which agent it is, and the point
// of exposing the machine name at all is that three agents on one box render
// as three identical rows.
//
// The suffix is matched against the node's own ai_type rather than a hard-coded
// agent list, so an agent we do not ship still works and a hostname that merely
// ends in an agent-like word is not mangled.
func machineDisplayName(name, aiType string) string {
	name = strings.TrimSpace(name)
	aiType = strings.TrimSpace(aiType)
	if name == "" || aiType == "" {
		return name
	}
	suffix := " " + aiType
	if !strings.HasSuffix(name, suffix) {
		return name
	}
	host := strings.TrimSpace(strings.TrimSuffix(name, suffix))
	if host == "" {
		return name
	}
	return host
}

// legacyDefaultAgentNodeName matches the display name ensureAgentNode used to
// mint: "<machine node id> (<agent>)", e.g. "node_587ae8bb0ef54b26 (hermes)".
// The leading node id is internal plumbing, so the web contact list rendered
// the node id instead of a name for every node the owner had not renamed.
var legacyDefaultAgentNodeName = regexp.MustCompile(`^node_[0-9a-f]+ \(([A-Za-z0-9_-]+)\)$`)

// repairLegacyAgentNodeName rewrites one of those legacy defaults into the
// current default, and returns any other name (i.e. an owner's rename)
// untouched.
func repairLegacyAgentNodeName(name string) string {
	match := legacyDefaultAgentNodeName.FindStringSubmatch(name)
	if match == nil {
		return name
	}
	return DefaultAgentNodeName(match[1])
}

// agentNodeBinding is what ensureAgentNode resolves for one agent: the stable
// node id the web stores, plus the RongCloud user id that owns the agent's IM
// account (needed to add the node to an owner's friend list).
type agentNodeBinding struct {
	NodeID          string
	RongcloudUserID string
}

// findMachineAgentNode returns the node already bound to (machine_id, ai_type).
//
// Matching on the identity columns rather than on the derived RongCloud user id
// is deliberate: nodes registered before agentNodeRCUserID existed carry the old
// "rc_node_<machine>_<agent>" id, and they must be reused rather than duplicated
// under a second RongCloud account.
func (s *NodeService) findMachineAgentNode(ctx context.Context, workspaceID pgtype.UUID, machineID, agent string) (db.RongcloudNode, bool, error) {
	nodes, err := s.queries.ListRongCloudNodesByWorkspace(ctx, workspaceID)
	if err != nil {
		return db.RongcloudNode{}, false, fmt.Errorf("list nodes: %w", err)
	}
	for _, n := range nodes {
		if isAgentNodeOfMachine(n, machineID, agent) {
			return n, true, nil
		}
	}
	return db.RongcloudNode{}, false, nil
}

// isAgentNodeOfMachine reports whether n is the node bound to agent on the
// machine identified by machineID.
//
// The machine's own infrastructure node is excluded even though it satisfies
// the (machine_id, ai_type) match: Register stamps it with the ai_type the
// device registered under (the CLI's --ai-type, "opencode" by default), so
// binding that same agent would otherwise resolve to the machinery node and
// reuse it — and both the device list and the friend list hide that node, so
// the freshly bound agent would be invisible everywhere. MachineAgentCredentials
// skips the machine node for exactly the same reason.
func isAgentNodeOfMachine(n db.RongcloudNode, machineID, agent string) bool {
	if isMachineNode(n) {
		return false
	}
	return n.MachineID == machineID && n.AiType.String == agent
}

// perAgentOpsRCUserID 是某个 agent 专属运维节点的融云 id："<agentRC>_ops"。
//
// 背景：早期 ops 运维节点是全机一个，同机所有智能体的「运维」入口都指向同
// 一个融云账号 —— 私聊会话按（自己, 对方）归档，于是打开智能体 B 的运维会
// 看到智能体 A 的运维聊天记录。per-agent ops 节点让每个智能体的运维会话拥
// 有独立对端，融云侧天然隔离。机器级 ops 节点保留（旧 CLI/旧会话兼容）。
func perAgentOpsRCUserID(agentRCUserID string) string { return agentRCUserID + "_ops" }

// classifyOpsRCUser 判定一个 ops 节点（ai_type=ops）融云账号的归属：
// perAgent=true 时 owner 是它服务的 agent 的融云 id，否则它是机器级共享 ops。
//
// 在野的机器级 ops 有两种命名，per-agent 一种：
//   - "ops_<key>"            新方案机器级（agentNodeRCUserID("ops", machineID)）
//   - "rc_node_<key>_ops"    旧方案机器级（agent=ops 走 "rc_node_<machine>_<agent>" 命名）
//   - "<agentRC>_ops"        per-agent（0.2.39+ CLI）
//
// 后两种都以 "_ops" 结尾，单看后缀必然误判（线上小久的机器级 ops 就是
// "rc_node_3300776275_ops"）：一旦把机器级 ops 错记为 per-agent，opsByMachine
// 落空、om_rongcloud_id 不下发，前端运维按钮直接消失。可靠判据是去掉
// "_ops" 后能否命中同机某个 agent 节点的融云 id；命中不了的一律按机器级
// 处理 —— 机器级是兼容兜底入口，多一个共享入口无害，漏掉则功能消失。
func classifyOpsRCUser(rcUserID string, agentRCs map[string]bool) (owner string, perAgent bool) {
	if !strings.HasSuffix(rcUserID, "_ops") {
		return "", false
	}
	base := strings.TrimSuffix(rcUserID, "_ops")
	if agentRCs[base] {
		return base, true
	}
	return "", false
}

// ensureAgentNode creates (or reuses) the rongcloud user + node + device
// credential for one agent on the given machine.
func (s *NodeService) ensureAgentNode(ctx context.Context, machineNode db.RongcloudNode, machineID, agent string) (agentNodeBinding, error) {
	return s.ensureAgentNodeWithRC(ctx, machineNode, machineID, agent, "")
}

// ensureAgentNodeWithRC 是 ensureAgentNode 的本体；rcOverride 非空时按融云 id
// 精确定位节点（per-agent ops 节点：同 (machine_id, ai_type=ops) 下有多行，
// 不能用 findMachineAgentNode 的首命中语义），其余创建/复用逻辑不变。
func (s *NodeService) ensureAgentNodeWithRC(ctx context.Context, machineNode db.RongcloudNode, machineID, agent, rcOverride string) (agentNodeBinding, error) {
	var existing db.RongcloudNode
	var found bool
	if rcOverride != "" {
		nodes, err := s.queries.ListRongCloudNodesByMachineID(ctx, machineID)
		if err != nil {
			return agentNodeBinding{}, fmt.Errorf("list machine nodes: %w", err)
		}
		for _, n := range nodes {
			if !isMachineNode(n) && n.RongcloudUserID == rcOverride {
				existing, found = n, true
				break
			}
		}
	} else {
		var err error
		existing, found, err = s.findMachineAgentNode(ctx, machineNode.WorkspaceID, machineID, agent)
		if err != nil {
			return agentNodeBinding{}, fmt.Errorf("lookup agent node: %w", err)
		}
	}
	// A node bound before the "<agent>_<machine>" scheme existed keeps its
	// RongCloud id: that id is the account's primary key, so re-deriving it
	// would mint a second account and list the same agent twice.
	rcUserID := agentNodeRCUserID(agent, machineID)
	if rcOverride != "" {
		rcUserID = rcOverride
	}
	if found && existing.RongcloudUserID != "" {
		rcUserID = existing.RongcloudUserID
	}
	// Default display name only — a node the owner renamed keeps its name below,
	// where the stored row wins over this value. 头像同理：默认 openclaw 图，
	// 用户已设自定义头像时下方 stored-row 路径胜出。
	name := DefaultAgentNodeName(agent)
	token, err := s.client.getUserToken(ctx, rcUserID, name, DefaultAgentNodePortrait(agent))
	if err != nil {
		return agentNodeBinding{}, fmt.Errorf("getUserToken: %w", err)
	}
	encToken := ""
	if s.box != nil {
		sealed, err := s.box.Seal([]byte(token))
		if err != nil {
			return agentNodeBinding{}, fmt.Errorf("encrypt token: %w", err)
		}
		encToken = base64.StdEncoding.EncodeToString(sealed)
	}
	rcUser, err := s.queries.GetRongCloudUserByRongCloudID(ctx, rcUserID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return agentNodeBinding{}, fmt.Errorf("lookup user: %w", err)
	}
	if err == nil {
		_, _ = s.queries.UpdateRongCloudUserToken(ctx, db.UpdateRongCloudUserTokenParams{
			ID:             rcUser.ID,
			TokenEncrypted: pgText(encToken),
		})
		// getToken does not update an existing RongCloud user's nickname, so
		// re-sync the authoritative database name on every re-bind. This also
		// repairs devices renamed through the web before the rename flow
		// started pushing to RongCloud. Best-effort: a stale RongCloud-side
		// nickname must not fail the bind itself. 头像取「库内已设值，否则默认
		// openclaw 图」—— 同样走 getToken 不会更新的字段，重绑时补推。
		portrait := rcUser.PortraitUri.String
		if portrait == "" {
			portrait = DefaultAgentNodePortrait(agent)
		}
		if rcUser.Name.Valid && rcUser.Name.String != "" {
			if _, err := s.client.refreshUser(ctx, rcUserID, rcUser.Name.String, portrait); err != nil {
				s.logger.Warn("rongcloud: refresh agent nickname on rebind failed",
					"rc_user", rcUserID, "error", err)
			}
		}
	} else {
		rcUser, err = s.queries.CreateRongCloudUser(ctx, db.CreateRongCloudUserParams{
			WorkspaceID:     machineNode.WorkspaceID,
			RongcloudUserID: rcUserID,
			Name:            pgText(name),
			PortraitUri:     pgText(DefaultAgentNodePortrait(agent)),
			TokenEncrypted:  pgText(encToken),
			IsAiNode:        true,
			NodeType:        "ai",
		})
		if err != nil {
			return agentNodeBinding{}, fmt.Errorf("create user: %w", err)
		}
	}
	node := existing
	nodeIDText := existing.NodeID
	if !found {
		nodeIDText = fmt.Sprintf("node_%s", hex.EncodeToString(rcUser.ID.Bytes[:8]))
		node, err = s.queries.CreateRongCloudNode(ctx, db.CreateRongCloudNodeParams{
			WorkspaceID:     machineNode.WorkspaceID,
			OwnerUserID:     machineNode.OwnerUserID,
			RongcloudUserID: rcUserID,
			NodeID:          nodeIDText,
			AiType:          pgText(agent),
			// A bound agent node is exactly what the discussion runtime
			// dispatches turns to, so it advertises the discussion capability
			// set rather than the empty list this used to hard-code. See
			// node_capabilities.go.
			Capabilities:   nodeCapabilitiesJSON(nil, agent),
			DeployStatus:   "offline",
			BindingVersion: 1,
			MachineID:      machineID,
		})
		if err != nil {
			return agentNodeBinding{}, fmt.Errorf("create node: %w", err)
		}
	} else if nodeIDText == "" {
		nodeIDText = fmt.Sprintf("node_%s", hex.EncodeToString(rcUser.ID.Bytes[:8]))
	}
	if found {
		// Re-binding an agent node is the natural repair point for a row created
		// before the discussion capability set existed.
		if refreshed := nodeCapabilitiesJSONFromStored(node.Capabilities, agent); string(refreshed) != string(node.Capabilities) {
			if updated, err := s.queries.UpdateRongCloudNodeCapabilities(ctx, db.UpdateRongCloudNodeCapabilitiesParams{
				ID:           node.ID,
				Capabilities: refreshed,
			}); err == nil {
				node = updated
			}
		}
	}
	// Device credential: only mint one when none exists yet. The plaintext
	// secret cannot be recovered later, so re-binding must not clobber it.
	if _, err := s.queries.GetRongCloudDeviceByNodeID(ctx, node.ID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return agentNodeBinding{}, fmt.Errorf("lookup device: %w", err)
		}
		credID, credSecret, err := generateDeviceCredential()
		if err != nil {
			return agentNodeBinding{}, fmt.Errorf("generate device credential: %w", err)
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
			return agentNodeBinding{}, fmt.Errorf("create device: %w", err)
		}
	}
	return agentNodeBinding{NodeID: nodeIDText, RongcloudUserID: rcUserID}, nil
}

// MachineAgentCredentials authenticates a device supervisor (machine node
// credential) and returns the connection material of every bound agent node
// on that machine.
//
// wantPerAgentOps 由新 CLI（0.2.39+）在 fetchDeviceNodes 时置真：为本机每个
// 已绑定的非 ops agent 确保一个专属运维节点（rc "<agentRC>_ops"）并随凭据
// 下发，supervisor 据此为每个 agent 各起一个 ops 子进程，运维会话按智能体
// 隔离。旧 CLI 不传该标志 → 不创建、不下发，行为与之前完全一致（全机共享
// 机器级 ops），避免「新服务端 + 旧设备端」时运维入口指向无人应答的账号。
func (s *NodeService) MachineAgentCredentials(ctx context.Context, machineNodeIDText, credentialID, secret string, wantPerAgentOps ...bool) ([]MachineAgentCredential, error) {
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
	if len(wantPerAgentOps) > 0 && wantPerAgentOps[0] {
		// 为本机每个已绑定的非 ops agent 确保专属运维节点（幂等）。单个失败
		// 不阻断整体下发：缺的 agent 退化为机器级 ops（前端回退 om_ 账号）。
		created := false
		for _, n := range nodes {
			if n.ID == machineNode.ID || isMachineNode(n) || !n.AiType.Valid || n.AiType.String == "" || n.AiType.String == OpsAgentType {
				continue
			}
			if _, err := s.ensureAgentNodeWithRC(ctx, machineNode, machineID, OpsAgentType, perAgentOpsRCUserID(n.RongcloudUserID)); err != nil {
				s.logger.Warn("rongcloud: ensure per-agent ops node failed",
					"agent_rc", n.RongcloudUserID, "error", err)
				continue
			}
			created = true
		}
		if created {
			// 重新拉一次，把刚建出来（或复用到）的 per-agent ops 行纳入凭据下发。
			if refreshed, err := s.queries.ListRongCloudNodesByMachineID(ctx, machineID); err == nil {
				nodes = refreshed
			}
		}
	}
	creds := make([]MachineAgentCredential, 0, len(nodes))
	// per-agent ops 的 OpsFor 判定要看「去掉 _ops 后是不是本机真实 agent」——
	// 旧方案的机器级 ops（"rc_node_<key>_ops"）同样以 _ops 结尾，误标 OpsFor
	// 会让 0.2.39+ supervisor 为一个不存在的 agent 多起一个 ops 子进程。
	agentRCs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if n.ID == machineNode.ID || isMachineNode(n) || !n.AiType.Valid || n.AiType.String == "" || n.AiType.String == OpsAgentType {
			continue
		}
		agentRCs[n.RongcloudUserID] = true
	}
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
		cred := MachineAgentCredential{
			Agent:         n.AiType.String,
			NodeID:        n.NodeID,
			RongCloudUser: n.RongcloudUserID,
			Token:         string(token),
			CredentialID:  device.CredentialID.String,
			DeviceSecret:  string(deviceSecret),
		}
		if n.AiType.String == OpsAgentType {
			if owner, perAgent := classifyOpsRCUser(n.RongcloudUserID, agentRCs); perAgent {
				cred.OpsFor = owner
			}
		}
		creds = append(creds, cred)
	}
	return creds, nil
}
