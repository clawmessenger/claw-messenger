package handler

// 下载代理端点的用例 —— 这个端点此前是**零覆盖**（验证报告 §8.2 的变异 M6：删掉
// 资产白名单，本文件之前的用例全绿），而它是整个功能里唯一公网可达、无 admin 鉴权
// 的入口。所以这里既覆盖正常路径（内容逐字节一致、缓存只回源一次、Range 续传），
// 也把「白名单/签名/过期/缺密钥」全部钉死。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeReleaseWeb 起一个假的 release 站点，只服务
// `/<repo>/releases/download/<tag>/<asset>`，并统计回源次数。
func fakeReleaseWeb(t *testing.T, asset string, payload []byte) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		want := fmt.Sprintf("/%s/releases/download/%s/%s",
			clawUpgradeReleasesRepo, clawUpgradeTagForVersion("1.3.0"), asset)
		if r.URL.Path != want {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, asset, time.Now(), bytes.NewReader(payload))
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

// downloadRequest 直接构造 handler 请求（handler 不读 *Handler 的任何字段）。
func downloadRequest(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agent/upgrade/download?"+query, nil)
	(&Handler{}).ClawUpgradeDownload(rec, req)
	return rec
}

func TestClawUpgradeDownloadServesVerifiedBytesAndCachesOnce(t *testing.T) {
	payload := bytes.Repeat([]byte("clawmessenger-upgrade-payload\n"), 64)
	server, hits := fakeReleaseWeb(t, clawUpgradeWindowsAsset, payload)
	t.Setenv("CLAW_UPGRADE_RELEASE_WEB", server.URL)
	t.Setenv("CLAW_UPGRADE_CACHE_DIR", t.TempDir())
	t.Setenv("MULTICA_CLAW_UPGRADE_TOKEN_SECRET", "test-secret")
	clawUpgradeResetCacheForTest()
	t.Cleanup(clawUpgradeResetCacheForTest)

	token := signClawUpgradeToken("1.3.0", clawUpgradeWindowsAsset, time.Now().Add(time.Minute))
	query := fmt.Sprintf("version=1.3.0&asset=%s&token=%s", clawUpgradeWindowsAsset, token)

	rec := downloadRequest(t, query)
	if rec.Code != http.StatusOK {
		t.Fatalf("a valid token must serve the asset, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("served %d bytes that do not match the upstream asset", rec.Body.Len())
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("first request must fetch upstream exactly once, got %d", got)
	}

	// 同版本第二次请求：直接吃盘上的缓存，不再回源（88MB × N 台靠的就是这条）。
	if rec := downloadRequest(t, query); rec.Code != http.StatusOK {
		t.Fatalf("cached download failed: %d", rec.Code)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("second request must be served from the cache, upstream hits = %d", got)
	}

	// Range：设备断线重试要从断点续传（缓存已在盘上，不必回源）。
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agent/upgrade/download?"+query, nil)
	req.Header.Set("Range", "bytes=10-19")
	(&Handler{}).ClawUpgradeDownload(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("a Range request must answer 206, got %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload[10:20]) {
		t.Fatalf("Range body = %q, want %q", rec.Body.Bytes(), payload[10:20])
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("a Range hit must not re-fetch upstream, hits = %d", got)
	}
}

func TestClawUpgradeDownloadRejectsEverythingWithoutAValidToken(t *testing.T) {
	payload := []byte("payload")
	server, hits := fakeReleaseWeb(t, clawUpgradeWindowsAsset, payload)
	t.Setenv("CLAW_UPGRADE_RELEASE_WEB", server.URL)
	t.Setenv("CLAW_UPGRADE_CACHE_DIR", t.TempDir())
	t.Setenv("MULTICA_CLAW_UPGRADE_TOKEN_SECRET", "test-secret")
	clawUpgradeResetCacheForTest()
	t.Cleanup(clawUpgradeResetCacheForTest)

	good := signClawUpgradeToken("1.3.0", clawUpgradeWindowsAsset, time.Now().Add(time.Minute))
	otherVersion := signClawUpgradeToken("1.3.1", clawUpgradeWindowsAsset, time.Now().Add(time.Minute))
	expired := signClawUpgradeToken("1.3.0", clawUpgradeWindowsAsset, time.Now().Add(-time.Minute))
	forged := hmacTokenForTest("not-the-server-secret", "1.3.0", clawUpgradeWindowsAsset, time.Now().Add(time.Hour))

	cases := []struct {
		name   string
		query  string
		status int
	}{
		{"no token at all", "version=1.3.0&asset=" + clawUpgradeWindowsAsset, http.StatusForbidden},
		{"version=latest", "version=latest&asset=" + clawUpgradeWindowsAsset + "&token=" + good, http.StatusBadRequest},
		{"version traversal", "version=../1.3.0&asset=" + clawUpgradeWindowsAsset + "&token=" + good, http.StatusBadRequest},
		{"version is a URL", "version=" + "http%3A%2F%2Fevil.example&asset=" + clawUpgradeWindowsAsset + "&token=" + good, http.StatusBadRequest},
		{"asset traversal", "version=1.3.0&asset=../SHA256SUMS&token=" + good, http.StatusBadRequest},
		{"another asset", "version=1.3.0&asset=SHA256SUMS&token=" + good, http.StatusBadRequest},
		{"token bound to another version", "version=1.3.0&asset=" + clawUpgradeWindowsAsset + "&token=" + otherVersion, http.StatusForbidden},
		{"expired token", "version=1.3.0&asset=" + clawUpgradeWindowsAsset + "&token=" + expired, http.StatusForbidden},
		{"forged token", "version=1.3.0&asset=" + clawUpgradeWindowsAsset + "&token=" + forged, http.StatusForbidden},
	}
	for _, tc := range cases {
		rec := downloadRequest(t, tc.query)
		if rec.Code != tc.status {
			t.Fatalf("%s: got %d (%s), want %d", tc.name, rec.Code, strings.TrimSpace(rec.Body.String()), tc.status)
		}
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("no rejected request may reach upstream, hits = %d", got)
	}
	// 拒绝之后缓存目录里也不该留下任何东西（否则可被用来写满磁盘）。
	entries, err := os.ReadDir(os.Getenv("CLAW_UPGRADE_CACHE_DIR"))
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a rejected download must not cache anything, found %d entries", len(entries))
	}
}

// fail closed：没配密钥时端点必须 503（而不是「不校验 token」或「回落到别的密钥」）。
func TestClawUpgradeDownloadIsDisabledWithoutASecret(t *testing.T) {
	server, hits := fakeReleaseWeb(t, clawUpgradeWindowsAsset, []byte("payload"))
	t.Setenv("CLAW_UPGRADE_RELEASE_WEB", server.URL)
	t.Setenv("CLAW_UPGRADE_CACHE_DIR", t.TempDir())
	t.Setenv("MULTICA_CLAW_UPGRADE_TOKEN_SECRET", "")
	t.Setenv("MULTICA_CLAW_ADMIN_JWT_SECRET", "some-admin-jwt-secret")
	clawUpgradeResetCacheForTest()
	t.Cleanup(clawUpgradeResetCacheForTest)

	// 就算攻击者用 admin JWT 的密钥自签一个 token，也不能把代理叫起来。
	forged := hmacTokenForTest("some-admin-jwt-secret", "1.3.0", clawUpgradeWindowsAsset, time.Now().Add(time.Hour))
	rec := downloadRequest(t, "version=1.3.0&asset="+clawUpgradeWindowsAsset+"&token="+forged)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a server without the upgrade secret must answer 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("a disabled proxy must never reach upstream, hits = %d", got)
	}
	// 运维要能从响应里一眼认出「不是 token 不对，是没配密钥」：403 与 503 的
	// 处置完全不同（前者查攻击/时钟，后者去补环境变量）。
	if body := rec.Body.String(); !strings.Contains(body, clawUpgradeProxyNotConfigured) {
		t.Fatalf("the 503 must carry the %s code, got %q", clawUpgradeProxyNotConfigured, body)
	}
	// 显式配了密钥 → 同一个端点立刻可用（这是上面那条 503 的反面）。
	t.Setenv("MULTICA_CLAW_UPGRADE_TOKEN_SECRET", "now-configured")
	token := signClawUpgradeToken("1.3.0", clawUpgradeWindowsAsset, time.Now().Add(time.Minute))
	ok := downloadRequest(t, "version=1.3.0&asset="+clawUpgradeWindowsAsset+"&token="+token)
	if ok.Code != http.StatusOK {
		t.Fatalf("with the secret configured the proxy must serve, got %d: %s", ok.Code, ok.Body.String())
	}
}

