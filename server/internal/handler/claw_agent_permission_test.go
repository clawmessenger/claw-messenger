package handler

// 绑定智能体权限读写的接口用例（task-007 P2）。
//
// 契约（与 web / CLI 逐字对齐）：
//
//	GET  /api/claw/nodes/{nodeId}/permissions  —— 只有 claimed_by_user_id 本人可读
//	PUT  /api/claw/nodes/{nodeId}/permissions  —— 只有 claimed_by_user_id 本人可写
//
// 「谁能读写」这条判定**刻意不看好友边**：好友边正是本次要补的那个缺口（被分享
// 出去的 agent 在好友边视角下也算「我的」，设备列表里 binding_role 甚至恒为
// "owner"）。用例必须能区分这两种「拥有者」。

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/rongcloud"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

// ---- 夹具 ------------------------------------------------------------------

// permissionTestHandler 是 testHandler 的副本外加一个可用的 NodeService。
//
// 用副本而不是改全局：`testHandler` 被整个 handler 包共用，拷一份改字段不会
// 影响别的用例（它们假定 RongCloudNode 为 nil）。
func permissionTestHandler(t *testing.T) *Handler {
	t.Helper()
	if testHandler == nil || testHandler.Queries == nil {
		t.Skip("handler test fixture not initialised (database unavailable)")
	}
	h := *testHandler
	h.RongCloudNode = rongcloud.NewNodeService(testHandler.Queries, nil, nil, nil)
	return &h
}

// seedClawUserRow 插一行 claw_im_users（C 端真人 = 权限的主人）。
//
// claw_im_users 主键是 user_id（TEXT，没有 id 列），所以用 InsertNoID 并显式
// 给出清理条件。插入前先删一次同名行：上一个进程如果中途失败，残留行会让
// user_id / username 的唯一约束把下一轮跑成「假失败」。
func seedClawUserRow(t *testing.T, userID, token string) {
	t.Helper()
	dbfx.Exec(t, `DELETE FROM claw_im_users WHERE user_id = $1 OR username = $2`,
		userID, "perm-"+userID)
	dbfx.InsertNoID(t, "claw_im_users", testutil.Cols{
		"user_id":         userID,
		"username":        "perm-" + userID,
		"nickname":        "Perm Owner",
		"password_hash":   "x",
		"rongcloud_token": token,
		"status":          "active",
	}, "user_id = $1", userID)
}

// seedPermissionNode 插一行 rongcloud_node，返回节点行 id。
func seedPermissionNode(t *testing.T, nodeID, rcUserID, claimedBy, machineID, aiType string) string {
	t.Helper()
	// rongcloud_user_id 有唯一约束：残留行会让重复跑变成假失败，先清。
	dbfx.Exec(t, `DELETE FROM rongcloud_node WHERE rongcloud_user_id = $1`, rcUserID)
	cols := testutil.Cols{
		"workspace_id":      testWorkspaceID,
		"owner_user_id":     testWorkspaceID,
		"rongcloud_user_id": rcUserID,
		"node_id":           nodeID,
		"ai_type":           aiType,
		"capabilities":      testutil.Raw(`'[]'::jsonb`),
		"deploy_status":     "online",
		"machine_id":        machineID,
	}
	if claimedBy != "" {
		cols["claimed_by_user_id"] = claimedBy
	}
	return dbfx.Insert(t, "rongcloud_node", cols)
}

func permissionURL(nodeID string) string {
	return "/api/claw/nodes/" + nodeID + "/permissions"
}

func permissionRequest(method, nodeID, token string, body any) *http.Request {
	req := testutil.JSONRequest(method, permissionURL(nodeID), body)
	req = testutil.WithURLParams(req, "nodeId", nodeID)
	if token != "" {
		req = testutil.WithHeaders(req, "Authorization", "Bearer "+token)
	}
	return req
}

// ---- 读写接口 --------------------------------------------------------------

