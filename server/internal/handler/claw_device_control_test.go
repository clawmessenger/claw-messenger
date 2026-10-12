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
