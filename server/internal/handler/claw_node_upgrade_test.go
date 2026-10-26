package handler

// 设备 exec 升级的服务端纯逻辑测试。
//
// 大部分用例只测「不需要数据库」的部分：波次放行、代次防抖、白名单/入参、
// token、版本比较、latest 缓存降级。这些规则一旦错了，表现是「批量升级同时下载
// 打爆带宽」「旧代次的失败把新代次覆盖成 failed」「前端传个 URL 就能让设备去下载
// 任意文件」—— 都是必须被钉死的规则。
//
// 例外是文件末尾那三个走真库的用例（S5 / M6 / S3）。它们的 bug 成因就是「只在
// 真库才炸」或「只在真 SQL 的 GROUP BY 里才显现」，mock 出来的行永远是对的 ——
// 所以它们用 TestMain 里那份 pool（DATABASE_URL 不可达时整个 package 会跳过）。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestPlanClawUpgradeWavesSplitsIntoBatches(t *testing.T) {
	machines := []string{"a", "b", "c", "d", "e", "f", "g"}
	waves := planClawUpgradeWaves(machines, 5)

	if len(waves) != len(machines) {
		t.Fatalf("planned %d targets, want %d", len(waves), len(machines))
	}
	want := []int{1, 1, 1, 1, 1, 2, 2}
	for i, wave := range waves {
		if wave.Wave != want[i] {
			t.Fatalf("machine %s in wave %d, want %d", wave.MachineID, wave.Wave, want[i])
		}
	}
	// 波次大小必须是入参决定，不能是「永远 5」：运维要按出口带宽调整。
	onePerWave := planClawUpgradeWaves(machines[:3], 1)
	if onePerWave[0].Wave != 1 || onePerWave[1].Wave != 2 || onePerWave[2].Wave != 3 {
		t.Fatalf("waveSize=1 must serialise machines, got %+v", onePerWave)
	}
}

func TestDedupeClawUpgradeMachinesCollapsesOneMachineWithManyNodes(t *testing.T) {
	// 同机多节点在页面上是多行，批量勾选很容易把一台机器勾两次 —— 那会变成
	// 同一台机器下两个 88MB。
	got := dedupeClawUpgradeMachines([]string{"m1", " m2 ", "m1", "", "m2", "m3"})
	want := []string{"m1", "m2", "m3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dedupe = %v, want %v", got, want)
	}
}

func TestUpgradeWaveOpenOnlyWhenNoEarlierWaveIsActive(t *testing.T) {
	if !upgradeWaveOpen(0) {
		t.Fatal("the first wave must always be open")
	}
	if upgradeWaveOpen(1) {
		t.Fatal("a later wave must wait while an earlier one is still active")
	}
}

func TestUpgradeReportDecisionRejectsStaleAndImpossibleReports(t *testing.T) {
	cases := []struct {
		name       string
		desired    int64
		report     int64
		current    string
		next       string
		wantApply  bool
		wantReason string
	}{
		{"fresh progress", 3, 3, "downloading", "verifying", true, ""},
		{"stale generation from an earlier request", 4, 3, "downloading", "failed", false, "stale_generation"},
		{"no request at all", 0, 0, "", "downloading", false, "stale_generation"},
		{"device cannot write pending itself", 2, 2, "downloading", "pending", false, "unknown_status"},
		{"bogus status", 2, 2, "downloading", "exploded", false, "unknown_status"},
		{"last stage wins", 2, 2, "restarting", "succeeded", true, ""},
		{"a finished row never goes back to downloading", 2, 2, "succeeded", "downloading", false, "already_finished"},
		{"failed then failed again is allowed (retry bookkeeping)", 2, 2, "failed", "failed", true, ""},
		{"a failed row never resurrects to downloading", 2, 2, "failed", "downloading", false, "already_finished"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apply, reason := upgradeReportDecision(tc.desired, tc.report, tc.current, tc.next)
			if apply != tc.wantApply || reason != tc.wantReason {
				t.Fatalf("decision = (%v,%q), want (%v,%q)", apply, reason, tc.wantApply, tc.wantReason)
			}
		})
	}
}

func TestClawUpgradeTerminal(t *testing.T) {
	for _, status := range []string{
		clawUpgradeStatusSucceeded, clawUpgradeStatusFailed, clawUpgradeStatusCancelled,
	} {
		if !clawUpgradeTerminal(status) {
			t.Fatalf("%s must be terminal", status)
		}
	}
	for _, status := range []string{
		"", clawUpgradeStatusPending, clawUpgradeStatusWaitingIdle, clawUpgradeStatusDownloading,
		clawUpgradeStatusVerifying, clawUpgradeStatusRestarting,
	} {
		if clawUpgradeTerminal(status) {
			t.Fatalf("%s must not be terminal", status)
		}
	}
}

func TestCompareClawVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.3.0", "1.2.0", 1},
		{"1.2.0", "1.3.0", -1},
		{"1.2.0", "1.2.0", 0},
		{"1.2.0-beta", "1.2.0", 0},
		{"1.10.0", "1.9.9", 1},
		{"2", "1.9.9", 1},
	}
	for _, tc := range cases {
		if got := compareClawVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("compare(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseClawSHA256Sums(t *testing.T) {
	sums := parseClawSHA256Sums(strings.Join([]string{
		strings.Repeat("a", 64) + "  clawmessenger-windows-x64.exe",
		strings.Repeat("b", 64) + " *clawmessenger-linux-x64",
		"# comment line",
		"",
		"not-a-hash  something",
	}, "\r\n"))

	if sums["clawmessenger-windows-x64.exe"] != strings.Repeat("a", 64) {
		t.Fatalf("windows asset sum = %q", sums["clawmessenger-windows-x64.exe"])
	}
	if sums["clawmessenger-linux-x64"] != strings.Repeat("b", 64) {
		t.Fatalf("star-prefixed entry must be parsed, got %q", sums["clawmessenger-linux-x64"])
	}
	if len(sums) != 2 {
		t.Fatalf("garbage lines must be ignored, got %v", sums)
	}
}

func TestClawUpgradeTokenIsBoundToVersionAndAsset(t *testing.T) {
	t.Setenv("MULTICA_CLAW_UPGRADE_TOKEN_SECRET", "test-secret")
	token := signClawUpgradeToken("1.3.0", clawUpgradeWindowsAsset, time.Now().Add(time.Minute))

	if ok, reason := verifyClawUpgradeToken(token, "1.3.0", clawUpgradeWindowsAsset); !ok {
		t.Fatalf("a freshly signed token must verify, got %q", reason)
	}
	// 换版本 / 换资产 / 篡改签名都必须拒绝 —— 否则一个 token 可以换任何东西。
	if ok, _ := verifyClawUpgradeToken(token, "1.3.1", clawUpgradeWindowsAsset); ok {
		t.Fatal("token must not verify for another version")
	}
	if ok, _ := verifyClawUpgradeToken(token, "1.3.0", "SHA256SUMS"); ok {
		t.Fatal("token must not verify for another asset")
	}
	if ok, _ := verifyClawUpgradeToken(token+"x", "1.3.0", clawUpgradeWindowsAsset); ok {
		t.Fatal("a tampered signature must not verify")
	}
	expired := signClawUpgradeToken("1.3.0", clawUpgradeWindowsAsset, time.Now().Add(-time.Minute))
	if ok, reason := verifyClawUpgradeToken(expired, "1.3.0", clawUpgradeWindowsAsset); ok || reason != "token 已过期" {
		t.Fatalf("expired token = (%v,%q)", ok, reason)
	}
}

func TestClawUpgradeMirrorsPreferGitHubThenServerProxy(t *testing.T) {
	release := &clawUpgradeRelease{
		Version: "1.3.0",
		Tag:     "clawmessenger-v1.3.0",
		Assets:  map[string]string{clawUpgradeWindowsAsset: "https://github.example/asset"},
	}
	mirrors, direct := clawUpgradeMirrors(release, "tok")

	if len(mirrors) != 2 {
		t.Fatalf("mirrors = %v", mirrors)
	}
	if mirrors[0] != "https://github.example/asset" || direct != mirrors[0] {
		t.Fatalf("the direct GitHub URL must come first, got %v", mirrors)
	}
	// 代理是服务端相对路径：设备用它自己的 serverUrl 拼，服务端不需要知道自己的
	// 公网地址。
	if !strings.HasPrefix(mirrors[1], "/api/agent/upgrade/download?") {
		t.Fatalf("proxy mirror must be server-relative, got %q", mirrors[1])
	}
	if !strings.Contains(mirrors[1], "version=1.3.0") || !strings.Contains(mirrors[1], "token=tok") {
		t.Fatalf("proxy mirror must carry version and token, got %q", mirrors[1])
	}
}

// 没配密钥时不允许把代理排进 mirrors：那个端点此刻只会 503，让设备白等一次超时。
func TestClawUpgradeMirrorsDropTheProxyWithoutASecret(t *testing.T) {
	release := &clawUpgradeRelease{
		Version: "1.3.0",
		Tag:     "clawmessenger-v1.3.0",
		Assets:  map[string]string{clawUpgradeWindowsAsset: "https://github.example/asset"},
	}
	mirrors, direct := clawUpgradeMirrors(release, "")
	if len(mirrors) != 1 || mirrors[0] != direct {
		t.Fatalf("without a token only the direct GitHub URL may remain, got %v", mirrors)
	}
}

// fail closed：没有显式配置密钥时，既签不出 token，也验不过任何 token。
//
// 这是审查报告 S2 的落地断言：如果哪天有人把 clawUpgradeTokenSecret 改回「回落
// JWT secret / 源码常量」，这两个断言会立刻红 —— 而那条回落意味着任何人都能自签
// 一个 token 打公网下载端点。
func TestClawUpgradeTokenNeedsItsOwnSecret(t *testing.T) {
	t.Setenv("MULTICA_CLAW_UPGRADE_TOKEN_SECRET", "")
	t.Setenv("MULTICA_CLAW_ADMIN_JWT_SECRET", "some-admin-jwt-secret")

	if secret := clawUpgradeTokenSecret(); secret != nil {
		t.Fatalf("the upgrade token must not borrow another secret, got %q", string(secret))
	}
	if token := signClawUpgradeToken("1.3.0", clawUpgradeWindowsAsset, time.Now().Add(time.Minute)); token != "" {
		t.Fatalf("no secret must mean no token, got %q", token)
	}
	// 用 admin JWT 的密钥签一个「看起来合法」的 token：仍然必须被拒。
	forged := hmacTokenForTest("some-admin-jwt-secret", "1.3.0", clawUpgradeWindowsAsset, time.Now().Add(time.Hour))
	if ok, reason := verifyClawUpgradeToken(forged, "1.3.0", clawUpgradeWindowsAsset); ok {
		t.Fatalf("a token forged with the admin JWT secret must not verify (%q)", reason)
	}
}

// hmacTokenForTest 在测试里自己签一个 token，模拟「攻击者知道某个密钥」。
func hmacTokenForTest(secret, version, asset string, exp time.Time) string {
	payload := fmt.Sprintf("%s|%s|%d", version, asset, exp.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + hex.EncodeToString(mac.Sum(nil))
}

// 下发响应里的 proxy_available 就是 clawUpgradeTokenSecretConfigured() 的取值
// （claw_admin_upgrade.go 的 clawAdminOK 直接透传）。这里钉住「它跟着环境变量走」：
//
// 为什么不能只靠下载端点的 503 用例：没配 secret 时服务端**根本不把代理地址发给
// 设备**（clawUpgradeMirrors 会把它排掉），设备也就不会来请求，那条 503 在
// 「忘配 secret」这个最需要提示的场景里永远打不出来。于是页面必须在下发那一刻
// 就知道「代理没开」，否则运维看到的只有一堆 download_failed（审查报告 M2）。
func TestClawUpgradeProxyAvailableFollowsTheSecret(t *testing.T) {
	t.Setenv("MULTICA_CLAW_UPGRADE_TOKEN_SECRET", "")
	t.Setenv("MULTICA_CLAW_ADMIN_JWT_SECRET", "some-admin-jwt-secret")
	if clawUpgradeTokenSecretConfigured() {
		t.Fatal("without MULTICA_CLAW_UPGRADE_TOKEN_SECRET the proxy must report itself as unavailable — borrowing the admin JWT secret is exactly what S2 forbids")
	}

	t.Setenv("MULTICA_CLAW_UPGRADE_TOKEN_SECRET", "now-configured")
	if !clawUpgradeTokenSecretConfigured() {
		t.Fatal("with an explicit secret the proxy must report itself as available")
	}

	// 只有空白字符等于没配：别让 `export MULTICA_CLAW_UPGRADE_TOKEN_SECRET=` 之后
	// 页面显示「代理可用」，然后设备拿到一堆 503。
	t.Setenv("MULTICA_CLAW_UPGRADE_TOKEN_SECRET", "   ")
	if clawUpgradeTokenSecretConfigured() {
		t.Fatal("a blank secret must be treated as unconfigured")
	}
}

func TestClawUpgradePlatformSupported(t *testing.T) {
	if !clawUpgradePlatformSupported(clawUpgradePlatformWindows) {
		t.Fatal("win32-x64 must be supported")
	}
	// 空 = 还没上报过平台（多半是 1.3.0 之前的版本，它根本没有领取升级指令的代
	// 码）。**必须判成不支持**：放它进波次会让这一行永远 pending，而波次闸会把
	// 后续所有机器一起堵死（审查报告 S3）。页面上改成「平台未知，先手动装一次」。
	if clawUpgradePlatformSupported("") {
		t.Fatal("an empty platform must fail closed, not be treated as upgradable")
	}
	for _, platform := range []string{"linux-x64", "darwin-arm64", "win32-arm64", "WIN32-X64"} {
		if clawUpgradePlatformSupported(platform) {
			t.Fatalf("%s must not be upgradable in this phase", platform)
		}
	}
}

// 全部被跳过时，400 的文案必须说清「为什么」，否则管理员只会看到「都不需要升级」。
func TestClawUpgradeSkipSummaryExplainsEveryReason(t *testing.T) {
	if got := clawUpgradeSkipSummary(nil); got != "没有可下发的设备" {
		t.Fatalf("empty summary = %q", got)
	}
	got := clawUpgradeSkipSummary([]clawUpgradeSkip{
		{MachineID: "a", Reason: "platform_unknown"},
		{MachineID: "b", Reason: "platform_unknown"},
		{MachineID: "c", Reason: "already_current"},
	})
	if !strings.Contains(got, "旧版本") || !strings.Contains(got, "2 台") {
		t.Fatalf("summary must name the reason and the count, got %q", got)
	}
	if !strings.Contains(got, "已是最新版本") {
		t.Fatalf("summary must mention already_current, got %q", got)
	}
	// 未知原因也不能吞掉（只列前三类是因为一条 400 消息不该变成表格）。
	if got := clawUpgradeSkipSummary([]clawUpgradeSkip{{MachineID: "x", Reason: "weird"}}); !strings.Contains(got, "weird") {
		t.Fatalf("unknown reasons must still show up, got %q", got)
	}
}

// 波次租约：一台永远不来领的机器不能在闸门上无限期地挡着后面的波次。
func TestClawUpgradeBlocksWaveOnlyWhileYoungAndUnfinished(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		status      string
		requestedAt time.Time
		want        bool
	}{
		{"pending and fresh blocks", clawUpgradeStatusPending, now.Add(-time.Minute), true},
		{"downloading and fresh blocks", clawUpgradeStatusDownloading, now.Add(-9 * time.Minute), true},
		{"waiting_idle and fresh blocks", clawUpgradeStatusWaitingIdle, now.Add(-time.Second), true},
		{"pending but past the lease stops blocking", clawUpgradeStatusPending, now.Add(-11 * time.Minute), false},
		{"downloading past the lease stops blocking", clawUpgradeStatusDownloading, now.Add(-time.Hour), false},
		{"succeeded never blocks", clawUpgradeStatusSucceeded, now.Add(-time.Second), false},
		{"failed never blocks", clawUpgradeStatusFailed, now.Add(-time.Second), false},
		{"cancelled never blocks", clawUpgradeStatusCancelled, now.Add(-time.Second), false},
		{"no status at all never blocks", "", now.Add(-time.Second), false},
		{"unknown requested_at blocks conservatively", clawUpgradeStatusPending, time.Time{}, true},
	}
	for _, tc := range cases {
		if got := clawUpgradeBlocksWave(tc.status, tc.requestedAt, now); got != tc.want {
			t.Fatalf("%s: clawUpgradeBlocksWave(%q) = %v, want %v", tc.name, tc.status, got, tc.want)
		}
	}
	// 闸门本身的语义不能变：只要有机器在挡，本波就不放行。
	if upgradeWaveOpen(1) || !upgradeWaveOpen(0) {
		t.Fatal("upgradeWaveOpen must only open when nothing blocks")
	}
}

func TestDecodeClawUpgradeRequestRejectsAnythingButMachineListAndForce(t *testing.T) {
	// 白名单的入参侧：前端只能表达「这几台机器，强制与否」。
	ok := httptest.NewRequest(http.MethodPost, "/api/admin/nodes/upgrade",
		strings.NewReader(`{"machine_ids":["m1","m2"],"force":true}`))
	body, err := decodeClawUpgradeRequest(ok)
	if err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
	if len(body.MachineIDs) != 2 || !body.Force {
		t.Fatalf("decoded = %+v", body)
	}

	for _, payload := range []string{
		`{"machine_ids":["m1"],"url":"http://evil.example/x.exe"}`,
		`{"machine_ids":["m1"],"version":"0.0.0"}`,
		`{"machine_ids":["m1"],"mirrors":["http://evil.example/x.exe"]}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/nodes/upgrade", strings.NewReader(payload))
		if _, err := decodeClawUpgradeRequest(req); err == nil {
			t.Fatalf("payload %s must be rejected, not silently ignored", payload)
		}
	}
}

// fakeGitHub 是一个最小可用的 releases/latest 后端，用于验证缓存与降级语义。
func fakeGitHub(t *testing.T, version string, fail *atomic.Bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail != nil && fail.Load() {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"tag_name": "clawmessenger-v" + version,
				"assets": []map[string]string{
					{"name": clawUpgradeWindowsAsset, "browser_download_url": "https://github.example/bin"},
					{"name": "SHA256SUMS", "browser_download_url": "http://" + r.Host + "/sums"},
				},
			})
		case r.URL.Path == "/sums":
			_, _ = w.Write([]byte(strings.Repeat("d", 64) + "  " + clawUpgradeWindowsAsset + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func TestResolveClawUpgradeLatestCachesAndDegradesToTheLastKnownVersion(t *testing.T) {
	clawUpgradeResetCacheForTest()
	defer clawUpgradeResetCacheForTest()

	var failing atomic.Bool
	server, hits := fakeGitHub(t, "1.3.0", &failing)
	t.Setenv("CLAW_UPGRADE_RELEASE_API", server.URL)
	t.Setenv("CLAW_UPGRADE_RELEASE_WEB", "https://github.example")

	ctx := context.Background()
	release, stale, err := resolveClawUpgradeLatest(ctx)
	if err != nil {
		t.Fatalf("first resolve failed: %v", err)
	}
	if release.Version != "1.3.0" || stale {
		t.Fatalf("release = %q stale=%v", release.Version, stale)
	}
	if release.SHA256[clawUpgradeWindowsAsset] != strings.Repeat("d", 64) {
		t.Fatalf("sha256 not carried over: %v", release.SHA256)
	}
	afterFirst := hits.Load()

	// 第二次必须命中缓存，不能再打 GitHub。
	if _, _, err := resolveClawUpgradeLatest(ctx); err != nil {
		t.Fatalf("cached resolve failed: %v", err)
	}
	if hits.Load() != afterFirst {
		t.Fatalf("second resolve hit GitHub again (%d -> %d)", afterFirst, hits.Load())
	}

	// 缓存过期 + GitHub 挂掉 = 用上次已知值并标记 stale，绝不假装成功。
	clawUpgradeLatest.mu.Lock()
	clawUpgradeLatest.cachedAt = time.Now().Add(-2 * clawUpgradeReleaseTTL)
	clawUpgradeLatest.mu.Unlock()
	failing.Store(true)

	degraded, degradedStale, err := resolveClawUpgradeLatest(ctx)
	if err != nil {
		t.Fatalf("degraded resolve must not fail when a previous value exists: %v", err)
	}
	if !degradedStale || degraded.Version != "1.3.0" {
		t.Fatalf("degraded = %q stale=%v", degraded.Version, degradedStale)
	}
}

func TestResolveClawUpgradeLatestFailsLoudlyWithoutAnyCache(t *testing.T) {
	clawUpgradeResetCacheForTest()
	defer clawUpgradeResetCacheForTest()

	var failing atomic.Bool
	failing.Store(true)
	server, _ := fakeGitHub(t, "1.3.0", &failing)
	t.Setenv("CLAW_UPGRADE_RELEASE_API", server.URL)

	_, _, err := resolveClawUpgradeLatest(context.Background())
	if err == nil {
		t.Fatal("resolve must fail when GitHub is unreachable and nothing is cached")
	}
	if !strings.Contains(err.Error(), "无法获取最新版本") {
		t.Fatalf("error must say the latest version is unavailable, got %v", err)
	}
}

func TestMachineIDForDeviceFallsBackToTheRongCloudID(t *testing.T) {
	// 与 AgentNodesForDevice 的机器键推导必须完全一致，否则版本行和列表行会落在
	// 两台「机器」上。
	if got := machineIDForDevice(dbNodeForTest("3300776275", "opencode_3300776275")); got != "3300776275" {
		t.Fatalf("machine id = %q", got)
	}
	if got := machineIDForDevice(dbNodeForTest("", "rc_node_7654321")); got != "7654321" {
		t.Fatalf("fallback machine id = %q", got)
	}
}

// ---- 真库用例（S5 / M6 / S3）------------------------------------------------
//
// 这一节的三条用例必须打真库：S5 的成因是 PostgreSQL 没有 min(uuid) 聚合，
// mock 出来的行永远「能取到」；S3 的成因是非 win32 机器占着波次不放行，而
// 「哪台机器被放进了波次」这件事只有把 resolve 和真表连起来看才成立。

// clawUpgradeTestMachine 造一个带时间戳的机器键：清理只按这些键删，绝不动
// 库里既有的行（这台库上还有别的测试在跑）。
func clawUpgradeTestMachine(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// insertClawUpgradeTestNode 造一行 rongcloud_node。machineID 为空时机器键会落到
// `rc_node_` 前缀的兜底分支（M6 的一致性）。
func insertClawUpgradeTestNode(t *testing.T, workspaceID, machineID, rcUserID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO rongcloud_node (workspace_id, owner_user_id, rongcloud_user_id, node_id, machine_id)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5)`,
		workspaceID, testUserID, rcUserID, rcUserID, machineID); err != nil {
		t.Fatalf("insert rongcloud_node %s: %v", rcUserID, err)
	}
}

// insertClawUpgradeTestReport 造一行「设备已上报版本/平台」的升级行。
func insertClawUpgradeTestReport(t *testing.T, workspaceID, machineID, platform, version string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO rongcloud_node_upgrade
			(machine_id, workspace_id, reported_platform, reported_version, reported_at)
		VALUES ($1, $2::uuid, $3, $4, now())`,
		machineID, workspaceID, platform, version); err != nil {
		t.Fatalf("insert rongcloud_node_upgrade %s: %v", machineID, err)
	}
}

// cleanupClawUpgradeTestRows 只删本用例造的行（按机器键 + 融云 id）。
func cleanupClawUpgradeTestRows(t *testing.T, machineIDs, rcUserIDs []string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM rongcloud_node_upgrade WHERE machine_id = ANY($1)`, machineIDs)
		_, _ = testPool.Exec(ctx, `DELETE FROM rongcloud_node WHERE rongcloud_user_id = ANY($1)`, rcUserIDs)
	})
}