// ---- 校验和强制点（clawUpgradeExpectedSum）的端到端用例 --------------------
//
// 为什么这三条现在必须有：digest 兜底上线前，「没有 SHA256SUMS 的版本」根本
// 下发不出去，`upgrade_download.go:204` 那次校验几乎从不触发；上线后这类版本会
// 正常下发，于是它从「几乎不触发」变成「经常触发」，而它校验用的正是 digest 值。
// 这是本次变更**唯一新增的强制校验点**：一旦这里退化成「不校验」，坏文件会进
// 缓存，所有回退到代理的设备一起失败，而且日志里只有一行没有异常的 info。

// sha256HexForTest 算 payload 的十六进制摘要（造「匹配 / 不匹配」两个场景）。
func sha256HexForTest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// setClawUpgradeCachedReleaseForTest 把 latest 缓存直接置成给定版本与摘要。
// 生产里这份缓存由 resolveClawUpgradeLatest 填充（digest 兜底走的是同一条路），
// 下载代理只读它 —— 所以这里直接写缓存就等价于「latest 解析出了这些摘要」。
func setClawUpgradeCachedReleaseForTest(version string, sums map[string]string) {
	clawUpgradeLatest.mu.Lock()
	clawUpgradeLatest.release = &clawUpgradeRelease{
		Version: version,
		Tag:     clawUpgradeTagForVersion(version),
		Assets:  map[string]string{},
		SHA256:  sums,
	}
	clawUpgradeLatest.mu.Unlock()
}