// TestClawNodePermission_PutRejectsNonOwner 钉住「只有主人可写」。
//
// 节点是**别人**认领的：非主人 PUT 必须 403，且不能落库 —— 只回 403 但库里
// 已经改了，是更隐蔽的一种失败。
func TestClawNodePermission_PutRejectsNonOwner(t *testing.T) {
	h := permissionTestHandler(t)
	nodeID := "perm-node-nonowner-" + strings.ReplaceAll(t.Name(), "/", "-")
	seedClawUserRow(t, "claw-user-intruder", "token-intruder")
	seedClawUserRow(t, "claw-user-realowner", "token-realowner")
	seedPermissionNode(t, nodeID, "rc_"+nodeID, "claw-user-realowner", "", "hermes")

	body := map[string]any{
		"allowPrivate": true, "allowGroup": true, "allowDiscussion": true,
		"allowChatroom": true, "allowFs": true, "behaviorPrompt": "被篡改",
	}
	resp := testutil.Call(t, h.ClawUpdateNodePermission,
		permissionRequest(http.MethodPut, nodeID, "token-intruder", body)).Want(http.StatusForbidden)
	if msg, _ := resp.Map()["message"].(string); !strings.Contains(msg, "主人") {
		t.Fatalf("403 message should explain ownership, got %q", msg)
	}

	// 落库校验：主人读回来必须还是默认值（全关 + 空准则）。
	// 这里用主人的 token 读 —— 非主人现在读也会 403，拿不到可断言的落库快照。
	readBody := testutil.Decode[clawEnvelope](t, h.ClawGetNodePermission,
		permissionRequest(http.MethodGet, nodeID, "token-realowner", nil), http.StatusOK)
	data, _ := readBody.Data.(map[string]any)
	if data["allowGroup"] != false || data["behaviorPrompt"] != "" {
		t.Fatalf("non-owner PUT must not persist: %v", data)
	}
}

// TestClawNodePermission_OwnerCanWriteAndReadBack 是主路径：主人 PUT → 200 且
// 落库；GET 回同样的值，defaultBehaviorPrompt 恒为模板。
func TestClawNodePermission_OwnerCanWriteAndReadBack(t *testing.T) {
	h := permissionTestHandler(t)
	nodeID := "perm-node-owner-" + strings.ReplaceAll(t.Name(), "/", "-")
	seedClawUserRow(t, "claw-user-owner", "token-owner")
	seedPermissionNode(t, nodeID, "rc_"+nodeID, "claw-user-owner", "", "hermes")

	body := map[string]any{
		"allowPrivate": false, "allowGroup": true, "allowDiscussion": true,
		"allowChatroom": false, "allowFs": true, "behaviorPrompt": "只说事实",
	}
	updated := testutil.Decode[clawEnvelope](t, h.ClawUpdateNodePermission,
		permissionRequest(http.MethodPut, nodeID, "token-owner", body), http.StatusOK)
	updData, _ := updated.Data.(map[string]any)
	if updData["allowGroup"] != true || updData["allowChatroom"] != false || updData["behaviorPrompt"] != "只说事实" {
		t.Fatalf("PUT response must echo the stored values, got %v", updData)
	}

	got := testutil.Decode[clawEnvelope](t, h.ClawGetNodePermission,
		permissionRequest(http.MethodGet, nodeID, "token-owner", nil), http.StatusOK)
	data, _ := got.Data.(map[string]any)
	for _, key := range []string{"allowPrivate", "allowGroup", "allowDiscussion", "allowChatroom", "allowFs"} {
		if _, ok := data[key]; !ok {
			t.Fatalf("GET response missing %q: %v", key, data)
		}
	}
	if data["allowGroup"] != true || data["behaviorPrompt"] != "只说事实" {
		t.Fatalf("GET must read back the persisted values, got %v", data)
	}
	if data["defaultBehaviorPrompt"] != rongcloud.DefaultAgentBehaviorPrompt {
		t.Fatalf("defaultBehaviorPrompt must be the server template, got %v", data["defaultBehaviorPrompt"])
	}
}

