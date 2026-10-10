package rongcloud

import (
	"strings"
	"testing"
)

func TestAgentNodeRCUserID(t *testing.T) {
	cases := []struct {
		agent   string
		machine string
		want    string
	}{
		{"hermes", "1234567890", "hermes_1234567890"},
		{"codex", "1234567890", "codex_1234567890"},
		{"opencode", "oc5yj7nusm", "opencode_oc5yj7nusm"},
		{"ops", "1234567890", "ops_1234567890"},
	}
	for _, tc := range cases {
		if got := agentNodeRCUserID(tc.agent, tc.machine); got != tc.want {
			t.Errorf("agentNodeRCUserID(%q, %q) = %q, want %q", tc.agent, tc.machine, got, tc.want)
		}
	}
}

// The device list parses node ids by their "<agent>_" prefix
// (clawmessenger-web/src/lib/ai-node-id.ts) and users read the id directly, so
// the agent name has to stay in front — it used to be a trailing suffix on an
// opaque "rc_node_<machine>_<agent>" id.
func TestAgentNodeRCUserIDKeepsAgentAsPrefix(t *testing.T) {
	id := agentNodeRCUserID("hermes", "1234567890")
	if !strings.HasPrefix(id, "hermes_") {
		t.Fatalf("agent must lead the node id, got %q", id)
	}
	if strings.Contains(id, "rc_node_") {
		t.Fatalf("agent node ids no longer use the rc_node_ namespace, got %q", id)
	}
}

// RongCloud caps a user id at 64 chars. The longest agent name we ship is
// "antigravity" (11), so the numeric machine id must stay well inside the cap.
func TestAgentNodeRCUserIDStaysWithinRongCloudLimit(t *testing.T) {
	id := agentNodeRCUserID("antigravity", strings.Repeat("9", 10))
	if len(id) > rongcloudRCIdentityLimit {
		t.Fatalf("node id %q is %d chars, over the %d char limit", id, len(id), rongcloudRCIdentityLimit)
	}
}

// A numeric machine id is short enough to be embedded verbatim, so node ids
// stay human-readable instead of being sha256-ed into "m_<24 hex>".
func TestMachineIdentityKeyKeepsNumericIDsVerbatim(t *testing.T) {
	if got := machineIdentityKey("1234567890"); got != "1234567890" {
		t.Fatalf("machineIdentityKey(numeric) = %q, want it unchanged", got)
	}
}

// The machine node keeps the "rc_node_<machine key>" id with no agent suffix;
// its agent children must not be mistaken for it, or they vanish from the
// device list.
func TestAgentNodeIsNotTheMachineNode(t *testing.T) {
	const machine = "1234567890"
	agent := testNode(machine, agentNodeRCUserID("hermes", machine), "hermes")
	if isMachineNode(agent) {
		t.Fatal("agent node must not be classified as the machine node")
	}
	if isHiddenNode(agent) {
		t.Fatal("a bound agent node must be visible")
	}
	// The ops node shares the naming scheme and must stay hidden.
	ops := testNode(machine, agentNodeRCUserID(OpsAgentType, machine), OpsAgentType)
	if !isHiddenNode(ops) {
		t.Fatal("ops agent node must stay hidden")
	}
	machineNode := testNode(machine, "rc_node_"+machine, "opencode")
	if !isMachineNode(machineNode) || !isHiddenNode(machineNode) {
		t.Fatal("machine node must stay hidden")
	}
}
