package rongcloud

// friend_delete_test.go 钉住 friend/delete 的两个容易漏掉的副作用：
//
//  1. **反向边也要删**。建边（绑定流程、approveRequest）是逐方向建的，删好友只删
//     自己那条会留下「对方好友列表里仍然有你」。客户端 SDK 的兜底路径用的是
//     DirectionType.BOTH，服务端语义必须与它一致 —— 否则同一个「删除好友」在
//     「服务端可用 / 只能走 SDK」两种情况下结果不同，而且差异只在对方那边可见。
//  2. **归属投影要清**。AI 节点的 owner 是融云好友边的反查结果
//     (rongcloud_node.claimed_by_user_id)，删了边却不清列，getDeviceOwner 会一直
//     答一个已经不是好友的账号，别人加这台设备会被导向错误的人。

import (
	"context"
	"strings"
	"testing"
)

func TestFriendDeleteRemovesBothDirectionsAndReleasesTheNodeClaim(t *testing.T) {
	rec, srv := newFriendEdgeServer(t)
	defer srv.Close()
	store, _ := newFriendRequestMemory()
	handler := newFriendRequestHandler(t, srv, store, fixedOwner(testOwnerUserID))

	released := make([]string, 0, 1)
	handler.friendStore.ReleaseNodeClaim = func(_ context.Context, nodeRCID, ownerRCID string) error {
		released = append(released, nodeRCID+"|"+ownerRCID)
		return nil
	}

	sendWebCommand(t, handler, testRequesterUserID, "friend", "delete", map[string]interface{}{
		"friendId": testNodeRCID,
	})

	reply := rec.last(t)
	if reply["status"] != "success" {
		t.Fatalf("friend delete should succeed, got %#v", reply)
	}
	want := []string{
		testRequesterUserID + "->" + testNodeRCID,
		testNodeRCID + "->" + testRequesterUserID,
	}
	if got := rec.recordedRemovals(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("removed edges = %v\nwant           %v（两个方向都要删，与 SDK 的 BOTH 一致）", got, want)
	}
	if strings.Join(released, ",") != testNodeRCID+"|"+testRequesterUserID {
		t.Errorf("released claims = %v, want the caller's own claim on %s", released, testNodeRCID)
	}
}
