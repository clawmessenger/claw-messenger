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

// The contact list renders the RongCloud-side name, so an agent node that was
// never renamed must still carry a human-readable name — not the internal
// machine node id that used to be glued on as "<machine node id> (<agent>)".
func TestDefaultAgentNodeNameIsTheAgentOnly(t *testing.T) {
	if got := DefaultAgentNodeName("hermes"); got != "hermes" {
		t.Fatalf("DefaultAgentNodeName(hermes) = %q, want %q", got, "hermes")
	}
	if got := DefaultAgentNodeName("  codex  "); got != "codex" {
		t.Fatalf("DefaultAgentNodeName should trim, got %q", got)
	}
	if strings.HasPrefix(DefaultAgentNodeName("hermes"), "node_") {
		t.Fatal("default agent name must not look like an internal node id")
	}
}

func TestRepairLegacyAgentNodeName(t *testing.T) {
	cases := []struct {
		stored string
		want   string
	}{
		// Minted by the old default: machine node id + agent.
		{"node_587ae8bb0ef54b26 (hermes)", "hermes"},
		{"node_b6bcaeae05bd4343 (opencode)", "opencode"},
		{"node_587ae8bb0ef54b26 (ops)", "ops"},
		// Owner-chosen names must survive untouched.
		{"我的电脑", "我的电脑"},
		{"小酒 hermes", "小酒 hermes"},
		{"node_587ae8bb0ef54b26", "node_587ae8bb0ef54b26"},
		{"device (hermes)", "device (hermes)"},
		{"node_587ae8bb0ef54b26 (hermes) 备用", "node_587ae8bb0ef54b26 (hermes) 备用"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := repairLegacyAgentNodeName(tc.stored); got != tc.want {
			t.Errorf("repairLegacyAgentNodeName(%q) = %q, want %q", tc.stored, got, tc.want)
		}
	}
}

// Repair is idempotent: a name that is already repaired must not be rewritten
// again on the next --sync-names run.
func TestRepairLegacyAgentNodeNameIsIdempotent(t *testing.T) {
	once := repairLegacyAgentNodeName("node_587ae8bb0ef54b26 (hermes)")
	if twice := repairLegacyAgentNodeName(once); twice != once {
		t.Fatalf("second repair changed %q into %q", once, twice)
	}
}

// The machine's own node is stamped with the registration ai_type (the CLI's
// --ai-type, "opencode" by default), so a (machine_id, ai_type) lookup would
// otherwise resolve it as the opencode agent's node and reuse it. Both the
// device list and the friend list hide that node, so the agent the user just
// bound would be invisible everywhere.
func TestIsAgentNodeOfMachineSkipsTheMachineNode(t *testing.T) {
	const machine = "1ziviig73i"

	machineNode := testNode(machine, "rc_node_"+machine, "opencode")
	if isAgentNodeOfMachine(machineNode, machine, "opencode") {
		t.Fatal("the machine node must never be resolved as an agent's node")
	}

	opencode := testNode(machine, agentNodeRCUserID("opencode", machine), "opencode")
	if !isAgentNodeOfMachine(opencode, machine, "opencode") {
		t.Fatal("the bound opencode node must resolve")
	}

	// Nodes bound before the "<agent>_<machine>" scheme existed are keyed only
	// by (machine_id, ai_type) and must still be reused, not duplicated.
	legacy := testNode(machine, "rc_node_"+machine+"_opencode", "opencode")
	if !isAgentNodeOfMachine(legacy, machine, "opencode") {
		t.Fatal("a legacy rc_node_<machine>_<agent> node must still be reused")
	}

	otherMachine := testNode("9999999999", agentNodeRCUserID("opencode", "9999999999"), "opencode")
	if isAgentNodeOfMachine(otherMachine, machine, "opencode") {
		t.Fatal("a node on another machine must not match")
	}

	hermes := testNode(machine, agentNodeRCUserID("hermes", machine), "hermes")
	if isAgentNodeOfMachine(hermes, machine, "opencode") {
		t.Fatal("a different agent on the same machine must not match")
	}
}
