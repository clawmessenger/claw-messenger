package rongcloud

import (
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const legacyMachineID = "clawmessenger-3cb5ee6e-eb6b-45d6-afcb-54c9ef5bac7c"

func retireNode(machineID, rcUserID string) db.RongcloudNode {
	return db.RongcloudNode{MachineID: machineID, RongcloudUserID: rcUserID}
}

func TestIsLegacyMachineIdentity(t *testing.T) {
	cases := []struct {
		name    string
		node    db.RongcloudNode
		maxLen  int
		legacy  bool
		explain string
	}{
		{
			name:    "cli uuid machine id is hashed and therefore legacy",
			node:    retireNode(legacyMachineID, "rc_node_m_e99ad19f108d9dad78ad07e3_codex"),
			maxLen:  MachineIdentityBudget,
			legacy:  true,
			explain: "50 chars overflows the 31-char budget so the server sha256-ed it",
		},
		{
			name:    "short machine id is current",
			node:    retireNode("7che3nsv0n", "rc_node_7che3nsv0n_codex"),
			maxLen:  MachineIdentityBudget,
			legacy:  false,
			explain: "8-11 base36 chars fit the budget and stay verbatim",
		},
		{
			name:    "missing machine id predates the column",
			node:    retireNode("", "rc_node_clawmessenger-3cb5ee6e-eb6b-45d6-afcb-54c9ef5bac7c"),
			maxLen:  MachineIdentityBudget,
			legacy:  true,
			explain: "every registration since records one",
		},
		{
			name:    "whitespace-only machine id counts as missing",
			node:    retireNode("   ", "rc_node_m_e99ad19f108d9dad78ad07e3"),
			maxLen:  MachineIdentityBudget,
			legacy:  true,
			explain: "trimmed before the comparison",
		},
		{
			name:    "mac-style id survives the budget sweep",
			node:    retireNode("aa:bb:cc:dd:ee:04", "rc_node_aa:bb:cc:dd:ee:04"),
			maxLen:  MachineIdentityBudget,
			legacy:  false,
			explain: "17 chars still fits the 31-char budget",
		},
		{
			name:    "mac-style id is swept by the short-format limit",
			node:    retireNode("aa:bb:cc:dd:ee:04", "rc_node_aa:bb:cc:dd:ee:04"),
			maxLen:  ShortMachineIDMax,
			legacy:  true,
			explain: "caller asked for nothing longer than a short id",
		},
		{
			name:    "boundary: exactly the budget is kept",
			node:    retireNode("abcdefghijklmnopqrstuvwxyz01234", "rc_node_abcdefghijklmnopqrstuvwxyz01234"),
			maxLen:  MachineIdentityBudget,
			legacy:  false,
			explain: "31 chars is the last value the server leaves verbatim",
		},
		{
			name:    "boundary: one over the budget is legacy",
			node:    retireNode("abcdefghijklmnopqrstuvwxyz012345", "rc_node_m_whatever"),
			maxLen:  MachineIdentityBudget,
			legacy:  true,
			explain: "32 chars was hashed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsLegacyMachineIdentity(tc.node, tc.maxLen)
			if got != tc.legacy {
				t.Fatalf("IsLegacyMachineIdentity = %v, want %v (%s)", got, tc.legacy, tc.explain)
			}
		})
	}
}

func TestPlanLegacyNodeRetirementSkipsCurrentNodes(t *testing.T) {
	groups := PlanLegacyNodeRetirement([]db.RongcloudNode{
		retireNode("7che3nsv0n", "rc_node_7che3nsv0n"),
		retireNode("7che3nsv0n", "rc_node_7che3nsv0n_codex"),
		retireNode("aa:bb:cc:dd:ee:04", "rc_node_aa:bb:cc:dd:ee:04"),
	}, MachineIdentityBudget)

	if len(groups) != 0 {
		t.Fatalf("planned %d groups, want none: %+v", len(groups), groups)
	}
}

func TestPlanLegacyNodeRetirementGroupsByMachine(t *testing.T) {
	groups := PlanLegacyNodeRetirement([]db.RongcloudNode{
		retireNode(legacyMachineID, "rc_node_m_e99ad19f108d9dad78ad07e3_codex"),
		retireNode(legacyMachineID, "rc_node_m_e99ad19f108d9dad78ad07e3"),
		retireNode(legacyMachineID, "rc_node_m_e99ad19f108d9dad78ad07e3_ops"),
		retireNode("clawmessenger-9841225c-0000-0000-0000-000000000000", "rc_node_m_2566cd6d064de806e13974ee"),
	}, MachineIdentityBudget)

	if len(groups) != 2 {
		t.Fatalf("planned %d groups, want 2", len(groups))
	}
	// Groups are sorted by machine id; the second uuid sorts after the first.
	first := groups[0]
	if first.MachineID != legacyMachineID {
		t.Fatalf("first group machine id = %q, want %q", first.MachineID, legacyMachineID)
	}
	if len(first.Nodes) != 3 {
		t.Fatalf("first group holds %d nodes, want 3", len(first.Nodes))
	}
	// Nodes within a group are sorted by account so output is diffable.
	want := []string{
		"rc_node_m_e99ad19f108d9dad78ad07e3",
		"rc_node_m_e99ad19f108d9dad78ad07e3_codex",
		"rc_node_m_e99ad19f108d9dad78ad07e3_ops",
	}
	for i, w := range want {
		if got := first.Nodes[i].RongcloudUserID; got != w {
			t.Fatalf("node[%d] = %q, want %q", i, got, w)
		}
	}
}

func TestPlanLegacyNodeRetirementKeepsUnknownMachinesApart(t *testing.T) {
	// Rows with no machine id must not be merged into one another: keying them
	// by machine id would collapse unrelated devices into a single group.
	groups := PlanLegacyNodeRetirement([]db.RongcloudNode{
		retireNode("", "rc_node_clawmessenger-aaaa"),
		retireNode("", "rc_node_clawmessenger-bbbb"),
	}, MachineIdentityBudget)

	if len(groups) != 2 {
		t.Fatalf("planned %d groups, want 2", len(groups))
	}
	for _, g := range groups {
		if g.MachineID != "" {
			t.Fatalf("group machine id = %q, want empty", g.MachineID)
		}
		if len(g.Nodes) != 1 {
			t.Fatalf("group holds %d nodes, want 1", len(g.Nodes))
		}
	}
}

func TestPlanLegacyNodeRetirementIsStable(t *testing.T) {
	nodes := []db.RongcloudNode{
		retireNode("", "rc_node_clawmessenger-bbbb"),
		retireNode(legacyMachineID, "rc_node_m_e99ad19f108d9dad78ad07e3_ops"),
		retireNode("", "rc_node_clawmessenger-aaaa"),
		retireNode(legacyMachineID, "rc_node_m_e99ad19f108d9dad78ad07e3"),
	}

	// Shuffled input must plan identically, or a dry run would not predict the
	// commit.
	shuffled := []db.RongcloudNode{nodes[2], nodes[3], nodes[0], nodes[1]}
	first := PlanLegacyNodeRetirement(nodes, MachineIdentityBudget)
	second := PlanLegacyNodeRetirement(shuffled, MachineIdentityBudget)

	if len(first) != len(second) {
		t.Fatalf("group counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].MachineID != second[i].MachineID {
			t.Fatalf("group[%d] machine id differs: %q vs %q", i, first[i].MachineID, second[i].MachineID)
		}
		if first[i].Nodes[0].RongcloudUserID != second[i].Nodes[0].RongcloudUserID {
			t.Fatalf("group[%d] first node differs", i)
		}
	}
}
