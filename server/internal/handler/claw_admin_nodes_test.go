package handler

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func testNodeRecord(nodeID, rcUser, aiType, machineID string) clawAdminNodeRecord {
	var id pgtype.UUID
	if err := id.Scan("5e88b789-55ef-4a7e-b090-9f4e3ff208d1"); err != nil {
		panic(err)
	}
	return clawAdminNodeRecord{
		ID:              id,
		NodeID:          nodeID,
		RongcloudUserID: rcUser,
		AIType:          aiType,
		MachineID:       machineID,
		CreatedAt:       time.Date(2026, 10, 9, 3, 36, 47, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 10, 9, 3, 36, 47, 0, time.UTC),
	}
}

func TestClawAdminNodeMatchesAppliesNodeTypeFilter(t *testing.T) {
	rec := testNodeRecord("node_abc", "opencode_3300776275", "opencode", "3300776275")

	if !clawAdminNodeMatches(rec, "小久 opencode", "", "", "opencode") {
		t.Fatal("an opencode node must match the opencode filter")
	}
	if clawAdminNodeMatches(rec, "小久 opencode", "", "", "hermes") {
		t.Fatal("an opencode node must not match the hermes filter")
	}
	// Empty filter means "全部节点".
	if !clawAdminNodeMatches(rec, "小久 opencode", "", "", "") {
		t.Fatal("an empty node_type filter must accept every node")
	}
}

func TestClawAdminNodeMatchesSearchesTheFieldsThePlaceholderPromises(t *testing.T) {
	rec := testNodeRecord("node_abc", "opencode_3300776275", "opencode", "3300776275")

	for _, keyword := range []string{
		"node_abc",            // 节点ID
		"小久 opencode",         // 名称（存于 rongcloud_user.name）
		"机房 A 的主力镜像节点",        // 描述（存于 rongcloud_system_config）
		"opencode_3300776275", // 融云 id
		"3300776275",          // machine id
		"OPENCODE",            // 大小写不敏感
	} {
		if !clawAdminNodeMatches(rec, "小久 opencode", "机房 A 的主力镜像节点", keyword, "") {
			t.Fatalf("keyword %q should match", keyword)
		}
	}
	if clawAdminNodeMatches(rec, "小久 opencode", "机房 A 的主力镜像节点", "不存在的关键词", "") {
		t.Fatal("an unrelated keyword must not match")
	}
}

func TestClawAdminNodeRowProjectsTheFrontendShape(t *testing.T) {
	h := &Handler{}
	rec := testNodeRecord("node_abc", "opencode_3300776275", "opencode", "3300776275")

	row := h.clawAdminNodeRowFor(rec, "小久 opencode", "说明", "850509", "小酒", true,
		clawAdminNodeControl{DesiredState: "running", DesiredVersion: 3})

	if row.DesiredState != "running" || row.DesiredVersion != 3 {
		t.Fatalf("desired state = %q / %d", row.DesiredState, row.DesiredVersion)
	}
	// 展示节点（普通 AI 节点）必须可启停。
	if !row.Controllable {
		t.Fatal("display node must be controllable")
	}

	if row.NodeID != "node_abc" {
		t.Fatalf("node_id = %q", row.NodeID)
	}
	if row.Name != "小久 opencode" {
		t.Fatalf("name = %q", row.Name)
	}
	if row.NodeType != "opencode" {
		t.Fatalf("node_type = %q", row.NodeType)
	}
	if row.UserID != "850509" || row.Username != "小酒" {
		t.Fatalf("归属 = %q / %q", row.UserID, row.Username)
	}
	if !row.IsSystemReserved {
		t.Fatal("is_system_reserved must be carried through")
	}
	if row.Description != "说明" {
		t.Fatalf("description = %q", row.Description)
	}
	// 线上在线态由心跳推导；没有心跳表时只能报离线，绝不能回落到库里的旧字段。
	if row.DeployStatus != "offline" {
		t.Fatalf("deploy_status = %q, want offline", row.DeployStatus)
	}
	if row.CreatedAt != "2026-10-09T03:36:47Z" {
		t.Fatalf("created_at = %q", row.CreatedAt)
	}
}

func TestClawAdminNodeRowFallsBackToAgentTypeForTheName(t *testing.T) {
	h := &Handler{}
	rec := testNodeRecord("node_abc", "opencode_3300776275", "opencode", "3300776275")

	row := h.clawAdminNodeRowFor(rec, "", "", "", "", false, clawAdminNodeControl{})

	// rongcloud_user.name 为空时显示 ai_type，而不是空串或内部 id。
	if row.Name != "opencode" {
		t.Fatalf("name = %q, want the agent type", row.Name)
	}
	if row.UserID != "" || row.Username != "" {
		t.Fatalf("an unclaimed node must not report归属: %q / %q", row.UserID, row.Username)
	}
}

