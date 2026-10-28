package handler

// 设备 exe 升级：latest 解析里「校验和从哪来」的测试。
//
// 这一组用例钉住的是管理端点「升级设备 exe」报 400「该平台资产缺少校验和」
// 那个故障。根因不是判据写错，而是 `release.SHA256` 只从 `SHA256SUMS` 取，
// 而那是**第二次** HTTP 下载；那一步失败被静默吞掉 → 空 map → **所有平台**
// 都被判成 asset_missing。
//
// 因此这里要钉的三件事：
//  1. `assets[].digest` 能在「没有 SHA256SUMS」时独立供给摘要（用例 1）；
//  2. 两个来源的优先级明确：SUMS 权威、digest 只补缺（用例 2、9）；
//  3. 任何一条来源坏掉都不能让整个解析失败 —— 否则症状会从「一个 400」
//     升级成「页面连版本都看不到」（用例 3、4、5）。
//
// 全部离线：`CLAW_UPGRADE_RELEASE_API` / `_WEB` 一律指向 httptest 假服务；
// 「网络错误」那条用自定义 RoundTripper 伪造，并且该 transport 对假服务以外的
// host 直接返回错误 —— 任何一次误打 api.github.com / github.com 都会当场变红，
// 而不是默默联网跑通。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// 三个互不相同的 64 位十六进制摘要，用来区分「SUMS 的值」与「digest 的值」。
const (
	clawDigestTestHex  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	clawSumsTestHex    = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	clawDigestOtherHex = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

// clawDigestFakeAsset 是假 Release 里的一条 `assets[]`。
// Digest 为空串时 JSON 里**不带** digest 字段 —— 模拟「老版本 GitHub / 镜像站
// 没回填 digest」的真实形态，而不是带一个空串。
type clawDigestFakeAsset struct {
	Name   string
	URL    string
	Digest string
}

// clawDigestFakeGitHub 起一个离线的假 Releases API：
//   - `/repos/.../releases/latest` 回 tag `clawmessenger-v1.4.0` 与给定 assets，
//     URL 里的 `{HOST}` 会替换成这个假服务的地址（SUMS 的下载地址必须落在假服务
//     上，否则用例会真的去下载）；
//   - `/sums` 交给 sums 处理（nil = 该资产压根没提供，下载路径不该被请求到）；
//   - 其余路径 404，并且 hits 会涨 —— 未预期请求是可见的，不会静默通过。
func clawDigestFakeGitHub(t *testing.T, assets []clawDigestFakeAsset, sums http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			payload := make([]map[string]string, 0, len(assets))
			for _, asset := range assets {
				item := map[string]string{
					"name":                 asset.Name,
					"browser_download_url": strings.ReplaceAll(asset.URL, "{HOST}", srv.URL),
				}
				if asset.Digest != "" {
					item["digest"] = asset.Digest
				}
				payload = append(payload, item)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "clawmessenger-v1.4.0",
				"assets":   payload,
			})
		case r.URL.Path == "/sums":
			if sums == nil {
				http.NotFound(w, r)
				return
			}
			sums(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// clawSumsBody 拼一份 SHA256SUMS（GitHub 上的两列格式：`<hex>  <name>`）。
func clawSumsBody(entries ...[2]string) string {
	var b strings.Builder
	for _, entry := range entries {
		fmt.Fprintf(&b, "%s  %s\n", entry[0], entry[1])
	}
	return b.String()
}

// clawSumsHandler 把一份 SUMS 内容当 200 回出去。
func clawSumsHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}
}

// clawDigestFetch 用指定的 client 跑一次 fetchClawUpgradeLatest，并把环境变量钉
// 到假服务上（t.Setenv 自动还原）。缓存必须每个用例清一次，否则上一个用例的
// latest 会被下一个看到。
func clawDigestFetch(t *testing.T, srv *httptest.Server, client *http.Client) (*clawUpgradeRelease, error) {
	t.Helper()
	clawUpgradeResetCacheForTest()
	t.Cleanup(clawUpgradeResetCacheForTest)
	t.Setenv("CLAW_UPGRADE_RELEASE_API", srv.URL)
	t.Setenv("CLAW_UPGRADE_RELEASE_WEB", srv.URL)
	if client == nil {
		client = &http.Client{}
	}
	t.Cleanup(clawUpgradeSetHTTPClientForTest(client))
	return fetchClawUpgradeLatest(context.Background(), client)
}

