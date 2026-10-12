package rongcloud

import (
	"encoding/json"
	"reflect"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestNodeCapabilitiesForDerivesDiscussionSetFromAgentType(t *testing.T) {
	for _, agent := range []string{"opencode", "openclaw", "hermes", "codex", "claude"} {
		got := NodeCapabilitiesFor(agent)
		if !reflect.DeepEqual(got, discussionCapabilities) {
			t.Fatalf("NodeCapabilitiesFor(%q) = %v, want the discussion set %v",
				agent, got, discussionCapabilities)
		}
	}
}

func TestNodeCapabilitiesForSkipsInternalNodes(t *testing.T) {
	// Empty ai_type is the per-machine infrastructure node before Register
	// stamps it; ops is the built-in maintenance agent. Neither is ever asked
	// to host a discussion, and both are hidden from every picker.
	for _, aiType := range []string{"", "   ", OpsAgentType} {
		if got := NodeCapabilitiesFor(aiType); len(got) != 0 {
			t.Fatalf("NodeCapabilitiesFor(%q) = %v, want none", aiType, got)
		}
	}
}

func TestNodeCapabilitiesForReturnsACopy(t *testing.T) {
	got := NodeCapabilitiesFor("opencode")
	got[0] = "mutated"
	if discussionCapabilities[0] == "mutated" {
		t.Fatal("NodeCapabilitiesFor leaked the package-level capability set")
	}
}

func TestMergeNodeCapabilitiesKeepsReportedValues(t *testing.T) {
	got := MergeNodeCapabilities([]string{"text_reply"}, "opencode")
	want := []string{
		"discussion_host",
		"discussion_model_routing",
		"discussion_role_recommendation",
		"discussion_roundtable",
		"text_reply",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeNodeCapabilities = %v, want %v", got, want)
	}
}

func TestMergeNodeCapabilitiesIsSortedAndDeduped(t *testing.T) {
	got := MergeNodeCapabilities([]string{"text_reply", "discussion_host", " text_reply ", ""}, "opencode")
	want := []string{
		"discussion_host",
		"discussion_model_routing",
		"discussion_role_recommendation",
		"discussion_roundtable",
		"text_reply",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeNodeCapabilities = %v, want %v", got, want)
	}
	// Stable output is what lets the write path skip a no-op UPDATE.
	if again := MergeNodeCapabilities(got, "opencode"); !reflect.DeepEqual(again, got) {
		t.Fatalf("merge is not idempotent: %v then %v", got, again)
	}
}

func TestMergeNodeCapabilitiesReturnsEmptyArrayForInternalNode(t *testing.T) {
	got := MergeNodeCapabilities(nil, OpsAgentType)
	if len(got) != 0 {
		t.Fatalf("MergeNodeCapabilities(ops) = %v, want empty", got)
	}
	// The column is NOT NULL, so the rendered JSON must still be an array.
	if encoded := string(nodeCapabilitiesJSON(nil, OpsAgentType)); encoded != "[]" {
		t.Fatalf("ops capabilities encode to %s, want []", encoded)
	}
}

func TestNodeCapabilitiesJSONFromStoredToleratesNonArrayJSON(t *testing.T) {
	// The column default is '{}' and at least one live row holds JSON null, so
	// re-deriving from a stored value must not fail on either shape.
	for _, stored := range [][]byte{nil, []byte(""), []byte("{}"), []byte("null")} {
		var got []string
		if err := json.Unmarshal(nodeCapabilitiesJSONFromStored(stored, "opencode"), &got); err != nil {
			t.Fatalf("stored %q produced invalid JSON: %v", stored, err)
		}
		if !reflect.DeepEqual(got, discussionCapabilities) {
			t.Fatalf("stored %q produced %v, want the discussion set", stored, got)
		}
	}
}

func TestNodeCapabilitiesJSONFromStoredPreservesReportedValues(t *testing.T) {
	var got []string
	if err := json.Unmarshal(nodeCapabilitiesJSONFromStored([]byte(`["text_reply"]`), "hermes"), &got); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"discussion_host",
		"discussion_model_routing",
		"discussion_role_recommendation",
		"discussion_roundtable",
		"text_reply",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestIsDisplayNodeHidesInternalNodes(t *testing.T) {
	machine := db.RongcloudNode{MachineID: "3300776275", RongcloudUserID: "rc_node_3300776275"}
	if IsDisplayNode(machine) {
		t.Fatal("the per-machine infrastructure node must not be user-facing")
	}
	ops := db.RongcloudNode{MachineID: "3300776275", RongcloudUserID: "ops_3300776275"}
	ops.AiType.String, ops.AiType.Valid = OpsAgentType, true
	if IsDisplayNode(ops) {
		t.Fatal("the ops maintenance agent must not be user-facing")
	}
	agent := db.RongcloudNode{MachineID: "3300776275", RongcloudUserID: "opencode_3300776275"}
	agent.AiType.String, agent.AiType.Valid = "opencode", true
	if !IsDisplayNode(agent) {
		t.Fatal("a bound agent node must be user-facing")
	}
}
