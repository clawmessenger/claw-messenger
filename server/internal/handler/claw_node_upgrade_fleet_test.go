package handler

// 全量失联收尾（clawUpgradeSweepStalledFleet）的边界测试。
//
// 单独一个文件而不是往 claw_node_upgrade_test.go 里加：那个文件正在被 QA 补租约
// 边界用例，两块改动落在同一个文件上只会互相踩。这个文件只覆盖「全量 sweep 没有
// 作用域依赖，且不会误杀排队中的指令」这一件事。

import (
	"context"
	"strings"
	"testing"
)

// TestClawUpgradeSweepFleetClosesEveryBatchAndLeavesPendingAlone 钉住两件事：
//
//  1. 全量 sweep **没有作用域依赖**：A 批次和 B 批次里各自卡住的行要一起被收尾。
//     这条正是「主列表那列永远转圈」的修复点 —— 主列表（clawAdminNodeUpgrades）
//     不知道自己关心哪个 batch，只能全量扫。
//  2. `pending` **不能**被它碰到：那两个状态（pending / waiting_idle）的语义就是
//     「等设备来领」，离线设备上线后还要靠它领走指令。全量扫的范围比按批次扫大
//     得多，一旦把状态集合写宽了，一次列表刷新就会把**所有**离线设备的排队指令
//     作废 —— 所以这条断言比按批次的同款断言更重要。
func TestClawUpgradeSweepFleetClosesEveryBatchAndLeavesPendingAlone(t *testing.T) {
	if testPool == nil || testHandler == nil {
		t.Skip("handler test database is not available")
	}
	ctx := context.Background()

	type fixture struct {
		machine string
		status  string
		stale   bool
	}
	rows := []fixture{
		{clawUpgradeTestMachine("upg-fleet-a"), clawUpgradeStatusDownloading, true},
		{clawUpgradeTestMachine("upg-fleet-b"), clawUpgradeStatusRestarting, true},
		{clawUpgradeTestMachine("upg-fleet-c"), clawUpgradeStatusPending, true},
		{clawUpgradeTestMachine("upg-fleet-d"), clawUpgradeStatusDownloading, false},
	}
	machines := make([]string, 0, len(rows))
	rcs := make([]string, 0, len(rows))
	for i := range rows {
		rc := "rc_node_" + rows[i].machine
		machines = append(machines, rows[i].machine)
		rcs = append(rcs, rc)
		insertClawUpgradeTestNode(t, testWorkspaceID, rows[i].machine, rc)
		insertClawUpgradeTestReport(t, testWorkspaceID, rows[i].machine, clawUpgradePlatformWindows, "1.2.0")
		// 前两条属于两个**不同的**批次：全量 sweep 必须同时覆盖它们。
		batch := "upg-fleet-x"
		if i == 1 {
			batch = "upg-fleet-y"
		} else if i == 2 {
			batch = "upg-fleet-z"
		}
		if _, err := testPool.Exec(ctx, `
			UPDATE rongcloud_node_upgrade
			   SET target_version = '1.3.0', desired_generation = 4, status = $2,
			       error_code = '', error_detail = '',
			       batch_id = $3, wave = 1, requested_at = now(),
			       updated_at = CASE WHEN $4 THEN now() - interval '20 minutes' ELSE now() END
			 WHERE machine_id = $1`, rows[i].machine, rows[i].status, batch, rows[i].stale); err != nil {
			t.Fatalf("seed upgrade row (%s): %v", rows[i].status, err)
		}
	}
	cleanupClawUpgradeTestRows(t, machines, rcs)

	if err := testHandler.clawUpgradeSweepStalledFleet(ctx); err != nil {
		t.Fatalf("fleet sweep: %v", err)
	}

	// 两个不同批次里卡住的行：都该被收尾，且 detail 是人话（带「原版本未受影响」）。
	for _, machine := range []string{machines[0], machines[1]} {
		up, found, err := testHandler.clawNodeUpgradeByMachine(ctx, machine)
		if err != nil || !found {
			t.Fatalf("read back %s: found=%v err=%v", machine, found, err)
		}
		if up.Status != clawUpgradeStatusFailed || up.ErrorCode != clawUpgradeOfflineErrorCode {
			t.Fatalf("%s: fleet sweep must close a stalled row regardless of batch, got (%q,%q)",
				machine, up.Status, up.ErrorCode)
		}
		if !strings.Contains(up.ErrorDetail, "原版本未受影响") {
			t.Fatalf("%s: device_offline must explain the device still runs the old version, got %q",
				machine, up.ErrorDetail)
		}
	}

	// pending：一条都不能动（离线设备的指令还要靠它领走）。
	pending, _, err := testHandler.clawNodeUpgradeByMachine(ctx, machines[2])
	if err != nil {
		t.Fatalf("read back pending: %v", err)
	}
	if pending.Status != clawUpgradeStatusPending || pending.ErrorCode != "" {
		t.Fatalf("the fleet sweep must never touch a queued instruction, got (%q,%q)",
			pending.Status, pending.ErrorCode)
	}

	// 刚上报过进度的那台（updated_at = now）：不能被判死，哪怕它的批次很旧。
	fresh, _, err := testHandler.clawNodeUpgradeByMachine(ctx, machines[3])
	if err != nil {
		t.Fatalf("read back fresh: %v", err)
	}
	if fresh.Status != clawUpgradeStatusDownloading {
		t.Fatalf("a device that reported progress just now must not be declared offline, got %q", fresh.Status)
	}

	// 反向：全量 sweep 之后，按作用域的那个仍然保留「双空不入库」的守卫 —— 它不该
	// 因为多了个全量版本就顺手变成「空参数也能扫全表」。
	if err := testHandler.clawUpgradeSweepStalled(ctx, "", ""); err != nil {
		t.Fatalf("the scoped sweep must no-op (not error) on empty scope: %v", err)
	}
	fresh, _, err = testHandler.clawNodeUpgradeByMachine(ctx, machines[3])
	if err != nil {
		t.Fatalf("read back fresh again: %v", err)
	}
	if fresh.Status != clawUpgradeStatusDownloading {
		t.Fatalf("an empty-scope scoped sweep must do nothing, got %q", fresh.Status)
	}
}

