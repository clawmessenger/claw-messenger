package rongcloud

import (
	"context"
	"strings"
	"testing"
)

// 这些用例锁的是「存量 agent 一次性换品牌头像」最容易错的三件事：
//
//  1. 融云 userId 里的 agent 名是 **CLI 名**，不是前端 kind（`cursor-agent` vs `cursor`）。
//     映射漏一组，那家 agent 就永远停在统一默认图，而且没有任何报错。
//  2. 机器主节点 / ops 节点和 agent 共用 rongcloud_user 表，绝不能跟着换图。
//  3. 用户自选的头像一次都不能覆盖。
func TestAgentKindFromRCUserID(t *testing.T) {
	cases := []struct {
		id   string
		kind string
		ok   bool
	}{
		// 现行 Go agent 节点：<cli>_<machineKey>
		{"kimi_1758900000000", "kimi", true},
		{"opencode_1758900000000", "opencode", true},
		{"claude_1758900000000", "claude", true},
		// 四组 CLI 名 ≠ kind 的，最容易漏
		{"cursor-agent_1758900000000", "cursor", true},
		{"kiro-cli_1758900000000", "kiro", true},
		{"agy_1758900000000", "antigravity", true},
		{"qodercli_1758900000000", "qoder", true},
		// 旧世代：rc_node_<machine>_<cli>
		{"rc_node_3300776275_kimi", "kimi", true},
		{"rc_node_3300776275_cursor-agent", "cursor", true},
		// 远古 openclaw
		{"claw_123456", "openclaw", true},
		// om_ 前缀是运维人格，剥掉后仍是同一个节点
		{"om_kimi_1758900000000", "kimi", true},

		// 机器主节点：没有 agent 段
		{"rc_node_3300776275", "", false},
		{"rc_node_02:95:06:48:f0:dc", "", false},
		// ops 节点：服务 agent 但不是 agent 本人
		{"rc_node_3300776275_ops", "", false},
		{"ops_3300776275", "", false},
		{"kimi_1758900000000_ops", "", false},
		// 没有官方图标的 CLI（前端 kind 清单里没有 omp）
		{"omp_1758900000000", "", false},
		// 真人 / 空值
		{"", "", false},
		{"rc_user_zhangsan", "", false},
		{"portrait-empty-abc", "", false},
	}
	for _, tc := range cases {
		kind, ok := AgentKindFromRCUserID(tc.id)
		if ok != tc.ok || kind != tc.kind {
			t.Errorf("AgentKindFromRCUserID(%q) = (%q, %v), want (%q, %v)", tc.id, kind, ok, tc.kind, tc.ok)
		}
	}
}

func TestAgentPortraitURI(t *testing.T) {
	portraitTestClearEnv(t)
	for _, kind := range []string{"kimi", "cursor", "antigravity", "qoder", "copilot"} {
		got, ok := AgentPortraitURI(kind)
		if !ok {
			t.Fatalf("AgentPortraitURI(%q) reported no brand portrait", kind)
		}
		want := AgentPortraitBaseURL + "/" + kind + "-256.png"
		if got != want {
			t.Errorf("AgentPortraitURI(%q) = %q, want %q", kind, got, want)
		}
	}
	if _, ok := AgentPortraitURI("omp"); ok {
		t.Error("omp 没有官方图标，不该给出地址（否则会借错图）")
	}
	if _, ok := AgentPortraitURI(""); ok {
		t.Error("空 kind 不该给出地址")
	}

	t.Setenv(AgentPortraitBaseEnv, "https://cdn.example.com/agent-avatars/")
	got, _ := AgentPortraitURI("kimi")
	if got != "https://cdn.example.com/agent-avatars/kimi-256.png" {
		t.Errorf("env 覆盖没生效（尾斜杠应被吃掉）: %q", got)
	}
}