// clawDigestWindowsAsset 是「带 digest 的 windows 资产」这条最常见的形状。
func clawDigestWindowsAsset(digest string) clawDigestFakeAsset {
	return clawDigestFakeAsset{
		Name:   clawUpgradeWindowsAsset,
		URL:    "{HOST}/download/" + clawUpgradeWindowsAsset,
		Digest: digest,
	}
}

// ---- 用例 1：只有 digest，没有 SHA256SUMS ----------------------------------

// 这是这次 400 的正面场景：Release 里没有 SHA256SUMS 资产（镜像站只同步了
// 二进制、或发布流水线没上传它），以前 release.SHA256 直接是空 map，于是所有平台
// 都「缺少校验和」。digest 与资产清单在同一个响应里，天然不会漏。
func TestClawUpgradeFetchLatestUsesAssetDigestWhenSumsIsAbsent(t *testing.T) {
	srv, hits := clawDigestFakeGitHub(t, []clawDigestFakeAsset{
		clawDigestWindowsAsset("sha256:" + clawDigestTestHex),
	}, nil)

	release, err := clawDigestFetch(t, srv, nil)
	if err != nil {
		t.Fatalf("Release 没有 SHA256SUMS 时解析不能失败（只有一个版本的兜底来源）: %v", err)
	}
	if got := release.SHA256[clawUpgradeWindowsAsset]; got != clawDigestTestHex {
		t.Fatalf("release.SHA256[%s] = %q，期望 digest 提供的 %q —— 只从 SHA256SUMS 取值就会退化成空 map，正是本次 400 的成因",
			clawUpgradeWindowsAsset, got, clawDigestTestHex)
	}
	if got := release.Assets[clawUpgradeWindowsAsset]; !strings.HasSuffix(got, "/download/"+clawUpgradeWindowsAsset) {
		t.Fatalf("资产下载地址没被收下: %v", release.Assets)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("没有 SHA256SUMS 资产时不该发生第二次下载，实际请求数 = %d", got)
	}
}

// ---- 用例 2：SUMS 与 digest 冲突，SUMS 权威 -------------------------------

// 设备侧是按 SUMS 校验的。若服务端用 digest 覆盖 SUMS，结果是「服务端说 A、
// 设备算出来 B」→ 下载后校验失败，且失败点离这里很远、很难回溯。
func TestClawUpgradeFetchLatestPrefersSumsOverDigestOnConflict(t *testing.T) {
	srv, _ := clawDigestFakeGitHub(t, []clawDigestFakeAsset{
		clawDigestWindowsAsset("sha256:" + clawDigestTestHex),
		{Name: "SHA256SUMS", URL: "{HOST}/sums"},
	}, clawSumsHandler(clawSumsBody([2]string{clawSumsTestHex, clawUpgradeWindowsAsset})))

	release, err := clawDigestFetch(t, srv, nil)
	if err != nil {
		t.Fatalf("两个来源都有值时更不能失败: %v", err)
	}
	if got := release.SHA256[clawUpgradeWindowsAsset]; got != clawSumsTestHex {
		t.Fatalf("release.SHA256[%s] = %q，期望 SUMS 的 %q（digest 的 %q 不得覆盖权威清单）",
			clawUpgradeWindowsAsset, got, clawSumsTestHex, clawDigestTestHex)
	}
}

// ---- 用例 3：SUMS 返回 500 ------------------------------------------------

// 镜像站/上游偶尔 5xx。这条不该把「拿不到 SUMS」升级成「拿不到版本」。
func TestClawUpgradeFetchLatestKeepsDigestWhenSumsReturns500(t *testing.T) {
	srv, hits := clawDigestFakeGitHub(t, []clawDigestFakeAsset{
		clawDigestWindowsAsset("sha256:" + clawDigestTestHex),
		{Name: "SHA256SUMS", URL: "{HOST}/sums"},
	}, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream boom", http.StatusInternalServerError)
	})

	release, err := clawDigestFetch(t, srv, nil)
	if err != nil {
		t.Fatalf("SHA256SUMS 返回 500 时解析必须仍然成功（digest 兜底）: %v", err)
	}
	if got := release.SHA256[clawUpgradeWindowsAsset]; got != clawDigestTestHex {
		t.Fatalf("SUMS 500 后 digest 必须顶上，got %q", got)
	}
	if got := hits.Load(); got < 2 {
		t.Fatalf("用例前提没成立：SUMS 的下载分支没被请求到（hits = %d）", got)
	}
}

// ---- 用例 4：SUMS 下载网络错误 --------------------------------------------

