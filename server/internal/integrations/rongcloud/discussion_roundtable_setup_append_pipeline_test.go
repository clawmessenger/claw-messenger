package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 续推（append）的**管线级**回归：不只验 appendCandidateNodes 这个纯函数，
// 而是跑完 recommendForSetupBatch 的整条链路（候选 → broker 往返 → 角色绑设备 →
// 落库/追加 → 落 setup_prompt 事件），再从两处读回真实状态：
//
//  1. 事件里的 `recommendationId`：续推必须保持不变（变了就等于「换一批」，
//     用户已添加角色的 role_id 当场失效）；
//  2. 库里的 `roles` 数组：续推必须是**追加**（上一批仍在）。
//
// 以及本次改动最可能引入的回归 —— 不排除历史设备后，新角色会不会被分到
// 「用户已添加角色占用的那台设备」上。一旦会，confirm 时
// `parseSetupConfirmRoles` 的 nodes 判重会以 ROLE_DEVICE_DUPLICATED 整单拒绝，
// 而用户看到的只是一句笼统的 400。
//
// 结论性断言 `assertRolesAssignable` 直接调 parseSetupConfirmRoles：
// 把「轮 1 采纳的那个角色 + 轮 2 全部角色」拼成 confirm 的入参，必须能过。
// 这就是用户点「开始讨论」那一刻服务端做的事，比只比较 node_id 字符串更贴近真实。

// respondRolesSender 造一个「收到推荐请求就当场回包」的发送端。
//
// broker 的 send 是在释放互斥锁**之后**才调用的，所以这里同步调 AcceptResponse
// 不会自锁：settle 会 close(done)，随后 AwaitRecommendation 立即返回。
//
// roles 回调按 (requestID, maxRoles) 造本批角色；notify 用来观测「第几轮」。
func respondRolesSender(t *testing.T, broker *NodeRequestBroker, roles func(round int, maxRoles int) []map[string]interface{}) NodeRequestSender {
	t.Helper()
	round := 0
	return func(_ context.Context, toUserID, content string) error {
		round++
		var envelope struct {
			RequestID string                 `json:"request_id"`
			Params    map[string]interface{} `json:"params"`
		}
		if err := json.Unmarshal([]byte(content), &envelope); err != nil {
			t.Errorf("推荐请求不是合法 JSON: %v", err)
			return err
		}
		maxRoles := 0
		if value, ok := envelope.Params["max_roles"].(float64); ok {
			maxRoles = int(value)
		}
		reply, err := json.Marshal(map[string]interface{}{
			"request_id": envelope.RequestID,
			"msg_type":   "command_result",
			"payload":    map[string]interface{}{"roles": roles(round, maxRoles)},
		})
		if err != nil {
			t.Errorf("拼装应答失败: %v", err)
			return err
		}
		// senderID 必须是 host 的融云 id，否则 broker 会以「回包来源不符」拒收。
		if !broker.AcceptResponse(toUserID, string(reply)) {
			t.Errorf("broker 未认领这次应答（request_id=%s）", envelope.RequestID)
		}
		return nil
	}
}

// rolesNamed 造 n 个互不同名、speaking_order 为 0..n-1 的角色（不带 node_id：
// 让服务端 assignRoleNodes 自己分配，与线上 Go 侧现状一致）。
func rolesNamed(prefix string, n int) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, n)
	for index := 0; index < n; index++ {
		out = append(out, map[string]interface{}{
			"role_name":      fmt.Sprintf("%s-%d", prefix, index+1),
			"role_prompt":    fmt.Sprintf("第 %d 个角色", index+1),
			"speaking_order": index,
		})
	}
	return out
}

// lastRolesPrompt 取最近一条 setup_prompt(roles) 事件，解出 payload。
func (f *setupFixture) lastRolesPrompt(t *testing.T, discussionID string) map[string]interface{} {
	t.Helper()
	events, err := f.rt.repo.ListEventsAfter(context.Background(), discussionID, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].EventType != setupEventPrompt {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(events[index].Payload, &payload); err != nil {
			continue
		}
		if step, _ := payload["step"].(string); step == setupStepRoles {
			return payload
		}
	}
	t.Fatalf("没有 setup_prompt(roles) 事件；当前事件: %v", f.eventTypes(t, discussionID))
	return nil
}

