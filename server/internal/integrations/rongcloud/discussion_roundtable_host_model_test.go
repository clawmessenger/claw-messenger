package rongcloud

import (
	"encoding/json"
	"testing"
)

// 主持人模型落快照的单元测试（task 2026-10-25/task-001）。
//
// 缺口本身不在「派发」这一侧：`discussion_interactive.go` 早就把
// `nullableString(role.Model)` 写进 assignment payload、并在 your_turn 的
// params 里带 model。缺的是**产生 role.Model 的那一侧** —— host 的角色板条目
// 从来不写 model 键。这三个写入点（setup / actions / stages）任何一处漏改，
// 都会表现为「有的讨论组生效、有的不生效」，所以这里把口径钉在纯函数上。

func hostModelFixture(model string) roundtableValidated {
	return roundtableValidated{
		host: roundtableHostConfig{
			nodeID:        "node-host",
			aiUserID:      "ai-host",
			displayName:   "虾说讨论主持人",
			avatar:        "https://example.test/host.png",
			hostPrompt:    "你是中立主持人",
			configVersion: "7",
			model:         model,
		},
	}
}

// TestSetupRoleSnapshotsCarriesHostModel 覆盖「配置了模型」的一侧：角色板的
// host 条目必须带 model（v3 引擎的 interactiveRoles 只从角色板取 model），
// host_config_snapshot 里也要留一份给排查用。
func TestSetupRoleSnapshotsCarriesHostModel(t *testing.T) {
	roleJSON, hostSnapshot := setupRoleSnapshots(roundtableCanonical{}, hostModelFixture("openai/gpt-5.6-sol"))

	var roles map[string]map[string]interface{}
	if err := json.Unmarshal(roleJSON, &roles); err != nil {
		t.Fatalf("role snapshot is not JSON: %v", err)
	}
	host, ok := roles["ai-host"]
	if !ok {
		t.Fatalf("role snapshot has no host entry: %s", roleJSON)
	}
	if host["model"] != "openai/gpt-5.6-sol" {
		t.Fatalf("host role model = %#v, want openai/gpt-5.6-sol", host["model"])
	}
	// 派发侧的读取口径：interactiveRoles 就是从角色板取 model 的。
	// ok=false 只表示「这份快照不能推进轮次」（本例没有普通成员），host 照常解析。
	_, parsedHost, _ := interactiveRoles(DiscussionRecord{RoleSnapshot: roleJSON})
	if parsedHost.MemberID != "ai-host" {
		t.Fatalf("interactiveRoles did not parse the host entry: %s", roleJSON)
	}
	if parsedHost.Model != "openai/gpt-5.6-sol" {
		t.Fatalf("interactiveRole.Model = %q, want openai/gpt-5.6-sol", parsedHost.Model)
	}

	var snapshot map[string]interface{}
	if err := json.Unmarshal(hostSnapshot, &snapshot); err != nil {
		t.Fatalf("host snapshot is not JSON: %v", err)
	}
	if snapshot["model"] != "openai/gpt-5.6-sol" {
		t.Fatalf("host snapshot model = %#v, want openai/gpt-5.6-sol", snapshot["model"])
	}
}

// TestSetupRoleSnapshotsOmitsModelWhenUnset 覆盖向后兼容的一侧：没配置模型时
// **不写** model 键（而不是写空串）。这条决定了「不选模型 = 行为与改造前完全
// 一致」——空串会让 role.Model 为空、params 不带 model，设备端 buildModelArgs
// 的 if (!model) 沿用设备默认，三者等价；但多写一个空键会污染已有快照。
func TestSetupRoleSnapshotsOmitsModelWhenUnset(t *testing.T) {
	roleJSON, hostSnapshot := setupRoleSnapshots(roundtableCanonical{}, hostModelFixture(""))

	var roles map[string]map[string]interface{}
	if err := json.Unmarshal(roleJSON, &roles); err != nil {
		t.Fatalf("role snapshot is not JSON: %v", err)
	}
	if _, present := roles["ai-host"]["model"]; present {
		t.Fatalf("host role entry must not carry a model key: %#v", roles["ai-host"])
	}
	var snapshot map[string]interface{}
	if err := json.Unmarshal(hostSnapshot, &snapshot); err != nil {
		t.Fatalf("host snapshot is not JSON: %v", err)
	}
	if _, present := snapshot["model"]; present {
		t.Fatalf("host snapshot must not carry a model key: %#v", snapshot)
	}
}

