package rongcloud

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Desired run-states for a bound agent node. These are the only two values the
// rongcloud_node_control.desired_state CHECK constraint admits, and they are
// also the supervisor's vocabulary: "running" means a child process should exist
// for this agent, "stopped" means it should not.
const (
	NodeDesiredRunning = "running"
	NodeDesiredStopped = "stopped"
)

// AuthorizeDevice verifies a machine node's device credential
// (nodeId + credentialId + secret) and returns the machine node row.
//
// The same check is inlined in Heartbeat, BindAgentsByDeviceCredential and
// MachineAgentCredentials; it is factored out here so the device-control
// endpoint does not become a fourth copy. Deliberately not retrofitted onto the
// existing three: one of them wraps the node lookup in a different error, and
// rewriting them is churn the current change does not need.
//
// Every failure collapses to ErrInvalidDeviceCredential. The caller cannot tell
// "no such node" from "wrong secret", which is the point: the endpoint is
// reachable with no session, so it must not become an existence oracle for node
// ids.
func (s *NodeService) AuthorizeDevice(ctx context.Context, machineNodeIDText, credentialID, secret string) (db.RongcloudNode, error) {
	if s.queries == nil {
		return db.RongcloudNode{}, errors.New("rongcloud: database not configured")
	}
	machineNode, err := s.queries.GetRongCloudNodeByNodeID(ctx, machineNodeIDText)
	if err != nil {
		return db.RongcloudNode{}, ErrInvalidDeviceCredential
	}
	device, err := s.queries.GetRongCloudDeviceByNodeAndCredential(ctx, db.GetRongCloudDeviceByNodeAndCredentialParams{
		NodeID:       machineNode.ID,
		CredentialID: pgtype.Text{String: credentialID, Valid: credentialID != ""},
	})
	if err != nil {
		return db.RongcloudNode{}, ErrInvalidDeviceCredential
	}
	if !device.CredentialSecretEncrypted.Valid || s.box == nil {
		return db.RongcloudNode{}, ErrInvalidDeviceCredential
	}
	sealed, err := base64.StdEncoding.DecodeString(device.CredentialSecretEncrypted.String)
	if err != nil {
		return db.RongcloudNode{}, ErrInvalidDeviceCredential
	}
	plain, err := s.box.Open(sealed)
	if err != nil || !hmac.Equal([]byte(plain), []byte(secret)) {
		return db.RongcloudNode{}, ErrInvalidDeviceCredential
	}
	return machineNode, nil
}

// AgentNodeRef identifies one bound agent node on a machine.
type AgentNodeRef struct {
	Agent  string      `json:"agent"`
	NodeID string      `json:"nodeId"`
	ID     pgtype.UUID `json:"-"`
}

// AgentNodesForDevice authenticates a machine device credential and returns the
// agent nodes bound to that machine — the set the supervisor is responsible for
// running.
//
// The machine key derivation is copied verbatim from MachineAgentCredentials
// rather than reinvented: both must resolve the same node set, and the two
// callers already share the same fallback (machine_id, else the RongCloud user
// id suffix). Splitting them would let the control channel and the credential
// channel disagree about which agents exist.
func (s *NodeService) AgentNodesForDevice(ctx context.Context, machineNodeIDText, credentialID, secret string) ([]AgentNodeRef, error) {
	machineNode, err := s.AuthorizeDevice(ctx, machineNodeIDText, credentialID, secret)
	if err != nil {
		return nil, err
	}
	machineID := machineNode.MachineID
	if machineID == "" {
		machineID = strings.TrimPrefix(machineNode.RongcloudUserID, "rc_node_")
	}
	nodes, err := s.queries.ListRongCloudNodesByMachineID(ctx, machineID)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: list machine nodes: %w", err)
	}
	out := make([]AgentNodeRef, 0, len(nodes))
	for _, n := range nodes {
		if n.ID == machineNode.ID {
			continue // the machine node itself is not an agent target
		}
		if !n.AiType.Valid || n.AiType.String == "" {
			continue
		}
		out = append(out, AgentNodeRef{Agent: n.AiType.String, NodeID: n.NodeID, ID: n.ID})
	}
	return out, nil
}