// TestClawAdminMachineWorkspacesTakesOneWorkspacePerMachine 钉住 S5：
// 同一 machine_id 在**两个 workspace** 下各有一行节点时，取 workspace 的查询必须
// 正常返回一个非空 uuid。原来的 `min(workspace_id)` 在 PG 上直接报
// 「function min(uuid) does not exist」，整个下发改 500。
func TestClawAdminMachineWorkspacesTakesOneWorkspacePerMachine(t *testing.T) {
	if testPool == nil || testHandler == nil {
		t.Skip("handler test database is not available")
	}
	ctx := context.Background()
	machine := clawUpgradeTestMachine("upgrade-s5")
	legacy := clawUpgradeTestMachine("upgrade-s5-legacy")
	rcA := "rc_node_" + machine + "-a"
	rcB := "rc_node_" + machine + "-b"
	rcLegacy := "rc_node_" + legacy
	cleanupClawUpgradeTestRows(t, []string{machine, legacy}, []string{rcA, rcB, rcLegacy})

	secondWorkspace := dbfx.Workspace(t, "claw upgrade second workspace", "claw-upgrade-"+machine)
	insertClawUpgradeTestNode(t, testWorkspaceID, machine, rcA)
	insertClawUpgradeTestNode(t, secondWorkspace, machine, rcB)
	// 没有 machine_id 的老行：机器键必须由 `rc_node_` 前缀兜底推出来。
	insertClawUpgradeTestNode(t, testWorkspaceID, "", rcLegacy)

	// 先把根因钉死：PostgreSQL 的 min 没有 uuid 版本，原写法在这台库上确实报错。
	var sink string
	if err := testPool.QueryRow(ctx,
		`SELECT min(workspace_id)::text FROM rongcloud_node WHERE machine_id = $1`, machine,
	).Scan(&sink); err == nil {
		t.Fatalf("min(uuid) unexpectedly worked — the S5 query is worth re-checking against this PostgreSQL")
	}

	got, err := testHandler.clawAdminMachineWorkspaces(ctx, []string{machine, legacy})
	if err != nil {
		t.Fatalf("clawAdminMachineWorkspaces: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 machine keys, got %d: %#v", len(got), got)
	}
	for _, key := range []string{machine, legacy} {
		if strings.TrimSpace(got[key]) == "" {
			t.Fatalf("machine %q resolved to an empty workspace: %#v", key, got)
		}
	}
	if got[machine] != testWorkspaceID && got[machine] != secondWorkspace {
		t.Fatalf("machine %q resolved to an unrelated workspace %q", machine, got[machine])
	}
	if got[legacy] != testWorkspaceID {
		t.Fatalf("the rc_node_ fallback machine key resolved to %q, want %q", got[legacy], testWorkspaceID)
	}
}