// TestNormalizeModelCatalog 覆盖设备回包的校验：形状/键集/唯一性/默认模型
// 任何一项不符都整份作废，绝不能让管理端拿半份目录去存一个设备跑不了的模型。
func TestNormalizeModelCatalog(t *testing.T) {
	valid := map[string]interface{}{
		"msg_type":        catalogResponseMsgType,
		"protocolVersion": float64(2),
		"requestId":       "catalog_x",
		"defaultModel":    "openai/gpt-5.6-sol",
		"providers": []interface{}{
			map[string]interface{}{
				"id":   "openai",
				"name": "OpenAI",
				"models": []interface{}{
					map[string]interface{}{"id": "gpt-5.6-sol", "name": "GPT 5.6 Sol"},
					map[string]interface{}{"id": "gpt-5-mini", "name": "GPT-5 mini"},
				},
			},
		},
		"timestamp": float64(1),
	}
	catalog, err := normalizeModelCatalog(valid)
	if err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}
	if catalog.DefaultModel != "openai/gpt-5.6-sol" || len(catalog.Providers) != 1 {
		t.Fatalf("catalog = %+v", catalog)
	}
	if !catalog.CatalogHasRoute("openai", "gpt-5.6-sol") {
		t.Fatal("CatalogHasRoute missed a listed model")
	}
	if catalog.CatalogHasRoute("openai", "nope") || catalog.CatalogHasRoute("other", "gpt-5.6-sol") {
		t.Fatal("CatalogHasRoute accepted a model that is not in the catalog")
	}

	// defaultModel 不在目录里 → 丢弃（不是整份作废，web 同款）。
	offRoute := cloneCatalogPayload(valid)
	offRoute["defaultModel"] = "openai/ghost"
	catalog, err = normalizeModelCatalog(offRoute)
	if err != nil {
		t.Fatalf("off-route default must not void the catalog: %v", err)
	}
	if catalog.DefaultModel != "" {
		t.Fatalf("defaultModel = %q, want empty", catalog.DefaultModel)
	}

	cases := map[string]map[string]interface{}{
		"no providers":     map[string]interface{}{"providers": "nope"},
		"provider no id":   map[string]interface{}{"providers": []interface{}{map[string]interface{}{"name": "x", "models": []interface{}{}}}},
		"model id with /": map[string]interface{}{"providers": []interface{}{map[string]interface{}{
			"id": "openai", "name": "OpenAI",
			"models": []interface{}{map[string]interface{}{"id": "a/b", "name": "x"}},
		}}},
		"duplicate model": map[string]interface{}{"providers": []interface{}{map[string]interface{}{
			"id": "openai", "name": "OpenAI",
			"models": []interface{}{
				map[string]interface{}{"id": "a", "name": "x"},
				map[string]interface{}{"id": "a", "name": "y"},
			},
		}}},
	}
	for name, payload := range cases {
		if _, err := normalizeModelCatalog(payload); err == nil {
			t.Errorf("%s: expected rejection, got none", name)
		}
	}
}

func cloneCatalogPayload(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// TestProviderModelsDistinguishesSilenceFromAbsence 钉住「没有信息」与「信息说没有」
// 的区别 —— 这正是管理端保存时三态判定的分水岭：
//
//	provider 不在目录   → found=false       → rejected（provider 明确不存在）
//	provider 在、清单空 → found=true, 0 条  → unverified（节点没上报，不是不存在）
//	provider 在、有清单 → found=true, N 条  → 再按 CatalogHasRoute 复核具体模型
//
// 早先只判 `CatalogHasRoute`，第二类会和第三类的「模型不在清单里」混成同一个
// rejected：自建 OpenAI 兼容端点的模型由服务端动态给出、节点侧只报得出 provider
// 名，于是这类节点上任何模型都存不进去 —— 前端级联是空列表，手填撞 400
// MODEL_NOT_AVAILABLE，表现就是「设置讨论主持 AI 模型的功能不可用」。
func TestProviderModelsDistinguishesSilenceFromAbsence(t *testing.T) {
	catalog := DiscussionModelCatalog{Providers: []DiscussionModelProvider{
		{ID: "custom", Name: "custom"},
		{ID: "openai", Name: "OpenAI", Models: []DiscussionModelEntry{{ID: "gpt-5.6-sol"}}},
	}}

	models, found := catalog.ProviderModels("custom")
	if !found {
		t.Fatal("provider 在目录里却没被认出来，会被误判成 provider 不存在")
	}
	if len(models) != 0 {
		t.Fatalf("custom 的模型清单应为空，got %d 条", len(models))
	}
	if catalog.CatalogHasRoute("custom", "anything") {
		t.Fatal("空清单的 provider 不该命中任何 route")
	}

	if _, found := catalog.ProviderModels("ghost"); found {
		t.Fatal("不在目录里的 provider 不该被认作 found")
	}

	models, found = catalog.ProviderModels("openai")
	if !found || len(models) != 1 {
		t.Fatalf("openai = (%d 条, found=%v), want (1, true)", len(models), found)
	}
	if !catalog.CatalogHasRoute("openai", "gpt-5.6-sol") {
		t.Fatal("有清单的 provider 应能定位到清单内的模型")
	}

	// 空 provider id 不该匹配到任何条目（NormalizeModel 已保证传入非空，
	// 这里只是把契约钉死，防止将来有人放宽校验）。
	if _, found := catalog.ProviderModels(""); found {
		t.Fatal("空 provider id 不该被认作 found")
	}
}
