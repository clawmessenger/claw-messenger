package rongcloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// NodeOwnedBy 是 ClawUpdateNode / ClawDeleteNode 的归属判据（owner → node 的
// 融云好友边，与绑定侧 bindAgentsToMachine 写的是同一条边）。两个端点都靠它
// 挡越权：node_id 形如 node_<16hex>，会出现在设备列表响应和前端 URL 里，
// 不能当凭据用。
//
// 这里额外钉住一件事：NodeOwnedBy 不能实现成 NodeClaimants 的薄封装。
// NodeClaimants 把「账号集合 → 节点」的整张映射塞进一个共享缓存槽，用单个
// 账号调一次就会把 admin 设备列表的归属列冲成只含该账号的映射（持续
// claimCacheTTL = 15s）。见 TestNodeOwnedByDoesNotPoisonTheClaimantsCache。

type claimFriendServer struct {
	mu       sync.Mutex
	calls    int
	lastUser string
	// friends 按账号给出好友列表；没列到的账号回空列表。
	friends map[string][]string
	// fail 为真时回一个非 200 的业务错误码，模拟融云侧故障。
	fail bool
}

func (s *claimFriendServer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *claimFriendServer) lastUserID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUser
}

func newClaimFriendServer(t *testing.T, s *claimFriendServer) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.calls++
		s.lastUser = r.PostForm.Get("userId")
		fail := s.fail
		userID := r.PostForm.Get("userId")
		friends := s.friends[userID]
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if fail {
			// postForm 把 code != 200 折成 error，正是调用方需要看到的失败。
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":         500,
				"errorMessage": "friend service unavailable",
			})
			return
		}
		list := make([]map[string]interface{}, 0, len(friends))
		for _, id := range friends {
			list = append(list, map[string]interface{}{"userId": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code":       200,
			"friendList": list,
		})
	}))
}

func newClaimNodeService(srv *httptest.Server) *NodeService {
	return NewNodeService(nil, newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger()), nil, testLogger())
}

func TestNodeOwnedByFollowsTheFriendEdge(t *testing.T) {
	cases := []struct {
		name  string
		owner string
		node  string
		want  bool
	}{
		{"edge present", "user-a", "rc_node_abc_agent", true},
		{"edge missing", "user-a", "rc_node_someone_else", false},
		{"node of another owner", "user-b", "rc_node_abc_agent", false},
		{"empty owner is never ownership", "", "rc_node_abc_agent", false},
		{"empty node id is never ownership", "user-a", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &claimFriendServer{friends: map[string][]string{
				"user-a": {"rc_node_abc_agent", "someone-else"},
				"user-b": {"rc_node_b_agent"},
			}}
			srv := newClaimFriendServer(t, stub)
			defer srv.Close()
			svc := newClaimNodeService(srv)

			owned, err := svc.NodeOwnedBy(context.Background(), tc.owner, db.RongcloudNode{RongcloudUserID: tc.node})
			if err != nil {
				t.Fatalf("NodeOwnedBy: %v", err)
			}
			if owned != tc.want {
				t.Errorf("owned(%q, %q) = %v, want %v", tc.owner, tc.node, owned, tc.want)
			}
			// 空参数不该白跑一次融云调用。
			if tc.owner == "" || tc.node == "" {
				if got := stub.callCount(); got != 0 {
					t.Errorf("RongCloud calls = %d, want 0 for empty input", got)
				}
			} else if got := stub.lastUserID(); got != tc.owner {
				t.Errorf("friend list queried for %q, want %q", got, tc.owner)
			}
		})
	}
}

// 融云挂了必须把错误交给调用方（HTTP 层折成 500），不能悄悄回 false ——
// 那会把一次融云抖动变成「你的设备不存在」。
func TestNodeOwnedByReportsUpstreamFailures(t *testing.T) {
	stub := &claimFriendServer{friends: map[string][]string{"user-a": {"rc_node_abc_agent"}}, fail: true}
	srv := newClaimFriendServer(t, stub)
	defer srv.Close()
	svc := newClaimNodeService(srv)

	owned, err := svc.NodeOwnedBy(context.Background(), "user-a", db.RongcloudNode{RongcloudUserID: "rc_node_abc_agent"})
	if err == nil {
		t.Fatalf("expected an error when RongCloud fails, got owned=%v", owned)
	}
	if owned {
		t.Errorf("a failed lookup must not grant ownership")
	}
}

func TestNodeOwnedByWithoutAClientFails(t *testing.T) {
	svc := NewNodeService(nil, nil, nil, testLogger())
	if _, err := svc.NodeOwnedBy(context.Background(), "user-a", db.RongcloudNode{RongcloudUserID: "rc_node_a"}); err == nil {
		t.Fatal("expected an error without an API client")
	}
}

// 单账号归属查询不得覆盖 NodeClaimants 的共享缓存。
func TestNodeOwnedByDoesNotPoisonTheClaimantsCache(t *testing.T) {
	stub := &claimFriendServer{friends: map[string][]string{
		"user-a": {"rc_node_a1", "rc_node_a2"},
		"user-b": {"rc_node_b1"},
	}}
	srv := newClaimFriendServer(t, stub)
	defer srv.Close()
	svc := newClaimNodeService(srv)
	ctx := context.Background()

	all, err := svc.NodeClaimants(ctx, []string{"user-a", "user-b"})
	if err != nil {
		t.Fatalf("NodeClaimants: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("claimants = %v, want three nodes", all)
	}

	if _, err := svc.NodeOwnedBy(ctx, "user-a", db.RongcloudNode{RongcloudUserID: "rc_node_a1"}); err != nil {
		t.Fatalf("NodeOwnedBy: %v", err)
	}

	// 缓存命中：仍然必须是两个账号的完整映射，而不是 user-a 一个人的。
	again, err := svc.NodeClaimants(ctx, []string{"user-a"})
	if err != nil {
		t.Fatalf("NodeClaimants after NodeOwnedBy: %v", err)
	}
	if len(again) != 3 {
		t.Errorf("claimants after NodeOwnedBy = %v, want the full three-node map", again)
	}
	if again["rc_node_b1"] != "user-b" {
		t.Errorf("lost user-b's node from the claimants cache: %v", again)
	}
}