// clawDigestFailSumsTransport 让 SHA256SUMS 的下载在网络层直接出错（连接被重置
// 这一类），其余请求转给 base。对假服务以外的 host 一律报错：用例必须离线，
// 一旦有代码路径想去打真 GitHub，这里就会红。
type clawDigestFailSumsTransport struct {
	base      http.RoundTripper
	allowHost string
	sumsPath  string
	sumsCalls atomic.Int32
}

func (t *clawDigestFailSumsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.allowHost != "" && req.URL.Host != t.allowHost {
		return nil, fmt.Errorf("测试禁止访问 %s（只允许假服务 %s）", req.URL.Host, t.allowHost)
	}
	if req.URL.Path == t.sumsPath {
		t.sumsCalls.Add(1)
		return nil, fmt.Errorf("模拟 SHA256SUMS 下载中断: read tcp: connection reset by peer")
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// 这是 Max 明确指出「改动前完全没有自动化覆盖」的那条分支：SUMS 下载在传输层
// 就失败（sumErr != nil，而不是非 200）。它有独立的 slog.Warn 分支，也正是本次
// 故障最可能的形态 —— 静默吞掉 → 所有平台缺校验和 → 管理端一个没有上下文的 400。
func TestClawUpgradeFetchLatestKeepsDigestWhenSumsDownloadErrors(t *testing.T) {
	srv, _ := clawDigestFakeGitHub(t, []clawDigestFakeAsset{
		clawDigestWindowsAsset("sha256:" + clawDigestTestHex),
		{Name: "SHA256SUMS", URL: "{HOST}/sums"},
	}, clawSumsHandler(clawSumsBody([2]string{clawSumsTestHex, clawUpgradeWindowsAsset})))

	transport := &clawDigestFailSumsTransport{
		allowHost: strings.TrimPrefix(srv.URL, "http://"),
		sumsPath:  "/sums",
	}
	release, err := clawDigestFetch(t, srv, &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("SHA256SUMS 下载网络出错时解析必须仍然成功（只有 digest 一路也要能下发）: %v", err)
	}
	if got := transport.sumsCalls.Load(); got == 0 {
		t.Fatalf("用例前提没成立：SUMS 的下载分支没有被走到，这条断言等于没测")
	}
	if got := release.SHA256[clawUpgradeWindowsAsset]; got != clawDigestTestHex {
		t.Fatalf("SUMS 下载失败后 digest 必须顶上，got %q（期望 %q）", got, clawDigestTestHex)
	}
}

// ---- 用例 4b：SUMS 读 Body 中断 -------------------------------------------

// clawDigestBrokenBody 是一个「状态码 200 但读到一半就断」的响应体 —— 镜像站返回
// 了头却把连接掐了，正是 `io.ReadAll` 出错那条分支（④）的形态。
type clawDigestBrokenBody struct{}

func (clawDigestBrokenBody) Read([]byte) (int, error) {
	return 0, fmt.Errorf("模拟 SHA256SUMS 读取中断: unexpected EOF")
}

func (clawDigestBrokenBody) Close() error { return nil }

// clawDigestBrokenSumsBodyTransport 让 SUMS 回一个「200 + 半截 body」的响应。
type clawDigestBrokenSumsBodyTransport struct {
	base      http.RoundTripper
	allowHost string
	sumsPath  string
	sumsCalls atomic.Int32
}

func (t *clawDigestBrokenSumsBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.allowHost != "" && req.URL.Host != t.allowHost {
		return nil, fmt.Errorf("测试禁止访问 %s（只允许假服务 %s）", req.URL.Host, t.allowHost)
	}
	if req.URL.Path == t.sumsPath {
		t.sumsCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       clawDigestBrokenBody{},
			Request:    req,
		}, nil
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// 这条钉住第四个失败分支（读 Body 出错）：SUMS 看起来「成功」但内容不完整，
// 不能把半份清单当完整清单用。
func TestClawUpgradeFetchLatestKeepsDigestWhenSumsBodyReadFails(t *testing.T) {
	srv, _ := clawDigestFakeGitHub(t, []clawDigestFakeAsset{
		clawDigestWindowsAsset("sha256:" + clawDigestTestHex),
		{Name: "SHA256SUMS", URL: "{HOST}/sums"},
	}, clawSumsHandler(clawSumsBody([2]string{clawSumsTestHex, clawUpgradeWindowsAsset})))

	transport := &clawDigestBrokenSumsBodyTransport{
		allowHost: strings.TrimPrefix(srv.URL, "http://"),
		sumsPath:  "/sums",
	}
	release, err := clawDigestFetch(t, srv, &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("SUMS 读 Body 中断时解析必须仍然成功（digest 兜底）: %v", err)
	}
	if got := transport.sumsCalls.Load(); got == 0 {
		t.Fatalf("用例前提没成立：SUMS 的下载分支没有被走到")
	}
	if got := release.SHA256[clawUpgradeWindowsAsset]; got != clawDigestTestHex {
		t.Fatalf("SUMS 内容读不完整时 digest 必须顶上，got %q（期望 %q）", got, clawDigestTestHex)
	}
}

// ---- 用例 5：两源都缺（fail-closed 未放宽） -------------------------------

// 拒绝必须发生在上层（下发那一刻 `len(sum) != 64`），但解析层不能凭空造一个
// 摘要出来 —— 那等于放行一个无法校验的二进制。
func TestClawUpgradeFetchLatestLeavesAssetWithoutDigestWhenBothSourcesMiss(t *testing.T) {
	const linuxAsset = "clawmessenger-linux-x64"

	cases := []struct {
		name   string
		assets []clawDigestFakeAsset
		sums   http.HandlerFunc
	}{
		{
			name: "SUMS 存在但不含该资产且无 digest",
			assets: []clawDigestFakeAsset{
				{Name: clawUpgradeWindowsAsset, URL: "{HOST}/download/" + clawUpgradeWindowsAsset},
				{Name: linuxAsset, URL: "{HOST}/download/" + linuxAsset},
				{Name: "SHA256SUMS", URL: "{HOST}/sums"},
			},
			sums: clawSumsHandler(clawSumsBody([2]string{clawDigestOtherHex, linuxAsset})),
		},
		{
			name: "完全没有 SHA256SUMS 也没有 digest",
			assets: []clawDigestFakeAsset{
				{Name: clawUpgradeWindowsAsset, URL: "{HOST}/download/" + clawUpgradeWindowsAsset},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := clawDigestFakeGitHub(t, tc.assets, tc.sums)
			release, err := clawDigestFetch(t, srv, nil)
			if err != nil {
				t.Fatalf("缺摘要不是解析错误（拒绝发生在下发那一刻），got err = %v", err)
			}
			if sum, exists := release.SHA256[clawUpgradeWindowsAsset]; exists {
				t.Fatalf("两源都缺时 SHA256 里不该有 %s 的条目（否则上层 len(sum)!=64 判据失效，会下发无法校验的包），got %q",
					clawUpgradeWindowsAsset, sum)
			}
			// 反向检查：SUMS 本身确实被解析了，所以上面那条不是「整份 SUMS 都没读到」造成的假绿。
			if tc.sums != nil {
				if got := release.SHA256[linuxAsset]; got != clawDigestOtherHex {
					t.Fatalf("用例前提没成立：SUMS 里 linux 的摘要没被解析出来，got %q", got)
				}
			}
		})
	}
}

// ---- 用例 6：parseClawAssetDigest 的表驱动 --------------------------------

// 只认 `sha256:<64 hex>`。其余形态一律返回空串（调用方当作「没有摘要」，绝不猜）：
// 猜错的代价是设备下载后校验失败，那一步在设备侧不可恢复。
// 命名里带 `TestClawUpgrade` 前缀是有意的：包外文档与验收清单里用的是
// `-run 'TestClawUpgrade'` 这条快速过滤，名字不以它开头的用例会被静默漏掉
// （实测 `-run 'TestClawUpgrade'` 只命中 17 例、本文件 0 例）。改名后
// `TestClawUpgrade` / `ClawUpgrade` / `Upgrade|ClawUpgrade` 三种过滤都能选中。
func TestClawUpgradeParseAssetDigestAcceptsOnlySHA256With64Hex(t *testing.T) {
	upper := strings.ToUpper(clawDigestTestHex)

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"小写算法前缀加小写摘要", "sha256:" + clawDigestTestHex, clawDigestTestHex},
		{"大写算法前缀加大写摘要统一成小写", "SHA256:" + upper, clawDigestTestHex},
		{"混合大小写算法前缀", "Sha256:" + upper, clawDigestTestHex},
		{"前后带空白", "  sha256:" + clawDigestTestHex + "\t\n", clawDigestTestHex},
		{"冒号后带空格", "sha256: " + upper + " ", clawDigestTestHex},
		{"sha512 前缀一律拒绝", "sha512:" + clawDigestTestHex, ""},
		{"无算法前缀的裸 64 位摘要", clawDigestTestHex, ""},
		{"63 位", "sha256:" + clawDigestTestHex[:63], ""},
		{"65 位", "sha256:" + clawDigestTestHex + "a", ""},
		{"空串", "", ""},
		{"只有空白", "   ", ""},
		{"无冒号", "sha256" + clawDigestTestHex, ""},
		{"冒号后为空", "sha256:", ""},
		{"含非十六进制字符", "sha256:" + clawDigestTestHex[:63] + "g", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseClawAssetDigest(tc.raw); got != tc.want {
				t.Fatalf("parseClawAssetDigest(%q) = %q，期望 %q", tc.raw, got, tc.want)
			}
		})
	}
}