// payloadRoleNodeIDs 取出事件里本批角色绑定的设备（保持顺序）。
func payloadRoleNodeIDs(t *testing.T, payload map[string]interface{}) []string {
	t.Helper()
	raw, ok := payload["roles"].([]interface{})
	if !ok {
		t.Fatalf("事件里没有 roles 数组: %#v", payload)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		role, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("roles 项不是对象: %#v", item)
		}
		nodeID, _ := role["nodeId"].(string)
		if nodeID == "" {
			t.Fatalf("角色没有 nodeId: %#v", role)
		}
		out = append(out, nodeID)
	}
	return out
}

// payloadRoleIDs 取出本批角色的 role_id（confirm 只认这个集合）。
func payloadRoleIDs(t *testing.T, payload map[string]interface{}) []string {
	t.Helper()
	raw, ok := payload["roles"].([]interface{})
	if !ok {
		t.Fatalf("事件里没有 roles 数组: %#v", payload)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		role, _ := item.(map[string]interface{})
		roleID, _ := role["role_id"].(string)
		if roleID == "" {
			t.Fatalf("角色没有 role_id: %#v", role)
		}
		out = append(out, roleID)
	}
	return out
}

// assertRolesAssignable 用 confirm 的真实解析器校验「用户已添加 + 本批」能过。
//
// 这是本次改动的**用户可见后果**：设备撞车不会在推荐阶段报错，只会在
// 用户点「开始讨论」时整单 400 `ROLE_DEVICE_DUPLICATED`。
func assertRolesAssignable(t *testing.T, added []map[string]interface{}, batch []map[string]interface{}) {
	t.Helper()
	merged := make([]interface{}, 0, len(added)+len(batch))
	for _, role := range added {
		merged = append(merged, role)
	}
	for _, role := range batch {
		nodeID, _ := role["nodeId"].(string)
		roleID, _ := role["role_id"].(string)
		name, _ := role["roleName"].(string)
		prompt, _ := role["roleInstructions"].(string)
		merged = append(merged, map[string]interface{}{
			"role_id": roleID, "role_name": name, "role_prompt": prompt, "node_id": nodeID,
		})
	}
	if _, err := parseSetupConfirmRoles(CommandContent{Params: map[string]interface{}{"roles": merged}}); err != nil {
		t.Fatalf("confirm 会被整单拒绝（%s）：用户已添加 %d 个 + 本批 %d 个角色"+
			"解不出合法入参 —— 说明新角色被分到了已被占用的设备上",
			err.Code, len(added), len(batch))
	}
}

// eventRoles 把事件里的角色转成 confirm 入参形状。
func eventRoles(t *testing.T, payload map[string]interface{}) []map[string]interface{} {
	t.Helper()
	raw, _ := payload["roles"].([]interface{})
	out := make([]map[string]interface{}, 0, len(raw))
	for _, item := range raw {
		role, _ := item.(map[string]interface{})
		out = append(out, role)
	}
	return out
}

// appendFixture 是一个「8 台在线候选设备 + 能回包的主持」的夹具。
func appendFixture(t *testing.T, candidates int) (*setupFixture, *NodeRequestBroker, []db.RongcloudNode, string) {
	t.Helper()
	f := setupSetupFixture(t)

	nodes := make([]db.RongcloudNode, 0, candidates)
	for index := 0; index < candidates; index++ {
		nodes = append(nodes, capTestCandidate(index))
	}
	f.rt.roundtable.nodeOwner = func(context.Context, string) ([]db.RongcloudNode, error) {
		return nodes, nil
	}
	if f.rt.nodeRequests == nil {
		f.rt.nodeRequests = NewNodeRequestBroker(testLogger())
	}
	broker := f.rt.nodeRequests
	_, discussionID := f.createShell(t, "req-"+uniqueID("t"))
	return f, broker, nodes, discussionID
}

