package handler

// workspace_delete_machine_upgrade_test.go 钉住 rongcloud_node_upgrade 在
// 工作区拆除时的归属决定：**摘掉 workspace 关联，保留机器状态**。
//
// 为什么不能删行：这张表的主键是 machine_id（见 migrations/601），一台机器解绑后
// 可以重绑到另一个工作区。删工作区时把行删掉，重绑后页面第一帧的「exe 版本」列会
// 全是「未知」，直到设备下一次 10 秒轮询才补回来；而这台机器其实一直在线。
//
// 为什么必须显式置空：601 刻意没有给 workspace_id 建 FK（解绑节点不该连带清版本），
// 所以 CASCADE 不会替我们清理 —— 只声明 workspaceDeleteDetach 而不实现，会留下
// 一行指向已删除工作区的孤儿数据，而 workspace_delete_manifest_test 只看 schema，
// 发现不了。这条用例补的就是「行为与声明一致」。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDeleteWorkspace_DetachesMachineUpgradeStateWithoutDeletingIt(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	slug := "handler-tests-delete-machine-upgrade"
	_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, slug)

	var wsID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description)
		VALUES ($1, $2, $3)
		RETURNING id
	`, "Handler Test Delete Machine Upgrade", slug, "machine upgrade detach test").Scan(&wsID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, wsID)
	})
	// DeleteWorkspace 自己也会校验一次「调用者是 owner」（见 handler 的
	// defense-in-depth），没有这条 member 会直接 404。
	if _, err := testPool.Exec(ctx,
		`INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`,
		wsID, testUserID); err != nil {
		t.Fatalf("create owner member: %v", err)
	}

	// machine_id 全表唯一，用纳秒时间戳避免与其它用例/真实设备行撞键。
	machineID := fmt.Sprintf("handler-test-machine-%d", time.Now().UnixNano())
	if _, err := testPool.Exec(ctx, `
		INSERT INTO rongcloud_node_upgrade (machine_id, workspace_id, reported_version, reported_platform)
		VALUES ($1, $2, '1.3.4', 'linux-x64')
	`, machineID, wsID); err != nil {
		t.Fatalf("insert machine upgrade row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM rongcloud_node_upgrade WHERE machine_id = $1`, machineID)
	})

	w := httptest.NewRecorder()
	req := newRequest("DELETE", "/api/workspaces/"+wsID, nil)
	req = withURLParam(req, "id", wsID)
	testHandler.DeleteWorkspace(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspace: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	var version string
	var workspaceID *string
	if err := testPool.QueryRow(ctx, `
		SELECT reported_version, workspace_id::text
		  FROM rongcloud_node_upgrade
		 WHERE machine_id = $1
	`, machineID).Scan(&version, &workspaceID); err != nil {
		t.Fatalf("machine upgrade row disappeared with the workspace: %v", err)
	}
	if workspaceID != nil {
		t.Errorf("workspace_id = %q, want NULL after teardown", *workspaceID)
	}
	if version != "1.3.4" {
		t.Errorf("reported_version = %q, want the machine's own report to survive", version)
	}
}