// ---- 用例 7：SUMS 正常且 digest 同值 —— 与改动前逐字一致 ------------------

// 回归面：改动前 SHA256 就是 SUMS 的内容。加入 digest 合并后，这条路径的返回值
// 必须一个字都不变（多出来的只有「SUMS 缺的资产」）。
func TestClawUpgradeFetchLatestKeepsSumsResultWhenDigestAgrees(t *testing.T) {
	const linuxAsset = "clawmessenger-linux-x64"

	srv, _ := clawDigestFakeGitHub(t, []clawDigestFakeAsset{
		clawDigestWindowsAsset("sha256:" + clawDigestTestHex),
		{Name: linuxAsset, URL: "{HOST}/download/" + linuxAsset},
		{Name: "SHA256SUMS", URL: "{HOST}/sums"},
	}, clawSumsHandler(clawSumsBody(
		[2]string{clawDigestTestHex, clawUpgradeWindowsAsset},
		[2]string{clawDigestOtherHex, linuxAsset},
	)))

	release, err := clawDigestFetch(t, srv, nil)
	if err != nil {
		t.Fatalf("SUMS 正常时解析不能失败: %v", err)
	}
	if got := release.SHA256[clawUpgradeWindowsAsset]; got != clawDigestTestHex {
		t.Fatalf("SUMS 与 digest 同值时结果被改动了: got %q", got)
	}
	if got := release.SHA256[linuxAsset]; got != clawDigestOtherHex {
		t.Fatalf("SUMS 里只有一份摘要的资产被改动了: got %q", got)
	}
	if got := len(release.SHA256); got != 2 {
		t.Fatalf("SHA256 条目数 = %d，期望 2（合并只增不减、不写空值）: %v", got, release.SHA256)
	}
	if release.Version != "1.4.0" || release.Tag != "clawmessenger-v1.4.0" {
		t.Fatalf("版本解析被影响: version=%q tag=%q", release.Version, release.Tag)
	}
}