func TestIsAgentPortraitURI(t *testing.T) {
	portraitTestClearEnv(t)
	base := AgentPortraitBaseURL
	yes := []string{
		base + "/kimi-256.png",
		base + "/cursor-256.png",
		base + "/kimi-256.png?v=2",
		base + "/kimi-256.png#x",
		"  " + base + "/kimi-256.png  ",
	}
	for _, url := range yes {
		if !IsAgentPortraitURI(url) {
			t.Errorf("IsAgentPortraitURI(%q) = false, want true", url)
		}
	}
	no := []string{
		"",
		DefaultUserPortraitURI,
		"https://cdn.example.com/uploads/u/me.png",
		base + "/kimi.svg",        // 前端 logo 的原图，不是写进融云的那份
		base + "/kimi-128.png",    // 尺寸不符
		base + "/omp-256.png",     // 没有图标的 kind
		base + "/unknown-256.png", // 不存在的 kind
		base + "/sub/kimi-256.png",
		"https://evil.example.com/claw-web/agent-avatars/kimi-256.png",
	}
	for _, url := range no {
		if IsAgentPortraitURI(url) {
			t.Errorf("IsAgentPortraitURI(%q) = true, want false", url)
		}
	}

	t.Setenv(AgentPortraitBaseEnv, "https://cdn.example.com/agent-avatars")
	if !IsAgentPortraitURI("https://cdn.example.com/agent-avatars/kimi-256.png") {
		t.Error("env 覆盖后应认新 base")
	}
	if IsAgentPortraitURI(base + "/kimi-256.png") {
		t.Error("env 覆盖后旧 base 不该再被认成自己的头像")
	}
}