// TestClawUpgradeSweepFleetReachesAcrossWorkspaces 钉住全量 sweep 的**作用域语义**：
//
// 升级任务是「一台机器一行」（rongcloud_node_upgrade 以 machine_id 为键），与它
// 所属的 workspace 无关。管理端主列表 GET /api/admin/nodes 是**平台级全站视角**，
// 它读的就是整张表，所以它触发的全量 sweep 必须覆盖所有 workspace 里卡住的行 ——
// 否则「另一个租户下那台掉线的设备」在主列表里会永远停在「下载中」。
//
// 期望语义（写在这里，是为了让后来人别把它当成越权 bug 改掉）：
//
//  1. 升级任务一机一行、与 workspace 无关；
//  2. 判死只写 status / error_code / error_detail（外加 updated_at），
//     不改 target_version / batch_id / desired_generation / wave，不移动也不复制行；
//  3. 「跨 workspace」不是数据泄露：sweep 只改升级表的状态字段，各读接口仍按调用方
//     自己的 workspace 鉴权返回内容，这里不返回任何跨租户的载荷。
//
// 反向断言（不能只测「被收尾」）：另一个 workspace 那行被改动时，它**不属于**本
// 测试的 workspace；如果哪天有人给 sweep 加上 `WHERE workspace_id = ...` 收窄，
// 这条会红 —— 那正是不该发生的回归（主列表不是按租户查的）。
func TestClawUpgradeSweepFleetReachesAcrossWorkspaces(t *testing.T) {
	if testPool == nil || testHandler == nil {
		t.Skip("handler test database is not available")
	}
	ctx := context.Background()

	// 造一个**独立的** workspace：故意不是 testWorkspaceID，用来证明全量 sweep 不按
	// 租户收窄。slug 用机器键派生，避免与其它测试撞。
	tag := clawUpgradeTestMachine("upg-xws")
	otherWS := dbfx.Workspace(t, "claw upgrade cross-workspace", tag)

	primary := clawUpgradeTestMachine("upg-xws-primary")
	foreign := clawUpgradeTestMachine("upg-xws-foreign")
	rcPrimary := "rc_node_" + primary
	rcForeign := "rc_node_" + foreign

	insertClawUpgradeTestNode(t, testWorkspaceID, primary, rcPrimary)
	insertClawUpgradeTestNode(t, otherWS, foreign, rcForeign)
	insertClawUpgradeTestReport(t, testWorkspaceID, primary, clawUpgradePlatformWindows, "1.2.0")
	insertClawUpgradeTestReport(t, otherWS, foreign, clawUpgradePlatformWindows, "1.2.0")
	cleanupClawUpgradeTestRows(t, []string{primary, foreign}, []string{rcPrimary, rcForeign})

	// 两行都卡在 downloading 且 20 分钟没动（超过 15 分钟租约）。给它们各自不同的
	// target/batch/代次/波次，用来验证判死**只碰三个状态字段**。
	seedStalled := func(machine, batch string, generation, wave int) {
		t.Helper()
		if _, err := testPool.Exec(ctx, `
			UPDATE rongcloud_node_upgrade
			   SET target_version = '1.3.0', desired_generation = $2, status = $3,
			       error_code = '', error_detail = '',
			       batch_id = $4, wave = $5, requested_at = now(),
			       updated_at = now() - interval '20 minutes'
			 WHERE machine_id = $1`,
			machine, generation, clawUpgradeStatusDownloading, batch, wave); err != nil {
			t.Fatalf("seed stalled row (%s): %v", machine, err)
		}
	}
	seedStalled(primary, "upg-xws-batch-primary", 7, 1)
	seedStalled(foreign, "upg-xws-batch-foreign", 9, 2)

	// 全量 sweep 只带一个 ctx，没有 tenant 参数 —— 这正是要证明的点。
	if err := testHandler.clawUpgradeSweepStalledFleet(ctx); err != nil {
		t.Fatalf("fleet sweep: %v", err)
	}

	// 两个 workspace 的卡住行都要被收尾成 failed / device_offline。
	for _, c := range []struct{ machine, ws string }{
		{primary, testWorkspaceID},
		{foreign, otherWS},
	} {
		up, found, err := testHandler.clawNodeUpgradeByMachine(ctx, c.machine)
		if err != nil || !found {
			t.Fatalf("read back %s (ws %s): found=%v err=%v", c.machine, c.ws, found, err)
		}
		if up.Status != clawUpgradeStatusFailed || up.ErrorCode != clawUpgradeOfflineErrorCode {
			t.Fatalf("%s (ws %s): fleet sweep must close a stalled row regardless of workspace, got (%q,%q)",
				c.machine, c.ws, up.Status, up.ErrorCode)
		}
		if !strings.Contains(up.ErrorDetail, "原版本未受影响") {
			t.Fatalf("%s (ws %s): device_offline must explain the device still runs the old version, got %q",
				c.machine, c.ws, up.ErrorDetail)
		}
		// 行的归属没被搬家：另一个 workspace 的行读完仍在另一个 workspace。
		if up.WorkspaceID != parseUUID(c.ws) {
			t.Fatalf("%s: sweep must not rewrite workspace_id, want %s got %s", c.machine, c.ws, up.WorkspaceID)
		}
	}

	// 「只写三个字段」：另一 workspace 那行的意图字段必须原样保留 —— 判死是给这一行
	// 盖个「设备不在」的章，不是重写这次升级的意图。
	foreignUp, _, err := testHandler.clawNodeUpgradeByMachine(ctx, foreign)
	if err != nil {
		t.Fatalf("read back foreign for intent check: %v", err)
	}
	if foreignUp.TargetVersion != "1.3.0" || foreignUp.DesiredGeneration != 9 ||
		foreignUp.BatchID != "upg-xws-batch-foreign" || foreignUp.Wave != 2 {
		t.Fatalf("sweep must only stamp status/error_code/detail, intent drifted: %+v", foreignUp)
	}
}