// ---- 用例 8：URL 为空、Name 非空 -----------------------------------------

// 契约（`fetchClawUpgradeLatest` 里明确写的）：Name 非空即进入处理，但只有 URL
// 非空才进 `Assets`。摘要收集与下载地址无关 —— 一个「有摘要没有地址」的资产仍要
// 留下摘要，这样地址修好后不用等缓存过期。
func TestClawUpgradeFetchLatestCollectsDigestForAssetWithoutDownloadURL(t *testing.T) {
	srv, _ := clawDigestFakeGitHub(t, []clawDigestFakeAsset{{
		Name:   clawUpgradeWindowsAsset,
		URL:    "",
		Digest: "sha256:" + clawDigestTestHex,
	}}, nil)

	release, err := clawDigestFetch(t, srv, nil)
	if err != nil {
		t.Fatalf("缺下载地址不是解析错误: %v", err)
	}
	if got, exists := release.Assets[clawUpgradeWindowsAsset]; exists {
		t.Fatalf("URL 为空的资产不该进 Assets（会被当成直连地址用）：got %q，Assets=%v", got, release.Assets)
	}
	if got := release.SHA256[clawUpgradeWindowsAsset]; got != clawDigestTestHex {
		t.Fatalf("摘要收集与下载地址无关，digest 仍必须被收下: got %q，期望 %q", got, clawDigestTestHex)
	}
	// 「URL 为空」在生产上唯一有意义的后果：下发指令时用 Tag 拼出直连地址
	// （clawUpgradeMirrors 的 fallback），而不是给设备一个 404 的空地址。
	mirrors, direct := clawUpgradeMirrors(release, clawUpgradeWindowsAsset, "")
	wantDirect := srv.URL + "/" + clawUpgradeReleasesRepo + "/releases/download/" + release.Tag + "/" + clawUpgradeWindowsAsset
	if direct != wantDirect {
		t.Fatalf("clawUpgradeMirrors 的直连地址 = %q，期望按 Tag 拼出的 %q（Assets 里没地址时的唯一退路）", direct, wantDirect)
	}
	if len(mirrors) != 1 || mirrors[0] != wantDirect {
		t.Fatalf("没有下载凭证时 mirrors 只该有一条直连地址，got %v", mirrors)
	}
}