// 25 个 kind 必须每个都能出地址 —— 少一个就是某家 agent 永远换不上图。
func TestAgentPortraitCoversEveryKind(t *testing.T) {
	portraitTestClearEnv(t)
	if len(agentPortraitKinds) != 25 {
		t.Fatalf("agentPortraitKinds has %d kinds, want 25 (must match the web AGENT_LOGO_BY_KIND)", len(agentPortraitKinds))
	}
	missing := make([]string, 0)
	for kind := range agentPortraitKinds {
		if _, ok := AgentPortraitURI(kind); !ok {
			missing = append(missing, kind)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("kinds without a portrait URL: %v", missing)
	}
}

// 每个前端 kind 都必须有 CLI 名能映射到它，否则那家在存量里永远匹配不到。
func TestAgentKindByCLINameCoversEveryKind(t *testing.T) {
	reached := make(map[string]bool, len(agentKindByCLIName))
	for _, kind := range agentKindByCLIName {
		reached[kind] = true
	}
	missing := make([]string, 0)
	for kind := range agentPortraitKinds {
		if !reached[kind] {
			missing = append(missing, kind)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("kinds unreachable from any CLI name: %v", missing)
	}
}

// ---- 行为：AgentPortraits 模式下真正写什么 -------------------------------

func TestSyncUserPortraitsAgentBrandDryRun(t *testing.T) {
	portraitTestClearEnv(t)
	pool, q, svc, rec, wsID := newPortraitSyncFixture(t)
	suffix := uniqueID("ap")

	kimiID := "kimi_" + suffix
	cursorID := "cursor-agent_" + suffix
	customID := "kiro-cli_" + suffix
	machineID := "rc_node_" + suffix
	cleanupPortraitUsers(t, pool, kimiID, cursorID, customID, machineID)

	// 默认图 → 该换成品牌图
	seedPortraitUser(t, q, wsID, kimiID, DefaultUserPortraitURI)
	// 空 portrait → 也该换成品牌图
	seedPortraitUser(t, q, wsID, cursorID, "")
	// 用户自选的头像 → 一个字都不许动
	seedPortraitUser(t, q, wsID, customID, "https://cdn.example.com/uploads/u/me.png")
	// 机器主节点 → 不是 agent，保持统一默认图
	seedPortraitUser(t, q, wsID, machineID, DefaultUserPortraitURI)

	outcomes, err := svc.SyncUserPortraits(context.Background(), wsID,
		BackfillFriendsOptions{DryRun: true, AgentPortraits: true})
	if err != nil {
		t.Fatalf("SyncUserPortraits: %v", err)
	}
	byUser := make(map[string]PortraitSyncOutcome, len(outcomes))
	for _, o := range outcomes {
		byUser[o.RCUser] = o
	}

	if got := byUser[kimiID]; got.Status != "planned" || !strings.HasSuffix(got.To, "/kimi-256.png") {
		t.Errorf("kimi: status=%q to=%q, want planned → kimi-256.png", got.Status, got.To)
	}
	if got := byUser[cursorID]; got.Status != "planned" || !strings.HasSuffix(got.To, "/cursor-256.png") {
		t.Errorf("cursor-agent: status=%q to=%q, want planned → cursor-256.png", got.Status, got.To)
	}
	if got := byUser[customID]; got.Status != "skipped" {
		t.Errorf("自定义头像被动了: status=%q", got.Status)
	}
	if got := byUser[machineID]; got.Status != "skipped" {
		t.Errorf("机器主节点被动了: status=%q", got.Status)
	}
	if refreshes := rec.requests("/user/refresh.json"); len(refreshes) != 0 {
		t.Fatalf("dry run 不该调用融云，实际发了 %d 个 refresh", len(refreshes))
	}
}

func TestSyncUserPortraitsAgentBrandCommitAndIdempotent(t *testing.T) {
	portraitTestClearEnv(t)
	pool, q, svc, rec, wsID := newPortraitSyncFixture(t)
	suffix := uniqueID("ac")

	kimiID := "kimi_" + suffix
	cleanupPortraitUsers(t, pool, kimiID)
	seedPortraitUser(t, q, wsID, kimiID, DefaultUserPortraitURI)

	ctx := context.Background()
	outcomes, err := svc.SyncUserPortraits(ctx, wsID, BackfillFriendsOptions{AgentPortraits: true})
	if err != nil {
		t.Fatalf("SyncUserPortraits: %v", err)
	}
	var updated bool
	for _, o := range outcomes {
		if o.RCUser == kimiID {
			if o.Status != "updated" {
				t.Fatalf("kimi 行状态 = %q, want updated (err=%q)", o.Status, o.Error)
			}
			updated = true
		}
	}
	if !updated {
		t.Fatal("kimi 行没出现在 outcomes 里")
	}

	// 融云侧收到了正确的 portraitUri
	wantPortrait, _ := AgentPortraitURI("kimi")
	refreshes := rec.requests("/user/refresh.json")
	var sawKimi bool
	for _, form := range refreshes {
		if form.Get("portraitUri") == wantPortrait {
			sawKimi = true
		}
	}
	if !sawKimi {
		t.Fatalf("融云没收到 %q，实际请求 %d 条", wantPortrait, len(refreshes))
	}
	// 本地库也落了同一个值
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(portrait_uri, '') FROM rongcloud_user WHERE rongcloud_user_id = $1`,
		kimiID).Scan(&stored); err != nil {
		t.Fatalf("read portrait: %v", err)
	}
	if stored != wantPortrait {
		t.Fatalf("库内 portrait = %q, want %q", stored, wantPortrait)
	}

	// 第二轮：已经是目标值 → 全 skip、零融云调用（幂等）
	before := len(rec.requests("/user/refresh.json"))
	outcomes, err = svc.SyncUserPortraits(ctx, wsID, BackfillFriendsOptions{AgentPortraits: true})
	if err != nil {
		t.Fatalf("second SyncUserPortraits: %v", err)
	}
	for _, o := range outcomes {
		if o.RCUser == kimiID && o.Status != "skipped" {
			t.Fatalf("重跑时 kimi 行 = %q, want skipped（幂等）", o.Status)
		}
	}
	if after := len(rec.requests("/user/refresh.json")); after != before {
		t.Fatalf("重跑不该再调融云，refresh 从 %d 变成 %d", before, after)
	}
}

// 回滚路径：`-sync-portraits`（AgentPortraits=false）必须能把品牌图刷回
// 统一默认图 —— 否则批量换过一次就再也退不回去了。
func TestSyncUserPortraitsRevertsBrandPortraitToUnified(t *testing.T) {
	portraitTestClearEnv(t)
	pool, q, svc, rec, wsID := newPortraitSyncFixture(t)
	suffix := uniqueID("ar")
	kimiID := "kimi_" + suffix
	cleanupPortraitUsers(t, pool, kimiID)

	brand, _ := AgentPortraitURI("kimi")
	seedPortraitUser(t, q, wsID, kimiID, brand)

	outcomes, err := svc.SyncUserPortraits(context.Background(), wsID, BackfillFriendsOptions{})
	if err != nil {
		t.Fatalf("SyncUserPortraits: %v", err)
	}
	var reverted bool
	for _, o := range outcomes {
		if o.RCUser != kimiID {
			continue
		}
		if o.Status != "updated" || o.To != DefaultUserPortraitURI {
			t.Fatalf("品牌图没有被刷回统一默认图: status=%q to=%q", o.Status, o.To)
		}
		reverted = true
	}
	if !reverted {
		t.Fatal("kimi 行没出现在 outcomes 里")
	}
	if !rec.requests("/user/refresh.json")[0].Has("portraitUri") {
		t.Fatal("回滚也要推融云，否则客户端还是旧图")
	}
}

// 融云侧没有这个账号（1002）时：报告 missing、不算失败、不写库、也不擅自补建。
// 这是线上真实形态——`kimi_8594527409` 这类残行在库里存在，融云侧从没注册过。
func TestSyncUserPortraitsAgentBrandMissingUserIsNotAFailure(t *testing.T) {
	portraitTestClearEnv(t)
	pool, q, svc, rec, wsID := newPortraitSyncFixture(t)
	id := "kimi_" + uniqueID("am")
	cleanupPortraitUsers(t, pool, id)
	seedPortraitUser(t, q, wsID, id, DefaultUserPortraitURI)
	rec.markUserNotFound(id)

	outcomes, err := svc.SyncUserPortraits(context.Background(), wsID, BackfillFriendsOptions{AgentPortraits: true})
	if err != nil {
		t.Fatalf("SyncUserPortraits 不该因为 1002 整体失败: %v", err)
	}

	var found bool
	for _, o := range outcomes {
		if o.RCUser != id {
			continue
		}
		found = true
		if o.Status != "missing" {
			t.Fatalf("1002 行的状态 = %q, want missing（不能是 failed）", o.Status)
		}
		if o.Error == "" {
			t.Fatal("missing 行要带原因，否则日志里看不出为什么没换成")
		}
	}
	if !found {
		t.Fatal("目标行没出现在 outcomes 里")
	}

	// 未开 -register-missing：绝不能去建账号。
	if calls := len(rec.requests("/user/getToken.json")); calls != 0 {
		t.Fatalf("默认不该注册缺失账号，实际调了 %d 次 getToken", calls)
	}
	// 库内保持原值（先推融云再落库，推失败就不写）。
	var stored string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(portrait_uri, '') FROM rongcloud_user WHERE rongcloud_user_id = $1`, id).Scan(&stored); err != nil {
		t.Fatalf("read portrait: %v", err)
	}
	if stored != DefaultUserPortraitURI {
		t.Fatalf("库内 portrait 被改了: %q", stored)
	}
}

// 显式开 -register-missing 时：用 getToken 一次的 name+portraitUri 把账号和头像
// 一起建出来，落库，状态记 registered。
func TestSyncUserPortraitsAgentBrandRegistersMissingUserWhenAsked(t *testing.T) {
	portraitTestClearEnv(t)
	pool, q, svc, rec, wsID := newPortraitSyncFixture(t)
	id := "kimi_" + uniqueID("ar")
	cleanupPortraitUsers(t, pool, id)
	seedPortraitUser(t, q, wsID, id, DefaultUserPortraitURI)
	rec.markUserNotFound(id)

	brand, _ := AgentPortraitURI("kimi")
	outcomes, err := svc.SyncUserPortraits(context.Background(), wsID, BackfillFriendsOptions{
		AgentPortraits:  true,
		RegisterMissing: true,
	})
	if err != nil {
		t.Fatalf("SyncUserPortraits: %v", err)
	}

	var found bool
	for _, o := range outcomes {
		if o.RCUser != id {
			continue
		}
		found = true
		if o.Status != "registered" {
			t.Fatalf("补建后的状态 = %q, want registered", o.Status)
		}
	}
	if !found {
		t.Fatal("目标行没出现在 outcomes 里")
	}

	// 只允许一次 getToken，且必须带上品牌头像（否则建出来的账号没图）。
	regs := rec.requests("/user/getToken.json")
	if len(regs) != 1 {
		t.Fatalf("getToken 调用 %d 次，want 1", len(regs))
	}
	if got := regs[0].Get("portraitUri"); got != brand {
		t.Fatalf("getToken 的 portraitUri = %q, want %q", got, brand)
	}
	if got := regs[0].Get("userId"); got != id {
		t.Fatalf("getToken 的 userId = %q, want %q", got, id)
	}

	var stored string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(portrait_uri, '') FROM rongcloud_user WHERE rongcloud_user_id = $1`, id).Scan(&stored); err != nil {
		t.Fatalf("read portrait: %v", err)
	}
	if stored != brand {
		t.Fatalf("库内 portrait = %q, want %q", stored, brand)
	}
}