func TestClawAdminNodeStatusExplainsItsSource(t *testing.T) {
	h := &Handler{}
	rec := testNodeRecord("node_abc", "opencode_3300776275", "opencode", "3300776275")

	status, detail := h.clawAdminNodeStatus(rec)

	if status != "offline" {
		t.Fatalf("status = %q, want offline when no heartbeat table is wired", status)
	}
	if detail == "" {
		t.Fatal("detail must explain where the status came from")
	}
}

// 机器名只能来自机器主节点的昵称，且必须去掉 CLI 拼上去的 " <aiType>" 后缀。
//
// 这条钉住的是一个很容易退化的行为：如果哪天有人图省事用 agent 节点自己的昵称
// 当机器名，三台机器上各自的 opencode 会全部显示成「opencode」，列表又回到
// 「三行一模一样」—— 而这正是本次需求要解决的问题。
func TestClawAdminMachineNamesComeFromTheMachineNode(t *testing.T) {
	records := []clawAdminNodeRecord{
		// 机器主节点：rc 必须等于 "rc_node_" + machineIdentityKey(machine_id)。
		testNodeRecord("node_m", "rc_node_3300776275", "opencode", "3300776275"),
		testNodeRecord("node_a", "rc_node_3300776275_hermes", "hermes", "3300776275"),
		testNodeRecord("node_b", "rc_node_3300776275_opencode", "opencode", "3300776275"),
	}
	names := map[string]string{
		"rc_node_3300776275":          "小久 opencode",
		"rc_node_3300776275_hermes":   "hermes",
		"rc_node_3300776275_opencode": "opencode",
	}
	h := &Handler{}

	got := h.clawAdminMachineNames(records, names)
	if got["3300776275"] != "小久" {
		t.Fatalf("machine name = %q, want 小久 (the aiType suffix must be stripped)", got["3300776275"])
	}
	if len(got) != 1 {
		t.Fatalf("only the machine node may contribute a machine name, got %#v", got)
	}

	// 没有机器主节点行时**不能**凭空造名字：宁可显示机器键，也不能把某个 agent
	// 的昵称当成整台机器的名字（那会让「升级」按错误的机器归并）。
	withoutMachineNode := h.clawAdminMachineNames(records[1:], names)
	if len(withoutMachineNode) != 0 {
		t.Fatalf("agent rows must never name the machine, got %#v", withoutMachineNode)
	}

	// 机器主节点的 aiType 为空（早期注册/探针行）：昵称原样保留，不要瞎截。
	probe := []clawAdminNodeRecord{testNodeRecord("node_p", "rc_node_02:95:06:48:f0:dc", "", "02:95:06:48:f0:dc")}
	probeNames := map[string]string{"rc_node_02:95:06:48:f0:dc": "bind-probe-node"}
	if name := h.clawAdminMachineNames(probe, probeNames)["02:95:06:48:f0:dc"]; name != "bind-probe-node" {
		t.Fatalf("a machine node without an aiType must keep its full nickname, got %q", name)
	}
}

// 机器名与设备主机名都要贴到行上，且主机名缺失时不能把机器名顶掉。
func TestClawAdminAttachUpgradeCarriesMachineNameAndHostname(t *testing.T) {
	h := &Handler{}
	rec := testNodeRecord("node_a", "rc_node_3300776275_hermes", "hermes", "3300776275")
	machineNames := map[string]string{"3300776275": "小久"}

	row := h.clawAdminAttachUpgrade(clawAdminNodeRow{}, rec, map[string]clawNodeUpgrade{
		"3300776275": {
			MachineID:        "3300776275",
			ReportedVersion:  "1.3.3",
			ReportedPlatform: "linux-x64",
			ReportedHostname: "SHGuoZiWei-10.1.1.225",
		},
	}, machineNames)
	if row.MachineName != "小久" || row.Hostname != "SHGuoZiWei-10.1.1.225" {
		t.Fatalf("row = %#v, want machine_name=小久 hostname=SHGuoZiWei-10.1.1.225", row)
	}

	// 机器还没上报过任何升级信息（没有行）：机器名仍然要给，否则列表第一屏
	// 全是「-」，管理员根本判断不出这行属于哪台机器。
	bare := h.clawAdminAttachUpgrade(clawAdminNodeRow{}, rec, map[string]clawNodeUpgrade{}, machineNames)
	if bare.MachineName != "小久" || bare.Hostname != "" {
		t.Fatalf("without an upgrade row the machine name must survive, got %#v", bare)
	}
}