// 给了 node_ids 时（前端算好的「剩余未参与」集合）：续推必须
//   - 复用同一个 recommendationId（append 的全部意义）；
//   - 把新角色追加进库里那一行（上一批角色不丢，否则用户已添加的 role_id 失效）；
//   - 新角色只落在 node_ids 之内，且**不会**落到用户已添加角色占用的设备上。
func TestSetupRecommendAppendWithNodeIDsKeepsRowAndAvoidsAddedDevice(t *testing.T) {
	f, broker, nodes, discussionID := appendFixture(t, 8)
	broker.BindSender(respondRolesSender(t, broker, func(round, maxRoles int) []map[string]interface{} {
		return rolesNamed(fmt.Sprintf("批次%d", round), maxRoles)
	}))

	// ---- 轮 1：首推（无 node_ids，服务端自己从 8 台里挑 5 个角色）----
	f.rt.recommendForSetupBatch(context.Background(), discussionID, 1, setupRecommendRequest{})
	first := f.lastRolesPrompt(t, discussionID)
	firstID, _ := first["recommendationId"].(string)
	if firstID == "" {
		t.Fatalf("轮 1 事件缺 recommendationId: %#v", first)
	}
	firstNodes := payloadRoleNodeIDs(t, first)
	if len(firstNodes) != recommendRoleCountCap {
		t.Fatalf("轮 1 角色数 = %d, want %d", len(firstNodes), recommendRoleCountCap)
	}

	// ---- 用户采纳了轮 1 的第 1 个角色 → 那台设备从此被占用 ----
	addedRole := eventRoles(t, first)[0]
	addedNode, _ := addedRole["nodeId"].(string)

	// 前端据此算出「剩余未参与」：8 台减去已占用的那台。
	remaining := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node.NodeID == addedNode {
			continue
		}
		remaining = append(remaining, node.NodeID)
	}
	if len(remaining) != len(nodes)-1 {
		t.Fatalf("剩余设备数 = %d, want %d", len(remaining), len(nodes)-1)
	}

	// ---- 轮 2：续推（append + node_ids）----
	f.rt.recommendForSetupBatch(context.Background(), discussionID, 2, setupRecommendRequest{
		nodeIDs:      remaining,
		nodeIDsGiven: true,
		appendMode:   true,
	})
	second := f.lastRolesPrompt(t, discussionID)
	secondID, _ := second["recommendationId"].(string)

	if secondID != firstID {
		t.Fatalf("续推换了推荐行：recommendationId %q → %q；"+
			"用户已添加角色的 role_id 会当场失效", firstID, secondID)
	}
	secondNodes := payloadRoleNodeIDs(t, second)
	if len(secondNodes) == 0 {
		t.Fatalf("续推没有推出任何角色")
	}
	// 不能碰用户已添加角色的设备：否则 confirm 以 ROLE_DEVICE_DUPLICATED 整单拒绝。
	for _, nodeID := range secondNodes {
		if nodeID == addedNode {
			t.Fatalf("续推把新角色分给了已被占用的设备 %s（node_ids 已排除它）", nodeID)
		}
	}
	// 也不会凭空放大候选：只可能落在前端给的 node_ids 之内（这些设备同时也在
	// 服务端自己的 eligible 里，roleCandidateNodes 求过交集）。
	allowed := make(map[string]bool, len(remaining))
	for _, nodeID := range remaining {
		allowed[nodeID] = true
	}
	for _, nodeID := range secondNodes {
		if !allowed[nodeID] {
			t.Fatalf("新角色落在 node_ids 之外：%s；node_ids=%v", nodeID, remaining)
		}
	}

	// ---- 库里的那一行必须是「追加」，不是「覆盖」----
	rec, err := f.queries.GetRongCloudRoleRecommendation(context.Background(), firstID)
	if err != nil {
		t.Fatalf("读推荐行失败: %v", err)
	}
	var stored []map[string]interface{}
	if err := json.Unmarshal(rec.Roles, &stored); err != nil {
		t.Fatalf("推荐行 roles 不是合法 JSON: %v", err)
	}
	if want := len(firstNodes) + len(secondNodes); len(stored) != want {
		t.Fatalf("推荐行角色数 = %d, want %d（上一批 %d + 本批 %d）；"+
			"覆盖写会让用户已添加的 role_id 查不到，confirm 整单被拒",
			len(stored), want, len(firstNodes), len(secondNodes))
	}
	// 上一批的 role_id 必须仍在库里（confirm 的 recommendedIDs 判据）。
	storedIDs := make(map[string]bool, len(stored))
	for _, role := range stored {
		if id, ok := role["role_id"].(string); ok {
			storedIDs[id] = true
		}
	}
	for _, roleID := range payloadRoleIDs(t, first) {
		if !storedIDs[roleID] {
			t.Fatalf("续推后上一批 role_id %s 从推荐行里消失了", roleID)
		}
	}

	// ---- 用户点「开始讨论」那一刻：已添加的 1 个 + 本批全部，必须能过 ----
	assertRolesAssignable(t, []map[string]interface{}{addedRole}, eventRoles(t, second))
}

