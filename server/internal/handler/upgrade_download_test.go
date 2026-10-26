package handler

// 下载代理端点的用例 —— 这个端点此前是**零覆盖**（验证报告 §8.2 的变异 M6：删掉
// 资产白名单，本文件之前的用例全绿），而它是整个功能里唯一公网可达、无 admin 鉴权
// 的入口。所以这里既覆盖正常路径（内容逐字节一致、缓存只回源一次、Range 续传），
// 也把「白名单/签名/过期/缺密钥」全部钉死。

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
