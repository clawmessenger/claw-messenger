package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// climax_node_ownership_test.go 钉住 ClawUpdateNode / ClawDeleteNode 的归属校验。
//
// 修复前：这两个端点只按 node_id 取行，取到就改/删，userID 只喂给
// RemoveFriends。而 node_id 形如 node_<16hex>（node_service.go 的 Register），
// 会出现在设备列表响应和前端 URL 里 —— 任意已登录用户拿到别人的 node_id
// 就能改设备名、甚至注销别人的 agent 融云账号（unregister=true 不可恢复）。
//
// 归属口径不是新造的字段：绑定侧写的是 owner → node 的融云好友边
// （node_service.go 的 bindAgentsToMachine），设备列表
// （LegacyNodeRecords）和 admin 归属列（NodeClaimants）都按它反查；
// rongcloud_node.owner_user_id 指的是 workspace 管理者，不是绑定的 claw
// 用户，拿它判归属会把同 workspace 的用户全放进来。
//
// 越权必须回与「节点不存在」同码同文案的 404：403/401 会让错误码差异成为
// node_id 存在性探针。

type clawNodeDocFakeDB struct {
	// tokens 是 claw_im_users.rongcloud_token → user_id。
	tokens map[string]string
}

func (f *clawNodeDocFakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if !strings.Contains(sql, "FROM claw_im_users") || !strings.Contains(sql, "rongcloud_token") {
		return &groupStubRow{err: errors.New("unexpected query: " + sql)}
	}
	token, _ := args[0].(string)
	if userID, ok := f.tokens[token]; ok {
		return &groupStubRow{values: []any{userID}}
	}
	return &groupStubRow{err: pgx.ErrNoRows}
}

func (f *clawNodeDocFakeDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("not stubbed")
}

func (f *clawNodeDocFakeDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("not stubbed")
}

// stubClawNodeOps 记录每一个被碰到的方法，好让「越权时一个写方法都没调用」
// 成为可断言的结论而不是阅读代码的推断。
type stubClawNodeOps struct {
	node db.RongcloudNode
	// ownedBy 是「这个 claw 用户是否拥有该节点」的答案。
	ownedBy map[string]bool

	lookupErr    error
	ownershipErr error

	ownershipChecks []string
	userLookups     int
	profileUpdates  []string
	unbinds         []string
	unregisters     []string
}

func (s *stubClawNodeOps) GetNodeByNodeID(_ context.Context, nodeIDText string) (db.RongcloudNode, error) {
	if s.lookupErr != nil {
		return db.RongcloudNode{}, s.lookupErr
	}
	node := s.node
	node.NodeID = nodeIDText
	return node, nil
}

func (s *stubClawNodeOps) NodeOwnedBy(_ context.Context, ownerRCUserID string, node db.RongcloudNode) (bool, error) {
	s.ownershipChecks = append(s.ownershipChecks, ownerRCUserID+"→"+node.RongcloudUserID)
	if s.ownershipErr != nil {
		return false, s.ownershipErr
	}
	return s.ownedBy[ownerRCUserID], nil
}

func (s *stubClawNodeOps) GetUserByRongCloudID(_ context.Context, rcUserID string) (db.RongcloudUser, error) {
	s.userLookups++
	return db.RongcloudUser{RongcloudUserID: rcUserID}, nil
}

func (s *stubClawNodeOps) UpdateUserProfile(_ context.Context, _ pgtype.UUID, name, _ string) error {
	s.profileUpdates = append(s.profileUpdates, name)
	return nil
}

func (s *stubClawNodeOps) UnbindNode(_ context.Context, node db.RongcloudNode, ownerRongCloudID string) error {
	s.unbinds = append(s.unbinds, ownerRongCloudID+"→"+node.NodeID)
	return nil
}

func (s *stubClawNodeOps) UnregisterNode(_ context.Context, node db.RongcloudNode, ownerRongCloudID string) error {
	s.unregisters = append(s.unregisters, ownerRongCloudID+"→"+node.NodeID)
	return nil
}

func newClawNodeHandler(ops *stubClawNodeOps) *Handler {
	return &Handler{
		DB: &clawNodeDocFakeDB{tokens: map[string]string{
			"token-a": "user-a",
			"token-b": "user-b",
		}},
		clawNodeOpsForTest: ops,
	}
}

