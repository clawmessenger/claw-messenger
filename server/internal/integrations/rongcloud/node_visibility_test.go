package rongcloud

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func testNode(machineID, rcUser, aiType string) db.RongcloudNode {
	return db.RongcloudNode{
		MachineID:       machineID,
		RongcloudUserID: rcUser,
		AiType:          pgtype.Text{String: aiType, Valid: aiType != ""},
	}
}

func TestIsMachineNode(t *testing.T) {
	const shortMachine = "02:22:9C:70:BB:FD"
	const longMachine = "clawmessenger-3cb5ee6e-eb6a-4b0e-9a3f-7f6a5b4c3d2e"

	cases := []struct {
		name string
		node db.RongcloudNode
		want bool
	}{
		{"short machine node", testNode(shortMachine, "rc_node_"+shortMachine, "opencode"), true},
		{"agent child of short machine", testNode(shortMachine, "rc_node_"+shortMachine+"_codex", "codex"), false},
		{"long machine node is hashed", testNode(longMachine, "rc_node_"+machineIdentityKey(longMachine), "opencode"), true},
		{"agent child of long machine", testNode(longMachine, "rc_node_"+machineIdentityKey(longMachine)+"_opencode", "opencode"), false},
		{"empty machine id is never a machine node", testNode("", "rc_node_whatever", "opencode"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMachineNode(tc.node); got != tc.want {
				t.Fatalf("isMachineNode = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsHiddenNode(t *testing.T) {
	const machine = "02:22:9C:70:BB:FD"

	cases := []struct {
		name string
		node db.RongcloudNode
		want bool
	}{
		{"ops agent is hidden", testNode(machine, "rc_node_"+machine+"_ops", OpsAgentType), true},
		{"machine infrastructure node is hidden", testNode(machine, "rc_node_"+machine, "opencode"), true},
		{"bound agent node is visible", testNode(machine, "rc_node_"+machine+"_opencode", "opencode"), false},
		{"another bound agent node is visible", testNode(machine, "rc_node_"+machine+"_hermes", "hermes"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHiddenNode(tc.node); got != tc.want {
				t.Fatalf("isHiddenNode = %v, want %v", got, tc.want)
			}
		})
	}
}