// TestClawUpgradeResolveSkipsUnsupportedPlatformSoItCannotStallLaterWaves 钉住 S3：
// 一个批次里混进一台非 win32 的机器，它**不参与**波次放行 —— 后面那波照常升完。
func TestClawUpgradeResolveSkipsUnsupportedPlatformSoItCannotStallLaterWaves(t *testing.T) {
	if testPool == nil || testHandler == nil {
		t.Skip("handler test database is not available")
	}
	ctx := context.Background()
	machineA := clawUpgradeTestMachine("upgrade-s3-a") // win32-x64，会进第 1 波
	machineB := clawUpgradeTestMachine("upgrade-s3-b") // linux-x64，必须被跳过
	machineC := clawUpgradeTestMachine("upgrade-s3-c") // win32-x64，必须能进第 2 波
	rcs := []string{"rc_node_" + machineA, "rc_node_" + machineB, "rc_node_" + machineC}
	cleanupClawUpgradeTestRows(t, []string{machineA, machineB, machineC}, rcs)

	for _, item := range []struct{ machine, platform, rc string }{
		{machineA, clawUpgradePlatformWindows, rcs[0]},
		{machineB, "linux-x64", rcs[1]},
		{machineC, clawUpgradePlatformWindows, rcs[2]},
	} {
		insertClawUpgradeTestNode(t, testWorkspaceID, item.machine, item.rc)
		insertClawUpgradeTestReport(t, testWorkspaceID, item.machine, item.platform, "1.2.0")
	}

	targets, skips, err := testHandler.clawAdminResolveUpgradeTargets(ctx,
		[]string{machineA, machineB, machineC}, "1.3.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(targets) != 2 || targets[0].MachineID != machineA || targets[1].MachineID != machineC {
		t.Fatalf("linux machine must not enter the wave plan, got %#v", machineIDsOf(targets))
	}
	if len(skips) != 1 || skips[0].MachineID != machineB || skips[0].Reason != "platform_unsupported" {
		t.Fatalf("want one platform_unsupported skip for %s, got %#v", machineB, skips)
	}

	// waveSize=1：A 在第 1 波、C 在第 2 波。A 报成功之后第 2 波必须放行 ——
	// 挡住它的不可能是 B（B 根本不在计划里），只可能是 A 还没结束。
	waves := planClawUpgradeWaves(machineIDsOf(targets), 1)
	if len(waves) != 2 || waves[0].Wave != 1 || waves[1].Wave != 2 {
		t.Fatalf("wave plan = %#v", waves)
	}
	now := time.Now()
	if clawUpgradeBlocksWave(clawUpgradeStatusSucceeded, now, now) {
		t.Fatalf("a finished wave-1 machine must not block wave 2")
	}
	if !clawUpgradeBlocksWave(clawUpgradeStatusPending, now, now) {
		t.Fatalf("a fresh pending wave-1 machine must block wave 2 (otherwise the gate is a no-op)")
	}

	// 反向：只有 B 的时候也必须被跳过，与「有没有 A/C 在旁边」无关。
	alone, aloneSkips, err := testHandler.clawAdminResolveUpgradeTargets(ctx, []string{machineB}, "1.3.0")
	if err != nil {
		t.Fatalf("resolve single: %v", err)
	}
	if len(alone) != 0 || len(aloneSkips) != 1 || aloneSkips[0].Reason != "platform_unsupported" {
		t.Fatalf("a lone linux machine must be skipped, got targets=%v skips=%#v", machineIDsOf(alone), aloneSkips)
	}

	// 反向：去掉干扰项后 A、C 仍然都被接受（证明不是「因为 B 存在才通过」）。
	pair, pairSkips, err := testHandler.clawAdminResolveUpgradeTargets(ctx,
		[]string{machineA, machineC}, "1.3.0")
	if err != nil {
		t.Fatalf("resolve pair: %v", err)
	}
	if len(pair) != 2 || len(pairSkips) != 0 {
		t.Fatalf("without the interferer both machines must be accepted, got %v skipped=%#v",
			machineIDsOf(pair), pairSkips)
	}
}