// ---- 用例 9：digest 只补缺（两源互补） ------------------------------------

// SUMS 里只有 linux，digest 只有 windows：两个都要在，且各自是自己的值。
// 这条同时反向验证了「只补缺」：不是「有 SUMS 就整体忽略 digest」。
func TestClawUpgradeFetchLatestDigestFillsOnlyAssetsMissingFromSums(t *testing.T) {
	const linuxAsset = "clawmessenger-linux-x64"

	srv, _ := clawDigestFakeGitHub(t, []clawDigestFakeAsset{
		clawDigestWindowsAsset("sha256:" + clawDigestTestHex),
		{Name: linuxAsset, URL: "{HOST}/download/" + linuxAsset},
		{Name: "SHA256SUMS", URL: "{HOST}/sums"},
	}, clawSumsHandler(clawSumsBody([2]string{clawDigestOtherHex, linuxAsset})))

	release, err := clawDigestFetch(t, srv, nil)
	if err != nil {
		t.Fatalf("解析不能失败: %v", err)
	}
	if got := release.SHA256[clawUpgradeWindowsAsset]; got != clawDigestTestHex {
		t.Fatalf("SUMS 缺的资产必须由 digest 补上，got %q（期望 %q）", got, clawDigestTestHex)
	}
	if got := release.SHA256[linuxAsset]; got != clawDigestOtherHex {
		t.Fatalf("SUMS 已有的资产不该被 digest 影响，got %q（期望 %q）", got, clawDigestOtherHex)
	}
}

// ---- 用例 10：下载代理侧的强制校验点（clawUpgradeExpectedSum） -------------

// 服务端下载代理在缓存资产时用这个摘要做一次校验（upgrade_download.go:204）。
//
// 为什么这条现在必须测：改动前「没有 SHA256SUMS 的版本」根本下发不出去，这条校验
// 几乎从不触发；改动后这类版本会正常下发，于是它变成**本次唯一新增的强制校验点**，
// 而它校验用的正是 digest 值。如果 digest 没被写进缓存，这里就退化成「不校验」——
// 一个坏文件进缓存，所有回退到代理的设备一起失败，且日志里没有任何异常。
func TestClawUpgradeExpectedSumReadsDigestFallbackFromCachedRelease(t *testing.T) {
	srv, _ := clawDigestFakeGitHub(t, []clawDigestFakeAsset{
		clawDigestWindowsAsset("sha256:" + clawDigestTestHex),
	}, nil)

	// 故意走真实的「解析 + 缓存」路径而不是手写缓存字段：要证明 digest-only 的
	// 解析结果穿过缓存层之后，下载代理仍然读得到。
	clawUpgradeResetCacheForTest()
	t.Cleanup(clawUpgradeResetCacheForTest)
	t.Setenv("CLAW_UPGRADE_RELEASE_API", srv.URL)
	t.Setenv("CLAW_UPGRADE_RELEASE_WEB", srv.URL)
	t.Cleanup(clawUpgradeSetHTTPClientForTest(&http.Client{}))

	release, stale, err := resolveClawUpgradeLatest(context.Background())
	if err != nil || stale {
		t.Fatalf("解析/缓存 latest 失败: err=%v stale=%v", err, stale)
	}
	if got := clawUpgradeExpectedSum(release.Version, clawUpgradeWindowsAsset); got != clawDigestTestHex {
		t.Fatalf("clawUpgradeExpectedSum(%q, %s) = %q，期望 digest 兜底值 %q —— 返回空就等于下载代理静默跳过校验",
			release.Version, clawUpgradeWindowsAsset, got, clawDigestTestHex)
	}
	// 缓存里的是 latest：旧版本不该拿 latest 的摘要去校验（那样必然 mismatch）。
	if got := clawUpgradeExpectedSum("1.3.9", clawUpgradeWindowsAsset); got != "" {
		t.Fatalf("非 latest 版本必须返回空摘要，got %q", got)
	}
	// 该版本没有摘要的资产同理。
	if got := clawUpgradeExpectedSum(release.Version, "clawmessenger-linux-arm64"); got != "" {
		t.Fatalf("没有摘要的资产必须返回空，got %q", got)
	}
	// 冷启动（缓存还没成功过）不许 panic，也不许编一个摘要出来。
	clawUpgradeResetCacheForTest()
	if got := clawUpgradeExpectedSum(release.Version, clawUpgradeWindowsAsset); got != "" {
		t.Fatalf("缓存为空时必须返回空摘要，got %q", got)
	}
}