// TestClawNodePermission_NonOwnerGetIsForbidden 钉住「非主人读不到」。
//
// 只断言状态码不够：泄漏的形态恰恰是「403 但 body 里已经带了正文」，或者
// 「403 但 data 里塞了开关状态」。所以先把主人配的准则写成一句可识别的原文，
// 再逐字确认它没出现在响应体里，字段名也不出现。
func TestClawNodePermission_NonOwnerGetIsForbidden(t *testing.T) {
	h := permissionTestHandler(t)
	nodeID := "perm-node-read-" + strings.ReplaceAll(t.Name(), "/", "-")
	seedClawUserRow(t, "claw-user-reader", "token-reader")
	seedClawUserRow(t, "claw-user-other-owner", "token-other")
	seedPermissionNode(t, nodeID, "rc_"+nodeID, "claw-user-other-owner", "", "hermes")

	// 主人自己配一条非默认值：非默认才能证明「读到的不是零值」这类误判。
	const ownerPrompt = "主人的私有准则-donotshare"
	if resp := testutil.Call(t, h.ClawUpdateNodePermission, permissionRequest(http.MethodPut, nodeID, "token-other",
		map[string]any{"allowPrivate": true, "behaviorPrompt": ownerPrompt})); resp.Code != http.StatusOK {
		t.Fatalf("seed owner prompt: %d %s", resp.Code, resp.Text())
	}

	resp := testutil.Call(t, h.ClawGetNodePermission,
		permissionRequest(http.MethodGet, nodeID, "token-reader", nil)).Want(http.StatusForbidden)

	// 文案与 PUT 同一句，避免前端出现两种 403 提示。
	if msg, _ := resp.Map()["message"].(string); msg != agentPermissionForbiddenMessage {
		t.Fatalf("403 message = %q, want %q", msg, agentPermissionForbiddenMessage)
	}

	body := resp.Text()
	if strings.Contains(body, ownerPrompt) {
		t.Fatalf("403 body leaks the owner's behaviorPrompt: %s", body)
	}
	if strings.Contains(body, rongcloud.DefaultAgentBehaviorPrompt) {
		t.Fatalf("403 body leaks the default template: %s", body)
	}
	for _, key := range []string{
		"behaviorPrompt", "defaultBehaviorPrompt",
		"allowPrivate", "allowGroup", "allowDiscussion", "allowChatroom", "allowFs",
	} {
		if strings.Contains(body, key) {
			t.Fatalf("403 body must not carry %q: %s", key, body)
		}
	}

	// 反向确认：同一行数据用主人的 token 是读得到的 —— 否则上面的 403 可能只是
	// 因为接口坏了，用例会假绿。
	ownerView := testutil.Decode[clawEnvelope](t, h.ClawGetNodePermission,
		permissionRequest(http.MethodGet, nodeID, "token-other", nil), http.StatusOK)
	if data, _ := ownerView.Data.(map[string]any); data["behaviorPrompt"] != ownerPrompt {
		t.Fatalf("owner GET must still read the prompt back, got %v", ownerView.Data)
	}
}

// TestClawNodePermission_EmptyPromptFallsBackToDefault 钉住「传空串 = 恢复默认」：
// 库里存空串，响应里 behaviorPrompt 为空、defaultBehaviorPrompt 仍是模板。
//
// 两个字段必须能区分「没设过」与「设成了和模板一样」—— 前端靠它决定「恢复默认」
// 时是清空输入框还是填模板正文。
func TestClawNodePermission_EmptyPromptFallsBackToDefault(t *testing.T) {
	h := permissionTestHandler(t)
	nodeID := "perm-node-empty-" + strings.ReplaceAll(t.Name(), "/", "-")
	seedClawUserRow(t, "claw-user-empty", "token-empty")
	seedPermissionNode(t, nodeID, "rc_"+nodeID, "claw-user-empty", "", "hermes")

	// 先写一条自定义，再用空白串清掉。
	if resp := testutil.Call(t, h.ClawUpdateNodePermission, permissionRequest(http.MethodPut, nodeID, "token-empty",
		map[string]any{"behaviorPrompt": "先写一条"})); resp.Code != http.StatusOK {
		t.Fatalf("seed prompt: %d %s", resp.Code, resp.Text())
	}
	if resp := testutil.Call(t, h.ClawUpdateNodePermission, permissionRequest(http.MethodPut, nodeID, "token-empty",
		map[string]any{"behaviorPrompt": "   "})); resp.Code != http.StatusOK {
		t.Fatalf("clear prompt: %d %s", resp.Code, resp.Text())
	}

	got := testutil.Decode[clawEnvelope](t, h.ClawGetNodePermission,
		permissionRequest(http.MethodGet, nodeID, "token-empty", nil), http.StatusOK)
	data, _ := got.Data.(map[string]any)
	if data["behaviorPrompt"] != "" {
		t.Fatalf("behaviorPrompt must be the raw stored value (empty), got %v", data["behaviorPrompt"])
	}
	if data["defaultBehaviorPrompt"] != rongcloud.DefaultAgentBehaviorPrompt {
		t.Fatalf("defaultBehaviorPrompt must stay the template, got %v", data["defaultBehaviorPrompt"])
	}
	// 派发侧读到空串时要真的回退到模板（接口与注入点同一口径）。
	if got := rongcloud.EffectiveAgentBehaviorPrompt(data["behaviorPrompt"].(string)); got != rongcloud.DefaultAgentBehaviorPrompt {
		t.Fatalf("dispatch side must fall back to the template, got %q", got)
	}
}