// dbNodeForTest 只填升级键推导需要的两个字段。
func dbNodeForTest(machineID, rcUserID string) db.RongcloudNode {
	var node db.RongcloudNode
	node.MachineID = machineID
	node.RongcloudUserID = rcUserID
	return node
}

// TestClawUpgradeProgressKeepsRequestedAtAcrossHeartbeats 钉住 R1。
//
// 设备每 10 秒轮询一次，每次都会走 `clawDeviceRecordUpgradeState` 把 `updated_at`
// 刷成 now()。管理端的「等待空闲（已等待 N 分钟）」如果读 `updated_at`，算出来永远
// 是 0 —— 一台已经等了 43 分钟的机器会显示「刚刚开始」（Vic 用真 PG 复现过）。
// 所以进度接口必须另外透出 `requested_at`（本次下发开始的时间）。
//
// 这条用例真跑一次心跳 upsert，断言 updated_at 变了而 requested_at 没变。
func TestClawUpgradeProgressKeepsRequestedAtAcrossHeartbeats(t *testing.T) {
	if testPool == nil || testHandler == nil {
		t.Skip("handler test database is not available")
	}
	ctx := context.Background()
	machine := clawUpgradeTestMachine("upgrade-r1")
	rc := "rc_node_" + machine
	cleanupClawUpgradeTestRows(t, []string{machine}, []string{rc})
	insertClawUpgradeTestNode(t, testWorkspaceID, machine, rc)
	insertClawUpgradeTestReport(t, testWorkspaceID, machine, clawUpgradePlatformWindows, "1.2.0")

	// 43 分钟前下发的，一直在等空闲。
	if _, err := testPool.Exec(ctx, `
		UPDATE rongcloud_node_upgrade
		   SET target_version = '1.3.1', desired_generation = 2, status = $2,
		       batch_id = 'upg-r1', wave = 1,
		       requested_at = now() - interval '43 minutes',
		       updated_at = now() - interval '43 minutes'
		 WHERE machine_id = $1`, machine, clawUpgradeStatusWaitingIdle); err != nil {
		t.Fatalf("seed upgrade row: %v", err)
	}

	up, found, err := testHandler.clawNodeUpgradeByMachine(ctx, machine)
	if err != nil || !found {
		t.Fatalf("read upgrade row: found=%v err=%v", found, err)
	}
	before := clawAdminUpgradeProgressFor(up)
	if before.RequestedAt == "" {
		t.Fatal("the progress payload must expose requested_at — the admin page needs it to say how long it has been waiting")
	}
	since, err := time.Parse(time.RFC3339, before.RequestedAt)
	if err != nil {
		t.Fatalf("requested_at is not RFC3339 (%q): %v", before.RequestedAt, err)
	}
	if minutes := int(time.Since(since).Minutes()); minutes < 42 || minutes > 44 {
		t.Fatalf("requested_at must be ~43 minutes old, got %d minutes", minutes)
	}

	// 真跑一次心跳上报（generation=0 的那条 10 秒轮询）。
	var node db.RongcloudNode
	if err := testPool.QueryRow(ctx, `
		SELECT id, workspace_id, rongcloud_user_id, machine_id
		  FROM rongcloud_node WHERE machine_id = $1`, machine).
		Scan(&node.ID, &node.WorkspaceID, &node.RongcloudUserID, &node.MachineID); err != nil {
		t.Fatalf("load node: %v", err)
	}
	if err := testHandler.clawDeviceRecordUpgradeState(ctx, node, "1.2.0", clawUpgradePlatformWindows, 0); err != nil {
		t.Fatalf("heartbeat upsert: %v", err)
	}

	up2, _, err := testHandler.clawNodeUpgradeByMachine(ctx, machine)
	if err != nil {
		t.Fatalf("read upgrade row again: %v", err)
	}
	after := clawAdminUpgradeProgressFor(up2)
	if after.RequestedAt != before.RequestedAt {
		t.Fatalf("a heartbeat must not move requested_at (%q -> %q) — otherwise the page loses the wait time",
			before.RequestedAt, after.RequestedAt)
	}
	if after.UpdatedAt == before.UpdatedAt {
		t.Fatal("the heartbeat must move updated_at, otherwise this test proves nothing")
	}
}