// 没给 node_ids 时（老前端 / 服务端自动选候选）：必须沿用整行排除兜底，
// 否则 assignRoleNodes 会挑到历史角色已占用的设备 → confirm ROLE_DEVICE_DUPLICATED。
func TestSetupRecommendAppendWithoutNodeIDsStillExcludesHistory(t *testing.T) {
	f, broker, _, discussionID := appendFixture(t, 8)
	broker.BindSender(respondRolesSender(t, broker, func(round, maxRoles int) []map[string]interface{} {
		return rolesNamed(fmt.Sprintf("批次%d", round), maxRoles)
	}))

	f.rt.recommendForSetupBatch(context.Background(), discussionID, 1, setupRecommendRequest{})
	first := f.lastRolesPrompt(t, discussionID)
	firstNodes := payloadRoleNodeIDs(t, first)

	// 续推但不带 node_ids：候选 = 全部 eligible − 整行历史角色设备。
	f.rt.recommendForSetupBatch(context.Background(), discussionID, 2, setupRecommendRequest{appendMode: true})
	second := f.lastRolesPrompt(t, discussionID)
	secondNodes := payloadRoleNodeIDs(t, second)
	if len(secondNodes) == 0 {
		t.Fatalf("续推没有推出任何角色（8 台设备里 5 台被历史占用，还剩 3 台可用）")
	}

	used := make(map[string]bool, len(firstNodes))
	for _, nodeID := range firstNodes {
		used[nodeID] = true
	}
	for _, nodeID := range secondNodes {
		if used[nodeID] {
			t.Fatalf("未给 node_ids 时把新角色分给了历史角色已占用的设备 %s；"+
				"confirm 会以 ROLE_DEVICE_DUPLICATED 整单拒绝", nodeID)
		}
	}

	// 同一推荐行 + 追加落库。
	firstID, _ := first["recommendationId"].(string)
	secondID, _ := second["recommendationId"].(string)
	if firstID != secondID {
		t.Fatalf("续推换了推荐行：%q → %q", firstID, secondID)
	}
	if len(secondNodes) != len(firstNodes) && len(secondNodes)+len(firstNodes) != 8 {
		// 8 台候选里轮 1 用掉 5 台，剩 3 台 → 轮 2 只能是 3 个角色。
		t.Fatalf("轮 2 角色数 = %d, want %d（剩余可用设备数）", len(secondNodes), 8-len(firstNodes))
	}
}

