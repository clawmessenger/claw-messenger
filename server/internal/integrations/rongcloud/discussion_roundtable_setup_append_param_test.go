package rongcloud

import "testing"

// S2「继续添加」在服务端只有两个可选参数：node_ids（本批允许的候选设备）与
// append（续推，新角色追加进当前推荐行、recommendationId 不变）。
//
// 这里只测**纯解析**：paramStringList 的 []/>8 判非法已由
// discussion_roundtable_roles_test.go 的 TestParamStringList 覆盖，本条补上
// append 的三种别名与宽松真值 —— 它决定「续推」还是「换一批」，判错会把用户
// 已添加的角色当场作废（服务端新建推荐行，旧 recommendationId 失效）。
//
// actionRequestRoleRecommend 本体依赖数据库（findConfiguringSetup），
// 那层「解析非法 → 400 INVALID_ROUNDTABLE_REQUEST」的映射没有纯单测覆盖。
func TestParamBoolAppendMode(t *testing.T) {
	cmd := func(params map[string]interface{}) CommandContent {
		return CommandContent{Params: params}
	}
	keys := []string{"append", "append_mode", "appendMode"}

	// 三种拼写都要认：web 发 append，设备 CLI/旧网关可能发 append_mode / appendMode。
	for _, key := range keys {
		if !paramBool(cmd(map[string]interface{}{key: true}), keys...) {
			t.Errorf("%s=true must be true", key)
		}
		if paramBool(cmd(map[string]interface{}{key: false}), keys...) {
			t.Errorf("%s=false must be false", key)
		}
	}

	// 宽松真值：JSON 走 encoding/json 的数字是 float64，字符串形态也见过。
	for name, value := range map[string]interface{}{
		"string true": "true",
		"string TRUE": " TRUE ",
		"string 1":    "1",
		"number 1":    float64(1),
	} {
		if !paramBool(cmd(map[string]interface{}{"append": value}), keys...) {
			t.Errorf("%s must be true", name)
		}
	}

	// 缺失 / 不认识一律 false：老前端不传时行为必须与「没有这个开关」完全一致。
	for name, params := range map[string]map[string]interface{}{
		"missing":     {},
		"nil params":  nil,
		"empty":       {"append": ""},
		"string no":   {"append": "no"},
		"zero number": {"append": float64(0)},
		"wrong type":  {"append": []interface{}{true}},
	} {
		if paramBool(cmd(params), keys...) {
			t.Errorf("%s must be false", name)
		}
	}

	// 别名顺序：先命中 append，不因后面还有一个 false 的 append_mode 被覆盖。
	if !paramBool(cmd(map[string]interface{}{"append": true, "appendMode": false}), keys...) {
		t.Error("the first present key wins")
	}
}