// TestClawNodePermission_RejectsOverlongPrompt 钉住 4000 字写入上限：超长准则
// 会让整条 prompt 顶破 CLI argv 上限，表现为「讨论突然没人说话」。
func TestClawNodePermission_RejectsOverlongPrompt(t *testing.T) {
	h := permissionTestHandler(t)
	nodeID := "perm-node-long-" + strings.ReplaceAll(t.Name(), "/", "-")
	seedClawUserRow(t, "claw-user-long", "token-long")
	seedPermissionNode(t, nodeID, "rc_"+nodeID, "claw-user-long", "", "hermes")

	body := map[string]any{"behaviorPrompt": strings.Repeat("规", 4001)}
	resp := testutil.Call(t, h.ClawUpdateNodePermission,
		permissionRequest(http.MethodPut, nodeID, "token-long", body)).Want(http.StatusBadRequest)
	if !strings.Contains(resp.Text(), "4000") {
		t.Fatalf("message should name the limit: %s", resp.Text())
	}
}

// TestClawNodePermission_MissingNodeIs404 边界：节点不存在时 404，不是 500 ——
// 前端据此区分「没这个节点」与「服务端坏了」。
func TestClawNodePermission_MissingNodeIs404(t *testing.T) {
	h := permissionTestHandler(t)
	seedClawUserRow(t, "claw-user-404", "token-404")
	testutil.Call(t, h.ClawGetNodePermission,
		permissionRequest(http.MethodGet, "perm-node-absent", "token-404", nil)).Want(http.StatusNotFound)
}

// ---- 设备轮询下发 ----------------------------------------------------------

// fixedTestBox 给设备凭据用的固定密钥（32 字节）。
func fixedTestBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

// seedDeviceCredential 写一行设备凭据（密文由调用方用同一把 box 封好）。
func seedDeviceCredential(t *testing.T, machineNodeRowID, credentialID string, sealed []byte) {
	t.Helper()
	// credential_id 唯一：残留行会让重复跑变成假失败。
	dbfx.Exec(t, `DELETE FROM rongcloud_device WHERE credential_id = $1`, credentialID)
	dbfx.Insert(t, "rongcloud_device", testutil.Cols{
		"workspace_id":                testWorkspaceID,
		"owner_user_id":               testWorkspaceID,
		"node_id":                     machineNodeRowID,
		"device_name":                 "perm-device-" + credentialID,
		"device_type":                 "desktop",
		"credential_id":               credentialID,
		"credential_secret_encrypted": base64.StdEncoding.EncodeToString(sealed),
		"status":                      "active",
	})
}

