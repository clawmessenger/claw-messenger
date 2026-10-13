package rongcloud

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// presenceServer 造一个只回答 /user/checkOnline.json 的融云假服务。
// status 为 "" 时返回融云错误码，用来验证 presence 查询失败的行为。
func presenceServer(t *testing.T, status string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/checkOnline.json" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if status == "" {
			fmt.Fprint(w, `{"code":401,"errorMessage":"signature failed"}`)
			return
		}
		fmt.Fprintf(w, `{"code":200,"status":%q}`, status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestNodeIsOnlineFallsBackToColumnWithoutClient 钉住无融云凭据时的回落行为，
// 单测与未装配融云的部署仍按历史口径（读列）。
func TestNodeIsOnlineFallsBackToColumnWithoutClient(t *testing.T) {
	onlineNode := db.RongcloudNode{DeployStatus: "online", RongcloudUserID: "rc_x"}
	offlineNode := db.RongcloudNode{DeployStatus: "offline", RongcloudUserID: "rc_x"}

	if !nodeIsOnline(context.Background(), nil, onlineNode) {
		t.Error("nil client + deploy_status=online should report online")
	}
	if nodeIsOnline(context.Background(), nil, offlineNode) {
		t.Error("nil client + deploy_status=offline should report offline")
	}
	// 没有融云 id 时同样回落读列（重名/未注册行）。
	bare := db.RongcloudNode{DeployStatus: "online"}
	srv := presenceServer(t, "1")
	client := newRongCloudAPIClient("k", "s", srv.URL, srv.Client(), testLogger())
	if !nodeIsOnline(context.Background(), client, bare) {
		t.Error("node without rongcloud id should fall back to the column")
	}
}

// TestNodeIsOnlineUsesRongCloudPresence 是本文件的核心回归：融云 presence 才是
// 在线判定的权威信号，deploy_status 只是缓存 —— 它说 offline 但融云说在线时，
// 必须判为在线（旧实现读列，导致主持节点永远离线）。
func TestNodeIsOnlineUsesRongCloudPresence(t *testing.T) {
	// 列说 offline，融云说在线 → 在线。
	srvOn := presenceServer(t, "1")
	clientOn := newRongCloudAPIClient("k", "s", srvOn.URL, srvOn.Client(), testLogger())
	staleOffline := db.RongcloudNode{DeployStatus: "offline", RongcloudUserID: "host_rc"}
	if !nodeIsOnline(context.Background(), clientOn, staleOffline) {
		t.Error("presence=1 must win over a stale deploy_status=offline")
	}

	// 列说 online，融云说离线 → 离线。
	srvOff := presenceServer(t, "0")
	clientOff := newRongCloudAPIClient("k", "s", srvOff.URL, srvOff.Client(), testLogger())
	staleOnline := db.RongcloudNode{DeployStatus: "online", RongcloudUserID: "host_rc"}
	if nodeIsOnline(context.Background(), clientOff, staleOnline) {
		t.Error("presence=0 must win over a stale deploy_status=online")
	}
}

// TestNodeIsOnlineFailsClosedOnPresenceError presence 查询失败按离线处理：
// 命令通道发不出去，谎报在线只会让流程在更靠后的地方失败。
func TestNodeIsOnlineFailsClosedOnPresenceError(t *testing.T) {
	srv := presenceServer(t, "")
	client := newRongCloudAPIClient("k", "s", srv.URL, srv.Client(), testLogger())
	node := db.RongcloudNode{DeployStatus: "online", RongcloudUserID: "host_rc"}
	if nodeIsOnline(context.Background(), client, node) {
		t.Error("presence lookup failure should fail closed (offline)")
	}
}

// seedHostNode 已被 seedNode 取代（见 chatroom_commands_test.go）。

// TestResolveHostUsesPresenceNotStaleColumn 端到端钉住 resolveHost：节点行
// deploy_status=offline（Go 侧无人维护该列），但融云 presence 在线时，宿主必须
// 解析成功，并且顺带把列刷成 online（原版 _sync_cached_presence 的语义）。
func TestResolveHostUsesPresenceNotStaleColumn(t *testing.T) {
	pool := actorTestPool(t)
	queries := db.New(pool)
	wsID := seedWorkspaceRow(t, pool)
	nodeID := "host-node-" + uniqueID("t")
	node := seedNode(t, queries, wsID, nodeID,
		`["discussion_host","discussion_roundtable","discussion_model_routing","discussion_role_recommendation"]`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM rongcloud_node WHERE id = $1`, node.ID)
	})
	// seedNode 写死 deploy_status=online；这里显式压成 offline，模拟 Go 侧
	// 真实库里那一列从来没人维护的状态（线上 12 个节点全是 offline）。
	if _, err := queries.UpdateRongCloudNodeDeployStatus(context.Background(),
		db.UpdateRongCloudNodeDeployStatusParams{ID: node.ID, DeployStatus: "offline"}); err != nil {
		t.Fatalf("force deploy_status=offline: %v", err)
	}
	node.DeployStatus = "offline"

	srv := presenceServer(t, "1")
	client := newRongCloudAPIClient("k", "s", srv.URL, srv.Client(), testLogger())

	svc := newRoundtableCreationService(queries, client)
	svc.hostLoader = func(context.Context) (DiscussionSystemHostConfig, bool, error) {
		return DiscussionSystemHostConfig{
			NodeID: node.ID.String(), DisplayName: "主持人", Enabled: true, ConfigVersion: "1",
		}, true, nil
	}

	host, hostErr := svc.resolveHost(context.Background())
	if hostErr != "" {
		t.Fatalf("resolveHost = %q, want success (presence says the node is online)", hostErr)
	}
	if host.nodeID != nodeID {
		t.Errorf("host.nodeID = %q, want %q", host.nodeID, nodeID)
	}
	if host.aiUserID != node.RongcloudUserID {
		t.Errorf("host.aiUserID = %q, want %q", host.aiUserID, node.RongcloudUserID)
	}

	refreshed, err := queries.GetRongCloudNodeByID(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("re-read node: %v", err)
	}
	if refreshed.DeployStatus != "online" {
		t.Errorf("deploy_status cache = %q, want online (presence snapshotted back)", refreshed.DeployStatus)
	}
}

// TestResolveHostRejectsPresenceOffline 融云侧离线时仍然拒绝，且缓存同步为 offline。
func TestResolveHostRejectsPresenceOffline(t *testing.T) {
	pool := actorTestPool(t)
	queries := db.New(pool)
	wsID := seedWorkspaceRow(t, pool)
	nodeID := "host-node-" + uniqueID("t")
	node := seedNode(t, queries, wsID, nodeID,
		`["discussion_host","discussion_roundtable","discussion_model_routing","discussion_role_recommendation"]`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM rongcloud_node WHERE id = $1`, node.ID)
	})

	srv := presenceServer(t, "0")
	client := newRongCloudAPIClient("k", "s", srv.URL, srv.Client(), testLogger())

	svc := newRoundtableCreationService(queries, client)
	svc.hostLoader = func(context.Context) (DiscussionSystemHostConfig, bool, error) {
		return DiscussionSystemHostConfig{
			NodeID: node.ID.String(), DisplayName: "主持人", Enabled: true, ConfigVersion: "1",
		}, true, nil
	}

	if _, hostErr := svc.resolveHost(context.Background()); hostErr != "SYSTEM_HOST_OFFLINE" {
		t.Fatalf("resolveHost = %q, want SYSTEM_HOST_OFFLINE", hostErr)
	}
}