// clawDownloadChecksumSetup 起假 release 站点并钉好环境，返回缓存目录与 payload。
func clawDownloadChecksumSetup(t *testing.T, payload []byte) (cacheDir string, hits *int32) {
	t.Helper()
	server, serverHits := fakeReleaseWeb(t, clawUpgradeWindowsAsset, payload)
	cacheDir = t.TempDir()
	t.Setenv("CLAW_UPGRADE_RELEASE_WEB", server.URL)
	t.Setenv("CLAW_UPGRADE_CACHE_DIR", cacheDir)
	t.Setenv("MULTICA_CLAW_UPGRADE_TOKEN_SECRET", "test-secret")
	clawUpgradeResetCacheForTest()
	t.Cleanup(clawUpgradeResetCacheForTest)
	return cacheDir, serverHits
}

// cachedAssetPath 返回某个版本目录下的落盘路径，并断言目录里没留下 .part 残片。
func cachedAssetPath(t *testing.T, cacheDir, version string) (string, bool) {
	t.Helper()
	dir := filepath.Join(cacheDir, version)
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read cache dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".part") {
			t.Fatalf("校验失败后不该留下半截文件: %s", entry.Name())
		}
	}
	dest := filepath.Join(dir, clawUpgradeWindowsAsset)
	info, err := os.Stat(dest)
	return dest, err == nil && info.Size() > 0
}