// TestClawDeviceControl_DeliversAgentPermissions 钉住设备轮询下发的 7 个新字段：
// agents[] 里每个 agent 带 5 个 bool + behaviorPrompt（生效值），顶层带 ownerUserId。
//
// 这是私聊闸门（allowPrivate + ownerUserId）与 allowFs 的唯一来源 —— 少下发一项，
// 设备端就退回到二进制默认值（全 false），在用的 agent 会集体不响应。
func TestClawDeviceControl_DeliversAgentPermissions(t *testing.T) {
	h := permissionTestHandler(t)
	box := fixedTestBox(t)
	h.RongCloudNode = rongcloud.NewNodeService(testHandler.Queries, nil, box, nil)

	suffix := strings.ReplaceAll(t.Name(), "/", "-")
	machineID := "perm-machine-" + suffix
	machineNodeID := "perm-machine-node-" + suffix
	agentNodeID := "perm-agent-node-" + suffix
	seedClawUserRow(t, "claw-user-device", "token-device")
	machineNodeRowID := seedPermissionNode(t, machineNodeID, "rc_node_"+machineID, "claw-user-device", machineID, "")
	seedPermissionNode(t, agentNodeID, "rc_"+agentNodeID, "claw-user-device", machineID, "hermes")

	// 设备凭据：密文用同一把 box 封。
	const credentialID = "dc_perm_test"
	secret := "perm-device-secret"
	sealed, err := box.Seal([]byte(secret))
	if err != nil {
		t.Fatalf("seal credential: %v", err)
	}
	seedDeviceCredential(t, machineNodeRowID, credentialID, sealed)

	// 给 agent 写一组非默认权限，确保下发的是库里的值而不是零值。
	if resp := testutil.Call(t, h.ClawUpdateNodePermission, permissionRequest(http.MethodPut, agentNodeID, "token-device",
		map[string]any{
			"allowPrivate": true, "allowGroup": false, "allowDiscussion": true,
			"allowChatroom": false, "allowFs": true, "behaviorPrompt": "设备侧准则",
		})); resp.Code != http.StatusOK {
		t.Fatalf("seed permissions: %d %s", resp.Code, resp.Text())
	}

	req := testutil.JSONRequest(http.MethodPost, "/api/claw/device/control", map[string]any{
		"nodeId": machineNodeID, "credentialId": credentialID, "secret": secret,
	})
	body := testutil.Decode[clawEnvelope](t, h.ClawDeviceControl, req, http.StatusOK)
	data, _ := body.Data.(map[string]any)

	if data["ownerUserId"] != "claw-user-device" {
		t.Fatalf("ownerUserId = %v, want the machine node's claimed_by_user_id", data["ownerUserId"])
	}
	agents, _ := data["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("agents = %v, want exactly one bound agent", data["agents"])
	}
	agent, _ := agents[0].(map[string]any)
	for _, key := range []string{
		"allowPrivate", "allowGroup", "allowDiscussion", "allowChatroom", "allowFs", "behaviorPrompt",
		// claimedBy 是**逐行**认领者：设备端用它判「这条私聊/这条 chat_stop 是不是
		// 这台 agent 的主人发的」。少下发它，多主人同机的设备就会退回设备级口径
		// （A 被当成 B 那台 agent 的主人、B 私聊自己的 agent 反被判陌生人）。
		"claimedBy",
	} {
		if _, ok := agent[key]; !ok {
			t.Fatalf("agents[0] missing %q: %v", key, agent)
		}
	}
	if agent["claimedBy"] != "claw-user-device" {
		t.Fatalf("claimedBy = %v, want this agent's own claimed_by_user_id", agent["claimedBy"])
	}
	if agent["allowPrivate"] != true || agent["allowGroup"] != false ||
		agent["allowDiscussion"] != true || agent["allowChatroom"] != false || agent["allowFs"] != true {
		t.Fatalf("agent permissions do not match the stored row: %v", agent)
	}
	if agent["behaviorPrompt"] != "设备侧准则" {
		t.Fatalf("behaviorPrompt = %v, want the effective stored value", agent["behaviorPrompt"])
	}
}

// TestClawDeviceControl_DefaultPromptWhenUnconfigured 钉住「没配过准则的 agent
// 也拿到模板」：空串下发会让 CLI 侧没有任何行为约束，而这是每轮都要带的。
func TestClawDeviceControl_DefaultPromptWhenUnconfigured(t *testing.T) {
	h := permissionTestHandler(t)
	box := fixedTestBox(t)
	h.RongCloudNode = rongcloud.NewNodeService(testHandler.Queries, nil, box, nil)

	suffix := strings.ReplaceAll(t.Name(), "/", "-")
	machineID := "perm-machine-default-" + suffix
	machineNodeID := "perm-machine-node-default-" + suffix
	agentNodeID := "perm-agent-node-default-" + suffix
	seedClawUserRow(t, "claw-user-device-default", "token-device-default")
	machineNodeRowID := seedPermissionNode(t, machineNodeID, "rc_node_"+machineID, "", machineID, "")
	seedPermissionNode(t, agentNodeID, "rc_"+agentNodeID, "", machineID, "hermes")

	const credentialID = "dc_perm_default"
	secret := "perm-device-secret-default"
	sealed, err := box.Seal([]byte(secret))
	if err != nil {
		t.Fatalf("seal credential: %v", err)
	}
	seedDeviceCredential(t, machineNodeRowID, credentialID, sealed)

	req := testutil.JSONRequest(http.MethodPost, "/api/claw/device/control", map[string]any{
		"nodeId": machineNodeID, "credentialId": credentialID, "secret": secret,
	})
	body := testutil.Decode[clawEnvelope](t, h.ClawDeviceControl, req, http.StatusOK)
	data, _ := body.Data.(map[string]any)

	// 未认领 → ownerUserId 空串（CLI 对空串按「不是主人」处理，私聊闸门默认关）。
	if data["ownerUserId"] != "" {
		t.Fatalf("unclaimed machine must deliver an empty ownerUserId, got %v", data["ownerUserId"])
	}
	agents, _ := data["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("agents = %v, want exactly one", data["agents"])
	}
	agent, _ := agents[0].(map[string]any)
	if agent["behaviorPrompt"] != rongcloud.DefaultAgentBehaviorPrompt {
		t.Fatalf("unconfigured agent must get the default template, got %v", agent["behaviorPrompt"])
	}
	if agent["allowPrivate"] != false || agent["allowFs"] != false {
		t.Fatalf("unconfigured agent must keep fail-closed defaults: %v", agent)
	}
}