// newClawNodeRequest 造一个带 chi 路由参数的请求（/api/claw/nodes/{nodeId}）。
func newClawNodeRequest(t *testing.T, method, nodeID, token, query string) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	target := "/api/claw/nodes/" + nodeID
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(method, target, strings.NewReader(`{"name":"新名字","portraitUri":""}`))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("nodeId", nodeID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	return req, httptest.NewRecorder()
}

func clawReplyCode(t *testing.T, rr *httptest.ResponseRecorder) (int, string) {
	t.Helper()
	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rr.Body.String(), err)
	}
	return body.Code, body.Message
}

// victimNode 是「别人的节点」：属于 user-b。
func victimNode() *stubClawNodeOps {
	return &stubClawNodeOps{
		node:    db.RongcloudNode{RongcloudUserID: "rc_node_victim_agent"},
		ownedBy: map[string]bool{"user-b": true},
	}
}

func TestClawDeleteNodeRejectsANodeOwnedBySomebodyElse(t *testing.T) {
	ops := victimNode()
	h := newClawNodeHandler(ops)
	req, rr := newClawNodeRequest(t, http.MethodDelete, "node_0123456789abcdef", "token-a", "")

	h.ClawDeleteNode(rr, req)

	code, message := clawReplyCode(t, rr)
	if rr.Code != 404 || code != 404 {
		t.Fatalf("status/code = %d/%d, want 404/404 (body=%s)", rr.Code, code, rr.Body.String())
	}
	// 与「节点不存在」同文案：能区分就等于给了 node_id 存在性探针。
	if message != "节点不存在" {
		t.Errorf("message = %q, want the same text as a missing node", message)
	}
	if len(ops.unbinds) != 0 {
		t.Errorf("UnbindNode must not run for a non-owner, got %v", ops.unbinds)
	}
	if len(ops.unregisters) != 0 {
		t.Errorf("UnregisterNode must not run for a non-owner, got %v", ops.unregisters)
	}
	if len(ops.ownershipChecks) != 1 || ops.ownershipChecks[0] != "user-a→rc_node_victim_agent" {
		t.Errorf("ownership check = %v, want the caller checked against the node", ops.ownershipChecks)
	}
}

// ?unregister=true 是更危险的分支（融云账号永久注销），归属校验不能只加在
// 缺省分支上 —— 否则换个参数就绕过去了。
func TestClawDeleteNodeUnregisterAlsoRequiresOwnership(t *testing.T) {
	ops := victimNode()
	h := newClawNodeHandler(ops)
	req, rr := newClawNodeRequest(t, http.MethodDelete, "node_0123456789abcdef", "token-a", "unregister=true")

	h.ClawDeleteNode(rr, req)

	code, message := clawReplyCode(t, rr)
	if rr.Code != 404 || code != 404 || message != "节点不存在" {
		t.Fatalf("status/code/message = %d/%d/%q, want 404/404/节点不存在", rr.Code, code, message)
	}
	if len(ops.unregisters) != 0 {
		t.Errorf("UnregisterNode must not run for a non-owner, got %v", ops.unregisters)
	}
	if len(ops.unbinds) != 0 {
		t.Errorf("UnbindNode must not run for a non-owner, got %v", ops.unbinds)
	}
}

func TestClawUpdateNodeRejectsANodeOwnedBySomebodyElse(t *testing.T) {
	ops := victimNode()
	h := newClawNodeHandler(ops)
	req, rr := newClawNodeRequest(t, http.MethodPost, "node_0123456789abcdef", "token-a", "")

	h.ClawUpdateNode(rr, req)

	code, message := clawReplyCode(t, rr)
	if rr.Code != 404 || code != 404 || message != "节点不存在" {
		t.Fatalf("status/code/message = %d/%d/%q, want 404/404/节点不存在", rr.Code, code, message)
	}
	if len(ops.profileUpdates) != 0 {
		t.Errorf("UpdateUserProfile must not run for a non-owner, got %v", ops.profileUpdates)
	}
	if ops.userLookups != 0 {
		t.Errorf("the node's rc user must not even be looked up for a non-owner, got %d lookups", ops.userLookups)
	}
}