// ---- 用例 11：一次响应里同名资产出现两次（last-wins） ---------------------

// GitHub 不允许同名资产，但镜像站 / 自建兼容 API 可能重复。当前实现是 last-wins：
// `Assets` 与 `digestByAsset` 都是后写覆盖前写。
//
// 这里**钉住现状**，不是断言它「正确」：如果将来改成「保留首个 + slog.Warn」，
// 这条用例必须同步改 —— 目的是别让这个行为悄悄漂移（静默变成 first-wins 会让
// 「服务端下发的摘要」和「设备实际下载到的文件」错配，且没有任何日志线索）。
func TestClawUpgradeFetchLatestKeepsLastDefinitionForDuplicateAssetName(t *testing.T) {
	srv, _ := clawDigestFakeGitHub(t, []clawDigestFakeAsset{
		{
			Name:   clawUpgradeWindowsAsset,
			URL:    "{HOST}/first/" + clawUpgradeWindowsAsset,
			Digest: "sha256:" + clawDigestTestHex,
		},
		{
			Name:   clawUpgradeWindowsAsset,
			URL:    "{HOST}/second/" + clawUpgradeWindowsAsset,
			Digest: "sha256:" + clawDigestOtherHex,
		},
	}, nil)

	release, err := clawDigestFetch(t, srv, nil)
	if err != nil {
		t.Fatalf("同名资产不该让解析失败: %v", err)
	}
	if got := release.Assets[clawUpgradeWindowsAsset]; !strings.HasSuffix(got, "/second/"+clawUpgradeWindowsAsset) {
		t.Fatalf("同名资产的下载地址必须是最后一条（现状 last-wins），got %q", got)
	}
	if got := release.SHA256[clawUpgradeWindowsAsset]; got != clawDigestOtherHex {
		t.Fatalf("同名资产的摘要必须与最后一条地址配对（同一条 assets[] 内自洽），got %q，期望 %q",
			got, clawDigestOtherHex)
	}
}

// ---- 用例 12、13：两条 Warn 必须真的打出来 --------------------------------
//
// 为什么要把日志也钉住：本次故障的本质就是「静默失败」—— SUMS 取不到这件事
// 在日志里一个字都没有，运维只能看到一个没有上下文的 400。R1 为此补了两条
// `slog.Warn`（两源冲突、取校验和失败）。如果后续重构把 Warn 悄悄删掉，行为上
// 没有任何测试会红，症状会原样回来。所以这里挂一个收集记录的 `slog.Handler`。
//
// 只断言**消息子串**，不锁字段顺序与完整文案：文案微调（例如去掉 `url` 字段、
// 换措辞）不该让用例红，但「这条告警没了」必须红。

// clawUpgradeLogSink 把日志记录收集到切片。records/mu 用指针共享，保证
// `With(...)` 派生出来的 handler 也写进同一份记录。
type clawUpgradeLogSink struct {
	mu      *sync.Mutex
	records *[]slog.Record
	attrs   []slog.Attr
}

func newClawUpgradeLogSink() *clawUpgradeLogSink {
	return &clawUpgradeLogSink{mu: &sync.Mutex{}, records: &[]slog.Record{}}
}

func (s *clawUpgradeLogSink) Enabled(context.Context, slog.Level) bool { return true }

func (s *clawUpgradeLogSink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 带上 With(...) 派生的 attrs，让「消息 + 字段」都能被看到（断言只用消息）。
	derived := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		derived.AddAttrs(a)
		return true
	})
	derived.AddAttrs(s.attrs...)
	*s.records = append(*s.records, derived)
	return nil
}

func (s *clawUpgradeLogSink) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return s
	}
	return &clawUpgradeLogSink{mu: s.mu, records: s.records, attrs: append(append([]slog.Attr{}, s.attrs...), attrs...)}
}

func (s *clawUpgradeLogSink) WithGroup(string) slog.Handler { return s }