// TestClawDeviceControl_DeliversSystemNodeID 钉住顶层的 systemNodeId。
//
// 设备端靠它区分「服务端让我干活」与「某个融云账号伪造了一条 command」：
// 服务端派发的 your_turn / recommend_roles 全部以**系统账号**身份私发
// （registration.go 里 sendPrivateMessage(systemNodeID, …)），而 chat_stop /
// card_action 是用户用自己的账号发的。这个字段不下发，command 闸门只能保持
// 放行（fail-open），伪造 your_turn 把那句 role_prompt 喂进 prompt 的路就还开着。
func TestClawDeviceControl_DeliversSystemNodeID(t *testing.T) {
	h := permissionTestHandler(t)
	box := fixedTestBox(t)
	h.RongCloudNode = rongcloud.NewNodeService(testHandler.Queries, nil, box, nil)
	// 生产里由 channel 工厂经 ChannelDeps.OnCredentials 回填；测试直接注入。
	h.RongCloudSystemNodeID = "rc-system-node"

	suffix := strings.ReplaceAll(t.Name(), "/", "-")
	machineID := "sys-machine-" + suffix
	machineNodeID := "sys-machine-node-" + suffix
	seedClawUserRow(t, "claw-user-device", "token-device")
	machineNodeRowID := seedPermissionNode(t, machineNodeID, "rc_node_"+machineID, "claw-user-device", machineID, "")

	const credentialID = "dc_sys_test"
	secret := "sys-device-secret"
	sealed, err := box.Seal([]byte(secret))
	if err != nil {
		t.Fatalf("seal credential: %v", err)
	}
	seedDeviceCredential(t, machineNodeRowID, credentialID, sealed)

	req := testutil.JSONRequest(http.MethodPost, "/api/claw/device/control", map[string]any{
		"nodeId": machineNodeID, "credentialId": credentialID, "secret": secret,
	})
	body := testutil.Decode[clawEnvelope](t, h.ClawDeviceControl, req, http.StatusOK)
	data, _ := body.Data.(map[string]any)

	if data["systemNodeId"] != "rc-system-node" {
		t.Fatalf("systemNodeId = %v, want the channel's system node id", data["systemNodeId"])
	}

	// 集成未装配时下发空串：设备端据此保持放行 —— 「不知道」不等于「可以伪造」，
	// 但也绝不能因为服务端没配好就把所有设备的讨论组掐死。这条是刻意的。
	h.RongCloudSystemNodeID = ""
	// 必须重新构造 request：体是一次性 reader，复用会退化成「缺少设备凭据」。
	req2 := testutil.JSONRequest(http.MethodPost, "/api/claw/device/control", map[string]any{
		"nodeId": machineNodeID, "credentialId": credentialID, "secret": secret,
	})
	body = testutil.Decode[clawEnvelope](t, h.ClawDeviceControl, req2, http.StatusOK)
	data, _ = body.Data.(map[string]any)
	if data["systemNodeId"] != "" {
		t.Fatalf("systemNodeId = %v, want empty when the integration is not wired", data["systemNodeId"])
	}
}