// 续推时前端把「已添加角色的设备」漏在 node_ids 里（客户端 bug / 手工构造请求）
// 也必须被服务端挡住：确认一下当前实现的真实行为，别把安全性全押在客户端上。
//
// 期望：node_ids 只与 eligible 求交集，所以**不能凭空放大候选**；但 node_ids 里
// 真有已占用设备时，服务端会照它分配（这是信任前端的必然代价）—— 这条用例把
// 这个边界钉住，避免将来有人误以为服务端会二次去重。
func TestSetupRecommendAppendTrustsGivenNodeIDsIntersectedWithEligible(t *testing.T) {
	f, broker, nodes, discussionID := appendFixture(t, 4)
	broker.BindSender(respondRolesSender(t, broker, func(round, maxRoles int) []map[string]interface{} {
		return rolesNamed(fmt.Sprintf("批次%d", round), maxRoles)
	}))
	// requestRoleRecommend 要求断点已到 roles（否则 409 DISCUSSION_SETUP_STEP_CONFLICT）。
	// 直接走 query 推进，不碰 actionSubmitSetupTopic —— 它会再起一条异步推荐管线。
	if _, err := f.queries.SubmitRongCloudDiscussionSetupTopic(context.Background(),
		db.SubmitRongCloudDiscussionSetupTopicParams{
			DiscussionID: discussionID, Topic: "续推候选是否会被放大", Goal: "",
		}); err != nil {
		t.Fatalf("推进到 roles 步失败: %v", err)
	}
	row, err := f.queries.GetConfiguringRongCloudDiscussionByChatroom(context.Background(),
		mustChatroomID(t, f, discussionID))
	if err != nil || row.SetupStep != setupStepRoles {
		t.Fatalf("setup_step = %q, %v; want roles", row.SetupStep, err)
	}

	// 请求里塞一台**不属于本人**的设备：绝不能出现在候选里。
	foreignNodeID := "node-not-owned-" + uniqueID("t")
	requested := []interface{}{foreignNodeID, nodes[0].NodeID, nodes[1].NodeID}

	result := f.rt.actionRequestRoleRecommend(context.Background(), CommandContent{
		Service: "discussion", Action: "requestRoleRecommend",
		Params: map[string]interface{}{
			"chatroom_id": mustChatroomID(t, f, discussionID),
			"node_ids":    requested,
			"append":      true,
		},
	}, f.actor)
	if result.code != 200 || result.data == nil {
		t.Fatalf("requestRoleRecommend 应 200（异步），got %d %s", result.code, result.message)
	}
	// 管线是异步的（go 出去）：等 roles 事件落地。
	f.waitRolesPrompt(t, discussionID)
	payload := f.lastRolesPrompt(t, discussionID)
	got := payloadRoleNodeIDs(t, payload)
	for _, nodeID := range got {
		if nodeID == foreignNodeID {
			t.Fatalf("候选被前端 node_ids 放大了：不属于本人的设备 %s 也进了候选", foreignNodeID)
		}
	}
	// 同时确认「只与本人在线合格集求交集」没有被放宽成「原样信任」：
	// 本用例里 4 台设备都有 discussion_participant，所以 2 台 -> 2 个角色。
	if len(got) != 2 {
		t.Fatalf("候选数 = %d, want 2（node_ids 里只有 2 台属本人的合格设备）: %v", len(got), got)
	}
}

// waitRolesPrompt 轮询直到出现 setup_prompt(roles)（异步推荐管线有调度延迟）。
// 若先等到失败卡，则把失败码打出来，别让用例只报一句「没有事件」。
func (f *setupFixture) waitRolesPrompt(t *testing.T, discussionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, err := f.rt.repo.ListEventsAfter(context.Background(), discussionID, 0)
		if err == nil {
			for _, event := range events {
				var payload map[string]interface{}
				if json.Unmarshal(event.Payload, &payload) != nil {
					continue
				}
				step, _ := payload["step"].(string)
				if step != setupStepRoles {
					continue
				}
				if event.EventType == setupEventPrompt {
					return
				}
				if event.EventType == setupEventRecommendFailed {
					code, _ := payload["code"].(string)
					t.Fatalf("续推失败（%s），没有 roles 卡", code)
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("roles 卡在 5s 内未出现；当前事件: %v", f.eventTypes(t, discussionID))
}

// mustChatroomID 由 discussion 反查 chatroom（requestRoleRecommend 只认 chatroom_id）。
// rongcloud_discussion.chatroom_id 存的是融云聊天室 id（TEXT），不是内部 UUID。
func mustChatroomID(t *testing.T, f *setupFixture, discussionID string) string {
	t.Helper()
	var chatroomID string
	err := f.pool.QueryRow(context.Background(),
		"SELECT chatroom_id FROM rongcloud_discussion WHERE discussion_id=$1", discussionID).Scan(&chatroomID)
	if err != nil {
		t.Fatalf("反查 chatroom_id 失败: %v", err)
	}
	return chatroomID
}
