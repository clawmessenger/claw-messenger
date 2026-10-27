package rongcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 讨论组角色推荐数上限（产品口径：3–5 个角色，硬顶 5）。
//
// 这两条用例守的是**下发侧**的那一半：`recommendForSetupAsync` 里
// `min(recommendRoleCountCap, len(candidates))` 一旦退回 `len(candidates)`，
// 手上有 8 台设备的用户就会被推荐 8 个角色，而向导一次只能配一个，用户要滑 8 轮；
// 反过来如果退回固定值（曾经是 3），只有 2 台设备的用户会收到 3 个角色、第 3 个
// 必然绑不到设备，服务端 confirm 整单拒。
//
// 注意与 `RecommendRolesMaxRoles`（=8，候选集的硬顶）区分：
//   - `RecommendRolesMaxRoles` 限制「候选设备最多 8 台」（roleCandidateNodes 里截断）
//   - `recommendRoleCountCap` 限制「请设备推荐几个角色」
//
// 断言点落在 broker 真正发出去的那条 command 上（`params.max_roles`），而不是
// 函数返回值 —— 后者在这一层根本不回调，只有抓包才能看见。

// capTestCandidate 造一台「在线 + 讨论能力齐备」的候选设备。
// 不走 DB：`nodeOwner` 是可注入的取归属函数，能不能当候选只取决于
// deploy_status（无融云客户端时 nodeIsOnline 回落到这一列）与能力位。
func capTestCandidate(index int) db.RongcloudNode {
	suffix := uniqueID("t")
	nodeID := fmt.Sprintf("cap-node-%s-%d", suffix, index)
	return db.RongcloudNode{
		NodeID:          nodeID,
		RongcloudUserID: "rc_" + nodeID,
		DeployStatus:    "online",
		Capabilities:    []byte(`["discussion_participant","discussion_roundtable","discussion_model_routing"]`),
	}
}

// captureSender 造一个「只抓请求、故意不应答」的发送端：
// 抓到的 envelope 写进 *captured，并返回错误让 broker 快速走失败分支。
func captureSender(t *testing.T, captured *map[string]interface{}) NodeRequestSender {
	t.Helper()
	return func(_ context.Context, _ string, content string) error {
		var envelope map[string]interface{}
		if err := json.Unmarshal([]byte(content), &envelope); err != nil {
			t.Errorf("推荐请求不是合法 JSON: %v", err)
			return errors.New("bad envelope")
		}
		*captured = envelope
		return errors.New("test: 故意不发上线（用例只抓请求）")
	}
}

// captureSetupRecommend 跑一次 setup 推荐管线，返回 broker 实际发出的命令信封。
//
// 发送端故意返回错误：这两条用例只关心「请求里带了几个角色」，不需要跑完整个
// 设备往返（跑完还会往 recommendation 表落一行、多一次 TTL 依赖）。
func captureSetupRecommend(t *testing.T, candidates int) (map[string]interface{}, *setupFixture, string) {
	t.Helper()
	f := setupSetupFixture(t)

	nodes := make([]db.RongcloudNode, 0, candidates)
	for index := 0; index < candidates; index++ {
		nodes = append(nodes, capTestCandidate(index))
	}
	f.rt.roundtable.nodeOwner = func(context.Context, string) ([]db.RongcloudNode, error) {
		return nodes, nil
	}

	var captured map[string]interface{}
	broker := NewNodeRequestBroker(testLogger())
	broker.BindSender(captureSender(t, &captured))
	f.rt.nodeRequests = broker

	_, discussionID := f.createShell(t, "req-"+uniqueID("t"))
	f.rt.recommendForSetupAsync(context.Background(), discussionID, 1)
	return captured, f, discussionID
}

func TestSetupRecommendCapsRolesAtFive(t *testing.T) {
	// 用例前提：产品口径必须严于候选硬顶，否则「8 台候选」这个场景证明不了任何事。
	if recommendRoleCountCap >= RecommendRolesMaxRoles {
		t.Fatalf("recommendRoleCountCap=%d 应小于候选硬顶 RecommendRolesMaxRoles=%d",
			recommendRoleCountCap, RecommendRolesMaxRoles)
	}

	captured, f, discussionID := captureSetupRecommend(t, RecommendRolesMaxRoles)
	if captured == nil {
		t.Fatalf("broker 没有发出角色推荐请求（发送端未被调用）")
	}

	// 抓的必须是角色推荐那一族，别被模型目录请求混进来。
	if got, _ := captured["action"].(string); got != recommendRolesAction {
		t.Fatalf("command action = %q, want %q", got, recommendRolesAction)
	}
	params, _ := captured["params"].(map[string]interface{})
	if params == nil {
		t.Fatalf("推荐请求缺 params：%#v", captured)
	}
	maxRoles, ok := params["max_roles"].(float64)
	if !ok {
		t.Fatalf("推荐请求没带 max_roles：%#v", params)
	}

	// 8 台候选 → 只能请设备推荐 5 个角色（不是 8）。
	if maxRoles != float64(recommendRoleCountCap) {
		t.Fatalf("8 台候选时 max_roles = %v, want %d（产品上限）而不是候选数 %d",
			maxRoles, recommendRoleCountCap, RecommendRolesMaxRoles)
	}

	// 管线确实走到了失败分支（发送失败 → 失败卡），说明我们抓到的不是中途别的请求。
	if !hasEvent(f.eventTypes(t, discussionID), setupEventRecommendFailed) {
		t.Fatalf("发送失败后应落 %s 事件；当前事件: %v",
			setupEventRecommendFailed, f.eventTypes(t, discussionID))
	}
}

