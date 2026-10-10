package rongcloud

import (
	"sort"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ShortMachineIDMax is the longest machine id the CLI mints since short ids
// were introduced (see packages/xiachat-cli/src/config.ts, MACHINE_ID_LENGTH =
// 10). Pass it as maxMachineIDLength to sweep anything outside the format.
const ShortMachineIDMax = 11

// MachineIdentityBudget is how many characters of a RongCloud user id the
// machine segment may occupy. RongCloud caps a user id at 64, and
// "rc_node_" (8) + "_" (1) + the longest agent name (24) are spoken for, which
// leaves 31. A machine id longer than this is sha256-ed by machineIdentityKey
// into an opaque "m_<24 hex>" key, so a node id reads like
// rc_node_m_e99ad19f108d9dad78ad07e3_codex and the machine identity is lost.
const MachineIdentityBudget = 31

// LegacyNodeGroup is the set of node rows that belong to one machine and whose
// RongCloud identities predate short machine ids.
type LegacyNodeGroup struct {
	// MachineID is the raw value from rongcloud_node.machine_id. It is empty
	// for rows created before the column existed, which are legacy by
	// definition: every registration since has recorded one.
	MachineID string
	Nodes     []db.RongcloudNode
}

// IsLegacyMachineIdentity reports whether a node's RongCloud identity was
// derived from a machine id that predates short ids.
//
// maxMachineIDLength is the longest machine id to keep: MachineIdentityBudget
// retires exactly the ids the server had to hash (the CLI's 50-character
// "clawmessenger-<uuid>"), while ShortMachineIDMax additionally retires
// anything that is merely longer than the 8-11 character format — useful for
// sweeping ids minted by other clients.
func IsLegacyMachineIdentity(n db.RongcloudNode, maxMachineIDLength int) bool {
	id := strings.TrimSpace(n.MachineID)
	if id == "" {
		return true
	}
	return len(id) > maxMachineIDLength
}

// PlanLegacyNodeRetirement picks the nodes worth retiring and groups them by
// machine so the caller can report "this device's node group" rather than a
// flat list. Groups and their nodes are sorted for stable, diffable output.
//
// Retiring is the only way to shrink an existing node id: a RongCloud user id
// is the account's primary key and cannot be renamed, so the device has to
// re-pair under a fresh short machine id and the old group is deleted. The
// alternative — pointing the group at new accounts in place — would rewrite
// every conversation the owner ever had with the node.
func PlanLegacyNodeRetirement(nodes []db.RongcloudNode, maxMachineIDLength int) []LegacyNodeGroup {
	byMachine := make(map[string][]db.RongcloudNode)
	var order []string
	for _, n := range nodes {
		if !IsLegacyMachineIdentity(n, maxMachineIDLength) {
			continue
		}
		key := strings.TrimSpace(n.MachineID)
		if key == "" {
			// No recorded machine identity: key the group by the node's own
			// account so a row is never merged into an unrelated machine.
			key = n.RongcloudUserID
		}
		if _, seen := byMachine[key]; !seen {
			order = append(order, key)
		}
		byMachine[key] = append(byMachine[key], n)
	}

	groups := make([]LegacyNodeGroup, 0, len(order))
	for _, key := range order {
		groupNodes := byMachine[key]
		sort.Slice(groupNodes, func(i, j int) bool {
			return groupNodes[i].RongcloudUserID < groupNodes[j].RongcloudUserID
		})
		machineID := key
		if strings.TrimSpace(groupNodes[0].MachineID) == "" {
			machineID = ""
		}
		groups = append(groups, LegacyNodeGroup{MachineID: machineID, Nodes: groupNodes})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].MachineID != groups[j].MachineID {
			return groups[i].MachineID < groups[j].MachineID
		}
		// Groups without a recorded machine id are keyed by their first node's
		// account, so fall back to that for a stable order.
		return groups[i].Nodes[0].RongcloudUserID < groups[j].Nodes[0].RongcloudUserID
	})
	return groups
}