// TestClawUpgradeAlreadyInstalledIsClosedByTheServer 钉住 R5。
//
// 设备上报的版本已经等于 target_version 时，服务端**不再**下发 88MB（这条不能退：
// 否则每次轮询都白下一次）。但原来的 `return nil, nil` 什么都不做 —— 一旦设备那次
// succeeded 回传丢了（账本写坏、断电、进程被杀），这一行会永久停在
// downloading/restarting，且不会自愈：页面上就是「卡住了」。
// 所以服务端要在这个短路分支里自己把状态推进到 succeeded。
func TestClawUpgradeAlreadyInstalledIsClosedByTheServer(t *testing.T) {
	if testPool == nil || testHandler == nil {
		t.Skip("handler test database is not available")
	}
	ctx := context.Background()
	machine := clawUpgradeTestMachine("upgrade-r5")
	rc := "rc_node_" + machine
	cleanupClawUpgradeTestRows(t, []string{machine}, []string{rc})
	insertClawUpgradeTestNode(t, testWorkspaceID, machine, rc)
	insertClawUpgradeTestReport(t, testWorkspaceID, machine, clawUpgradePlatformWindows, "1.3.0")
	node := dbNodeForTest(machine, rc)

	// 三种「进行中」的状态都必须被收尾：卡在 downloading、卡在 restarting（交换成功
	// 但账本丢了就是这个）、以及还没开始的 pending。
	for _, status := range []string{
		clawUpgradeStatusPending,
		clawUpgradeStatusDownloading,
		clawUpgradeStatusRestarting,
	} {
		if _, err := testPool.Exec(ctx, `
			UPDATE rongcloud_node_upgrade
			   SET target_version = '1.3.0', desired_generation = 5, status = $2,
			       error_code = '', error_detail = '',
			       batch_id = 'upg-r5', wave = 1, requested_at = now()
			 WHERE machine_id = $1`, machine, status); err != nil {
			t.Fatalf("seed upgrade row (%s): %v", status, err)
		}

		intent, err := testHandler.clawDeviceUpgradeIntent(ctx, node, "1.3.0")
		if err != nil {
			t.Fatalf("intent (%s): %v", status, err)
		}
		if intent != nil {
			t.Fatalf("an already-installed machine must not be asked to download again (%s), got %+v", status, intent)
		}

		up, found, err := testHandler.clawNodeUpgradeByMachine(ctx, machine)
		if err != nil || !found {
			t.Fatalf("read back (%s): found=%v err=%v", status, found, err)
		}
		if up.Status != clawUpgradeStatusSucceeded {
			t.Fatalf("status %q must be closed to succeeded by the server, got %q", status, up.Status)
		}
	}

	// 已经是终态的不要重复改：再调一次，updated_at 必须原样不动。
	up, _, err := testHandler.clawNodeUpgradeByMachine(ctx, machine)
	if err != nil {
		t.Fatalf("read back terminal row: %v", err)
	}
	if _, err := testHandler.clawDeviceUpgradeIntent(ctx, node, "1.3.0"); err != nil {
		t.Fatalf("intent on a terminal row: %v", err)
	}
	again, _, err := testHandler.clawNodeUpgradeByMachine(ctx, machine)
	if err != nil {
		t.Fatalf("read back terminal row again: %v", err)
	}
	if !again.UpdatedAt.Equal(up.UpdatedAt) {
		t.Fatalf("a terminal row must not be touched again (%v -> %v)", up.UpdatedAt, again.UpdatedAt)
	}
}

