package handler

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
)

func testNodeRef(agent, nodeID, uuid string) rongcloud.AgentNodeRef {
	var id pgtype.UUID
	if uuid != "" {
		if err := id.Scan(uuid); err != nil {
			panic(err)
		}
	}
	return rongcloud.AgentNodeRef{Agent: agent, NodeID: nodeID, ID: id}
}

func testNodeRefRC(agent, nodeID, uuid, rcUserID string) rongcloud.AgentNodeRef {
	ref := testNodeRef(agent, nodeID, uuid)
	ref.RongcloudUserID = rcUserID
	return ref
}

// machineRefs 是一台机器的节点视图：两个业务 agent + 各自的 per-agent 运维
// 账号 + 一个机器级 ops（旧 CLI）。
func machineRefs() []rongcloud.AgentNodeRef {
	return []rongcloud.AgentNodeRef{
		testNodeRefRC("opencode", "node_open", "5e88b789-55ef-4a7e-b090-9f4e3ff208d1", "opencode_330076275"),
		testNodeRefRC("ops", "node_ops_open", "9e5bff58-97d0-43e8-8e88-f1c2b468c456", "opencode_330076275_ops"),
		testNodeRefRC("hermes", "node_hermes", "0b0f6b3e-1111-4a7e-b090-000000000001", "hermes_330076275"),
		testNodeRefRC("ops", "node_ops_hermes", "0b0f6b3e-2222-4a7e-b090-000000000002", "hermes_330076275_ops"),
		testNodeRefRC("ops", "node_ops_legacy", "0b0f6b3e-3333-4a7e-b090-000000000003", "rc_node_330076275_ops"),
	}
}

func TestControlTargetAllowedMachineNodeManagesEveryAgent(t *testing.T) {
	refs := machineRefs()
	actor := controlActor{NodeID: "node_machine", IsMachine: true}
	for _, ref := range refs {
		if !controlTargetAllowed(actor, refs, ref) {
			t.Errorf("machine node must be able to control %s", ref.Agent)
		}
	}
}

func TestControlTargetAllowedPerAgentOpsOnlyOwnsItsAccount(t *testing.T) {
	refs := machineRefs()
	actor := controlActor{
		NodeID:          "node_ops_open",
		RongcloudUserID: "opencode_330076275_ops",
		AIType:          rongcloud.OpsAgentType,
	}

	if !controlTargetAllowed(actor, refs, refs[0]) {
		t.Error("per-agent ops must be able to control the agent it serves")
	}
	if controlTargetAllowed(actor, refs, refs[2]) {
		t.Error("per-agent ops must NOT be able to control another account's agent")
	}
	// 运维子进程停自己等于自杀，不属于「本账号」的智能体。
	if controlTargetAllowed(actor, refs, refs[1]) {
		t.Error("per-agent ops must not be able to control its own ops node")
	}
}

func TestControlTargetAllowedMachineLevelOpsKeepsLegacyReach(t *testing.T) {
	refs := machineRefs()
	actor := controlActor{
		NodeID:          "node_ops_legacy",
		RongcloudUserID: "rc_node_330076275_ops",
		AIType:          rongcloud.OpsAgentType,
	}
	if !controlTargetAllowed(actor, refs, refs[2]) {
		t.Error("machine-level ops (old CLI) keeps whole-machine reach")
	}
}

func TestControlTargetAllowedPlainAgentOnlyControlsItself(t *testing.T) {
	refs := machineRefs()
	actor := controlActor{
		NodeID:          "node_open",
		RongcloudUserID: "opencode_330076275",
		AIType:          "opencode",
	}
	if !controlTargetAllowed(actor, refs, refs[0]) {
		t.Error("an agent must be able to control itself")
	}
	if controlTargetAllowed(actor, refs, refs[2]) {
		t.Error("an agent must not control another agent")
	}
}

func TestPerAgentOpsOwnerIgnoresLegacyMachineOps(t *testing.T) {
	refs := machineRefs()
	if _, ok := perAgentOpsOwner("rc_node_330076275_ops", refs); ok {
		t.Error("legacy machine-level ops must not be classified as per-agent")
	}
	if owner, ok := perAgentOpsOwner("hermes_330076275_ops", refs); !ok || owner != "hermes_330076275" {
		t.Errorf("want hermes owner, got %q/%v", owner, ok)
	}
	if _, ok := perAgentOpsOwner("opencode_330076275", refs); ok {
		t.Error("a plain agent rc is not an ops account")
	}
}