func (s *clawUpgradeLogSink) snapshot() []slog.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]slog.Record{}, *s.records...)
}

// find 返回第一条消息包含 substr 的记录。
func (s *clawUpgradeLogSink) find(substr string) (slog.Record, bool) {
	for _, r := range s.snapshot() {
		if strings.Contains(r.Message, substr) {
			return r, true
		}
	}
	return slog.Record{}, false
}

// messages 只在断言失败时用来打印现场。
func (s *clawUpgradeLogSink) messages() []string {
	out := make([]string, 0)
	for _, r := range s.snapshot() {
		out = append(out, r.Level.String()+" "+r.Message)
	}
	return out
}

// clawUpgradeCaptureLogs 把默认 logger 换成收集器，并在用例结束时复原。
// 先保存原 logger 再 defer/T.Cleanup 恢复 —— 不恢复会污染同包其它用例的日志
// （它们不关心日志，但把默认 handler 永久换成切片收集器会让真实运维日志消失）。
func clawUpgradeCaptureLogs(t *testing.T) *clawUpgradeLogSink {
	t.Helper()
	previous := slog.Default()
	sink := newClawUpgradeLogSink()
	slog.SetDefault(slog.New(sink))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return sink
}

// 两源冲突：必须以 SUMS 为准**并且**留一条 Warn。
// 静默取一边的表现是「设备 checksum_mismatch、服务端零信号」—— 与本次要修的
// 「静默失败」是同一个病。
func TestClawUpgradeFetchLatestWarnsWhenSumsAndDigestDisagree(t *testing.T) {
	sink := clawUpgradeCaptureLogs(t)

	srv, _ := clawDigestFakeGitHub(t, []clawDigestFakeAsset{
		clawDigestWindowsAsset("sha256:" + clawDigestTestHex),
		{Name: "SHA256SUMS", URL: "{HOST}/sums"},
	}, clawSumsHandler(clawSumsBody([2]string{clawSumsTestHex, clawUpgradeWindowsAsset})))

	release, err := clawDigestFetch(t, srv, nil)
	if err != nil {
		t.Fatalf("两源冲突不是解析错误: %v", err)
	}
	record, ok := sink.find("两源不一致")
	if !ok {
		t.Fatalf("两源给不同摘要时必须留一条 Warn（否则线上只剩设备侧 checksum_mismatch，服务端零信号）。实际日志: %v",
			sink.messages())
	}
	if record.Level != slog.LevelWarn {
		t.Fatalf("这条告警必须是 Warn 级别，got %v", record.Level)
	}
	// 告警不能改变结论：值仍然是 SUMS 的。
	if got := release.SHA256[clawUpgradeWindowsAsset]; got != clawSumsTestHex {
		t.Fatalf("告警之后取值仍是 SUMS 权威，got %q 期望 %q", got, clawSumsTestHex)
	}
}

// SUMS 下载失败：必须留一条「取校验和失败」Warn。
// 这正是本次 400 的原始形态 —— 当时四个失败分支一个字都不打。
func TestClawUpgradeFetchLatestWarnsWhenSumsDownloadFails(t *testing.T) {
	sink := clawUpgradeCaptureLogs(t)

	srv, _ := clawDigestFakeGitHub(t, []clawDigestFakeAsset{
		clawDigestWindowsAsset("sha256:" + clawDigestTestHex),
		{Name: "SHA256SUMS", URL: "{HOST}/sums"},
	}, clawSumsHandler(clawSumsBody([2]string{clawSumsTestHex, clawUpgradeWindowsAsset})))

	transport := &clawDigestFailSumsTransport{
		allowHost: strings.TrimPrefix(srv.URL, "http://"),
		sumsPath:  "/sums",
	}
	release, err := clawDigestFetch(t, srv, &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("SUMS 下载失败不是解析错误: %v", err)
	}
	record, ok := sink.find("取校验和失败")
	if !ok {
		t.Fatalf("SUMS 下载失败必须留一条 Warn（本次 400 的成因就是这条静默）。实际日志: %v", sink.messages())
	}
	if record.Level != slog.LevelWarn {
		t.Fatalf("这条告警必须是 Warn 级别，got %v", record.Level)
	}
	// 兜底仍然生效：告警不等于放弃 digest。
	if got := release.SHA256[clawUpgradeWindowsAsset]; got != clawDigestTestHex {
		t.Fatalf("告警之后 digest 兜底仍必须生效，got %q 期望 %q", got, clawDigestTestHex)
	}
}