// TestClawUpgradeDoesNotMarkInstalledWhenTheDeviceReportsAnOldVersion 是 R5 的**反向**
// 用例：设备报 1.2.0、目标是 1.3.0 —— 绝不能因为「服务端自己会收尾」就把这一行标成
// succeeded。把没升级的标成完成，比让它卡着更糟（管理员会以为已经推完了）。
//
// 版本比较只用**本次请求里的 currentVersion**，所以这条同时也是「不要去读库里的
// reported_version」的断言：库里那一列会被 10 秒心跳（generation=0）并发写。
func TestClawUpgradeDoesNotMarkInstalledWhenTheDeviceReportsAnOldVersion(t *testing.T) {
	if testPool == nil || testHandler == nil {
		t.Skip("handler test database is not available")
	}
	ctx := context.Background()
	machine := clawUpgradeTestMachine("upgrade-r5-old")
	rc := "rc_node_" + machine
	cleanupClawUpgradeTestRows(t, []string{machine}, []string{rc})
	insertClawUpgradeTestNode(t, testWorkspaceID, machine, rc)
	// 库里 reported_version 先写成目标版本（模拟心跳把它刷成了新的），本次请求却报
	// 1.2.0 —— 只有「用请求里的版本」才会判成「还没升」。
	insertClawUpgradeTestReport(t, testWorkspaceID, machine, clawUpgradePlatformWindows, "1.3.0")

	if _, err := testPool.Exec(ctx, `
		UPDATE rongcloud_node_upgrade
		   SET target_version = '1.3.0', desired_generation = 9, status = $2,
		       batch_id = 'upg-r5-old', wave = 1, requested_at = now()
		 WHERE machine_id = $1`, machine, clawUpgradeStatusRestarting); err != nil {
		t.Fatalf("seed upgrade row: %v", err)
	}

	// 别让这条用例真的去打 GitHub：latest 解析失败只会让 intent 返回 nil，行不动。
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(api.Close)
	t.Setenv("CLAW_UPGRADE_RELEASE_API", api.URL)

	before, _, err := testHandler.clawNodeUpgradeByMachine(ctx, machine)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	if _, err := testHandler.clawDeviceUpgradeIntent(ctx, dbNodeForTest(machine, rc), "1.2.0"); err != nil {
		t.Fatalf("intent: %v", err)
	}
	after, _, err := testHandler.clawNodeUpgradeByMachine(ctx, machine)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if after.Status == clawUpgradeStatusSucceeded {
		t.Fatalf("a device that still reports 1.2.0 must never be marked succeeded (target was 1.3.0)")
	}
	if after.Status != before.Status || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("a not-yet-upgraded row must be left untouched (%q -> %q, %v -> %v)",
			before.Status, after.Status, before.UpdatedAt, after.UpdatedAt)
	}
}