// 摘要不匹配 → 必须拒绝，且**不落盘**：坏文件进了缓存，所有回退到代理的设备
// 会一起失败（设备侧 checksum_mismatch），而服务端只剩一行 info。
func TestClawUpgradeDownloadRejectsAssetThatFailsTheChecksum(t *testing.T) {
	payload := bytes.Repeat([]byte("clawmessenger-upgrade-payload\n"), 32)
	cacheDir, hits := clawDownloadChecksumSetup(t, payload)
	setClawUpgradeCachedReleaseForTest("1.3.0", map[string]string{
		// 故意给一个「格式合法但和内容对不上」的摘要。
		clawUpgradeWindowsAsset: strings.Repeat("0", 64),
	})

	token := signClawUpgradeToken("1.3.0", clawUpgradeWindowsAsset, time.Now().Add(time.Minute))
	rec := downloadRequest(t, fmt.Sprintf("version=1.3.0&asset=%s&token=%s", clawUpgradeWindowsAsset, token))

	if rec.Code == http.StatusOK {
		t.Fatalf("摘要不匹配的资产必须被拒绝，实际 200 且发了 %d 字节", rec.Body.Len())
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("摘要不匹配应回 502（回源失败），got %d: %s", rec.Code, rec.Body.String())
	}
	if bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatal("被拒绝的响应里不该出现资产内容")
	}
	if _, cached := cachedAssetPath(t, cacheDir, "1.3.0"); cached {
		t.Fatal("校验失败的资产绝不能落盘（否则所有回退到代理的设备都会拿到坏文件）")
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("应该恰好回源一次（下载完才发现不匹配），hits = %d", got)
	}
}

// 摘要匹配 → 正常下发，且盘上缓存的内容与上游逐字节一致。
// 这是「校验通过」的正面路径：只测拒绝会让「永远拒绝」也绿。
func TestClawUpgradeDownloadServesAssetThatPassesTheChecksum(t *testing.T) {
	payload := bytes.Repeat([]byte("clawmessenger-upgrade-payload\n"), 32)
	cacheDir, _ := clawDownloadChecksumSetup(t, payload)
	setClawUpgradeCachedReleaseForTest("1.3.0", map[string]string{
		clawUpgradeWindowsAsset: sha256HexForTest(payload),
	})

	token := signClawUpgradeToken("1.3.0", clawUpgradeWindowsAsset, time.Now().Add(time.Minute))
	rec := downloadRequest(t, fmt.Sprintf("version=1.3.0&asset=%s&token=%s", clawUpgradeWindowsAsset, token))

	if rec.Code != http.StatusOK {
		t.Fatalf("摘要匹配时必须正常下发，got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("下发内容与上游不一致（%d 字节）", rec.Body.Len())
	}
	dest, cached := cachedAssetPath(t, cacheDir, "1.3.0")
	if !cached {
		t.Fatal("校验通过的资产必须落盘（下一台设备直接吃缓存）")
	}
	onDisk, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read cached asset: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Fatal("盘上缓存的内容与上游不一致")
	}
}

// 摘要为空（该版本/该资产没有任何摘要来源）→ 现状是**不校验也不拒绝**，照常
// 下发并缓存。
//
// 这里钉的是现状，不是「推荐做法」：设备的升级指令里本来就带 sha256，设备侧还会
// 校验一次；所以服务端这一步是「多一道保险」而不是唯一门禁。真正不允许发生的是
// **静默把不匹配的当匹配**（用例 A 已钉住）。若将来改成「没有摘要就拒绝」，这条
// 用例必须同步改 —— 那种改动会让「SUMS 与 digest 都缺」的版本连代理都下不了。
func TestClawUpgradeDownloadSkipsChecksumWhenTheReleaseHasNone(t *testing.T) {
	payload := bytes.Repeat([]byte("clawmessenger-upgrade-payload\n"), 32)
	cacheDir, _ := clawDownloadChecksumSetup(t, payload)
	// 空 map：latest 解析完成了，但没有任何来源给出这个资产的摘要。
	setClawUpgradeCachedReleaseForTest("1.3.0", map[string]string{})

	token := signClawUpgradeToken("1.3.0", clawUpgradeWindowsAsset, time.Now().Add(time.Minute))
	rec := downloadRequest(t, fmt.Sprintf("version=1.3.0&asset=%s&token=%s", clawUpgradeWindowsAsset, token))

	if rec.Code != http.StatusOK {
		t.Fatalf("没有摘要时的现状是照常下发（设备侧仍会校验），got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("下发内容与上游不一致（%d 字节）", rec.Body.Len())
	}
	if _, cached := cachedAssetPath(t, cacheDir, "1.3.0"); !cached {
		t.Fatal("没有摘要时现状也会落盘（缓存语义不该被校验分支改变）")
	}
}