// 兼容性：owner 删自己的节点必须照旧 200，且仍然走 UnbindNode（它内部才是
// 删行 + 删融云好友边，见 node_service.go 的 UnbindNode）。
func TestClawDeleteNodeStillUnbindsTheOwnersNode(t *testing.T) {
	ops := &stubClawNodeOps{
		node:    db.RongcloudNode{RongcloudUserID: "rc_node_a_agent"},
		ownedBy: map[string]bool{"user-a": true},
	}
	h := newClawNodeHandler(ops)
	req, rr := newClawNodeRequest(t, http.MethodDelete, "node_0123456789abcdef", "token-a", "")

	h.ClawDeleteNode(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if len(ops.unbinds) != 1 || ops.unbinds[0] != "user-a→node_0123456789abcdef" {
		t.Errorf("UnbindNode calls = %v, want one for user-a", ops.unbinds)
	}
	if len(ops.unregisters) != 0 {
		t.Errorf("plain delete must not unregister, got %v", ops.unregisters)
	}
}

func TestClawDeleteNodeUnregistersTheOwnersNodeOnRequest(t *testing.T) {
	ops := &stubClawNodeOps{
		node:    db.RongcloudNode{RongcloudUserID: "rc_node_a_agent"},
		ownedBy: map[string]bool{"user-a": true},
	}
	h := newClawNodeHandler(ops)
	req, rr := newClawNodeRequest(t, http.MethodDelete, "node_0123456789abcdef", "token-a", "unregister=true")

	h.ClawDeleteNode(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if len(ops.unregisters) != 1 {
		t.Errorf("UnregisterNode calls = %v, want one", ops.unregisters)
	}
	if len(ops.unbinds) != 0 {
		t.Errorf("unregister implies unbind inside the service, the handler must not double-call it: %v", ops.unbinds)
	}
}

func TestClawUpdateNodeStillRenamesTheOwnersNode(t *testing.T) {
	ops := &stubClawNodeOps{
		node:    db.RongcloudNode{RongcloudUserID: "rc_node_a_agent"},
		ownedBy: map[string]bool{"user-a": true},
	}
	h := newClawNodeHandler(ops)
	req, rr := newClawNodeRequest(t, http.MethodPost, "node_0123456789abcdef", "token-a", "")

	h.ClawUpdateNode(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if len(ops.profileUpdates) != 1 || ops.profileUpdates[0] != "新名字" {
		t.Errorf("UpdateUserProfile calls = %v, want the new name", ops.profileUpdates)
	}
}

// 归属查不出来（融云抖动）时不能当「不存在」也不能当「有权限」：回 500，
// 用户重试即可。降级成 404 会把一次上游故障说成「你的设备没了」。
func TestClawNodeWriteFailsClosedOnOwnershipLookupErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(h *Handler, rr *httptest.ResponseRecorder, req *http.Request)
	}{
		{"delete", func(h *Handler, rr *httptest.ResponseRecorder, req *http.Request) { h.ClawDeleteNode(rr, req) }},
		{"update", func(h *Handler, rr *httptest.ResponseRecorder, req *http.Request) { h.ClawUpdateNode(rr, req) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := &stubClawNodeOps{
				node:         db.RongcloudNode{RongcloudUserID: "rc_node_a_agent"},
				ownedBy:      map[string]bool{"user-a": true},
				ownershipErr: errors.New("rongcloud: API error code 500"),
			}
			h := newClawNodeHandler(ops)
			req, rr := newClawNodeRequest(t, http.MethodPost, "node_0123456789abcdef", "token-a", "")
			tc.call(h, rr, req)

			if rr.Code != 500 {
				t.Fatalf("status = %d, want 500 (body=%s)", rr.Code, rr.Body.String())
			}
			if len(ops.unbinds)+len(ops.unregisters)+len(ops.profileUpdates) != 0 {
				t.Errorf("no write may run when ownership could not be resolved")
			}
		})
	}
}

// 未登录（没有 Bearer）必须在查节点之前就被挡掉 —— 归属校验不能成为唯一
// 的鉴权。
func TestClawNodeWriteRequiresABearerToken(t *testing.T) {
	ops := victimNode()
	h := newClawNodeHandler(ops)
	req, rr := newClawNodeRequest(t, http.MethodDelete, "node_0123456789abcdef", "", "")

	h.ClawDeleteNode(rr, req)

	if rr.Code != 401 {
		t.Fatalf("status = %d, want 401 (body=%s)", rr.Code, rr.Body.String())
	}
	if len(ops.ownershipChecks) != 0 {
		t.Errorf("an unauthenticated request must not reach the ownership check")
	}
}

// 编译期护栏：生产路径的 *rongcloud.NodeService 必须满足 clawNodeOps，
// 否则 ClawDeleteNode 里那句 return h.RongCloudNode 会静默变成 nil 接口。
var _ clawNodeOps = (*rongcloud.NodeService)(nil)
