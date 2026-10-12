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

	row := h.clawAdminNodeRowFor(rec, "小久 opencode", "说明", "850509", "小酒", true)

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

	row := h.clawAdminNodeRowFor(rec, "", "", "", "", false)

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