func TestSetupRecommendRoleCapFollowsCandidateCount(t *testing.T) {
	// 候选比上限少时必须按候选数下发：固定下发 5 会让「只有 3 台设备」的用户
	// 收到 5 个角色，多出来的角色绑不上设备，confirm 整单被拒。
	captured, _, _ := captureSetupRecommend(t, 3)
	if captured == nil {
		t.Fatalf("broker 没有发出角色推荐请求（发送端未被调用）")
	}
	params, _ := captured["params"].(map[string]interface{})
	if params == nil {
		t.Fatalf("推荐请求缺 params：%#v", captured)
	}
	maxRoles, _ := params["max_roles"].(float64)
	if maxRoles != 3 {
		t.Fatalf("3 台候选时 max_roles = %v, want 3（min(上限, 候选数)）", maxRoles)
	}
}

// 小程序（C 端）走的是 `actionRecommendRoles`（带 node_ids 的 recommend_roles 命令），
// 与 web 的 setup 首推/换一批是**两条不同代码路径**。同一个产品口径（最多 5 个角色）
// 必须在两处都成立，否则会出现「web 推 5 个、小程序推 8 个」的跨端不一致。
//
// 这条复用与 setup 那条完全相同的接缝（broker BindSender 抓真实下发的 command），
// 只是入口换成命令路径；候选集合由命令参数 node_ids 指定，不需要额外建讨论行。
func TestActionRecommendRolesCapsRolesAtFive(t *testing.T) {
	if recommendRoleCountCap >= RecommendRolesMaxRoles {
		t.Fatalf("recommendRoleCountCap=%d 应小于候选硬顶 RecommendRolesMaxRoles=%d",
			recommendRoleCountCap, RecommendRolesMaxRoles)
	}

	f := setupSetupFixture(t)

	// 8 台在线、能力齐备的候选（正好是 node_ids 允许的上限）。
	nodes := make([]db.RongcloudNode, 0, RecommendRolesMaxRoles)
	nodeIDs := make([]interface{}, 0, RecommendRolesMaxRoles)
	for index := 0; index < RecommendRolesMaxRoles; index++ {
		node := capTestCandidate(index)
		nodes = append(nodes, node)
		nodeIDs = append(nodeIDs, node.NodeID)
	}
	f.rt.roundtable.nodeOwner = func(context.Context, string) ([]db.RongcloudNode, error) {
		return nodes, nil
	}

	var captured map[string]interface{}
	broker := NewNodeRequestBroker(testLogger())
	broker.BindSender(captureSender(t, &captured))
	f.rt.nodeRequests = broker

	result := f.rt.actionRecommendRoles(context.Background(), CommandContent{
		Service: "discussion",
		Action:  "recommendRoles",
		Params: map[string]interface{}{
			"topic":    "发布计划评审",
			"node_ids": nodeIDs,
		},
	}, f.actor)

	if captured == nil {
		t.Fatalf("broker 没有发出角色推荐请求（发送端未被调用）；命令返回 %d %s", result.code, result.message)
	}
	if got, _ := captured["action"].(string); got != recommendRolesAction {
		t.Fatalf("command action = %q, want %q", got, recommendRolesAction)
	}
	params, _ := captured["params"].(map[string]interface{})
	if params == nil {
		t.Fatalf("推荐请求缺 params：%#v", captured)
	}
	maxRoles, ok := params["max_roles"].(float64)
	if !ok {
		t.Fatalf("推荐请求没带 max_roles：%#v", params)
	}
	// 8 台候选 → 5，不是 8（与 web 的 recommendForSetupAsync 同口径）。
	if maxRoles != float64(recommendRoleCountCap) {
		t.Fatalf("node_ids 给 8 台时 max_roles = %v, want %d（产品上限）而不是候选数 %d",
			maxRoles, recommendRoleCountCap, RecommendRolesMaxRoles)
	}
	// 发送失败 → 命令回 503 且码是 broker 的失败码（证明真的走了这一次往返）。
	if result.code != 503 {
		t.Fatalf("发送失败时命令应回 503，got %d %s", result.code, result.message)
	}
}