func TestApplyDeviceControlOverridesDefaultsToRunning(t *testing.T) {
	refs := []rongcloud.AgentNodeRef{
		testNodeRef("opencode", "node_a", "5e88b789-55ef-4a7e-b090-9f4e3ff208d1"),
		testNodeRef("hermes", "node_b", "9e5bff58-97d0-43e8-8e88-f1c2b468c456"),
	}

	got := applyDeviceControlOverrides(refs, map[string]clawDeviceControlState{})

	if len(got) != 2 {
		t.Fatalf("want 2 states, got %d", len(got))
	}
	for _, state := range got {
		if state.DesiredState != rongcloud.NodeDesiredRunning {
			t.Errorf("%s: want running, got %q", state.Agent, state.DesiredState)
		}
		// Version 0 is how the device tells "never asked" from "pressed stop".
		if state.Version != 0 {
			t.Errorf("%s: want version 0, got %d", state.Agent, state.Version)
		}
	}
}

func TestApplyDeviceControlOverridesHonoursStoredState(t *testing.T) {
	const stoppedUUID = "5e88b789-55ef-4a7e-b090-9f4e3ff208d1"
	refs := []rongcloud.AgentNodeRef{
		testNodeRef("opencode", "node_a", stoppedUUID),
		testNodeRef("hermes", "node_b", "9e5bff58-97d0-43e8-8e88-f1c2b468c456"),
	}
	overrides := map[string]clawDeviceControlState{
		stoppedUUID: {DesiredState: rongcloud.NodeDesiredStopped, Version: 4},
	}

	got := applyDeviceControlOverrides(refs, overrides)

	if got[0].DesiredState != rongcloud.NodeDesiredStopped || got[0].Version != 4 {
		t.Errorf("opencode: want stopped/4, got %s/%d", got[0].DesiredState, got[0].Version)
	}
	if got[0].NodeID != "node_a" || got[0].Agent != "opencode" {
		t.Errorf("identity lost in projection: %+v", got[0])
	}
	if got[1].DesiredState != rongcloud.NodeDesiredRunning || got[1].Version != 0 {
		t.Errorf("hermes: want running/0, got %s/%d", got[1].DesiredState, got[1].Version)
	}
}

func TestApplyDeviceControlOverridesSkipsRowsForUnknownNodes(t *testing.T) {
	refs := []rongcloud.AgentNodeRef{
		testNodeRef("opencode", "node_a", "5e88b789-55ef-4a7e-b090-9f4e3ff208d1"),
	}
	overrides := map[string]clawDeviceControlState{
		"00000000-0000-0000-0000-000000000000": {DesiredState: rongcloud.NodeDesiredStopped, Version: 9},
	}

	got := applyDeviceControlOverrides(refs, overrides)

	if len(got) != 1 || got[0].DesiredState != rongcloud.NodeDesiredRunning {
		t.Fatalf("stray control row leaked into the projection: %+v", got)
	}
}

// A ref without a usable uuid still has to be reported — the supervisor needs to
// know about every bound agent, and dropping one would silently leave it
// unmanaged.
func TestApplyDeviceControlOverridesHandlesMissingUUID(t *testing.T) {
	refs := []rongcloud.AgentNodeRef{testNodeRef("opencode", "node_a", "")}

	got := applyDeviceControlOverrides(refs, map[string]clawDeviceControlState{
		"5e88b789-55ef-4a7e-b090-9f4e3ff208d1": {DesiredState: rongcloud.NodeDesiredStopped, Version: 2},
	})

	if len(got) != 1 {
		t.Fatalf("want 1 state, got %d", len(got))
	}
	if got[0].DesiredState != rongcloud.NodeDesiredRunning || got[0].Version != 0 {
		t.Errorf("want running/0 fallback, got %s/%d", got[0].DesiredState, got[0].Version)
	}
}

func TestApplyDeviceControlOverridesEmptyInput(t *testing.T) {
	got := applyDeviceControlOverrides(nil, nil)
	if got == nil {
		t.Fatal("want an empty slice, not nil — it is marshalled straight into the response")
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %d", len(got))
	}
}

// 老设备兜底：0.2.45 及更早的 CLI 会把融云 id 当 target 发上来（它从
// <machineRC>_<agent> 里取 agent 名时算错）。三种写法都要能落到同一个节点。
func TestMatchControlTargetAcceptsNameNodeIDAndRcUserID(t *testing.T) {
	refs := machineRefs()

	for _, target := range []string{"hermes", "node_hermes", "hermes_330076275"} {
		got := matchControlTarget(refs, target)
		if got == nil {
			t.Fatalf("target %q: want a match, got nil", target)
		}
		if got.Agent != "hermes" {
			t.Errorf("target %q: want hermes, got %s", target, got.Agent)
		}
	}
}

func TestMatchControlTargetRejectsUnknownAndEmpty(t *testing.T) {
	refs := machineRefs()
	for _, target := range []string{"", "   ", "rc_node_3300776275", "codex"} {
		if got := matchControlTarget(refs, target); got != nil {
			t.Errorf("target %q: want nil, got %s", target, got.Agent)
		}
	}
}
